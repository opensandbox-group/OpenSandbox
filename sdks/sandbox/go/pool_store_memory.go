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
	"fmt"
	"sync"
	"time"
)

// poolLock tracks the distributed lock state for a single pool.
type poolLock struct {
	ownerID   string
	expiresAt time.Time
}

// poolState is the internal state for a single pool.
type poolState struct {
	mu sync.Mutex

	// idle entries indexed by sandbox ID for O(1) existence checks.
	idleMap map[string]*IdleEntry
	// FIFO order of sandbox IDs. May contain stale IDs (lazy cleanup).
	idleQueue []string

	// primary lock state.
	lock poolLock

	// configurable per-pool settings.
	idleTTL time.Duration
	maxIdle int
}

// InMemoryPoolStateStore is a pure in-memory implementation of PoolStateStore.
// It is safe for concurrent use from multiple goroutines and supports real
// lock tracking with TTL, making it suitable for unit testing pool logic.
type InMemoryPoolStateStore struct {
	mu    sync.RWMutex
	pools map[string]*poolState
}

// NewInMemoryPoolStateStore creates a new InMemoryPoolStateStore.
func NewInMemoryPoolStateStore() *InMemoryPoolStateStore {
	return &InMemoryPoolStateStore{
		pools: make(map[string]*poolState),
	}
}

// getOrCreatePool returns the poolState for the given pool, creating it if needed.
// Caller must NOT hold s.mu.
func (s *InMemoryPoolStateStore) getOrCreatePool(poolName string) *poolState {
	s.mu.RLock()
	ps, ok := s.pools[poolName]
	s.mu.RUnlock()
	if ok {
		return ps
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check after acquiring write lock.
	if ps, ok = s.pools[poolName]; ok {
		return ps
	}
	ps = &poolState{
		idleMap: make(map[string]*IdleEntry),
		idleTTL: DefaultIdleTimeout,
		maxIdle: 0, // default; overwritten by SetMaxIdle during pool Start
	}
	s.pools[poolName] = ps
	return ps
}

// TryTakeIdle atomically takes the oldest idle sandbox from the pool.
// Expired entries are silently dropped inline.
func (s *InMemoryPoolStateStore) TryTakeIdle(_ context.Context, poolName string) (string, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	for len(ps.idleQueue) > 0 {
		id := ps.idleQueue[0]
		ps.idleQueue[0] = "" // allow GC of the string
		ps.idleQueue = ps.idleQueue[1:]

		entry, exists := ps.idleMap[id]
		if !exists {
			continue
		}
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(ps.idleMap, id)
			continue
		}
		delete(ps.idleMap, id)
		ps.compactQueueIfNeeded()
		return id, nil
	}
	ps.compactQueueIfNeeded()
	return "", nil
}

// TryTakeIdleWithMinTTL takes the oldest idle sandbox that has at least
// minRemaining TTL left. Entries alive but below threshold are returned in
// DiscardedAliveSandboxIDs.
func (s *InMemoryPoolStateStore) TryTakeIdleWithMinTTL(_ context.Context, poolName string, minRemaining time.Duration) (*TakeIdleResult, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	result := &TakeIdleResult{}

	for len(ps.idleQueue) > 0 {
		id := ps.idleQueue[0]
		ps.idleQueue[0] = "" // allow GC of the string
		ps.idleQueue = ps.idleQueue[1:]

		entry, exists := ps.idleMap[id]
		if !exists {
			continue
		}

		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(ps.idleMap, id)
			continue
		}

		remaining := entry.ExpiresAt.Sub(now)
		if !entry.ExpiresAt.IsZero() && remaining < minRemaining {
			delete(ps.idleMap, id)
			result.DiscardedAliveSandboxIDs = append(result.DiscardedAliveSandboxIDs, id)
			continue
		}

		delete(ps.idleMap, id)
		result.SandboxID = id
		ps.compactQueueIfNeeded()
		return result, nil
	}
	ps.compactQueueIfNeeded()
	return result, nil
}

// PutIdle adds a sandbox to the idle pool. Idempotent: if already present, no-op.
func (s *InMemoryPoolStateStore) PutIdle(_ context.Context, poolName string, sandboxID string) error {
	if sandboxID == "" {
		return fmt.Errorf("opensandbox: sandboxID must not be blank")
	}
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	if existing, exists := ps.idleMap[sandboxID]; exists {
		if existing.ExpiresAt.IsZero() || now.Before(existing.ExpiresAt) {
			return nil // still alive, idempotent no-op
		}
		// Expired entry — remove from map and rebuild queue to drop stale position,
		// then fall through to re-add with fresh TTL at the tail (FIFO).
		delete(ps.idleMap, sandboxID)
		ps.rebuildQueue()
	}

	expiresAt := now.Add(ps.idleTTL)
	entry := &IdleEntry{
		SandboxID: sandboxID,
		ExpiresAt: expiresAt,
	}
	ps.idleMap[sandboxID] = entry
	ps.idleQueue = append(ps.idleQueue, sandboxID)
	return nil
}

// RemoveIdle removes a sandbox from the idle pool map. The queue is cleaned up
// lazily during take operations. Idempotent.
func (s *InMemoryPoolStateStore) RemoveIdle(_ context.Context, poolName string, sandboxID string) error {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	delete(ps.idleMap, sandboxID)
	return nil
}

// TryAcquirePrimaryLock attempts to acquire the primary lock for the pool.
// Succeeds if the lock is unheld or expired.
func (s *InMemoryPoolStateStore) TryAcquirePrimaryLock(_ context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	if ps.lock.ownerID != "" && now.Before(ps.lock.expiresAt) {
		// Lock is held and not expired.
		if ps.lock.ownerID == ownerID {
			// Same owner re-acquiring; extend TTL.
			ps.lock.expiresAt = now.Add(ttl)
			return true, nil
		}
		return false, nil
	}

	// Lock is unheld or expired; acquire.
	ps.lock.ownerID = ownerID
	ps.lock.expiresAt = now.Add(ttl)
	return true, nil
}

// RenewPrimaryLock extends the lock TTL. Only succeeds if caller is the
// current owner and the lock has not expired.
func (s *InMemoryPoolStateStore) RenewPrimaryLock(_ context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	if ps.lock.ownerID != ownerID {
		return false, nil
	}
	if now.After(ps.lock.expiresAt) {
		// Lock has expired; owner no longer holds it.
		ps.lock.ownerID = ""
		return false, nil
	}

	ps.lock.expiresAt = now.Add(ttl)
	return true, nil
}

// ReleasePrimaryLock releases the primary lock. Only succeeds if caller is the
// current owner.
func (s *InMemoryPoolStateStore) ReleasePrimaryLock(_ context.Context, poolName string, ownerID string) error {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.lock.ownerID == ownerID {
		ps.lock.ownerID = ""
		ps.lock.expiresAt = time.Time{}
	}
	return nil
}

// ReapExpiredIdle removes all fully expired idle entries and rebuilds the queue.
func (s *InMemoryPoolStateStore) ReapExpiredIdle(_ context.Context, poolName string, now time.Time) error {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	// Remove expired from map.
	for id, entry := range ps.idleMap {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(ps.idleMap, id)
		}
	}

	// Rebuild queue to only include live entries.
	ps.rebuildQueue()
	return nil
}

// ReapExpiredIdleWithMinTTL removes expired and near-expiry idle entries.
// Returns IDs of entries that were still alive but below the TTL threshold.
func (s *InMemoryPoolStateStore) ReapExpiredIdleWithMinTTL(_ context.Context, poolName string, now time.Time, minRemaining time.Duration) (*ReapResult, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	result := &ReapResult{}

	for id, entry := range ps.idleMap {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			// Fully expired.
			delete(ps.idleMap, id)
		} else if !entry.ExpiresAt.IsZero() {
			remaining := entry.ExpiresAt.Sub(now)
			if remaining < minRemaining {
				// Alive but below threshold.
				delete(ps.idleMap, id)
				result.DiscardedAliveSandboxIDs = append(result.DiscardedAliveSandboxIDs, id)
			}
		}
	}

	// Rebuild queue.
	ps.rebuildQueue()
	return result, nil
}

// SnapshotCounters returns current pool counters, excluding expired entries.
func (s *InMemoryPoolStateStore) SnapshotCounters(_ context.Context, poolName string) (*StoreCounters, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	count := 0
	for _, entry := range ps.idleMap {
		if entry.ExpiresAt.IsZero() || now.Before(entry.ExpiresAt) {
			count++
		}
	}
	return &StoreCounters{IdleCount: count}, nil
}

// SnapshotIdleEntries returns all current idle entries in FIFO order,
// excluding expired entries.
func (s *InMemoryPoolStateStore) SnapshotIdleEntries(_ context.Context, poolName string) ([]IdleEntry, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now()
	var entries []IdleEntry
	for _, id := range ps.idleQueue {
		entry, exists := ps.idleMap[id]
		if !exists {
			continue
		}
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			continue
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

// GetMaxIdle returns the stored maxIdle value for the pool.
func (s *InMemoryPoolStateStore) GetMaxIdle(_ context.Context, poolName string) (int, error) {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.maxIdle, nil
}

// SetMaxIdle persists the maxIdle value for the pool.
func (s *InMemoryPoolStateStore) SetMaxIdle(_ context.Context, poolName string, maxIdle int) error {
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.maxIdle = maxIdle
	return nil
}

// SetIdleEntryTTL persists the idle entry TTL for the pool.
func (s *InMemoryPoolStateStore) SetIdleEntryTTL(_ context.Context, poolName string, ttl time.Duration) error {
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	ps := s.getOrCreatePool(poolName)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.idleTTL = ttl
	return nil
}

// compactQueueIfNeeded copies the queue to a right-sized slice when the
// underlying array has grown much larger than needed. Must be called with
// ps.mu held.
func (ps *poolState) compactQueueIfNeeded() {
	if cap(ps.idleQueue) > 64 && cap(ps.idleQueue) > 2*len(ps.idleQueue) {
		compacted := make([]string, len(ps.idleQueue))
		copy(compacted, ps.idleQueue)
		ps.idleQueue = compacted
	}
}

// rebuildQueue rebuilds the idleQueue to only include IDs that exist in idleMap,
// preserving FIFO order. Must be called with ps.mu held.
func (ps *poolState) rebuildQueue() {
	newQueue := make([]string, 0, len(ps.idleMap))
	for _, id := range ps.idleQueue {
		if _, exists := ps.idleMap[id]; exists {
			newQueue = append(newQueue, id)
		}
	}
	ps.idleQueue = newQueue
}

// Compile-time interface check.
var _ PoolStateStore = (*InMemoryPoolStateStore)(nil)
