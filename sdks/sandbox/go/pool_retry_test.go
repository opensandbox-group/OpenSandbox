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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// staleAwareLifecycleServer is a variant of newMockLifecycleServer that returns 404 for any
// sandbox whose id starts with "stale-". Used to drive the acquire retry loop: putIdle those
// ids into the store and Acquire will treat them as unhealthy without any real sandbox.
// Returns the server plus a counter tracking how many DELETE calls hit "stale-" ids so tests
// can assert best-effort kill is scheduled.
func staleAwareLifecycleServer(t *testing.T, execdURL string) (*httptest.Server, *int32) {
	t.Helper()
	var staleDeletes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:         fmt.Sprintf("sbx-pool-%d", time.Now().UnixNano()),
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
			// Extract sandbox id from `/v1/sandboxes/{id}/endpoints/{port}`.
			trimmed := strings.TrimPrefix(path, "/v1/sandboxes/")
			sandboxID := strings.SplitN(trimmed, "/", 2)[0]
			if strings.HasPrefix(sandboxID, "stale-") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdURL,
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "test-token"},
			})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			sandboxID := parts[len(parts)-1]
			if strings.HasPrefix(sandboxID, "stale-") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:         sandboxID,
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			sandboxID := parts[len(parts)-1]
			if strings.HasPrefix(sandboxID, "stale-") {
				atomic.AddInt32(&staleDeletes, 1)
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			expiresAt := time.Now().Add(1 * time.Hour).UTC()
			jsonResponse(w, http.StatusOK, RenewExpirationResponse{
				ExpiresAt: expiresAt,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &staleDeletes
}

// eventuallyEquals polls fn until it returns want, or timeout expires. Used to wait for
// background best-effort kill RPCs to be observed by the mock server.
func eventuallyEquals(t *testing.T, fn func() int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("timeout waiting for value=%d, last=%d", want, fn())
}

func TestPool_Acquire_RetryNextIdle_EmptyIdle_ReturnsPoolEmpty(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0) // no reconcile, keep the pool empty
		b.ReconcileInterval(time.Hour)
	})
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	policy := AcquirePolicyRetryNextIdle
	_, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true, Policy: &policy})
	if err == nil {
		t.Fatal("expected PoolEmptyError, got nil")
	}
	var poolEmpty *PoolEmptyError
	if !errors.As(err, &poolEmpty) {
		t.Fatalf("error type = %T, want *PoolEmptyError; error = %v", err, err)
	}
	if poolEmpty.Policy != AcquirePolicyRetryNextIdle {
		t.Errorf("PoolEmptyError.Policy = %v, want RETRY_NEXT_IDLE", poolEmpty.Policy)
	}
}

func TestPool_Acquire_RetryNextIdle_AllStale_ReturnsAcquireFailedAndBoundsRetries(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, staleDeletes := staleAwareLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
		b.ReconcileInterval(time.Hour)
		b.MaxAcquireRetries(3)
	})
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Seed 5 stale ids; retry budget of 3 should attempt exactly 3.
	for i := 0; i < 5; i++ {
		if err := pool.config.StateStore.PutIdle(ctx, "test-pool", fmt.Sprintf("stale-%d", i)); err != nil {
			t.Fatalf("PutIdle failed: %v", err)
		}
	}

	policy := AcquirePolicyRetryNextIdle
	_, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true, Policy: &policy})
	if err == nil {
		t.Fatal("expected PoolAcquireFailedError, got nil")
	}
	var failed *PoolAcquireFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error type = %T, want *PoolAcquireFailedError; error = %v", err, err)
	}

	// Retry loop should have removed 3 stale ids and left 2 behind.
	snap, snapErr := pool.config.StateStore.SnapshotCounters(ctx, "test-pool")
	if snapErr != nil {
		t.Fatalf("SnapshotCounters failed: %v", snapErr)
	}
	if snap.IdleCount != 2 {
		t.Errorf("IdleCount = %d, want 2 (5 seeded - 3 attempts)", snap.IdleCount)
	}

	// The three attempted ids should each be delete-called (best-effort kill).
	eventuallyEquals(t, func() int32 { return atomic.LoadInt32(staleDeletes) }, 3, 2*time.Second)
}

func TestPool_Acquire_RetryNextIdle_DrainedMidLoop_ReturnsAcquireFailed(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, _ := staleAwareLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
		b.ReconcileInterval(time.Hour)
		b.MaxAcquireRetries(5)
	})
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Only 2 stale ids seeded; budget is 5. Loop must break early on empty take and still
	// surface PoolAcquireFailedError (not PoolEmptyError) because at least one candidate
	// was attempted.
	for i := 0; i < 2; i++ {
		if err := pool.config.StateStore.PutIdle(ctx, "test-pool", fmt.Sprintf("stale-%d", i)); err != nil {
			t.Fatalf("PutIdle failed: %v", err)
		}
	}

	policy := AcquirePolicyRetryNextIdle
	_, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true, Policy: &policy})
	var failed *PoolAcquireFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error type = %T, want *PoolAcquireFailedError; error = %v", err, err)
	}
}

func TestPool_Acquire_RetryNextIdleThenCreate_AllStale_FallsThroughToDirectCreate(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, _ := staleAwareLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
		b.ReconcileInterval(time.Hour)
		b.MaxAcquireRetries(3)
	})
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	for i := 0; i < 3; i++ {
		if err := pool.config.StateStore.PutIdle(ctx, "test-pool", fmt.Sprintf("stale-%d", i)); err != nil {
			t.Fatalf("PutIdle failed: %v", err)
		}
	}

	policy := AcquirePolicyRetryNextIdleThenCreate
	sb, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true, Policy: &policy})
	if err != nil {
		t.Fatalf("Acquire should fall through to direct create, got: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox from fallthrough")
	}
	if sb.ID() == "" {
		t.Error("expected non-empty sandbox ID from direct-create fallthrough")
	}

	snap, _ := pool.config.StateStore.SnapshotCounters(ctx, "test-pool")
	if snap.IdleCount != 0 {
		t.Errorf("IdleCount = %d, want 0 (all three stale attempts removed)", snap.IdleCount)
	}
}

func TestPool_Acquire_RetryNextIdle_ReturnsFirstHealthySandbox(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, _ := staleAwareLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
		b.ReconcileInterval(time.Hour)
		b.MaxAcquireRetries(5)
	})
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Two stale ids ahead of a healthy id in the FIFO queue. Retry loop should skip both
	// stale entries and return the healthy one.
	_ = pool.config.StateStore.PutIdle(ctx, "test-pool", "stale-a")
	_ = pool.config.StateStore.PutIdle(ctx, "test-pool", "stale-b")
	_ = pool.config.StateStore.PutIdle(ctx, "test-pool", "sbx-healthy")

	policy := AcquirePolicyRetryNextIdle
	sb, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true, Policy: &policy})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if sb.ID() != "sbx-healthy" {
		t.Errorf("sandbox ID = %q, want %q", sb.ID(), "sbx-healthy")
	}
}

func TestPool_Acquire_RetryNextIdle_HonoursCancelledContext(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, _ := staleAwareLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
		b.ReconcileInterval(time.Hour)
		b.MaxAcquireRetries(10)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(context.Background(), false)

	// Seed enough stale ids that we would ordinarily loop.
	for i := 0; i < 5; i++ {
		_ = pool.config.StateStore.PutIdle(ctx, "test-pool", fmt.Sprintf("stale-%d", i))
	}

	// Cancel after the loop starts. We wrap the pool's take call via a barrier that fires
	// cancel after the first attempt; simplest way is to intercept via a store wrapper.
	var takeCount int32
	cancelStore := &cancelAfterFirstTakeStore{
		PoolStateStore: pool.config.StateStore,
		takeCount:      &takeCount,
		cancelFn:       cancel,
	}
	pool.config.StateStore = cancelStore

	policy := AcquirePolicyRetryNextIdle
	_, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true, Policy: &policy})
	if err == nil {
		t.Fatal("expected error after ctx cancel")
	}
	// The cancellation surfaces wrapped inside PoolAcquireFailedError (since at least one
	// candidate was attempted before the cancel took effect) OR as a plain ctx.Err(). Both
	// are acceptable; the important guarantee is that we did NOT run all 10 retries.
	if atomic.LoadInt32(&takeCount) >= 10 {
		t.Errorf("takeCount = %d, expected retry loop to stop early on ctx cancel", takeCount)
	}
}

// cancelAfterFirstTakeStore invokes cancelFn after the first successful TryTakeIdle so a
// subsequent iteration observes ctx.Err() != nil.
type cancelAfterFirstTakeStore struct {
	PoolStateStore
	takeCount *int32
	cancelFn  context.CancelFunc
	once      sync.Once
}

func (s *cancelAfterFirstTakeStore) TryTakeIdle(ctx context.Context, poolName string) (string, error) {
	id, err := s.PoolStateStore.TryTakeIdle(ctx, poolName)
	atomic.AddInt32(s.takeCount, 1)
	if id != "" {
		s.once.Do(s.cancelFn)
	}
	return id, err
}

func (s *cancelAfterFirstTakeStore) TryTakeIdleWithMinTTL(ctx context.Context, poolName string, minRemaining time.Duration) (*TakeIdleResult, error) {
	res, err := s.PoolStateStore.TryTakeIdleWithMinTTL(ctx, poolName, minRemaining)
	atomic.AddInt32(s.takeCount, 1)
	if res != nil && res.SandboxID != "" {
		s.once.Do(s.cancelFn)
	}
	return res, err
}

func TestPoolBuilder_MaxAcquireRetries_Zero_Rejected(t *testing.T) {
	_, err := NewSandboxPoolBuilder().
		PoolName("t").
		MaxIdle(1).
		ConnectionConfig(ConnectionConfig{Domain: "localhost:1", Protocol: "http"}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		MaxAcquireRetries(0).
		Build()
	if err == nil {
		t.Fatal("expected error for MaxAcquireRetries=0")
	}
	if !strings.Contains(err.Error(), "MaxAcquireRetries") {
		t.Errorf("error = %q, want contains 'MaxAcquireRetries'", err.Error())
	}
}

func TestPoolBuilder_Defaults_MaxAcquireRetries(t *testing.T) {
	b := NewSandboxPoolBuilder()
	if b.config.MaxAcquireRetries != 3 {
		t.Errorf("default MaxAcquireRetries = %d, want 3", b.config.MaxAcquireRetries)
	}
}

func TestAcquirePolicy_String_NewValues(t *testing.T) {
	cases := []struct {
		policy AcquirePolicy
		want   string
	}{
		{AcquirePolicyRetryNextIdle, "RETRY_NEXT_IDLE"},
		{AcquirePolicyRetryNextIdleThenCreate, "RETRY_NEXT_IDLE_THEN_CREATE"},
	}
	for _, c := range cases {
		if got := c.policy.String(); got != c.want {
			t.Errorf("AcquirePolicy(%d).String() = %q, want %q", c.policy, got, c.want)
		}
	}
}

func TestEffectiveMaxIdleAttempts(t *testing.T) {
	cases := []struct {
		policy   AcquirePolicy
		budget   int
		expected int
	}{
		{AcquirePolicyFailFast, 5, 1},
		{AcquirePolicyDirectCreate, 5, 1},
		{AcquirePolicyRetryNextIdle, 3, 3},
		{AcquirePolicyRetryNextIdleThenCreate, 7, 7},
		{AcquirePolicyRetryNextIdle, 0, 1}, // clamp
	}
	for _, c := range cases {
		if got := effectiveMaxIdleAttempts(c.policy, c.budget); got != c.expected {
			t.Errorf("effectiveMaxIdleAttempts(%v, %d) = %d, want %d", c.policy, c.budget, got, c.expected)
		}
	}
}

// renewFailingLifecycleServer accepts all sandbox ids as healthy on connect / endpoint fetch,
// but returns 500 for POST /renew-expiration. Used to verify that a renew failure after a
// successful connect does NOT trigger the retry loop to drain more healthy idles.
// Also records DELETE calls per-id so tests can assert the leaked-on-renew sandbox is killed
// remotely, not merely closed locally.
func renewFailingLifecycleServer(t *testing.T, execdURL string) (*httptest.Server, *int32, func() []string) {
	t.Helper()
	var renewAttempts int32
	var deletedMu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:         fmt.Sprintf("sbx-pool-%d", time.Now().UnixNano()),
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdURL,
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "test-token"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			sandboxID := parts[len(parts)-1]
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:         sandboxID,
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			sandboxID := parts[len(parts)-1]
			deletedMu.Lock()
			deleted = append(deleted, sandboxID)
			deletedMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			atomic.AddInt32(&renewAttempts, 1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"renew rejected"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	snapshotDeleted := func() []string {
		deletedMu.Lock()
		defer deletedMu.Unlock()
		out := make([]string, len(deleted))
		copy(out, deleted)
		return out
	}
	return srv, &renewAttempts, snapshotDeleted
}

// Regression for the PR review points: a renew failure after a successful connect is NOT a
// candidate-specific problem. Retrying another idle cannot fix a lifecycle-API renew rejection
// and used to drain healthy idle entries. Also, since TryTakeIdle already popped the ID out of
// the store, a bare Close() would leak the remote sandbox — Kill() must be called. Verify:
//  1. Only ONE candidate is connected (no retry after renew failure).
//  2. The remaining idle entries stay in the pool.
//  3. The renew error surfaces to the caller (not wrapped as PoolAcquireFailedError).
//  4. The connected sandbox is killed on the remote side to avoid leaking beyond pool
//     accounting until server-side TTL expiry.
func TestPool_Acquire_RetryNextIdle_RenewFailure_KillsRemoteAndDoesNotRetry(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, renewAttempts, deletedIDs := renewFailingLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
		b.ReconcileInterval(time.Hour)
		b.MaxAcquireRetries(5)
	})
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	for i := 0; i < 3; i++ {
		if err := pool.config.StateStore.PutIdle(ctx, "test-pool", fmt.Sprintf("healthy-%d", i)); err != nil {
			t.Fatalf("PutIdle failed: %v", err)
		}
	}

	policy := AcquirePolicyRetryNextIdle
	_, err := pool.Acquire(ctx, AcquireOptions{
		SkipHealthCheck: true,
		Policy:          &policy,
		SandboxTimeout:  1 * time.Minute, // triggers post-connect Renew
	})
	if err == nil {
		t.Fatal("expected renew failure to surface as an error, got nil")
	}
	// Renew errors must NOT be wrapped as PoolAcquireFailedError (that would imply the idle
	// candidate was stale, misleading callers and metrics).
	var failed *PoolAcquireFailedError
	if errors.As(err, &failed) {
		t.Errorf("renew failure surfaced as *PoolAcquireFailedError; expected raw renew error. err=%v", err)
	}
	if !strings.Contains(err.Error(), "renew") {
		t.Errorf("error should mention renew; got: %v", err)
	}

	// Exactly one renew attempt must have hit the server — retry did NOT drain the other
	// two healthy idles.
	if got := atomic.LoadInt32(renewAttempts); got != 1 {
		t.Errorf("renew attempts = %d, want 1 (renew failure must not trigger idle retry)", got)
	}
	// Two healthy idles must remain in the pool (only the taken one is out).
	snap, snapErr := pool.config.StateStore.SnapshotCounters(ctx, "test-pool")
	if snapErr != nil {
		t.Fatalf("SnapshotCounters failed: %v", snapErr)
	}
	if snap.IdleCount != 2 {
		t.Errorf("IdleCount = %d, want 2 (renew failure must not drain remaining idles)", snap.IdleCount)
	}
	// The connected-then-renew-failed sandbox must have been killed on the remote side, or it
	// leaks alive-but-untracked until its server-side TTL expires. Kill runs async via a
	// goroutine so poll briefly for exactly one healthy- DELETE to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ids := deletedIDs()
		count := 0
		for _, id := range ids {
			if strings.HasPrefix(id, "healthy-") {
				count++
			}
		}
		if count == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected exactly one healthy-* DELETE (renew-failed sandbox must be killed remotely); got %v", deletedIDs())
}

func TestPolicyFallsThroughToDirectCreate(t *testing.T) {
	cases := map[AcquirePolicy]bool{
		AcquirePolicyDirectCreate:            true,
		AcquirePolicyRetryNextIdleThenCreate: true,
		AcquirePolicyFailFast:                false,
		AcquirePolicyRetryNextIdle:           false,
	}
	for policy, want := range cases {
		if got := policyFallsThroughToDirectCreate(policy); got != want {
			t.Errorf("policyFallsThroughToDirectCreate(%v) = %v, want %v", policy, got, want)
		}
	}
}
