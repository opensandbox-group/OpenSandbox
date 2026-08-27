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

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==========================================================================
// Single-node InMemory pool E2E tests
// ==========================================================================

func TestPool_WarmupAcquireFailFastAndCommand(t *testing.T) {
	tag := poolTag("go-pool")
	store := opensandbox.NewInMemoryPoolStateStore()
	pool := createTestPool(t, "pool-"+tag, "owner-"+tag, store, tag, poolMaxIdle, nil)

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	eventually(t, "pool becomes healthy with warm idle",
		func() bool { return snapshotState(pool) == "HEALTHY" && snapshotIdle(pool) >= 1 }, 0, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sb, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })

	require.True(t, sb.IsHealthy(ctx))

	exec, err := sb.RunCommand(ctx, "echo go-pool-basic-ok", nil)
	require.NoError(t, err)
	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)
	require.Contains(t, exec.Text(), "go-pool-basic-ok")
}

func TestPool_ResizeReleaseFailFastAndDirectCreate(t *testing.T) {
	tag := poolTag("go-pool")
	store := opensandbox.NewInMemoryPoolStateStore()
	pool := createTestPool(t, "pool-"+tag, "owner-"+tag, store, tag, poolMaxIdle, nil)

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	eventually(t, "pool has warm idle", func() bool { return snapshotIdle(pool) >= 1 }, 0, 0)

	require.NoError(t, pool.Resize(context.Background(), 0))
	released, err := pool.ReleaseAllIdle(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, released, 0)
	eventually(t, "idle drains after resize zero", func() bool { return snapshotIdle(pool) == 0 }, 0, 0)

	ctx := context.Background()
	_, err = pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	var emptyErr *opensandbox.PoolEmptyError
	require.True(t, errors.As(err, &emptyErr), "expected PoolEmptyError, got %T: %v", err, err)

	direct, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = direct.Kill(context.Background()); _ = direct.Close() })
	require.True(t, direct.IsHealthy(ctx))
}

func TestPool_StaleIdleFallbackShutdownRestartSnapshot(t *testing.T) {
	tag := poolTag("go-pool")
	store := opensandbox.NewInMemoryPoolStateStore()
	poolName := "pool-" + tag
	pool := createTestPool(t, poolName, "owner-"+tag, store, tag, poolMaxIdle, nil)

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	// Inject a stale (nonexistent) sandbox ID.
	require.NoError(t, store.PutIdle(context.Background(), poolName, fmt.Sprintf("missing-%d", time.Now().UnixNano())))

	ctx := context.Background()
	fallback, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fallback.Kill(context.Background()); _ = fallback.Close() })
	require.True(t, fallback.IsHealthy(ctx))

	// Shutdown then verify PoolNotRunningError.
	require.NoError(t, pool.Shutdown(ctx, true))
	_, err = pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	var notRunErr *opensandbox.PoolNotRunningError
	require.True(t, errors.As(err, &notRunErr), "expected PoolNotRunningError, got %T: %v", err, err)

	snap, err := pool.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, "STOPPED", snap.LifecycleState.String())

	// Restart and verify snapshot_idle_entries have ExpiresAt.
	require.NoError(t, pool.Start(ctx))
	eventually(t, "pool restarts and warms idle",
		func() bool { return snapshotState(pool) == "HEALTHY" && snapshotIdle(pool) >= 1 }, 0, 0)

	entries, err := pool.SnapshotIdleEntries(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, entry := range entries {
		assert.NotEmpty(t, entry.SandboxID)
		assert.True(t, entry.ExpiresAt.After(time.Now()), "entry ExpiresAt should be in the future")
	}
}

// TestPool_RetryNextIdleSkipsStaleAndReturnsHealthyWarm verifies that AcquirePolicyRetryNextIdle
// pops and discards a stale idle candidate, then returns the next healthy warm sandbox in the
// FIFO idle queue.
//
// Real-world scenario: the pool's idle queue holds a stale entry ahead of a healthy warm
// sandbox — e.g. a leftover after a control-plane blip that killed the remote sandbox but
// left its id in the shared idle queue, or a slow-cold-starting custom template that fails
// its first ready-check. The caller wants a warm sandbox and is willing to skip a bounded
// number of bad candidates before giving up.
//
// Setup: pre-inject one stale id into the store before Start() so FIFO ordering guarantees
// it sits ahead of the real idle the reconciler creates to reach maxIdle=2. The pool then
// holds steady at IdleCount=2=[stale, real], neither shrinking nor rewarming.
func TestPool_RetryNextIdleSkipsStaleAndReturnsHealthyWarm(t *testing.T) {
	tag := poolTag("go-pool-retry-next-idle")
	store := opensandbox.NewInMemoryPoolStateStore()
	poolName := "pool-" + tag

	staleID := fmt.Sprintf("stale-%d", time.Now().UnixNano())
	require.NoError(t, store.PutIdle(context.Background(), poolName, staleID))

	pool := createTestPool(t, poolName, "owner-"+tag, store, tag, 2, &poolCreateOpts{
		maxAcquireRetries:   3,
		acquireReadyTimeout: 5 * time.Second,
	})

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	eventually(t, "pool warms one real idle behind pre-injected stale entry",
		func() bool { return snapshotIdle(pool) == 2 }, 0, 0)

	// Assert the stale id is at the HEAD of the FIFO idle queue. If a future store
	// implementation ever reordered on PutIdle, or if the reconciler somehow reaped the
	// stale before warmup landed, we want to catch it here instead of silently taking
	// the "healthy first" happy path and mis-calling it RETRY coverage.
	entriesBefore, err := store.SnapshotIdleEntries(context.Background(), poolName)
	require.NoError(t, err)
	require.Len(t, entriesBefore, 2)
	require.Equal(t, staleID, entriesBefore[0].SandboxID,
		"expected stale id at idle head; got queue=%v",
		[]string{entriesBefore[0].SandboxID, entriesBefore[1].SandboxID})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sb, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyRetryNextIdle),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })

	require.NotEqual(t, staleID, sb.ID(), "acquired sandbox must not be the stale id")
	require.True(t, sb.IsHealthy(ctx), "acquired sandbox should be healthy")

	exec, err := sb.RunCommand(ctx, "echo go-retry-next-idle-ok", nil)
	require.NoError(t, err)
	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)
	require.Contains(t, exec.Text(), "go-retry-next-idle-ok")

	// Stale id must not silently reappear in the idle queue.
	entries, err := store.SnapshotIdleEntries(context.Background(), poolName)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotEqual(t, staleID, entry.SandboxID, "stale id should have been removed by retry loop")
	}
}

// TestPool_RetryNextIdleThenCreateFallsThroughWhenAllStale verifies that
// AcquirePolicyRetryNextIdleThenCreate exhausts the retry budget on stale idle entries and
// then falls through to direct-create so the caller still gets a working sandbox.
//
// Real-world scenario: after a network flap every warm sandbox is unreachable; the caller
// cannot afford to wait for reconcile to drain them and still needs a working sandbox.
func TestPool_RetryNextIdleThenCreateFallsThroughWhenAllStale(t *testing.T) {
	tag := poolTag("go-pool-retry-then-create")
	store := opensandbox.NewInMemoryPoolStateStore()
	poolName := "pool-" + tag

	// Long reconcileInterval so the reconciler cannot race the acquire loop and shrink
	// stale entries out from under the RETRY_NEXT_IDLE_THEN_CREATE path. We need the
	// loop to actually exhaust maxAcquireRetries on stale connect failures (loopExhausted
	// path) before falling through, not to short-circuit via "idle buffer drained".
	pool := createTestPool(t, poolName, "owner-"+tag, store, tag, 0, &poolCreateOpts{
		maxAcquireRetries:   3,
		acquireReadyTimeout: 3 * time.Second,
		reconcileInterval:   5 * time.Minute,
	})

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	staleIDs := []string{
		fmt.Sprintf("stale-a-%d", time.Now().UnixNano()),
		fmt.Sprintf("stale-b-%d", time.Now().UnixNano()),
		fmt.Sprintf("stale-c-%d", time.Now().UnixNano()),
	}
	for _, id := range staleIDs {
		require.NoError(t, store.PutIdle(context.Background(), poolName, id))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sb, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyRetryNextIdleThenCreate),
	})
	require.NoError(t, err, "retry-then-create should fall through to direct create")
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })

	for _, id := range staleIDs {
		require.NotEqual(t, id, sb.ID(), "acquired sandbox must not be any stale id")
	}
	require.True(t, sb.IsHealthy(ctx))

	exec, err := sb.RunCommand(ctx, "echo go-retry-then-create-ok", nil)
	require.NoError(t, err)
	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)
	require.Contains(t, exec.Text(), "go-retry-then-create-ok")

	// Under the long ReconcileInterval configured on this pool, only the acquire retry
	// loop can pop entries from the idle queue. If the retry loop truly exhausted all
	// 3 stale candidates before falling through to direct-create, all 3 stale ids must
	// have been removed. If it short-circuited earlier (e.g. after a single attempt),
	// some stale ids would remain and this assertion catches the regression.
	remaining, err := store.SnapshotIdleEntries(context.Background(), poolName)
	require.NoError(t, err)
	remainingIDs := make(map[string]bool, len(remaining))
	for _, entry := range remaining {
		remainingIDs[entry.SandboxID] = true
	}
	for _, id := range staleIDs {
		require.False(t, remainingIDs[id],
			"retry loop did not fully exhaust stale queue; %s remained", id)
	}
}

// TestPool_RetryNextIdleAllStaleRaisesAfterBoundedRetries verifies that
// AcquirePolicyRetryNextIdle respects the maxAcquireRetries bound and raises
// PoolAcquireFailedError without falling through to direct-create when every candidate
// is stale.
func TestPool_RetryNextIdleAllStaleRaisesAfterBoundedRetries(t *testing.T) {
	tag := poolTag("go-pool-retry-raise")
	store := opensandbox.NewInMemoryPoolStateStore()
	poolName := "pool-" + tag

	// Long reconcileInterval: prevent the reconciler from shrinking stale ids before the
	// acquire loop can pop them. We need to prove the bound stops the loop after exactly
	// 2 stale attempts, not that the queue happened to drain.
	pool := createTestPool(t, poolName, "owner-"+tag, store, tag, 0, &poolCreateOpts{
		maxAcquireRetries:   2,
		acquireReadyTimeout: 3 * time.Second,
		reconcileInterval:   5 * time.Minute,
	})

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	// Inject more stales than the retry budget so we can also assert the loop stops at the
	// bound instead of draining the whole queue.
	for i := 0; i < 3; i++ {
		require.NoError(t, store.PutIdle(context.Background(),
			poolName, fmt.Sprintf("stale-%d-%d", i, time.Now().UnixNano())))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	_, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyRetryNextIdle),
	})
	var acquireFailed *opensandbox.PoolAcquireFailedError
	require.True(t, errors.As(err, &acquireFailed),
		"expected PoolAcquireFailedError, got %T: %v", err, err)

	// RETRY_NEXT_IDLE must not fall through to direct-create, so no tagged sandbox should
	// have been created.
	assert.Equal(t, 0, countTaggedSandboxes(t, tag),
		"RETRY_NEXT_IDLE must not fall through to direct-create")
}

func TestPool_LifecycleIdempotencyResizeRewarm(t *testing.T) {
	tag := poolTag("go-pool")
	store := opensandbox.NewInMemoryPoolStateStore()
	poolName := "pool-" + tag
	pool := createTestPool(t, poolName, "owner-"+tag, store, tag, poolMaxIdle, nil)

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })
	eventually(t, "pool warms before lifecycle checks", func() bool { return snapshotIdle(pool) >= 1 }, 0, 0)

	ctx := context.Background()

	// Idempotent shutdown.
	require.NoError(t, pool.Shutdown(ctx, false))
	require.NoError(t, pool.Shutdown(ctx, false))
	assert.Equal(t, "STOPPED", snapshotLifecycle(pool))

	_, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	var notRunErr *opensandbox.PoolNotRunningError
	require.True(t, errors.As(err, &notRunErr))

	// Release after stop.
	_, err = pool.ReleaseAllIdle(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, snapshotIdle(pool))

	// Inject fake IDs and release.
	require.NoError(t, store.PutIdle(ctx, poolName, "injected-a"))
	require.NoError(t, store.PutIdle(ctx, poolName, "injected-b"))
	released, err := pool.ReleaseAllIdle(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, released)
	assert.Equal(t, 0, snapshotIdle(pool))

	// Restart, resize to 0 then back.
	require.NoError(t, pool.Start(ctx))
	eventually(t, "pool rewarms after restart", func() bool { return snapshotIdle(pool) >= 1 }, 0, 0)

	require.NoError(t, pool.Resize(ctx, 0))
	_, _ = pool.ReleaseAllIdle(ctx)
	eventually(t, "remote sandboxes cleaned up",
		func() bool { return snapshotIdle(pool) == 0 && countTaggedSandboxes(t, tag) == 0 },
		60*time.Second, 0)

	require.NoError(t, pool.Resize(ctx, 1))
	eventually(t, "resize from zero to positive rewarms idle",
		func() bool { return snapshotState(pool) == "HEALTHY" && snapshotIdle(pool) >= 1 }, 0, 0)
}

func TestPool_ConcurrentAcquireAndShutdown(t *testing.T) {
	tag := poolTag("go-pool")
	store := opensandbox.NewInMemoryPoolStateStore()
	pool := createTestPool(t, "pool-"+tag, "owner-"+tag, store, tag, poolMaxIdle, nil)

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })
	eventually(t, "pool reaches target idle", func() bool { return snapshotIdle(pool) >= poolMaxIdle }, 0, 0)

	var (
		mu          sync.Mutex
		acquiredIDs = make(map[string]bool)
		borrowed    []*opensandbox.Sandbox
		errs        []error
		start       = make(chan struct{})
		wg          sync.WaitGroup
	)
	t.Cleanup(func() { cleanupBorrowed(borrowed) })

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			sb, err := pool.Acquire(context.Background(), opensandbox.AcquireOptions{
				SandboxTimeout: 5 * time.Minute,
				Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			mu.Lock()
			assert.False(t, acquiredIDs[sb.ID()], "duplicate sandbox ID: %s", sb.ID())
			acquiredIDs[sb.ID()] = true
			borrowed = append(borrowed, sb)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	require.Empty(t, errs, "unexpected errors during concurrent acquire: %v", errs)
	assert.Len(t, acquiredIDs, 4)

	// Race acquire vs shutdown.
	require.NoError(t, pool.Resize(context.Background(), 1))
	require.NoError(t, pool.Start(context.Background()))
	eventually(t, "pool rewarmed before shutdown race", func() bool { return snapshotIdle(pool) >= 1 }, 0, 0)

	start2 := make(chan struct{})
	var wg2 sync.WaitGroup
	var raceErrs []error

	for i := 0; i < 4; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			<-start2
			sb, err := pool.Acquire(context.Background(), opensandbox.AcquireOptions{
				SandboxTimeout: 5 * time.Minute,
				Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
			})
			if err != nil {
				var notRunErr *opensandbox.PoolNotRunningError
				if !errors.As(err, &notRunErr) {
					mu.Lock()
					raceErrs = append(raceErrs, err)
					mu.Unlock()
				}
				return
			}
			mu.Lock()
			borrowed = append(borrowed, sb)
			mu.Unlock()
		}()
	}
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		<-start2
		_ = pool.Shutdown(context.Background(), true)
	}()
	close(start2)
	wg2.Wait()

	assert.Empty(t, raceErrs, "unexpected errors during shutdown race: %v", raceErrs)
}

func TestPool_ConcurrentStartShutdownStress(t *testing.T) {
	tag := poolTag("go-pool")
	store := opensandbox.NewInMemoryPoolStateStore()
	pool := createTestPool(t, "pool-"+tag, "owner-"+tag, store, tag, poolMaxIdle, nil)

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	var (
		mu   sync.Mutex
		errs []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			for j := 0; j < 3; j++ {
				var err error
				if idx%2 == 0 {
					err = pool.Start(context.Background())
				} else {
					err = pool.Shutdown(context.Background(), idx%3 == 0)
				}
				if err != nil {
					// PoolNotRunningError is expected when Start races with Shutdown.
					var notRunning *opensandbox.PoolNotRunningError
					if !errors.As(err, &notRunning) {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Empty(t, errs, "unexpected errors during lifecycle stress: %v", errs)

	require.NoError(t, pool.Start(context.Background()))
	eventually(t, "pool remains usable after lifecycle stress",
		func() bool { return snapshotIdle(pool) >= 1 }, 0, 0)
}

func TestPool_WarmupPreparerAndIsolation(t *testing.T) {
	tag := poolTag("go-pool")
	markerPath := "/tmp/" + tag + "-prepared.txt"

	preparer := func(ctx context.Context, sb *opensandbox.Sandbox) error {
		_, err := sb.RunCommand(ctx, fmt.Sprintf("printf prepared > %s", markerPath), nil)
		return err
	}

	preparedPool := createTestPool(t, "prepared-pool-"+tag, "prepared-owner-"+tag,
		opensandbox.NewInMemoryPoolStateStore(), tag, 1,
		&poolCreateOpts{warmupSandboxPreparer: preparer})

	require.NoError(t, preparedPool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(preparedPool) })

	eventually(t, "prepared pool warms", func() bool { return snapshotIdle(preparedPool) >= 1 }, 0, 0)

	ctx := context.Background()
	sb, err := preparedPool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })

	exec, err := sb.RunCommand(ctx, "cat "+markerPath, nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "prepared")

	// Isolation: second pool with different tag.
	otherTag := poolTag("go-pool-other")
	otherPool := createTestPool(t, "pool-"+otherTag, "owner-"+otherTag,
		opensandbox.NewInMemoryPoolStateStore(), otherTag, 1, nil)
	require.NoError(t, otherPool.Start(ctx))
	t.Cleanup(func() { cleanupPool(otherPool); cleanupTaggedSandboxes(t, otherTag) })

	eventually(t, "other pool warms", func() bool { return snapshotIdle(otherPool) >= 1 }, 0, 0)

	// Draining one pool should not affect the other.
	require.NoError(t, preparedPool.Resize(ctx, 0))
	_, _ = preparedPool.ReleaseAllIdle(ctx)
	eventually(t, "prepared pool drains", func() bool { return snapshotIdle(preparedPool) == 0 }, 0, 0)
	assert.GreaterOrEqual(t, snapshotIdle(otherPool), 1)

	cleanupTaggedSandboxes(t, tag)
}

func TestPool_WarmupConcurrencyReachesTargetAndStaysBounded(t *testing.T) {
	tag := poolTag("go-pool")
	pool := createTestPool(t, "pool-"+tag, "owner-"+tag,
		opensandbox.NewInMemoryPoolStateStore(), tag, 3,
		&poolCreateOpts{warmupConcurrency: 2})

	require.NoError(t, pool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(pool); cleanupTaggedSandboxes(t, tag) })

	eventually(t, "concurrent warmup fills configured idle target",
		func() bool { return snapshotIdle(pool) >= 3 && countTaggedSandboxes(t, tag) <= 3 },
		90*time.Second, 0)
}

func TestPool_BrokenConnectionDegrades(t *testing.T) {
	tag := poolTag("go-pool")
	badTag := poolTag("go-pool-bad")
	badCfg := brokenConnectionConfig()

	badPool := createTestPool(t, "bad-pool-"+tag, "bad-owner-"+tag,
		opensandbox.NewInMemoryPoolStateStore(), badTag, 1,
		&poolCreateOpts{
			connectionConfig:    &badCfg,
			degradedThreshold:   1,
			warmupReadyTimeout:  1 * time.Second,
			acquireReadyTimeout: 1 * time.Second,
		})

	require.NoError(t, badPool.Start(context.Background()))
	t.Cleanup(func() { cleanupPool(badPool); cleanupTaggedSandboxes(t, badTag) })

	eventually(t, "bad pool enters degraded state",
		func() bool { return snapshotState(badPool) == "DEGRADED" },
		60*time.Second, 0)

	snap, err := badPool.Snapshot(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, snap.LastError)
	assert.Equal(t, 0, snap.IdleCount)

	ctx := context.Background()
	_, err = badPool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 1 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	var emptyErr *opensandbox.PoolEmptyError
	require.True(t, errors.As(err, &emptyErr))

	_, err = badPool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 1 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	require.Error(t, err)

	// Healthy pool works independently.
	healthyTag := poolTag("go-pool-good")
	healthyPool := createTestPool(t, "healthy-pool-"+tag, "healthy-owner-"+tag,
		opensandbox.NewInMemoryPoolStateStore(), healthyTag, 1, nil)
	require.NoError(t, healthyPool.Start(ctx))
	t.Cleanup(func() { cleanupPool(healthyPool); cleanupTaggedSandboxes(t, healthyTag) })

	eventually(t, "healthy pool still warms after broken pool path",
		func() bool { return snapshotIdle(healthyPool) >= 1 }, 0, 0)

	sb, err := healthyPool.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })
	require.True(t, sb.IsHealthy(ctx))
}

// ==========================================================================
// Redis distributed pool E2E tests
// ==========================================================================

func skipWithoutRedis(t *testing.T) *redis.Client {
	t.Helper()
	redisURL := os.Getenv("OPENSANDBOX_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("Set OPENSANDBOX_TEST_REDIS_URL to run Redis-backed pool E2E tests")
	}
	opts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	client := redis.NewClient(opts)
	require.NoError(t, client.Ping(context.Background()).Err(), "Redis not reachable")
	t.Cleanup(func() { client.Close() })
	return client
}

func newRedisE2EStore(t *testing.T, client *redis.Client, prefix string) *poolredis.RedisPoolStateStore {
	t.Helper()
	store, err := poolredis.NewRedisPoolStateStore(poolredis.RedisPoolStateStoreConfig{
		Client:    client,
		KeyPrefix: prefix,
	})
	if err != nil {
		t.Fatalf("NewRedisPoolStateStore: %v", err)
	}
	return store
}

func cleanupRedisKeys(t *testing.T, client *redis.Client, prefix string) {
	t.Helper()
	ctx := context.Background()
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, prefix+":*", 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

func TestRedisPool_CrossNodeAcquireResizeDirectCreate(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-pool-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	storeB := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, "owner-a-"+tag, storeA, tag, poolMaxIdle, nil)
	poolB := createTestPool(t, poolName, "owner-b-"+tag, storeB, tag, poolMaxIdle, nil)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupPool(poolB)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	require.NoError(t, poolA.Start(context.Background()))
	require.NoError(t, poolB.Start(context.Background()))
	eventually(t, "Redis pool warms", func() bool { return snapshotIdle(poolA) >= 1 }, 0, 0)

	ctx := context.Background()
	sb, err := poolB.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })
	require.True(t, sb.IsHealthy(ctx))

	exec, err := sb.RunCommand(ctx, "echo go-redis-dist-ok", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "go-redis-dist-ok")

	// Shared resize to 0.
	require.NoError(t, poolB.Resize(ctx, 0))
	eventually(t, "Redis idle drains after shared resize", func() bool { return snapshotIdle(poolA) == 0 }, 0, 0)
	time.Sleep(poolReconcileInterval * 2)
	assert.Equal(t, 0, snapshotIdle(poolA))

	_, err = poolA.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 2 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	var emptyErr *opensandbox.PoolEmptyError
	require.True(t, errors.As(err, &emptyErr))

	// Direct create fallback.
	direct, err := poolA.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = direct.Kill(context.Background()); _ = direct.Close() })
	require.True(t, direct.IsHealthy(ctx))
	assert.Equal(t, 0, snapshotIdle(poolA))
}

func TestRedisPool_PrimaryFailoverRestart(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-failover-" + tag
	ownerA := "owner-a-" + tag
	ownerB := "owner-b-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	storeB := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, ownerA, storeA, tag, 1, nil)
	poolB := createTestPool(t, poolName, ownerB, storeB, tag, 1, nil)
	lockKey := storeA.PrimaryLockKey(poolName)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupPool(poolB)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	ctx := context.Background()
	require.NoError(t, poolA.Start(ctx))
	eventually(t, "first Redis node owns primary lock and warms",
		func() bool {
			v, _ := rdb.Get(ctx, lockKey).Result()
			return v == ownerA && snapshotIdle(poolA) >= 1
		}, 0, 0)

	require.NoError(t, poolB.Start(ctx))
	require.NoError(t, poolA.Shutdown(ctx, false))
	require.NoError(t, poolB.Resize(ctx, 1))

	eventually(t, "primary lock fails over to remaining Redis node",
		func() bool {
			v, _ := rdb.Get(ctx, lockKey).Result()
			return v == ownerB && snapshotIdle(poolB) >= 1
		}, 60*time.Second, 0)

	// Restart node A and apply resize jitter.
	require.NoError(t, poolA.Start(ctx))
	for i := 0; i < 6; i++ {
		p := poolA
		if i%2 != 0 {
			p = poolB
		}
		_ = p.Resize(ctx, i%3)
		time.Sleep(200 * time.Millisecond)
	}
	require.NoError(t, poolB.Resize(ctx, 1))
	eventually(t, "Redis restart and resize jitter converge",
		func() bool { return snapshotIdle(poolA) <= 1 && countTaggedSandboxes(t, tag) <= 2 },
		60*time.Second, 0)
}

func TestRedisPool_RestartOverwritesStaleMaxIdle(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-restart-config-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, "owner-a-"+tag, storeA, tag, 1, nil)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	ctx := context.Background()
	require.NoError(t, poolA.Start(ctx))
	eventually(t, "initial Redis pool warms", func() bool { return snapshotIdle(poolA) >= 1 }, 0, 0)

	require.NoError(t, poolA.Resize(ctx, 0))
	eventually(t, "initial Redis pool drains to zero", func() bool { return snapshotIdle(poolA) == 0 }, 0, 0)
	require.NoError(t, poolA.Shutdown(ctx, false))

	storeB := newRedisE2EStore(t, rdb, prefix)
	poolB := createTestPool(t, poolName, "owner-b-"+tag, storeB, tag, 2, nil)
	t.Cleanup(func() { cleanupPool(poolB) })

	require.NoError(t, poolB.Start(ctx))
	eventually(t, "restart with same Redis namespace uses new configured max_idle",
		func() bool {
			snap, err := poolB.Snapshot(ctx)
			return err == nil && snap.MaxIdle == 2 && snap.IdleCount >= 2
		}, 0, 0)
}

func TestRedisPool_SecondaryResizeAppliedByPrimary(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-secondary-resize-" + tag
	ownerA := "owner-a-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	storeB := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, ownerA, storeA, tag, 2, nil)
	poolB := createTestPool(t, poolName, "owner-b-"+tag, storeB, tag, 2, nil)
	lockKey := storeA.PrimaryLockKey(poolName)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupPool(poolB)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	ctx := context.Background()
	require.NoError(t, poolA.Start(ctx))
	eventually(t, "primary Redis node owns lock and warms",
		func() bool {
			v, _ := rdb.Get(ctx, lockKey).Result()
			return v == ownerA && snapshotIdle(poolA) >= 2
		}, 0, 0)
	require.NoError(t, poolB.Start(ctx))

	require.NoError(t, poolB.Resize(ctx, 0))
	eventually(t, "secondary resize to zero is applied by primary",
		func() bool {
			v, _ := rdb.Get(ctx, lockKey).Result()
			return v == ownerA && snapshotIdle(poolA) == 0
		}, 0, 0)

	require.NoError(t, poolB.Resize(ctx, 2))
	eventually(t, "secondary resize up is applied by primary",
		func() bool {
			v, _ := rdb.Get(ctx, lockKey).Result()
			return v == ownerA && snapshotIdle(poolA) >= 2
		}, 0, 0)
}

func TestRedisPool_ConcurrentCrossNodeAcquireAtomicTake(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-concurrent-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	storeB := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, "owner-a-"+tag, storeA, tag, poolMaxIdle, nil)
	poolB := createTestPool(t, poolName, "owner-b-"+tag, storeB, tag, poolMaxIdle, nil)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupPool(poolB)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	ctx := context.Background()
	require.NoError(t, poolA.Start(ctx))
	require.NoError(t, poolB.Start(ctx))
	eventually(t, "Redis pool warms two idle", func() bool { return snapshotIdle(poolA) >= 2 }, 0, 0)

	var (
		mu       sync.Mutex
		borrowed []*opensandbox.Sandbox
		ids      = make(map[string]bool)
		start    = make(chan struct{})
		wg       sync.WaitGroup
	)
	t.Cleanup(func() { cleanupBorrowed(borrowed) })

	for i, p := range []*opensandbox.DefaultSandboxPool{poolA, poolB} {
		wg.Add(1)
		go func(pool *opensandbox.DefaultSandboxPool, idx int) {
			defer wg.Done()
			<-start
			sb, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
				SandboxTimeout: 5 * time.Minute,
				Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
			})
			require.NoError(t, err)
			mu.Lock()
			assert.False(t, ids[sb.ID()], "duplicate sandbox ID")
			ids[sb.ID()] = true
			borrowed = append(borrowed, sb)
			mu.Unlock()
		}(p, i)
	}
	close(start)
	wg.Wait()
	assert.Len(t, ids, 2)

	// Pure-store contention test: 50 IDs, 16 goroutines.
	contentionStore := newRedisE2EStore(t, rdb, prefix)
	contentionPool := "redis-store-contention-" + tag
	for i := 0; i < 50; i++ {
		require.NoError(t, contentionStore.PutIdle(ctx, contentionPool, fmt.Sprintf("id-%d", i)))
	}

	var (
		takenMu sync.Mutex
		taken   = make(map[string]bool)
		wg2     sync.WaitGroup
	)
	for g := 0; g < 16; g++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for {
				id, err := contentionStore.TryTakeIdle(ctx, contentionPool)
				if err != nil || id == "" {
					return
				}
				takenMu.Lock()
				assert.False(t, taken[id], "duplicate take: %s", id)
				taken[id] = true
				takenMu.Unlock()
			}
		}()
	}
	wg2.Wait()
	assert.Len(t, taken, 50)

	counters, err := contentionStore.SnapshotCounters(ctx, contentionPool)
	require.NoError(t, err)
	assert.Equal(t, 0, counters.IdleCount)
}

func TestRedisPool_ExpiredIdleReapedByTakeNotSnapshot(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-expired-idle-" + tag
	t.Cleanup(func() { cleanupRedisKeys(t, rdb, prefix) })

	store := newRedisE2EStore(t, rdb, prefix)
	ctx := context.Background()

	require.NoError(t, store.SetIdleEntryTTL(ctx, poolName, 50*time.Millisecond))
	require.NoError(t, store.PutIdle(ctx, poolName, fmt.Sprintf("expired-%d", time.Now().UnixNano())))
	time.Sleep(100 * time.Millisecond)

	// SnapshotCounters filters expired entries, so it should already report 0.
	counters, err := store.SnapshotCounters(ctx, poolName)
	require.NoError(t, err)
	assert.Equal(t, 0, counters.IdleCount)

	// TryTakeIdle also returns empty for expired entries.
	id, err := store.TryTakeIdle(ctx, poolName)
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestRedisPool_ConcurrentAcquireResizeJitter(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-acquire-resize-jitter-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	storeB := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, "owner-a-"+tag, storeA, tag, poolMaxIdle, nil)
	poolB := createTestPool(t, poolName, "owner-b-"+tag, storeB, tag, poolMaxIdle, nil)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupPool(poolB)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	ctx := context.Background()
	require.NoError(t, poolA.Start(ctx))
	require.NoError(t, poolB.Start(ctx))
	eventually(t, "Redis jitter pool warms two idle", func() bool { return snapshotIdle(poolA) >= 2 }, 0, 0)

	var (
		mu          sync.Mutex
		acquiredIDs = make(map[string]bool)
		borrowed    []*opensandbox.Sandbox
		wg          sync.WaitGroup
	)
	t.Cleanup(func() { cleanupBorrowed(borrowed) })

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p := poolA
			if idx%2 != 0 {
				p = poolB
			}
			sb, err := p.Acquire(ctx, opensandbox.AcquireOptions{
				SandboxTimeout: 5 * time.Minute,
				Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
			})
			require.NoError(t, err)
			mu.Lock()
			assert.False(t, acquiredIDs[sb.ID()], "duplicate sandbox ID")
			acquiredIDs[sb.ID()] = true
			borrowed = append(borrowed, sb)
			mu.Unlock()

			exec, err := sb.RunCommand(ctx, fmt.Sprintf("echo go-redis-jitter-%d", idx), nil)
			require.NoError(t, err)
			require.NotNil(t, exec.ExitCode)
			assert.Equal(t, 0, *exec.ExitCode)
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			p := poolA
			if i%2 != 0 {
				p = poolB
			}
			_ = p.Resize(ctx, i%3)
			time.Sleep(200 * time.Millisecond)
		}
		_ = poolB.Resize(ctx, 2)
	}()
	wg.Wait()

	assert.Len(t, acquiredIDs, 4)
	eventually(t, "Redis acquire plus resize jitter converges and stays bounded",
		func() bool { return snapshotIdle(poolA) <= 2 && countTaggedSandboxes(t, tag) <= 8 },
		90*time.Second, 0)
}

func TestRedisPool_StaleIdleRemovedAndDirectCreateFallback(t *testing.T) {
	rdb := skipWithoutRedis(t)
	tag := poolTag("go-redis")
	prefix := "opensandbox:e2e:" + tag
	poolName := "redis-stale-" + tag

	storeA := newRedisE2EStore(t, rdb, prefix)
	storeB := newRedisE2EStore(t, rdb, prefix)
	poolA := createTestPool(t, poolName, "owner-a-"+tag, storeA, tag, 0, nil)
	poolB := createTestPool(t, poolName, "owner-b-"+tag, storeB, tag, 0, nil)
	t.Cleanup(func() {
		cleanupPool(poolA)
		cleanupPool(poolB)
		cleanupTaggedSandboxes(t, tag)
		cleanupRedisKeys(t, rdb, prefix)
	})

	ctx := context.Background()
	require.NoError(t, poolA.Start(ctx))
	require.NoError(t, poolB.Start(ctx))

	// Inject a nonexistent sandbox ID.
	require.NoError(t, storeA.PutIdle(ctx, poolName, fmt.Sprintf("missing-%d", time.Now().UnixNano())))

	_, err := poolB.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 2 * time.Second,
		Policy:         policyPtr(opensandbox.AcquirePolicyFailFast),
	})
	var failedErr *opensandbox.PoolAcquireFailedError
	require.True(t, errors.As(err, &failedErr), "expected PoolAcquireFailedError, got %T: %v", err, err)

	counters, err := storeA.SnapshotCounters(ctx, poolName)
	require.NoError(t, err)
	assert.Equal(t, 0, counters.IdleCount)

	sb, err := poolB.Acquire(ctx, opensandbox.AcquireOptions{
		SandboxTimeout: 5 * time.Minute,
		Policy:         policyPtr(opensandbox.AcquirePolicyDirectCreate),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()); _ = sb.Close() })
	require.True(t, sb.IsHealthy(ctx))
}

// ---------- Helpers ----------

func policyPtr(p opensandbox.AcquirePolicy) *opensandbox.AcquirePolicy {
	return &p
}
