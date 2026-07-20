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
	"testing"
	"time"
)

func newTestStore(t *testing.T) *InMemoryPoolStateStore {
	t.Helper()
	return NewInMemoryPoolStateStore()
}

func mustPutIdle(t *testing.T, store *InMemoryPoolStateStore, pool, id string) {
	t.Helper()
	if err := store.PutIdle(context.Background(), pool, id); err != nil {
		t.Fatalf("PutIdle(%q, %q) unexpected error: %v", pool, id, err)
	}
}

func TestInMemoryStore_TryTakeIdle_FIFO(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	mustPutIdle(t, store, pool, "sb-1")
	mustPutIdle(t, store, pool, "sb-2")
	mustPutIdle(t, store, pool, "sb-3")

	id, err := store.TryTakeIdle(ctx, pool)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-1" {
		t.Fatalf("expected sb-1 (oldest), got %q", id)
	}

	id, err = store.TryTakeIdle(ctx, pool)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-2" {
		t.Fatalf("expected sb-2, got %q", id)
	}

	id, err = store.TryTakeIdle(ctx, pool)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-3" {
		t.Fatalf("expected sb-3, got %q", id)
	}
}

func TestInMemoryStore_TryTakeIdle_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, err := store.TryTakeIdle(ctx, "empty-pool")
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "" {
		t.Fatalf("expected empty string from empty pool, got %q", id)
	}
}

func TestInMemoryStore_TryTakeIdle_SkipsExpired(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	// Set a very short TTL so the first entry expires quickly.
	if err := store.SetIdleEntryTTL(ctx, pool, 10*time.Millisecond); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}

	mustPutIdle(t, store, pool, "sb-expire")

	// Now set a long TTL and add another entry.
	if err := store.SetIdleEntryTTL(ctx, pool, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-valid")

	// Wait for the first entry to expire.
	time.Sleep(15 * time.Millisecond)

	id, err := store.TryTakeIdle(ctx, pool)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if id != "sb-valid" {
		t.Fatalf("expected sb-valid (expired sb-expire should be skipped), got %q", id)
	}
}

func TestInMemoryStore_TryTakeIdleWithMinTTL(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	// Set TTL to 100ms; entries will have ~100ms remaining.
	if err := store.SetIdleEntryTTL(ctx, pool, 100*time.Millisecond); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-near-expiry")

	// Set a long TTL and add another entry.
	if err := store.SetIdleEntryTTL(ctx, pool, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-long-lived")

	// Request minimum 1 hour remaining; sb-near-expiry should be discarded alive.
	result, err := store.TryTakeIdleWithMinTTL(ctx, pool, time.Hour)
	if err != nil {
		t.Fatalf("TryTakeIdleWithMinTTL error: %v", err)
	}
	if result.SandboxID != "sb-long-lived" {
		t.Fatalf("expected sb-long-lived, got %q", result.SandboxID)
	}
	if len(result.DiscardedAliveSandboxIDs) != 1 || result.DiscardedAliveSandboxIDs[0] != "sb-near-expiry" {
		t.Fatalf("expected [sb-near-expiry] in discarded, got %v", result.DiscardedAliveSandboxIDs)
	}
}

func TestInMemoryStore_PutIdle_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	mustPutIdle(t, store, pool, "sb-1")
	mustPutIdle(t, store, pool, "sb-1") // duplicate

	counters, err := store.SnapshotCounters(ctx, pool)
	if err != nil {
		t.Fatalf("SnapshotCounters error: %v", err)
	}
	if counters.IdleCount != 1 {
		t.Fatalf("expected idle count 1 after duplicate put, got %d", counters.IdleCount)
	}

	// Take should yield exactly one entry.
	id, _ := store.TryTakeIdle(ctx, pool)
	if id != "sb-1" {
		t.Fatalf("expected sb-1, got %q", id)
	}
	id, _ = store.TryTakeIdle(ctx, pool)
	if id != "" {
		t.Fatalf("expected empty after taking the only entry, got %q", id)
	}
}

func TestInMemoryStore_RemoveIdle_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	// Remove from non-existent pool should not error.
	if err := store.RemoveIdle(ctx, pool, "sb-nonexistent"); err != nil {
		t.Fatalf("RemoveIdle on non-existent should not error: %v", err)
	}

	mustPutIdle(t, store, pool, "sb-1")

	if err := store.RemoveIdle(ctx, pool, "sb-1"); err != nil {
		t.Fatalf("RemoveIdle error: %v", err)
	}
	// Second remove should be no-op.
	if err := store.RemoveIdle(ctx, pool, "sb-1"); err != nil {
		t.Fatalf("RemoveIdle (second call) error: %v", err)
	}

	id, _ := store.TryTakeIdle(ctx, pool)
	if id != "" {
		t.Fatalf("expected empty after remove, got %q", id)
	}
}

func TestInMemoryStore_Lock_AcquireAndRelease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	acquired, err := store.TryAcquirePrimaryLock(ctx, pool, "owner-1", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire lock")
	}

	// Another owner should fail.
	acquired, err = store.TryAcquirePrimaryLock(ctx, pool, "owner-2", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if acquired {
		t.Fatal("expected owner-2 to fail acquiring lock held by owner-1")
	}

	// Release by owner.
	if err := store.ReleasePrimaryLock(ctx, pool, "owner-1"); err != nil {
		t.Fatalf("ReleasePrimaryLock error: %v", err)
	}

	// Now owner-2 should succeed.
	acquired, err = store.TryAcquirePrimaryLock(ctx, pool, "owner-2", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected owner-2 to acquire lock after release")
	}
}

func TestInMemoryStore_Lock_TTLExpiry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	acquired, err := store.TryAcquirePrimaryLock(ctx, pool, "owner-1", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire lock")
	}

	// Wait for TTL to expire.
	time.Sleep(25 * time.Millisecond)

	// Another owner should now be able to acquire.
	acquired, err = store.TryAcquirePrimaryLock(ctx, pool, "owner-2", 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquirePrimaryLock error: %v", err)
	}
	if !acquired {
		t.Fatal("expected owner-2 to acquire lock after TTL expiry")
	}
}

func TestInMemoryStore_Lock_RenewByOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	_, _ = store.TryAcquirePrimaryLock(ctx, pool, "owner-1", 5*time.Second)

	renewed, err := store.RenewPrimaryLock(ctx, pool, "owner-1", 10*time.Second)
	if err != nil {
		t.Fatalf("RenewPrimaryLock error: %v", err)
	}
	if !renewed {
		t.Fatal("expected owner to renew their own lock")
	}
}

func TestInMemoryStore_Lock_RenewByNonOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	_, _ = store.TryAcquirePrimaryLock(ctx, pool, "owner-1", 5*time.Second)

	renewed, err := store.RenewPrimaryLock(ctx, pool, "owner-2", 10*time.Second)
	if err != nil {
		t.Fatalf("RenewPrimaryLock error: %v", err)
	}
	if renewed {
		t.Fatal("expected non-owner renew to fail")
	}
}

func TestInMemoryStore_ReapExpired(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	// Set short TTL and add entries.
	if err := store.SetIdleEntryTTL(ctx, pool, 50*time.Millisecond); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-1")
	mustPutIdle(t, store, pool, "sb-2")

	// Set long TTL and add another.
	if err := store.SetIdleEntryTTL(ctx, pool, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-3")

	// Advance time past the short TTL.
	time.Sleep(55 * time.Millisecond)

	if err := store.ReapExpiredIdle(ctx, pool, time.Now()); err != nil {
		t.Fatalf("ReapExpiredIdle error: %v", err)
	}

	entries, err := store.SnapshotIdleEntries(ctx, pool)
	if err != nil {
		t.Fatalf("SnapshotIdleEntries error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after reap, got %d", len(entries))
	}
	if entries[0].SandboxID != "sb-3" {
		t.Fatalf("expected sb-3 to survive reap, got %q", entries[0].SandboxID)
	}
}

func TestInMemoryStore_ReapExpiredWithMinTTL(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	// sb-near: 200ms TTL (will be alive but below 1h threshold).
	if err := store.SetIdleEntryTTL(ctx, pool, 200*time.Millisecond); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-near")

	// sb-healthy: 24h TTL.
	if err := store.SetIdleEntryTTL(ctx, pool, 24*time.Hour); err != nil {
		t.Fatalf("SetIdleEntryTTL error: %v", err)
	}
	mustPutIdle(t, store, pool, "sb-healthy")

	result, err := store.ReapExpiredIdleWithMinTTL(ctx, pool, time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("ReapExpiredIdleWithMinTTL error: %v", err)
	}

	if len(result.DiscardedAliveSandboxIDs) != 1 || result.DiscardedAliveSandboxIDs[0] != "sb-near" {
		t.Fatalf("expected [sb-near] in discarded alive, got %v", result.DiscardedAliveSandboxIDs)
	}

	entries, err := store.SnapshotIdleEntries(ctx, pool)
	if err != nil {
		t.Fatalf("SnapshotIdleEntries error: %v", err)
	}
	if len(entries) != 1 || entries[0].SandboxID != "sb-healthy" {
		t.Fatalf("expected only sb-healthy, got %v", entries)
	}
}

func TestInMemoryStore_SnapshotCounters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "test-pool"

	mustPutIdle(t, store, pool, "sb-1")
	mustPutIdle(t, store, pool, "sb-2")
	mustPutIdle(t, store, pool, "sb-3")

	counters, err := store.SnapshotCounters(ctx, pool)
	if err != nil {
		t.Fatalf("SnapshotCounters error: %v", err)
	}
	if counters.IdleCount != 3 {
		t.Fatalf("expected idle count 3, got %d", counters.IdleCount)
	}

	// Remove one and verify.
	_ = store.RemoveIdle(ctx, pool, "sb-2")
	counters, err = store.SnapshotCounters(ctx, pool)
	if err != nil {
		t.Fatalf("SnapshotCounters error: %v", err)
	}
	if counters.IdleCount != 2 {
		t.Fatalf("expected idle count 2 after remove, got %d", counters.IdleCount)
	}
}

func TestInMemoryStore_MultiPoolIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mustPutIdle(t, store, "pool-a", "sb-a1")
	mustPutIdle(t, store, "pool-a", "sb-a2")
	mustPutIdle(t, store, "pool-b", "sb-b1")

	// Take from pool-a should not affect pool-b.
	id, _ := store.TryTakeIdle(ctx, "pool-a")
	if id != "sb-a1" {
		t.Fatalf("expected sb-a1 from pool-a, got %q", id)
	}

	countersA, _ := store.SnapshotCounters(ctx, "pool-a")
	countersB, _ := store.SnapshotCounters(ctx, "pool-b")

	if countersA.IdleCount != 1 {
		t.Fatalf("pool-a expected 1 idle, got %d", countersA.IdleCount)
	}
	if countersB.IdleCount != 1 {
		t.Fatalf("pool-b expected 1 idle, got %d", countersB.IdleCount)
	}

	// Lock on pool-a should not affect pool-b.
	acquired, _ := store.TryAcquirePrimaryLock(ctx, "pool-a", "owner-1", 5*time.Second)
	if !acquired {
		t.Fatal("expected to acquire lock on pool-a")
	}

	acquired, _ = store.TryAcquirePrimaryLock(ctx, "pool-b", "owner-2", 5*time.Second)
	if !acquired {
		t.Fatal("expected to acquire lock on pool-b independently")
	}
}

func TestInMemoryStore_ConcurrentAccess(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pool := "concurrent-pool"

	const goroutines = 20
	var wg sync.WaitGroup

	// Pre-populate some entries.
	for i := 0; i < 10; i++ {
		mustPutIdle(t, store, pool, idForIndex(i))
	}

	// Launch concurrent goroutines doing mixed operations.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			switch idx % 5 {
			case 0:
				// Put
				_ = store.PutIdle(ctx, pool, idForIndex(100+idx))
			case 1:
				// Take
				_, _ = store.TryTakeIdle(ctx, pool)
			case 2:
				// Remove
				_ = store.RemoveIdle(ctx, pool, idForIndex(idx%10))
			case 3:
				// Snapshot
				_, _ = store.SnapshotCounters(ctx, pool)
			case 4:
				// Lock operations
				_, _ = store.TryAcquirePrimaryLock(ctx, pool, idForIndex(idx), 100*time.Millisecond)
				_, _ = store.RenewPrimaryLock(ctx, pool, idForIndex(idx), 100*time.Millisecond)
				_ = store.ReleasePrimaryLock(ctx, pool, idForIndex(idx))
			}
		}(g)
	}

	wg.Wait()

	// If we got here without a race detector panic, concurrency is correct.
	// Just verify the store is still functional.
	counters, err := store.SnapshotCounters(ctx, pool)
	if err != nil {
		t.Fatalf("SnapshotCounters error after concurrent access: %v", err)
	}
	if counters.IdleCount < 0 {
		t.Fatalf("idle count should be non-negative, got %d", counters.IdleCount)
	}
}

func idForIndex(i int) string {
	return "sb-" + intToStr(i)
}

// intToStr converts an int to string without importing strconv (keep imports minimal).
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	digits := make([]byte, 0, 10)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	if neg {
		digits = append(digits, '-')
	}
	// Reverse.
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
