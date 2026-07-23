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

const (
	reconcileBackoffBase = 30 * time.Second
	reconcileMaxBackoff  = 24 * time.Hour
)

// reconcileState tracks the health and backoff state of the reconcile loop.
type reconcileState struct {
	mu                sync.Mutex
	degradedThreshold int
	failureCount      int
	backoffAttempts   int
	backoffUntil      time.Time
	lastError         string
	healthState       PoolHealthState
}

// newReconcileState creates a new reconcileState with the given degraded threshold.
func newReconcileState(degradedThreshold int) *reconcileState {
	return &reconcileState{
		degradedThreshold: degradedThreshold,
		healthState:       PoolHealthy,
	}
}

// recordSuccess resets the failure state and marks the pool as healthy.
func (s *reconcileState) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failureCount = 0
	s.backoffAttempts = 0
	s.backoffUntil = time.Time{}
	s.lastError = ""
	s.healthState = PoolHealthy
}

// recordFailure records a single failure. Delegates to recordFailures.
func (s *reconcileState) recordFailure(err error) {
	s.recordFailures(1, err)
}

// recordFailures records count failures in one call. If the cumulative count
// reaches or exceeds the degraded threshold, the pool transitions to degraded
// state and backoffAttempts is incremented (escalating the backoff duration).
// In production, this is called once per reconcile tick, so backoff escalates
// per failing tick, not per individual failed sandbox creation.
func (s *reconcileState) recordFailures(count int, err error) {
	if count <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failureCount += count
	if err != nil {
		s.lastError = err.Error()
	}
	if s.failureCount >= s.degradedThreshold {
		s.healthState = PoolDegraded
		s.backoffAttempts++
		shift := s.backoffAttempts - 1
		if shift > 15 {
			shift = 15
		}
		backoff := reconcileBackoffBase * (1 << shift)
		if backoff > reconcileMaxBackoff {
			backoff = reconcileMaxBackoff
		}
		s.backoffUntil = time.Now().Add(backoff)
	}
}

// shouldBackoff returns true if the reconciler is in a backoff period.
func (s *reconcileState) shouldBackoff() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.backoffUntil)
}

// snapshot returns a point-in-time view of the reconcile health state.
func (s *reconcileState) snapshot() (PoolHealthState, int, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backoffActive := time.Now().Before(s.backoffUntil)
	return s.healthState, s.failureCount, backoffActive, s.lastError
}

// reconcileTick performs a single reconciliation pass. It is designed to be
// called periodically by the pool's background loop.
//
// Logic (follows OSEP-0005 and Kotlin/Python SDKs):
//  1. Acquire the primary lock (leader election). If it fails, return immediately.
//  2. Reap expired idle entries, killing any discarded-alive sandboxes.
//  3. If idle count exceeds maxIdle, shrink excess entries.
//  4. If a deficit exists and we are not in backoff, create sandboxes up to warmupConcurrency.
//
// The leader lock is NOT released at end of tick; it is held until TTL expires
// or renew fails. This reduces lock contention in distributed (Redis) scenarios.
// Only Shutdown releases the lock explicitly.
func reconcileTick(
	ctx context.Context,
	cfg *PoolConfig,
	store PoolStateStore,
	state *reconcileState,
	logger PoolLogger,
	createFn func(ctx context.Context, reason PooledSandboxCreateReason) (string, error),
	deleteFn func(sandboxID string),
) {
	poolName := cfg.PoolName
	ownerID := cfg.OwnerID
	lockTTL := cfg.PrimaryLockTTL

	// Step 1: Try to acquire the primary lock.
	acquired, err := store.TryAcquirePrimaryLock(ctx, poolName, ownerID, lockTTL)
	if err != nil {
		logger.Warn("reconcile: lock acquire error", "pool_name", poolName, "error", err)
		state.recordFailure(err)
		return
	}
	if !acquired {
		logger.Debug("reconcile: not primary, skipping", "pool_name", poolName)
		return
	}

	// Step 2: Reap expired idle entries.
	minTTL := cfg.AcquireMinRemainingTTL
	if minTTL > 0 {
		reapResult, reapErr := store.ReapExpiredIdleWithMinTTL(ctx, poolName, time.Now(), minTTL)
		if reapErr != nil {
			logger.Warn("reconcile: reap error", "pool_name", poolName, "error", reapErr)
		} else if reapResult != nil && len(reapResult.DiscardedAliveSandboxIDs) > 0 {
			logger.Debug("reconcile: reaped near-expiry sandboxes",
				"pool_name", poolName,
				"count", len(reapResult.DiscardedAliveSandboxIDs))
			for _, id := range reapResult.DiscardedAliveSandboxIDs {
				deleteFn(id)
			}
		}
	} else {
		if reapErr := store.ReapExpiredIdle(ctx, poolName, time.Now()); reapErr != nil {
			logger.Warn("reconcile: reap error", "pool_name", poolName, "error", reapErr)
		}
	}

	// Step 3: Snapshot counters and determine current state.
	counters, err := store.SnapshotCounters(ctx, poolName)
	if err != nil {
		logger.Warn("reconcile: snapshot error", "pool_name", poolName, "error", err)
		return
	}
	maxIdle, err := store.GetMaxIdle(ctx, poolName)
	if err != nil {
		logger.Warn("reconcile: get maxIdle error", "pool_name", poolName, "error", err)
		return
	}
	idleCount := counters.IdleCount

	// Step 4: If idle > maxIdle, shrink excess.
	if idleCount > maxIdle {
		excess := idleCount - maxIdle
		toRemove := intMin(excess, cfg.WarmupConcurrency)
		logger.Debug("reconcile: shrinking excess idle",
			"pool_name", poolName,
			"idle", idleCount,
			"max_idle", maxIdle,
			"to_remove", toRemove)
		shrinkErr := false
		removedCount := 0
		for i := 0; i < toRemove; i++ {
			renewed, renewErr := store.RenewPrimaryLock(ctx, poolName, ownerID, lockTTL)
			if renewErr != nil || !renewed {
				logger.Warn("reconcile: lost lock during shrink", "pool_name", poolName)
				return
			}
			sandboxID, takeErr := store.TryTakeIdle(ctx, poolName)
			if takeErr != nil {
				logger.Warn("reconcile: TryTakeIdle error during shrink", "pool_name", poolName, "error", takeErr)
				state.recordFailure(takeErr)
				shrinkErr = true
				break
			}
			if sandboxID == "" {
				break
			}
			deleteFn(sandboxID)
			removedCount++
		}
		if !shrinkErr && removedCount > 0 {
			state.recordSuccess()
		}
		return
	}

	// Step 5: If deficit > 0 and not in backoff, create sandboxes.
	deficit := maxIdle - idleCount
	if deficit <= 0 {
		return
	}
	if state.shouldBackoff() {
		logger.Debug("reconcile: backoff active, skipping replenish",
			"pool_name", poolName,
			"deficit", deficit)
		return
	}

	renewed, err := store.RenewPrimaryLock(ctx, poolName, ownerID, lockTTL)
	if err != nil || !renewed {
		logger.Warn("reconcile: lost lock before create", "pool_name", poolName)
		return
	}

	toCreate := intMin(deficit, cfg.WarmupConcurrency)
	logger.Debug("reconcile: filling deficit",
		"pool_name", poolName,
		"idle", idleCount,
		"deficit", deficit,
		"to_create", toCreate)

	type createResult struct {
		sandboxID string
		err       error
	}

	results := make([]createResult, toCreate)
	var wg sync.WaitGroup
	for i := 0; i < toCreate; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[idx] = createResult{err: fmt.Errorf("panic in createFn: %v", r)}
				}
			}()
			select {
			case <-ctx.Done():
				results[idx] = createResult{err: ctx.Err()}
			default:
				id, createErr := createFn(ctx, CreateReasonWarmup)
				results[idx] = createResult{sandboxID: id, err: createErr}
			}
		}(i)
	}
	wg.Wait()

	var createdIDs []string
	var lastCreateErr error
	failCount := 0
	for _, r := range results {
		if r.err != nil {
			failCount++
			lastCreateErr = r.err
			logger.Warn("reconcile: sandbox create failed",
				"pool_name", poolName,
				"error", r.err)
		} else if r.sandboxID != "" {
			createdIDs = append(createdIDs, r.sandboxID)
		}
	}
	if failCount > 0 {
		state.recordFailures(failCount, lastCreateErr)
	}

	// Place created sandboxes into idle pool; record success per-putIdle.
	for i, id := range createdIDs {
		renewed, renewErr := store.RenewPrimaryLock(ctx, poolName, ownerID, lockTTL)
		if renewErr != nil || !renewed {
			for _, orphanID := range createdIDs[i:] {
				deleteFn(orphanID)
			}
			logger.Warn("reconcile: lost lock before putIdle, killing orphans",
				"pool_name", poolName,
				"orphan_count", len(createdIDs)-i)
			return
		}
		if putErr := store.PutIdle(ctx, poolName, id); putErr != nil {
			state.recordFailure(putErr)
			// Remove potentially-stored entry and kill the current sandbox.
			_ = store.RemoveIdle(ctx, poolName, id)
			deleteFn(id)
			// Kill remaining orphans.
			for _, orphanID := range createdIDs[i+1:] {
				deleteFn(orphanID)
			}
			logger.Warn("reconcile: putIdle failed, killing orphans",
				"pool_name", poolName,
				"error", putErr,
				"orphan_count", len(createdIDs)-i)
			return
		}
		state.recordSuccess()
	}

	if len(createdIDs) > 0 {
		logger.Debug("reconcile: created sandboxes",
			"pool_name", poolName,
			"count", len(createdIDs))
	}
}

func intMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}
