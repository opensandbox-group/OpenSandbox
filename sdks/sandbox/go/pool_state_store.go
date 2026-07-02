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

package opensandbox

import (
	"context"
	"sync"
	"time"
)

// PoolStateStore defines the interface for managing pool idle sandbox state.
// Implementations must be safe for concurrent use.
type PoolStateStore interface {
	// TryTakeIdle atomically removes and returns one idle sandbox ID.
	// Returns ("", false, nil) if no idle sandbox is available.
	TryTakeIdle(ctx context.Context, poolName string) (sandboxID string, ok bool, err error)
	// PutIdle adds a sandbox to the idle set with an expiration time.
	PutIdle(ctx context.Context, poolName string, sandboxID string, expiresAt time.Time) error
	// RemoveIdle removes a specific sandbox from the idle set.
	RemoveIdle(ctx context.Context, poolName string, sandboxID string) error
	// ReapExpired removes all idle entries whose expiresAt is before now.
	ReapExpired(ctx context.Context, poolName string, now time.Time) error
	// CountIdle returns the number of idle sandboxes.
	CountIdle(ctx context.Context, poolName string) (int, error)

	// TryAcquireLock attempts to acquire the primary reconciler lock.
	TryAcquireLock(ctx context.Context, poolName, ownerID string, ttl time.Duration) (bool, error)
	// RenewLock extends the TTL of an existing lock held by ownerID.
	RenewLock(ctx context.Context, poolName, ownerID string, ttl time.Duration) (bool, error)
	// ReleaseLock releases the primary lock if held by ownerID.
	ReleaseLock(ctx context.Context, poolName, ownerID string) error
}

// idleEntry represents a sandbox in the idle set.
type idleEntry struct {
	SandboxID string
	ExpiresAt time.Time
}

// lockEntry represents the primary leader lock.
type lockEntry struct {
	OwnerID   string
	ExpiresAt time.Time
}

// InMemoryPoolStateStore is a single-process in-memory implementation of PoolStateStore.
type InMemoryPoolStateStore struct {
	mu    sync.Mutex
	idle  map[string][]idleEntry // poolName -> entries
	locks map[string]*lockEntry  // poolName -> lock
}

// NewInMemoryPoolStateStore creates a new in-memory state store.
func NewInMemoryPoolStateStore() *InMemoryPoolStateStore {
	return &InMemoryPoolStateStore{
		idle:  make(map[string][]idleEntry),
		locks: make(map[string]*lockEntry),
	}
}

func (s *InMemoryPoolStateStore) TryTakeIdle(_ context.Context, poolName string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := s.idle[poolName]
	now := time.Now()

	// Scan past expired entries to find a valid one
	for len(entries) > 0 {
		entry := entries[0]
		entries = entries[1:]

		if now.Before(entry.ExpiresAt) {
			s.idle[poolName] = entries
			return entry.SandboxID, true, nil
		}
		// Entry expired, skip and continue
	}

	s.idle[poolName] = entries
	return "", false, nil
}

func (s *InMemoryPoolStateStore) PutIdle(_ context.Context, poolName string, sandboxID string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.idle[poolName] = append(s.idle[poolName], idleEntry{
		SandboxID: sandboxID,
		ExpiresAt: expiresAt,
	})
	return nil
}

func (s *InMemoryPoolStateStore) RemoveIdle(_ context.Context, poolName string, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := s.idle[poolName]
	for i, e := range entries {
		if e.SandboxID == sandboxID {
			s.idle[poolName] = append(entries[:i], entries[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *InMemoryPoolStateStore) ReapExpired(_ context.Context, poolName string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := s.idle[poolName]
	kept := entries[:0]
	for _, e := range entries {
		if now.Before(e.ExpiresAt) {
			kept = append(kept, e)
		}
	}
	s.idle[poolName] = kept
	return nil
}

func (s *InMemoryPoolStateStore) CountIdle(_ context.Context, poolName string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idle[poolName]), nil
}

func (s *InMemoryPoolStateStore) TryAcquireLock(_ context.Context, poolName, ownerID string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lock := s.locks[poolName]
	now := time.Now()

	// Lock is free or expired
	if lock == nil || now.After(lock.ExpiresAt) {
		s.locks[poolName] = &lockEntry{
			OwnerID:   ownerID,
			ExpiresAt: now.Add(ttl),
		}
		return true, nil
	}

	// Already held by this owner
	if lock.OwnerID == ownerID {
		lock.ExpiresAt = now.Add(ttl)
		return true, nil
	}

	return false, nil
}

func (s *InMemoryPoolStateStore) RenewLock(_ context.Context, poolName, ownerID string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lock := s.locks[poolName]
	if lock == nil || lock.OwnerID != ownerID {
		return false, nil
	}

	lock.ExpiresAt = time.Now().Add(ttl)
	return true, nil
}

func (s *InMemoryPoolStateStore) ReleaseLock(_ context.Context, poolName, ownerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	lock := s.locks[poolName]
	if lock != nil && lock.OwnerID == ownerID {
		delete(s.locks, poolName)
	}
	return nil
}
