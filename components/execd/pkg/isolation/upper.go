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
	"sync"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// UpperManager manages upper directories for overlay workspaces.
type UpperManager struct {
	root      string
	maxBytes  int64
	removeAll func(string) error
	mu        sync.Mutex
	entries   map[string]*UpperEntry
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
}

// NewUpperManager creates an upper directory manager. As part of startup it
// reclaims stale session directories left under root by a previous execd
// lifetime: the session table lives only in memory, so every execd-allocated
// child of root is orphaned by definition and gets removed. Children without
// the execd session layout are left untouched (root is operator-configured
// and must stay safe to point at a directory shared with other data).
func NewUpperManager(root string, maxBytes int64) (*UpperManager, error) {
	if root == "" {
		return nil, errors.New("upper: root path is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("upper: create root %s: %w", root, err)
	}
	m := &UpperManager{
		root:      root,
		maxBytes:  maxBytes,
		removeAll: os.RemoveAll,
		entries:   make(map[string]*UpperEntry),
	}
	m.reclaimStale()
	return m, nil
}

// reclaimStale is a startup-only sweep that removes session directories
// left under root by a previous execd lifetime (crash, OOM, container
// restart, or a pooled sandbox whose agent is restarted between occupants).
// Session state is memory-only and dies with the process, so no correct
// behavior depends on stale upper directories surviving a restart; leaving
// them would leak disk and expose one occupant's session data to the next.
// Call it only before the manager tracks any live entry.
//
// Only children with the execd-allocated layout (a directory containing an
// upper/ subdirectory) are reclaimed: upper_root is operator-configured,
// and pointing it at a directory shared with other data — valid before this
// sweep existed — must not erase unrelated children on upgrade.
//
// Children whose removal fails — e.g. an upper still referenced by a mount
// from the previous lifetime — are registered as released entries so the
// collector retries them once the blocker is gone and usage accounting keeps
// counting their bytes toward upper_max_bytes.
func (m *UpperManager) reclaimStale() {
	children, err := os.ReadDir(m.root)
	if err != nil {
		log.Warn("upper: list stale entries under %s: %v", m.root, err)
		return
	}

	var removed int
	var failed int
	var skipped int
	for _, child := range children {
		path := filepath.Join(m.root, child.Name())
		if !dirExists(filepath.Join(path, "upper")) {
			// Not an execd-allocated session directory; never touch it.
			skipped++
			continue
		}
		if err := m.removeAll(path); err != nil {
			failed++
			log.Warn("upper: reclaim stale session dir %s: %v", path, err)
			m.entries[child.Name()] = &UpperEntry{
				Pairs: []UpperDirPair{{
					UpperDir: filepath.Join(path, "upper"),
					WorkDir:  filepath.Join(path, "work"),
				}},
				InUse: false,
			}
			continue
		}
		removed++
	}
	if removed > 0 || failed > 0 || skipped > 0 {
		log.Info(
			"upper: reclaimed %d stale session dir(s) under %s (%d failed, %d unrecognized skipped)",
			removed, m.root, failed, skipped,
		)
	}
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
// <root>/<id>/work (the layout reclaimStale recognizes); pair i>0 at
// <root>/<id>/upper-<i> and work-<i>. Returns the session ID and the pairs.
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

	id := newSessionID()
	sessionDir := filepath.Join(m.root, id)
	pairs = make([]UpperDirPair, 0, n)
	for i := range n {
		upperName, workName := "upper", "work"
		if i > 0 {
			upperName = fmt.Sprintf("upper-%d", i)
			workName = fmt.Sprintf("work-%d", i)
		}
		upperDir := filepath.Join(sessionDir, upperName)
		workDir := filepath.Join(sessionDir, workName)

		if err := os.MkdirAll(upperDir, 0o755); err != nil {
			// Best-effort rollback of the partially allocated session dir.
			_ = m.removeAll(sessionDir)
			return "", nil, fmt.Errorf("upper: mkdir %s: %w", upperDir, err)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			_ = m.removeAll(sessionDir)
			return "", nil, fmt.Errorf("upper: mkdir %s: %w", workDir, err)
		}
		pairs = append(pairs, UpperDirPair{UpperDir: upperDir, WorkDir: workDir})
	}

	m.entries[id] = &UpperEntry{Pairs: pairs, InUse: true}

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
// All pairs share one execd-allocated parent, so removing it removes every
// pair. Caller must hold m.mu and the entry must have at least one pair.
func (e *UpperEntry) sessionDir() string {
	return filepath.Dir(e.Pairs[0].UpperDir)
}

// Remove immediately deletes an upper directory.
func (m *UpperManager) Remove(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[sessionID]
	if !ok {
		return fmt.Errorf("upper: session %s not found", sessionID)
	}
	if len(e.Pairs) == 0 {
		delete(m.entries, sessionID)
		return nil
	}

	// Mark the entry released before removal. A transient filesystem error must
	// leave the directory tracked so CollectWithErrors can retry it later.
	e.InUse = false
	upperParent := e.sessionDir()
	if err := m.removeAll(upperParent); err != nil {
		return err
	}
	delete(m.entries, sessionID)
	return nil
}

// Collect runs one garbage collection pass, removing all released entries.
func (m *UpperManager) Collect() []string {
	freed, _ := m.CollectWithErrors()
	return freed
}

// CollectWithErrors runs one garbage collection pass and reports every
// released entry that could not be removed. Failed entries remain tracked for
// a later retry.
func (m *UpperManager) CollectWithErrors() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var freed []string
	var cleanupErr error
	for id, e := range m.entries {
		if !e.InUse {
			if len(e.Pairs) == 0 {
				freed = append(freed, id)
				delete(m.entries, id)
				continue
			}
			upperParent := e.sessionDir()
			if err := m.removeAll(upperParent); err != nil {
				cleanupErr = errors.Join(
					cleanupErr,
					fmt.Errorf("upper: collect session %s: %w", id, err),
				)
				continue
			}
			freed = append(freed, id)
			delete(m.entries, id)
		}
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
