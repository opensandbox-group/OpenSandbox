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

//go:build integration

package poolredis

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/redis/go-redis/v9"
)

func newRedisTestStore(t *testing.T) *RedisPoolStateStore {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   0,
	})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available at localhost:6379: %v", err)
	}
	store, err := NewRedisPoolStateStore(RedisPoolStateStoreConfig{
		Client:    client,
		KeyPrefix: "opensandbox:test",
	})
	if err != nil {
		t.Fatalf("NewRedisPoolStateStore: %v", err)
	}
	return store
}

func cleanupPool(t *testing.T, store *RedisPoolStateStore, poolName string) {
	t.Helper()
	ctx := context.Background()
	keys := []string{
		store.idleListKey(poolName),
		store.idleExpiresKey(poolName),
		store.PrimaryLockKey(poolName),
		store.maxIdleKey(poolName),
		store.idleTTLKey(poolName),
	}
	store.client.Del(ctx, keys...)
}

func TestRedisStore_TryTakeIdle_FIFO(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "fifo-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	// Set a long TTL so entries do not expire during the test.
	if err := store.SetIdleEntryTTL(ctx, poolName, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}

	if err := store.PutIdle(ctx, poolName, "sb-1"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-2"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-3"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	id, err := store.TryTakeIdle(ctx, poolName)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-1" {
		t.Fatalf("expected sb-1 (oldest), got %q", id)
	}

	id, err = store.TryTakeIdle(ctx, poolName)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-2" {
		t.Fatalf("expected sb-2, got %q", id)
	}

	id, err = store.TryTakeIdle(ctx, poolName)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-3" {
		t.Fatalf("expected sb-3, got %q", id)
	}

	// Pool should be empty now.
	id, err = store.TryTakeIdle(ctx, poolName)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "" {
		t.Fatalf("expected empty from exhausted pool, got %q", id)
	}
}

func TestRedisStore_TryTakeIdleWithMinTTL(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "minttl-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	// Put one entry with a very short TTL.
	if err := store.SetIdleEntryTTL(ctx, poolName, 500*time.Millisecond); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-short"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	// Put another entry with a long TTL.
	if err := store.SetIdleEntryTTL(ctx, poolName, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-long"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	// Request minimum 1 hour remaining; sb-short should be discarded alive.
	result, err := store.TryTakeIdleWithMinTTL(ctx, poolName, time.Hour)
	if err != nil {
		t.Fatalf("TryTakeIdleWithMinTTL error: %v", err)
	}
	if result.SandboxID != "sb-long" {
		t.Fatalf("expected sb-long, got %q", result.SandboxID)
	}
	if len(result.DiscardedAliveSandboxIDs) != 1 || result.DiscardedAliveSandboxIDs[0] != "sb-short" {
		t.Fatalf("expected [sb-short] in discarded, got %v", result.DiscardedAliveSandboxIDs)
	}
}

func TestRedisStore_PutIdle_Idempotent(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "idempotent-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	if err := store.SetIdleEntryTTL(ctx, poolName, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}

	if err := store.PutIdle(ctx, poolName, "sb-1"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	// Second put of same ID should be a no-op (entry is still alive).
	if err := store.PutIdle(ctx, poolName, "sb-1"); err != nil {
		t.Fatalf("PutIdle (duplicate) error: %v", err)
	}

	counters, err := store.SnapshotCounters(ctx, poolName)
	if err != nil {
		t.Fatalf("SnapshotCounters error: %v", err)
	}
	if counters.IdleCount != 1 {
		t.Fatalf("expected idle count 1 after duplicate put, got %d", counters.IdleCount)
	}

	// Take should yield exactly one entry.
	id, _ := store.TryTakeIdle(ctx, poolName)
	if id != "sb-1" {
		t.Fatalf("expected sb-1, got %q", id)
	}
	id, _ = store.TryTakeIdle(ctx, poolName)
	if id != "" {
		t.Fatalf("expected empty after taking the only entry, got %q", id)
	}
}

func TestRedisStore_RemoveIdle(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "remove-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	if err := store.SetIdleEntryTTL(ctx, poolName, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}

	// Remove from non-existent should not error.
	if err := store.RemoveIdle(ctx, poolName, "sb-nonexistent"); err != nil {
		t.Fatalf("RemoveIdle on non-existent should not error: %v", err)
	}

	if err := store.PutIdle(ctx, poolName, "sb-1"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-2"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	if err := store.RemoveIdle(ctx, poolName, "sb-1"); err != nil {
		t.Fatalf("RemoveIdle error: %v", err)
	}
	// Second remove should be no-op.
	if err := store.RemoveIdle(ctx, poolName, "sb-1"); err != nil {
		t.Fatalf("RemoveIdle (second call) error: %v", err)
	}

	// Only sb-2 should remain.
	id, _ := store.TryTakeIdle(ctx, poolName)
	if id != "sb-2" {
		t.Fatalf("expected sb-2 after removing sb-1, got %q", id)
	}
	id, _ = store.TryTakeIdle(ctx, poolName)
	if id != "" {
		t.Fatalf("expected empty after taking remaining entry, got %q", id)
	}
}

func TestRedisStore_Lock_AcquireRenewRelease(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "lock-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	// Acquire lock.
	acquired, err := store.TryAcquirePrimaryLock(ctx, poolName, "owner-1", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire lock")
	}

	// Renew by same owner should succeed.
	renewed, err := store.RenewPrimaryLock(ctx, poolName, "owner-1", 10*time.Second)
	if err != nil {
		t.Fatalf("RenewPrimaryLock error: %v", err)
	}
	if !renewed {
		t.Fatal("expected owner to renew their own lock")
	}

	// Another owner should fail to acquire.
	acquired, err = store.TryAcquirePrimaryLock(ctx, poolName, "owner-2", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if acquired {
		t.Fatal("expected owner-2 to fail acquiring lock held by owner-1")
	}

	// Release by owner.
	if err := store.ReleasePrimaryLock(ctx, poolName, "owner-1"); err != nil {
		t.Fatalf("ReleasePrimaryLock error: %v", err)
	}

	// Now owner-2 should succeed.
	acquired, err = store.TryAcquirePrimaryLock(ctx, poolName, "owner-2", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected owner-2 to acquire lock after release")
	}
}

func TestRedisStore_Lock_NonOwnerRenewFails(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "lock-nonowner-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	// Acquire lock as owner-1.
	acquired, err := store.TryAcquirePrimaryLock(ctx, poolName, "owner-1", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire lock")
	}

	// Renew as owner-2 should fail.
	renewed, err := store.RenewPrimaryLock(ctx, poolName, "owner-2", 10*time.Second)
	if err != nil {
		t.Fatalf("RenewPrimaryLock error: %v", err)
	}
	if renewed {
		t.Fatal("expected non-owner renew to fail")
	}

	// Release as non-owner should not release.
	if err := store.ReleasePrimaryLock(ctx, poolName, "owner-2"); err != nil {
		t.Fatalf("ReleasePrimaryLock error: %v", err)
	}

	// Original owner should still be able to renew.
	renewed, err = store.RenewPrimaryLock(ctx, poolName, "owner-1", 10*time.Second)
	if err != nil {
		t.Fatalf("RenewPrimaryLock error: %v", err)
	}
	if !renewed {
		t.Fatal("expected original owner to still hold the lock")
	}
}

func TestRedisStore_ReapExpired(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "reap-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	// Add entries with very short TTL.
	if err := store.SetIdleEntryTTL(ctx, poolName, 200*time.Millisecond); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-expire-1"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-expire-2"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	// Add one with long TTL.
	if err := store.SetIdleEntryTTL(ctx, poolName, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	if err := store.PutIdle(ctx, poolName, "sb-survive"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	// Wait for short-TTL entries to expire.
	time.Sleep(250 * time.Millisecond)

	// Reap expired entries.
	if err := store.ReapExpiredIdle(ctx, poolName, time.Now()); err != nil {
		t.Fatalf("ReapExpiredIdle error: %v", err)
	}

	// Only sb-survive should remain.
	entries, err := store.SnapshotIdleEntries(ctx, poolName)
	if err != nil {
		t.Fatalf("SnapshotIdleEntries error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after reap, got %d", len(entries))
	}
	if entries[0].SandboxID != "sb-survive" {
		t.Fatalf("expected sb-survive, got %q", entries[0].SandboxID)
	}
}

func TestRedisStore_MultiPoolIsolation(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolA := "poolA-" + t.Name()
	poolB := "poolB-" + t.Name()
	t.Cleanup(func() {
		cleanupPool(t, store, poolA)
		cleanupPool(t, store, poolB)
	})

	if err := store.SetIdleEntryTTL(ctx, poolA, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	if err := store.SetIdleEntryTTL(ctx, poolB, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}

	if err := store.PutIdle(ctx, poolA, "sb-a1"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, poolA, "sb-a2"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, poolB, "sb-b1"); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	// Take from pool-A should not affect pool-B.
	id, _ := store.TryTakeIdle(ctx, poolA)
	if id != "sb-a1" {
		t.Fatalf("expected sb-a1 from poolA, got %q", id)
	}

	countersA, _ := store.SnapshotCounters(ctx, poolA)
	countersB, _ := store.SnapshotCounters(ctx, poolB)

	if countersA.IdleCount != 1 {
		t.Fatalf("poolA expected 1 idle, got %d", countersA.IdleCount)
	}
	if countersB.IdleCount != 1 {
		t.Fatalf("poolB expected 1 idle, got %d", countersB.IdleCount)
	}

	// Lock on pool-A should not affect pool-B.
	acquired, _ := store.TryAcquirePrimaryLock(ctx, poolA, "owner-1", 5*time.Second)
	if !acquired {
		t.Fatal("expected to acquire lock on poolA")
	}

	acquired, _ = store.TryAcquirePrimaryLock(ctx, poolB, "owner-2", 5*time.Second)
	if !acquired {
		t.Fatal("expected to acquire lock on poolB independently")
	}
}

func TestRedisStore_ConcurrentAccess(t *testing.T) {
	store := newRedisTestStore(t)
	ctx := context.Background()
	poolName := "concurrent-" + t.Name()
	t.Cleanup(func() { cleanupPool(t, store, poolName) })

	if err := store.SetIdleEntryTTL(ctx, poolName, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}

	// Pre-populate some entries.
	for i := 0; i < 10; i++ {
		if err := store.PutIdle(ctx, poolName, "sb-pre-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("PutIdle pre-populate error: %v", err)
		}
	}

	const goroutines = 20
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			switch idx % 5 {
			case 0:
				_ = store.PutIdle(ctx, poolName, "sb-concurrent-"+strconv.Itoa(100+idx))
			case 1:
				_, _ = store.TryTakeIdle(ctx, poolName)
			case 2:
				_ = store.RemoveIdle(ctx, poolName, "sb-pre-"+strconv.Itoa(idx%10))
			case 3:
				_, _ = store.SnapshotCounters(ctx, poolName)
			case 4:
				ownerID := "owner-" + strconv.Itoa(idx)
				_, _ = store.TryAcquirePrimaryLock(ctx, poolName, ownerID, 100*time.Millisecond)
				_, _ = store.RenewPrimaryLock(ctx, poolName, ownerID, 100*time.Millisecond)
				_ = store.ReleasePrimaryLock(ctx, poolName, ownerID)
			}
		}(g)
	}

	wg.Wait()

	// If we got here without a panic or deadlock, concurrency is correct.
	counters, err := store.SnapshotCounters(ctx, poolName)
	if err != nil {
		t.Fatalf("SnapshotCounters error after concurrent access: %v", err)
	}
	if counters.IdleCount < 0 {
		t.Fatalf("idle count should be non-negative, got %d", counters.IdleCount)
	}
}

func TestRedisStore_WrapsClientFailures(t *testing.T) {
	// Connect to an unreachable address so every operation fails.
	store, err := NewRedisPoolStateStore(RedisPoolStateStoreConfig{
		Client: redis.NewClient(&redis.Options{
			Addr:        "localhost:1",
			DialTimeout: 100 * time.Millisecond,
		}),
		KeyPrefix: "opensandbox:test",
	})
	if err != nil {
		t.Fatalf("NewRedisPoolStateStore: %v", err)
	}
	ctx := context.Background()

	_, err = store.GetMaxIdle(ctx, "pool-broken")
	if err == nil {
		t.Fatal("expected error from broken Redis client, got nil")
	}

	var storeErr *opensandbox.PoolStateStoreUnavailableError
	if !errors.As(err, &storeErr) {
		t.Fatalf("expected *PoolStateStoreUnavailableError, got %T: %v", err, err)
	}
	if storeErr.Operation != "GetMaxIdle" {
		t.Errorf("Operation = %q, want %q", storeErr.Operation, "GetMaxIdle")
	}
}
