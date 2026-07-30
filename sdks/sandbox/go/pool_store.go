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
	"time"
)

// PoolStateStore is the abstraction for pool state persistence.
// Implementations must be safe for concurrent use.
// PutIdle and RemoveIdle must be idempotent.
type PoolStateStore interface {
	// TryTakeIdle atomically takes the oldest idle sandbox from the pool.
	// Returns empty string if no idle sandbox is available.
	TryTakeIdle(ctx context.Context, poolName string) (string, error)

	// TryTakeIdleWithMinTTL atomically takes the oldest idle sandbox that has
	// at least minRemaining TTL left. Entries that are alive but below the
	// threshold are returned in DiscardedAliveSandboxIDs for caller cleanup.
	TryTakeIdleWithMinTTL(ctx context.Context, poolName string, minRemaining time.Duration) (*TakeIdleResult, error)

	// PutIdle adds a sandbox to the idle pool. Idempotent.
	PutIdle(ctx context.Context, poolName string, sandboxID string) error

	// RemoveIdle removes a sandbox from the idle pool. Idempotent.
	RemoveIdle(ctx context.Context, poolName string, sandboxID string) error

	// TryAcquirePrimaryLock attempts to acquire the primary (leader) lock.
	TryAcquirePrimaryLock(ctx context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error)

	// RenewPrimaryLock extends the primary lock TTL. Only succeeds if caller is the current owner.
	RenewPrimaryLock(ctx context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error)

	// ReleasePrimaryLock releases the primary lock. Only succeeds if caller is the current owner.
	ReleasePrimaryLock(ctx context.Context, poolName string, ownerID string) error

	// ReapExpiredIdle removes fully expired idle entries.
	// The now parameter is a hint; distributed implementations may use
	// server-side time for consistency (e.g., Redis TIME command).
	ReapExpiredIdle(ctx context.Context, poolName string, now time.Time) error

	// ReapExpiredIdleWithMinTTL removes expired and near-expiry idle entries.
	// Returns IDs of entries that were still alive but below the TTL threshold.
	// The now parameter is a hint; see ReapExpiredIdle.
	ReapExpiredIdleWithMinTTL(ctx context.Context, poolName string, now time.Time, minRemaining time.Duration) (*ReapResult, error)

	// SnapshotCounters returns current pool counters.
	SnapshotCounters(ctx context.Context, poolName string) (*StoreCounters, error)

	// SnapshotIdleEntries returns all current idle entries in FIFO order.
	SnapshotIdleEntries(ctx context.Context, poolName string) ([]IdleEntry, error)

	// GetMaxIdle returns the stored maxIdle value for the pool.
	GetMaxIdle(ctx context.Context, poolName string) (int, error)

	// SetMaxIdle persists the maxIdle value for the pool.
	SetMaxIdle(ctx context.Context, poolName string, maxIdle int) error

	// SetIdleEntryTTL persists the idle entry TTL for the pool.
	SetIdleEntryTTL(ctx context.Context, poolName string, ttl time.Duration) error
}
