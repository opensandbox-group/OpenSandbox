// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package isolation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const cleanupRegistryName = ".execd-cleanup"

// UpperManager manages upper directories for overlay workspaces.
type UpperManager struct {
	root         string
	cleanupRoot  string
	maxBytes     int64
	removeAll    func(string) error
	removeRecord func(string) error
	mu           sync.Mutex
	entries      map[string]*UpperEntry
}

// UpperDirPair is one allocated upper + work directory pair for a single
// overlay mount.
type UpperDirPair struct {
	UpperDir string
	WorkDir  string
}

// UpperEntry tracks the directory pairs allocated to one session.
type UpperEntry struct {
	Pairs []UpperDirPair
	InUse bool
	dir   string
}

// NewUpperManager creates an upper directory manager. A durable cleanup
// registry under root records every execd-owned session outside the session
// subtree, so cleanup can resume after partial deletion or process restart.
// Unrecorded children are operator data and are never reclaimed.
func NewUpperManager(root string, maxBytes int64) (*UpperManager, error) {
	if root == "" {
		return nil, errors.New("upper: root path is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("upper: create root %s: %w", root, err)
	}
	cleanupRoot := filepath.Join(root, cleanupRegistryName)
	if err := ensurePrivateDir(cleanupRoot); err != nil {
		return nil, fmt.Errorf("upper: initialize cleanup registry %s: %w", cleanupRoot, err)
	}
	m := &UpperManager{
		root:         root,
		cleanupRoot:  cleanupRoot,
		maxBytes:     maxBytes,
		removeAll:    os.RemoveAll,
		removeRecord: os.Remove,
		entries:      make(map[string]*UpperEntry),
	}
	if err := m.reclaimStale(); err != nil {
		return nil, err
	}
	return m, nil
}

// reclaimStale recovers sessions recorded by a previous execd lifetime.
// Cleanup records are outside the removable session subtree, so an
// interrupted RemoveAll can no longer erase the only proof of ownership.
func (m *UpperManager) reclaimStale() error {
	records, err := os.ReadDir(m.cleanupRoot)
	if err != nil {
		return fmt.Errorf("upper: list cleanup records under %s: %w", m.cleanupRoot, err)
	}

	var removed int
	var failed int
	var skipped int
	for _, record := range records {
		id := record.Name()
		if !validSessionID(id) {
			skipped++
			continue
		}

		recordPath := filepath.Join(m.cleanupRoot, id)
		info, err := os.Lstat(recordPath)
		if err != nil {
			failed++
			log.Warn("upper: inspect cleanup record %s: %v", recordPath, err)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			skipped++
			continue
		}

		sessionDir := filepath.Join(m.root, id)
		entry := &UpperEntry{
			Pairs: sessionPairs(sessionDir),
			InUse: false,
			dir:   sessionDir,
		}
		m.entries[id] = entry
		if err := m.cleanupEntryLocked(id, entry); err != nil {
			failed++
			log.Warn("upper: reclaim recorded session dir %s: %v", sessionDir, err)
			continue
		}
		removed++
	}
	if removed > 0 || failed > 0 || skipped > 0 {
		log.Info(
			"upper: reclaimed %d recorded stale session dir(s) under %s (%d failed, %d unrecognized skipped)",
			removed, m.root, failed, skipped,
		)
	}
	return nil
}

var ErrUpperLimitExceeded = errors.New("upper: total usage exceeds configured limit")

// Allocate creates a new session directory holding a single upper + work
// pair. Returns the session ID and the directories. Returns
// ErrUpperLimitExceeded if maxBytes > 0 and current usage already meets or
// exceeds the limit.
func (m *UpperManager) Allocate() (sessionID, upperDir, workDir string, err error) {
	id, pairs, err := m.AllocateN(1)
	if err != nil {
		return "", "", "", err
	}
	return id, pairs[0].UpperDir, pairs[0].WorkDir, nil
}

// AllocateN creates a new session directory holding n upper + work pairs,
// one per overlay mount. Pair 0 lives at <root>/<id>/upper and
// <root>/<id>/work; pair i>0 at <root>/<id>/upper-<i> and work-<i>.
// Returns ErrUpperLimitExceeded if maxBytes > 0 and current usage already
// meets or exceeds the limit.
func (m *UpperManager) AllocateN(n int) (sessionID string, pairs []UpperDirPair, err error) {
	if n <= 0 {
		return "", nil, fmt.Errorf("upper: pair count must be positive, got %d", n)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.maxBytes > 0 {
		usage, usageErr := m.usageLocked()
		if usageErr == nil && usage >= m.maxBytes {
			return "", nil, fmt.Errorf("%w: %d >= %d bytes", ErrUpperLimitExceeded, usage, m.maxBytes)
		}
	}

	id, err := m.createCleanupRecordLocked()
	if err != nil {
		return "", nil, err
	}
	sessionDir := filepath.Join(m.root, id)
	pairs = make([]UpperDirPair, 0, n)
	failAllocation := func(cause error) (string, []UpperDirPair, error) {
		rollbackErr := m.rollbackAllocationLocked(id, sessionDir, pairs)
		if rollbackErr != nil {
			return "", nil, errors.Join(cause, rollbackErr)
		}
		return "", nil, cause
	}
	for i := range n {
		upperName, workName := "upper", "work"
		if i > 0 {
			upperName = fmt.Sprintf("upper-%d", i)
			workName = fmt.Sprintf("work-%d", i)
		}
		upperDir := filepath.Join(sessionDir, upperName)
		workDir := filepath.Join(sessionDir, workName)

		if err := os.MkdirAll(upperDir, 0o755); err != nil {
			return failAllocation(fmt.Errorf("upper: mkdir %s: %w", upperDir, err))
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return failAllocation(fmt.Errorf("upper: mkdir %s: %w", workDir, err))
		}
		pairs = append(pairs, UpperDirPair{UpperDir: upperDir, WorkDir: workDir})
	}

	m.entries[id] = &UpperEntry{Pairs: pairs, InUse: true, dir: sessionDir}
	return id, pairs, nil
}

// Release marks an upper directory as available for GC.
func (m *UpperManager) Release(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[sessionID]; ok {
		e.InUse = false
	}
}

// sessionDir returns the session directory holding the entry's pairs.
// Caller must hold m.mu.
func (e *UpperEntry) sessionDir() string {
	if e.dir != "" {
		return e.dir
	}
	if len(e.Pairs) == 0 {
		return ""
	}
	return filepath.Dir(e.Pairs[0].UpperDir)
}

// Remove immediately deletes a session directory and retires its cleanup
// record. Failures leave the released entry and record tracked for retry.
func (m *UpperManager) Remove(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[sessionID]
	if !ok {
		return fmt.Errorf("upper: session %s not found", sessionID)
	}
	if err := m.cleanupEntryLocked(sessionID, e); err != nil {
		return fmt.Errorf("upper: remove session %s: %w", sessionID, err)
	}
	return nil
}

// Collect runs one garbage collection pass, removing all released entries.
func (m *UpperManager) Collect() []string {
	freed, _ := m.CollectWithErrors()
	return freed
}

// CollectWithErrors runs one garbage collection pass and reports every
// released entry that could not be cleaned or retired. Failed entries remain
// tracked for a later retry.
func (m *UpperManager) CollectWithErrors() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var freed []string
	var cleanupErr error
	for id, e := range m.entries {
		if e.InUse {
			continue
		}
		if err := m.cleanupEntryLocked(id, e); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("upper: collect session %s: %w", id, err),
			)
			continue
		}
		freed = append(freed, id)
	}
	return freed, cleanupErr
}

// Usage returns the current total size of all upper directories in bytes.
func (m *UpperManager) Usage() (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usageLocked()
}

// usageLocked calculates usage without acquiring the mutex. Caller must hold m.mu.
// Entries whose upper directory no longer exists (e.g. a stale residue entry
// partially removed before a GC retry) contribute zero instead of failing the
// whole sum.
func (m *UpperManager) usageLocked() (int64, error) {
	var total int64
	for _, e := range m.entries {
		for _, p := range e.Pairs {
			size, err := dirSize(p.UpperDir)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return 0, err
			}
			total += size
		}
	}
	return total, nil
}

func (m *UpperManager) Root() string {
	return m.root
}

func (m *UpperManager) MaxBytes() int64 {
	return m.maxBytes
}

func (m *UpperManager) cleanupEntryLocked(id string, e *UpperEntry) error {
	// Mark the entry released before filesystem cleanup. It is only forgotten
	// after both the session subtree and its durable identity are gone.
	e.InUse = false
	if sessionDir := e.sessionDir(); sessionDir != "" {
		if err := m.removeAll(sessionDir); err != nil {
			return fmt.Errorf("remove session directory %s: %w", sessionDir, err)
		}
	}
	if err := m.retireCleanupRecord(id); err != nil {
		return err
	}
	delete(m.entries, id)
	return nil
}

func (m *UpperManager) createCleanupRecordLocked() (string, error) {
	for range 16 {
		id := newSessionID()
		recordPath := filepath.Join(m.cleanupRoot, id)
		if err := os.Mkdir(recordPath, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", fmt.Errorf("upper: create cleanup record %s: %w", recordPath, err)
		}
		return id, nil
	}
	return "", errors.New("upper: could not allocate unique session ID")
}

func (m *UpperManager) rollbackAllocationLocked(
	id string,
	sessionDir string,
	pairs []UpperDirPair,
) error {
	var cleanupErr error
	if err := m.removeAll(sessionDir); err != nil {
		cleanupErr = fmt.Errorf("remove partial session %s: %w", sessionDir, err)
	}
	if cleanupErr == nil {
		if err := m.retireCleanupRecord(id); err != nil {
			cleanupErr = err
		}
	}
	if cleanupErr == nil {
		return nil
	}

	if len(pairs) == 0 {
		pairs = sessionPairs(sessionDir)
	}
	m.entries[id] = &UpperEntry{Pairs: pairs, InUse: false, dir: sessionDir}
	return fmt.Errorf("upper: rollback session %s: %w", id, cleanupErr)
}

func (m *UpperManager) retireCleanupRecord(id string) error {
	recordPath := filepath.Join(m.cleanupRoot, id)
	if err := m.removeRecord(recordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("retire cleanup record %s: %w", recordPath, err)
	}
	return nil
}

func sessionPairs(sessionDir string) []UpperDirPair {
	pairs := []UpperDirPair{{
		UpperDir: filepath.Join(sessionDir, "upper"),
		WorkDir:  filepath.Join(sessionDir, "work"),
	}}
	children, err := os.ReadDir(sessionDir)
	if err != nil {
		return pairs
	}
	for _, child := range children {
		name := child.Name()
		if !strings.HasPrefix(name, "upper-") {
			continue
		}
		upperDir := filepath.Join(sessionDir, name)
		if !dirExists(upperDir) {
			continue
		}
		suffix := strings.TrimPrefix(name, "upper-")
		pairs = append(pairs, UpperDirPair{
			UpperDir: upperDir,
			WorkDir:  filepath.Join(sessionDir, "work-"+suffix),
		})
	}
	return pairs
}

func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("path exists and is not a real directory")
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("path exists and is not a real directory")
		}
	}
	return nil
}

func validSessionID(id string) bool {
	if len(id) == 32 {
		for _, c := range id {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
		return true
	}
	if !strings.HasPrefix(id, "fallback-") || len(id) == len("fallback-") {
		return false
	}
	for _, c := range id[len("fallback-"):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cryptographic randomness shouldn't fail. Fall back to a
		// timestamp-based name as last resort.
		return fmt.Sprintf("fallback-%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
