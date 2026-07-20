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

// mockSandboxCreator implements PooledSandboxCreator for testing.
type mockSandboxCreator struct {
	createFn func(ctx context.Context, createCtx PooledSandboxCreateContext) (*Sandbox, error)
}

func (m *mockSandboxCreator) Create(ctx context.Context, createCtx PooledSandboxCreateContext) (*Sandbox, error) {
	if m.createFn != nil {
		return m.createFn(ctx, createCtx)
	}
	return &Sandbox{id: "mock-sandbox-id"}, nil
}

// newMockExecdServer creates a mock execd server that responds 200 to any request.
func newMockExecdServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMockLifecycleServer creates a mock sandbox lifecycle API server.
// It handles POST /v1/sandboxes, GET /v1/sandboxes/{id}, DELETE /v1/sandboxes/{id},
// POST /v1/sandboxes/{id}/renew-expiration, and GET /v1/sandboxes/{id}/endpoints/{port}.
func newMockLifecycleServer(t *testing.T, execdURL string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			// Create sandbox
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:         fmt.Sprintf("sbx-pool-%d", time.Now().UnixNano()),
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && strings.Contains(path, "/endpoints/"):
			// Get endpoint - return the execd mock URL
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdURL,
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "test-token"},
			})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
			// Get sandbox info
			parts := strings.Split(path, "/")
			sandboxID := parts[len(parts)-1]
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:         sandboxID,
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			// Delete sandbox
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			// Renew expiration
			expiresAt := time.Now().Add(1 * time.Hour).UTC()
			jsonResponse(w, http.StatusOK, RenewExpirationResponse{
				ExpiresAt: expiresAt,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestPool creates a DefaultSandboxPool pointing to the mock server.
func newTestPool(t *testing.T, serverURL string, opts ...func(*SandboxPoolBuilder)) *DefaultSandboxPool {
	t.Helper()

	// Strip scheme from URL to get domain.
	domain := serverURL

	builder := NewSandboxPoolBuilder().
		PoolName("test-pool").
		MaxIdle(2).
		ConnectionConfig(ConnectionConfig{
			Domain:   domain,
			Protocol: "http",
		}).
		CreationSpec(PoolCreationSpec{
			Image: "python:3.12",
		}).
		ReconcileInterval(100 * time.Millisecond)

	for _, opt := range opts {
		opt(builder)
	}

	pool, err := builder.Build()
	if err != nil {
		t.Fatalf("newTestPool: Build failed: %v", err)
	}
	return pool
}

// ---------- Builder Tests ----------

func TestPoolBuilder_Defaults(t *testing.T) {
	b := NewSandboxPoolBuilder()
	if b.config.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want 30s", b.config.ReconcileInterval)
	}
	if b.config.PrimaryLockTTL != 60*time.Second {
		t.Errorf("PrimaryLockTTL = %v, want 60s", b.config.PrimaryLockTTL)
	}
	if b.config.DegradedThreshold != 3 {
		t.Errorf("DegradedThreshold = %d, want 3", b.config.DegradedThreshold)
	}
	if b.config.DrainTimeout != 30*time.Second {
		t.Errorf("DrainTimeout = %v, want 30s", b.config.DrainTimeout)
	}
	if b.config.AcquireReadyTimeout != 30*time.Second {
		t.Errorf("AcquireReadyTimeout = %v, want 30s", b.config.AcquireReadyTimeout)
	}
	if b.config.WarmupReadyTimeout != 30*time.Second {
		t.Errorf("WarmupReadyTimeout = %v, want 30s", b.config.WarmupReadyTimeout)
	}
	if b.config.EmptyBehavior != AcquirePolicyDirectCreate {
		t.Errorf("EmptyBehavior = %v, want DIRECT_CREATE", b.config.EmptyBehavior)
	}
}

func TestPoolBuilder_MissingPoolName(t *testing.T) {
	_, err := NewSandboxPoolBuilder().
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		Build()
	if err == nil {
		t.Fatal("expected error for missing PoolName")
	}
	if !strings.Contains(err.Error(), "PoolName is required") {
		t.Errorf("error = %q, want contains 'PoolName is required'", err.Error())
	}
}

func TestPoolBuilder_MissingConnectionConfig(t *testing.T) {
	_, err := NewSandboxPoolBuilder().
		PoolName("test").
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		Build()
	if err == nil {
		t.Fatal("expected error for missing ConnectionConfig")
	}
	if !strings.Contains(err.Error(), "ConnectionConfig is required") {
		t.Errorf("error = %q, want contains 'ConnectionConfig is required'", err.Error())
	}
}

func TestPoolBuilder_MissingCreationSpec(t *testing.T) {
	_, err := NewSandboxPoolBuilder().
		PoolName("test").
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		Build()
	if err == nil {
		t.Fatal("expected error for missing CreationSpec")
	}
	if !strings.Contains(err.Error(), "CreationSpec") {
		t.Errorf("error = %q, want contains 'CreationSpec'", err.Error())
	}
}

func TestPoolBuilder_CustomCreatorWithoutCreationSpec(t *testing.T) {
	creator := &mockSandboxCreator{}
	pool, err := NewSandboxPoolBuilder().
		PoolName("test-custom").
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		SandboxCreator(creator).
		Build()
	if err != nil {
		t.Fatalf("expected no error with custom creator, got %v", err)
	}
	if pool == nil {
		t.Fatal("expected non-nil pool")
	}
}

func TestPoolBuilder_ValidBuild(t *testing.T) {
	pool, err := NewSandboxPoolBuilder().
		PoolName("valid-pool").
		MaxIdle(5).
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		Build()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if pool == nil {
		t.Fatal("expected non-nil pool")
	}
}

func TestPoolBuilder_AcquireMinRemainingTTL_Negative(t *testing.T) {
	_, err := NewSandboxPoolBuilder().
		PoolName("test").
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		AcquireMinRemainingTTL(-1 * time.Second).
		Build()
	if err == nil {
		t.Fatal("expected error for negative AcquireMinRemainingTTL")
	}
	if !strings.Contains(err.Error(), "AcquireMinRemainingTTL") {
		t.Errorf("error = %q, want contains 'AcquireMinRemainingTTL'", err.Error())
	}
}

func TestPoolBuilder_AcquireMinRemainingTTL_ExceedsIdleTimeout(t *testing.T) {
	_, err := NewSandboxPoolBuilder().
		PoolName("test").
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		IdleTimeout(10 * time.Minute).
		AcquireMinRemainingTTL(10 * time.Minute). // equal to IdleTimeout, should fail
		Build()
	if err == nil {
		t.Fatal("expected error when AcquireMinRemainingTTL >= IdleTimeout")
	}
	if !strings.Contains(err.Error(), "AcquireMinRemainingTTL") {
		t.Errorf("error = %q, want contains 'AcquireMinRemainingTTL'", err.Error())
	}
}

func TestPoolBuilder_DefaultWarmupConcurrency(t *testing.T) {
	// MaxIdle=10 => max(1, ceil(10*0.2)) = 2
	pool, err := NewSandboxPoolBuilder().
		PoolName("warmup-test").
		MaxIdle(10).
		ConnectionConfig(ConnectionConfig{Domain: "localhost:8080", Protocol: "http"}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if pool.config.WarmupConcurrency != 2 {
		t.Errorf("WarmupConcurrency = %d, want 2", pool.config.WarmupConcurrency)
	}
}

func TestPoolBuilder_DefaultPollingIntervals(t *testing.T) {
	b := NewSandboxPoolBuilder()
	if b.config.AcquireHealthCheckPollingInterval != 200*time.Millisecond {
		t.Errorf("AcquireHealthCheckPollingInterval = %v, want 200ms", b.config.AcquireHealthCheckPollingInterval)
	}
	if b.config.WarmupHealthCheckPollingInterval != 200*time.Millisecond {
		t.Errorf("WarmupHealthCheckPollingInterval = %v, want 200ms", b.config.WarmupHealthCheckPollingInterval)
	}
}

// ---------- Lifecycle Tests ----------

func TestPool_Start_Success(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	pool.mu.Lock()
	state := pool.lifecycleState
	pool.mu.Unlock()

	if state != PoolLifecycleRunning {
		t.Errorf("lifecycleState = %v, want RUNNING", state)
	}
}

func TestPool_Start_AlreadyStarted(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Second start should be idempotent (return nil).
	err = pool.Start(ctx)
	if err != nil {
		t.Errorf("second Start should return nil, got %v", err)
	}
}

func TestPool_Shutdown_Graceful(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	err = pool.Shutdown(ctx, true)
	if err != nil {
		t.Fatalf("Shutdown(graceful) failed: %v", err)
	}

	pool.mu.Lock()
	state := pool.lifecycleState
	pool.mu.Unlock()

	if state != PoolLifecycleStopped {
		t.Errorf("lifecycleState = %v, want STOPPED", state)
	}
}

func TestPool_Shutdown_NotStarted(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	// Shutdown without Start should go directly to STOPPED.
	err := pool.Shutdown(ctx, false)
	if err != nil {
		t.Fatalf("Shutdown(not started) failed: %v", err)
	}

	pool.mu.Lock()
	state := pool.lifecycleState
	pool.mu.Unlock()

	if state != PoolLifecycleStopped {
		t.Errorf("lifecycleState = %v, want STOPPED", state)
	}
}

// ---------- Acquire Tests ----------

func TestPool_Acquire_FromIdle(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Pre-populate store with an idle entry.
	sandboxID := "sbx-idle-1"
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", sandboxID); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}

	sb, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox")
	}
	if sb.ID() != sandboxID {
		t.Errorf("sandbox ID = %q, want %q", sb.ID(), sandboxID)
	}
}

func TestPool_Acquire_Empty_DirectCreate(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Pool is empty, default policy is DIRECT_CREATE.
	sb, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err != nil {
		t.Fatalf("Acquire (direct create) failed: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox from direct create")
	}
	if sb.ID() == "" {
		t.Error("expected non-empty sandbox ID")
	}
}

func TestPool_Acquire_Empty_FailFast(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.EmptyBehavior(AcquirePolicyFailFast)
	})
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Pool is empty with FAIL_FAST policy.
	_, err = pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err == nil {
		t.Fatal("expected PoolEmptyError, got nil")
	}
	var poolEmptyErr *PoolEmptyError
	if !isPoolEmptyError(err, &poolEmptyErr) {
		t.Errorf("error type = %T, want *PoolEmptyError; error = %v", err, err)
	}
}

// failingTakeStore wraps InMemoryPoolStateStore but injects errors on TryTakeIdle
// and TryTakeIdleWithMinTTL, simulating a store outage.
type failingTakeStore struct {
	*InMemoryPoolStateStore
	takeErr error
}

func (s *failingTakeStore) TryTakeIdle(_ context.Context, _ string) (string, error) {
	return "", s.takeErr
}

func (s *failingTakeStore) TryTakeIdleWithMinTTL(_ context.Context, _ string, _ time.Duration) (*TakeIdleResult, error) {
	return nil, s.takeErr
}

func TestPool_Acquire_StoreError_DirectCreate_FallsThrough(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	store := &failingTakeStore{
		InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
		takeErr:                fmt.Errorf("connection refused"),
	}

	domain := lifecycleSrv.URL
	pool, err := NewSandboxPoolBuilder().
		PoolName("test-store-fallthrough").
		MaxIdle(2).
		ConnectionConfig(ConnectionConfig{
			Domain:   domain,
			Protocol: "http",
		}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		StateStore(store).
		ReconcileInterval(time.Hour). // don't auto-reconcile
		Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Under DirectCreate (default), a store error should fall through
	// to direct create instead of returning an error.
	sb, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err != nil {
		t.Fatalf("Acquire should fall through to direct create on store error, got: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox from direct create fallback")
	}
	if sb.ID() == "" {
		t.Error("expected non-empty sandbox ID")
	}
}

func TestPool_Acquire_StoreError_FailFast_ReturnsError(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	store := &failingTakeStore{
		InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
		takeErr:                fmt.Errorf("connection refused"),
	}

	domain := lifecycleSrv.URL
	pool, err := NewSandboxPoolBuilder().
		PoolName("test-store-failfast").
		MaxIdle(2).
		ConnectionConfig(ConnectionConfig{
			Domain:   domain,
			Protocol: "http",
		}).
		CreationSpec(PoolCreationSpec{Image: "python:3.12"}).
		StateStore(store).
		EmptyBehavior(AcquirePolicyFailFast).
		ReconcileInterval(time.Hour).
		Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Under FailFast, a store error should be propagated immediately.
	_, err = pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err == nil {
		t.Fatal("expected PoolStateStoreUnavailableError, got nil")
	}
	var storeErr *PoolStateStoreUnavailableError
	if !errors.As(err, &storeErr) {
		t.Errorf("error type = %T, want *PoolStateStoreUnavailableError; error = %v", err, err)
	}
}

func TestPool_Acquire_PoolNotRunning(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	// Do not start the pool - Acquire should fail.
	_, err := pool.Acquire(ctx, AcquireOptions{})
	if err == nil {
		t.Fatal("expected PoolNotRunningError, got nil")
	}
	var notRunning *PoolNotRunningError
	if !isPoolNotRunningError(err, &notRunning) {
		t.Errorf("error type = %T, want *PoolNotRunningError; error = %v", err, err)
	}
}

func TestPool_Acquire_WithTimeout(t *testing.T) {
	var renewCalled int32
	execdSrv := newMockExecdServer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:         "sbx-timeout-test",
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdSrv.URL,
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "tok"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			atomic.AddInt32(&renewCalled, 1)
			jsonResponse(w, http.StatusOK, RenewExpirationResponse{
				ExpiresAt: time.Now().Add(1 * time.Hour).UTC(),
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:     id,
				Status: SandboxStatus{State: StateRunning},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	pool := newTestPool(t, srv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Acquire with SandboxTimeout set - should trigger renew.
	sb, err := pool.Acquire(ctx, AcquireOptions{
		SandboxTimeout:  30 * time.Minute,
		SkipHealthCheck: true,
	})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox")
	}
	if atomic.LoadInt32(&renewCalled) == 0 {
		t.Error("expected renew-expiration to be called when SandboxTimeout is set")
	}
}

// ---------- Other Tests ----------

func TestPool_Resize(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Resize(ctx, 5)
	if err != nil {
		t.Fatalf("Resize failed: %v", err)
	}

	// Verify both store and local config are updated.
	storeMaxIdle, err := pool.config.StateStore.GetMaxIdle(ctx, pool.config.PoolName)
	if err != nil {
		t.Fatalf("GetMaxIdle failed: %v", err)
	}
	if storeMaxIdle != 5 {
		t.Errorf("store MaxIdle = %d, want 5", storeMaxIdle)
	}
	pool.mu.Lock()
	localMaxIdle := pool.config.MaxIdle
	pool.mu.Unlock()
	if localMaxIdle != 5 {
		t.Errorf("config MaxIdle = %d, want 5", localMaxIdle)
	}
}

func TestPool_Resize_InvalidValue(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Resize(ctx, -1)
	if err == nil {
		t.Fatal("expected error for negative resize value")
	}
	if !strings.Contains(err.Error(), "must be >= 0") {
		t.Errorf("error = %q, want contains 'must be >= 0'", err.Error())
	}
}

func TestPool_Snapshot(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	snap, err := pool.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.LifecycleState != PoolLifecycleRunning {
		t.Errorf("LifecycleState = %v, want RUNNING", snap.LifecycleState)
	}
	if snap.MaxIdle != 2 {
		t.Errorf("MaxIdle = %d, want 2", snap.MaxIdle)
	}
}

func TestPool_SnapshotIdleEntries(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	// Pre-populate the store.
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", "sbx-snap-1"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", "sbx-snap-2"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}

	entries, err := pool.SnapshotIdleEntries(ctx)
	if err != nil {
		t.Fatalf("SnapshotIdleEntries failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].SandboxID != "sbx-snap-1" {
		t.Errorf("entries[0].SandboxID = %q, want sbx-snap-1", entries[0].SandboxID)
	}
	if entries[1].SandboxID != "sbx-snap-2" {
		t.Errorf("entries[1].SandboxID = %q, want sbx-snap-2", entries[1].SandboxID)
	}
}

func TestPool_Concurrent_Acquire(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	sandboxes := make([]*Sandbox, goroutines)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			sb, acquireErr := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
			errs[idx] = acquireErr
			sandboxes[idx] = sb
		}(i)
	}
	wg.Wait()

	var successCount int
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Acquire failed: %v", i, errs[i])
		} else if sandboxes[i] == nil {
			t.Errorf("goroutine %d: got nil sandbox", i)
		} else {
			successCount++
		}
	}
	if successCount != goroutines {
		t.Errorf("successCount = %d, want %d", successCount, goroutines)
	}
}

// ---------- Shutdown Behavior Tests ----------

func TestPool_Shutdown_DoesNotReleaseIdle(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
	})
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Pre-populate store with idle entries.
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", "sbx-idle-a"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", "sbx-idle-b"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}

	// Graceful shutdown should NOT release idle sandboxes.
	err = pool.Shutdown(ctx, true)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// Verify idle entries are still in the store.
	counters, err := pool.config.StateStore.SnapshotCounters(ctx, "test-pool")
	if err != nil {
		t.Fatalf("SnapshotCounters failed: %v", err)
	}
	if counters.IdleCount != 2 {
		t.Errorf("idle count after shutdown = %d, want 2 (shutdown should not release idle)", counters.IdleCount)
	}
}

func TestPool_Shutdown_NonGraceful_DoesNotReleaseIdle(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0)
	})
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Pre-populate store with idle entries.
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", "sbx-idle-c"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}

	// Non-graceful shutdown should NOT release idle sandboxes.
	err = pool.Shutdown(ctx, false)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// Verify idle entry is still in the store.
	counters, err := pool.config.StateStore.SnapshotCounters(ctx, "test-pool")
	if err != nil {
		t.Fatalf("SnapshotCounters failed: %v", err)
	}
	if counters.IdleCount != 1 {
		t.Errorf("idle count after non-graceful shutdown = %d, want 1", counters.IdleCount)
	}
}

// ---------- Restart Tests ----------

func TestPool_Start_AfterShutdown(t *testing.T) {
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)

	pool := newTestPool(t, lifecycleSrv.URL)
	ctx := context.Background()

	// First lifecycle: start and shutdown.
	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("first Start failed: %v", err)
	}

	err = pool.Shutdown(ctx, true)
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	pool.mu.Lock()
	state := pool.lifecycleState
	pool.mu.Unlock()
	if state != PoolLifecycleStopped {
		t.Fatalf("expected STOPPED after shutdown, got %v", state)
	}

	// Second lifecycle: restart from STOPPED state.
	err = pool.Start(ctx)
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	pool.mu.Lock()
	state = pool.lifecycleState
	pool.mu.Unlock()
	if state != PoolLifecycleRunning {
		t.Errorf("expected RUNNING after restart, got %v", state)
	}

	// Acquire should work after restart.
	sb, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err != nil {
		t.Fatalf("Acquire after restart failed: %v", err)
	}
	if sb == nil {
		t.Fatal("expected non-nil sandbox after restart")
	}
}

// ---------- Direct Create Close Tests ----------

func TestPool_DirectCreate_RenewFailure_ClosesCalled(t *testing.T) {
	execdSrv := newMockExecdServer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:         "sbx-renew-fail-test",
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdSrv.URL,
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "tok"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			// Renew always fails.
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":"INTERNAL","message":"renew failed"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:     id,
				Status: SandboxStatus{State: StateRunning},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	pool := newTestPool(t, srv.URL)
	ctx := context.Background()

	err := pool.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(ctx, false)

	// Acquire with SandboxTimeout should fail because renew always fails.
	_, err = pool.Acquire(ctx, AcquireOptions{
		SandboxTimeout:  30 * time.Minute,
		SkipHealthCheck: true,
	})
	if err == nil {
		t.Fatal("expected error from Acquire when renew fails")
	}
	if !strings.Contains(err.Error(), "renew failed") {
		t.Errorf("error = %q, want contains 'renew failed'", err.Error())
	}
}

func TestPoolStateStoreUnavailableError(t *testing.T) {
	cause := fmt.Errorf("connection refused")
	err := &PoolStateStoreUnavailableError{Operation: "TryTakeIdle", Cause: cause}

	// Error message includes operation and cause.
	msg := err.Error()
	if !strings.Contains(msg, "unavailable") {
		t.Errorf("error message should contain 'unavailable', got %q", msg)
	}
	if !strings.Contains(msg, "TryTakeIdle") {
		t.Errorf("error message should contain operation name, got %q", msg)
	}

	// Unwrap returns the cause.
	if err.Unwrap() != cause {
		t.Errorf("Unwrap() = %v, want %v", err.Unwrap(), cause)
	}

	// errors.As matches the correct type.
	var target *PoolStateStoreUnavailableError
	if !errors.As(err, &target) {
		t.Fatal("errors.As should match *PoolStateStoreUnavailableError")
	}
	if target.Operation != "TryTakeIdle" {
		t.Errorf("Operation = %q, want %q", target.Operation, "TryTakeIdle")
	}

}

// ---------- Helpers ----------

// isPoolEmptyError checks if err is or wraps a *PoolEmptyError.
func isPoolEmptyError(err error, target **PoolEmptyError) bool {
	if e, ok := err.(*PoolEmptyError); ok {
		*target = e
		return true
	}
	return false
}

// isPoolNotRunningError checks if err is or wraps a *PoolNotRunningError.
func isPoolNotRunningError(err error, target **PoolNotRunningError) bool {
	if e, ok := err.(*PoolNotRunningError); ok {
		*target = e
		return true
	}
	return false
}
