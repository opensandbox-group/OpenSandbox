// Copyright 2026 Alibaba Group Holding Ltd.
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
	"os"
	"path/filepath"
	"sync"
)

// UpperManager manages upper directories for overlay workspaces.
type UpperManager struct {
	root     string
	maxBytes int64
	mu       sync.Mutex
	entries  map[string]*UpperEntry
}

// UpperEntry tracks one allocated upper directory.
type UpperEntry struct {
	UpperDir string
	WorkDir  string
	InUse    bool
}

// NewUpperManager creates an upper directory manager.
func NewUpperManager(root string, maxBytes int64) (*UpperManager, error) {
	if root == "" {
		return nil, errors.New("upper: root path is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("upper: create root %s: %w", root, err)
	}
	return &UpperManager{
		root:     root,
		maxBytes: maxBytes,
		entries:  make(map[string]*UpperEntry),
	}, nil
}

// ErrUpperLimitExceeded is returned when the upper directory size limit is exceeded.
var ErrUpperLimitExceeded = errors.New("upper: total usage exceeds configured limit")

// Allocate creates a new upper + work directory pair. Returns the session ID
// and the directories. Returns ErrUpperLimitExceeded if maxBytes > 0 and
// current usage already meets or exceeds the limit.
func (m *UpperManager) Allocate() (sessionID, upperDir, workDir string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.maxBytes > 0 {
		usage, usageErr := m.usageLocked()
		if usageErr == nil && usage >= m.maxBytes {
			return "", "", "", fmt.Errorf("%w: %d >= %d bytes", ErrUpperLimitExceeded, usage, m.maxBytes)
		}
	}

	id := newSessionID()
	upperDir = filepath.Join(m.root, id, "upper")
	workDir = filepath.Join(m.root, id, "work")

	if err := os.MkdirAll(upperDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("upper: mkdir %s: %w", upperDir, err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		os.RemoveAll(filepath.Dir(upperDir))
		return "", "", "", fmt.Errorf("upper: mkdir %s: %w", workDir, err)
	}

	m.entries[id] = &UpperEntry{
		UpperDir: upperDir,
		WorkDir:  workDir,
		InUse:    true,
	}

	return id, upperDir, workDir, nil
}

// Release marks an upper directory as available for GC.
func (m *UpperManager) Release(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[sessionID]; ok {
		e.InUse = false
	}
}

// Remove immediately deletes an upper directory.
func (m *UpperManager) Remove(sessionID string) error {
	m.mu.Lock()
	e, ok := m.entries[sessionID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("upper: session %s not found", sessionID)
	}
	delete(m.entries, sessionID)
	m.mu.Unlock()

	upperParent := filepath.Dir(e.UpperDir)
	return os.RemoveAll(upperParent)
}

// Collect runs one garbage collection pass, removing all released entries.
func (m *UpperManager) Collect() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var freed []string
	for id, e := range m.entries {
		if !e.InUse {
			upperParent := filepath.Dir(e.UpperDir)
			if err := os.RemoveAll(upperParent); err == nil {
				freed = append(freed, id)
				delete(m.entries, id)
			}
		}
	}
	return freed
}

// Usage returns the current total size of all upper directories in bytes.
func (m *UpperManager) Usage() (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usageLocked()
}

// usageLocked calculates usage without acquiring the mutex. Caller must hold m.mu.
func (m *UpperManager) usageLocked() (int64, error) {
	var total int64
	for _, e := range m.entries {
		size, err := dirSize(e.UpperDir)
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}

// Root returns the manager's root path.
func (m *UpperManager) Root() string {
	return m.root
}

// MaxBytes returns the configured byte limit.
func (m *UpperManager) MaxBytes() int64 {
	return m.maxBytes
}

// newSessionID generates a random hex session ID.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cryptographic randomness shouldn't fail. Fall back to a
		// timestamp-based name as last resort.
		return fmt.Sprintf("fallback-%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// dirSize walks a directory and returns total bytes used.
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
