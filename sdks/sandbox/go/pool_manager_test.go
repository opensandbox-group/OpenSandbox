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

// killRecorder is a mock lifecycle server that records DELETE calls and can be
// told to fail them or to stall before answering.
type killRecorder struct {
	srv     *httptest.Server
	deleted atomic.Int32
	fail    atomic.Bool
	delay   atomic.Int64 // nanoseconds
}

func newKillRecorder(t *testing.T) *killRecorder {
	t.Helper()
	rec := &killRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if d := time.Duration(rec.delay.Load()); d > 0 {
			time.Sleep(d)
		}
		rec.deleted.Add(1)
		if rec.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func newTestPoolManager(t *testing.T, store PoolStateStore, serverURL string) *SandboxPoolManager {
	t.Helper()
	manager, err := NewSandboxPoolManagerBuilder().
		StateStore(store).
		ConnectionConfig(ConnectionConfig{Domain: serverURL, Protocol: "http"}).
		OwnerID("test-pool-manager").
		Build()
	if err != nil {
		t.Fatalf("newTestPoolManager: Build failed: %v", err)
	}
	return manager
}

func seedIdle(t *testing.T, store PoolStateStore, poolName string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := store.PutIdle(ctx, poolName, fmt.Sprintf("sbx-idle-%d", i)); err != nil {
			t.Fatalf("seedIdle: PutIdle failed: %v", err)
		}
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// ---------- Destroy Protocol Tests ----------

func TestSandboxPoolManager_Destroy(t *testing.T) {
	tests := []struct {
		name          string
		idleCount     int
		killsFail     bool
		options       PoolDestroyOptions
		wantDrained   int
		wantKilled    int
		wantDeletions int32
	}{
		{
			name:    "empty pool",
			options: PoolDestroyOptions{},
		},
		{
			name:          "drains and kills every idle sandbox",
			idleCount:     3,
			wantDrained:   3,
			wantKilled:    3,
			wantDeletions: 3,
		},
		{
			name:          "kill failures are best-effort",
			idleCount:     2,
			killsFail:     true,
			wantDrained:   2,
			wantKilled:    0,
			wantDeletions: 2,
		},
		{
			name:          "zero drain timeout disables the deadline",
			idleCount:     2,
			options:       PoolDestroyOptions{DrainTimeout: durationPtr(0)},
			wantDrained:   2,
			wantKilled:    2,
			wantDeletions: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := NewInMemoryPoolStateStore()
			rec := newKillRecorder(t)
			rec.fail.Store(tt.killsFail)
			seedIdle(t, store, "test-pool", tt.idleCount)

			manager := newTestPoolManager(t, store, rec.srv.URL)
			result, err := manager.Destroy(ctx, "test-pool", tt.options)
			if err != nil {
				t.Fatalf("Destroy failed: %v", err)
			}

			if result.State != PoolDestroyStateDestroyed {
				t.Errorf("state = %s, want DESTROYED", result.State)
			}
			if result.PoolName != "test-pool" {
				t.Errorf("poolName = %q, want %q", result.PoolName, "test-pool")
			}
			if !result.PersistentStateCleared {
				t.Error("PersistentStateCleared = false, want true")
			}
			if result.DrainedIdleCount != tt.wantDrained {
				t.Errorf("DrainedIdleCount = %d, want %d", result.DrainedIdleCount, tt.wantDrained)
			}
			if result.KilledIdleCount != tt.wantKilled {
				t.Errorf("KilledIdleCount = %d, want %d", result.KilledIdleCount, tt.wantKilled)
			}
			if got := rec.deleted.Load(); got != tt.wantDeletions {
				t.Errorf("DELETE requests = %d, want %d", got, tt.wantDeletions)
			}

			state, err := store.GetDestroyState(ctx, "test-pool")
			if err != nil {
				t.Fatalf("GetDestroyState failed: %v", err)
			}
			if state != PoolDestroyStateDestroyed {
				t.Errorf("store destroy state = %s, want DESTROYED", state)
			}
			counters, err := store.SnapshotCounters(ctx, "test-pool")
			if err != nil {
				t.Fatalf("SnapshotCounters failed: %v", err)
			}
			if counters.IdleCount != 0 {
				t.Errorf("idle count after destroy = %d, want 0", counters.IdleCount)
			}
		})
	}
}

// recordingPoolLogger captures warnings so tests can assert on best-effort paths.
type recordingPoolLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *recordingPoolLogger) Info(_ string, _ ...interface{})  {}
func (l *recordingPoolLogger) Debug(_ string, _ ...interface{}) {}

func (l *recordingPoolLogger) Warn(msg string, _ ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *recordingPoolLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

func TestSandboxPoolManager_Destroy_LogsBestEffortKillFailures(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	rec := newKillRecorder(t)
	rec.fail.Store(true)
	seedIdle(t, store, "test-pool", 2)

	logger := &recordingPoolLogger{}
	manager, err := NewSandboxPoolManagerBuilder().
		StateStore(store).
		ConnectionConfig(ConnectionConfig{Domain: rec.srv.URL, Protocol: "http"}).
		PoolLogger(logger).
		Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	result, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{})
	if err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}
	if result.State != PoolDestroyStateDestroyed {
		t.Errorf("state = %s, want DESTROYED (kill failures must not abort the destroy)", result.State)
	}
	if got := logger.warnCount(); got != 2 {
		t.Errorf("warn count = %d, want 2 (one per failed kill)", got)
	}
}

func TestSandboxPoolManager_Destroy_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	rec := newKillRecorder(t)
	seedIdle(t, store, "test-pool", 2)

	manager := newTestPoolManager(t, store, rec.srv.URL)
	if _, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{}); err != nil {
		t.Fatalf("first Destroy failed: %v", err)
	}

	result, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{})
	if err != nil {
		t.Fatalf("second Destroy failed: %v", err)
	}
	if result.State != PoolDestroyStateDestroyed {
		t.Errorf("state = %s, want DESTROYED", result.State)
	}
	if result.PersistentStateCleared {
		t.Error("PersistentStateCleared = true on a repeat destroy, want false")
	}
	if result.DrainedIdleCount != 0 || result.KilledIdleCount != 0 {
		t.Errorf("repeat destroy drained/killed = %d/%d, want 0/0", result.DrainedIdleCount, result.KilledIdleCount)
	}
	if got := rec.deleted.Load(); got != 2 {
		t.Errorf("DELETE requests = %d, want 2 (the repeat destroy must not kill again)", got)
	}
}

func TestSandboxPoolManager_Destroy_DrainTimeoutLeavesFence(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	rec := newKillRecorder(t)
	rec.delay.Store(int64(30 * time.Millisecond))
	seedIdle(t, store, "test-pool", 5)

	manager := newTestPoolManager(t, store, rec.srv.URL)
	_, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{
		DrainTimeout: durationPtr(10 * time.Millisecond),
	})

	var incomplete *PoolDestroyIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("Destroy error = %v, want *PoolDestroyIncompleteError", err)
	}
	if incomplete.PoolName != "test-pool" {
		t.Errorf("PoolName = %q, want %q", incomplete.PoolName, "test-pool")
	}

	// The namespace stays fenced so a retry can finish the job.
	state, err := store.GetDestroyState(ctx, "test-pool")
	if err != nil {
		t.Fatalf("GetDestroyState failed: %v", err)
	}
	if state != PoolDestroyStateDestroying {
		t.Errorf("state after timeout = %s, want DESTROYING", state)
	}

	rec.delay.Store(0)
	result, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{})
	if err != nil {
		t.Fatalf("retry Destroy failed: %v", err)
	}
	if result.State != PoolDestroyStateDestroyed {
		t.Errorf("state after retry = %s, want DESTROYED", result.State)
	}
}

func TestSandboxPoolManager_Destroy_TombstoneTTLExpires(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	rec := newKillRecorder(t)

	manager := newTestPoolManager(t, store, rec.srv.URL)
	if _, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{
		TombstoneTTL: durationPtr(20 * time.Millisecond),
	}); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	if err := store.PutIdle(ctx, "test-pool", "sbx-blocked"); err == nil {
		t.Fatal("PutIdle succeeded while the tombstone was live, want *PoolDestroyedError")
	}

	time.Sleep(40 * time.Millisecond)

	state, err := store.GetDestroyState(ctx, "test-pool")
	if err != nil {
		t.Fatalf("GetDestroyState failed: %v", err)
	}
	if state != PoolDestroyStateActive {
		t.Errorf("state after tombstone TTL = %s, want ACTIVE", state)
	}
	if err := store.PutIdle(ctx, "test-pool", "sbx-rebound"); err != nil {
		t.Errorf("PutIdle after tombstone expiry failed: %v", err)
	}
}

func TestSandboxPoolManager_Destroy_ZeroTombstoneTTLNeverExpires(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	rec := newKillRecorder(t)

	manager := newTestPoolManager(t, store, rec.srv.URL)
	if _, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{
		TombstoneTTL: durationPtr(0),
	}); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	state, err := store.GetDestroyState(ctx, "test-pool")
	if err != nil {
		t.Fatalf("GetDestroyState failed: %v", err)
	}
	if state != PoolDestroyStateDestroyed {
		t.Errorf("state = %s, want DESTROYED (a zero TTL must never expire)", state)
	}
}

func TestSandboxPoolManager_Destroy_InvalidOptions(t *testing.T) {
	tests := []struct {
		name     string
		poolName string
		options  PoolDestroyOptions
	}{
		{name: "blank pool name", poolName: "   ", options: PoolDestroyOptions{}},
		{name: "unsupported strategy", poolName: "test-pool", options: PoolDestroyOptions{Strategy: PoolDestroyStrategy(99)}},
		{name: "negative drain timeout", poolName: "test-pool", options: PoolDestroyOptions{DrainTimeout: durationPtr(-time.Second)}},
		{name: "negative tombstone TTL", poolName: "test-pool", options: PoolDestroyOptions{TombstoneTTL: durationPtr(-time.Second)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewInMemoryPoolStateStore()
			rec := newKillRecorder(t)
			manager := newTestPoolManager(t, store, rec.srv.URL)

			if _, err := manager.Destroy(context.Background(), tt.poolName, tt.options); err == nil {
				t.Fatal("Destroy succeeded, want validation error")
			}

			// A rejected destroy must not have fenced anything.
			state, err := store.GetDestroyState(context.Background(), "test-pool")
			if err != nil {
				t.Fatalf("GetDestroyState failed: %v", err)
			}
			if state != PoolDestroyStateActive {
				t.Errorf("state = %s, want ACTIVE", state)
			}
		})
	}
}

// ---------- Fence Observation Tests ----------

func TestSandboxPoolManager_Destroy_FenceStopsLivePool(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)
	store := NewInMemoryPoolStateStore()

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(2).ReconcileInterval(10 * time.Millisecond)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = pool.Shutdown(context.Background(), false) })

	waitForIdleCount(t, store, "test-pool", 2)

	manager := newTestPoolManager(t, store, lifecycleSrv.URL)
	if _, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{}); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	// The still-running pool must not replenish the destroyed namespace.
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		counters, err := store.SnapshotCounters(ctx, "test-pool")
		if err != nil {
			t.Fatalf("SnapshotCounters failed: %v", err)
		}
		if counters.IdleCount != 0 {
			t.Fatalf("idle count = %d after destroy, want 0 (fenced pool must not warm up)", counters.IdleCount)
		}
	}

	// The reconcile tick observes the fence and stops the pool outright.
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := pool.Snapshot(ctx)
		if err != nil {
			t.Fatalf("Snapshot failed: %v", err)
		}
		if snapshot.LifecycleState == PoolLifecycleStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool state = %s after destroy, want STOPPED", snapshot.LifecycleState)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A peer starting fresh against the tombstoned namespace must refuse to run.
	peer := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(2)
	})
	err := peer.Start(ctx)
	var destroyed *PoolDestroyedError
	if !errors.As(err, &destroyed) {
		t.Fatalf("peer Start error = %v, want *PoolDestroyedError", err)
	}
}

// countingLifecycleServer is a mock lifecycle API that records how many
// sandboxes were created and killed.
type countingLifecycleServer struct {
	srv     *httptest.Server
	created atomic.Int32
	deleted atomic.Int32
}

func newCountingLifecycleServer(t *testing.T, execdURL string) *countingLifecycleServer {
	t.Helper()
	c := &countingLifecycleServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			c.created.Add(1)
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:         fmt.Sprintf("sbx-created-%d", c.created.Load()),
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: execdURL,
				Headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": "test-token"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/"):
			parts := strings.Split(path, "/")
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:         parts[len(parts)-1],
				Status:     SandboxStatus{State: StateRunning},
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				CreatedAt:  time.Now().UTC(),
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			c.deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			jsonResponse(w, http.StatusOK, RenewExpirationResponse{ExpiresAt: time.Now().Add(time.Hour).UTC()})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// scriptedDestroyStateStore overrides GetDestroyState so a test can drive the
// fence independently of the rest of the store.
type scriptedDestroyStateStore struct {
	*InMemoryPoolStateStore

	// err, when set, is returned from every GetDestroyState call.
	err error
	// activeCalls is how many leading calls report ACTIVE before the namespace
	// starts reporting DESTROYED. Ignored when err is set.
	activeCalls int32

	calls atomic.Int32
}

func (s *scriptedDestroyStateStore) GetDestroyState(_ context.Context, _ string) (PoolDestroyState, error) {
	n := s.calls.Add(1)
	if s.err != nil {
		return PoolDestroyStateActive, s.err
	}
	if n <= s.activeCalls {
		return PoolDestroyStateActive, nil
	}
	return PoolDestroyStateDestroyed, nil
}

// TestSandboxPoolManager_Destroy_BlocksDirectCreateOnLivePool covers the case a
// store-level fence alone cannot: a peer that is still RUNNING when the fence
// lands would otherwise find an empty idle buffer and mint a fresh sandbox into
// the retired namespace via the direct-create fallthrough.
func TestSandboxPoolManager_Destroy_BlocksDirectCreateOnLivePool(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycle := newCountingLifecycleServer(t, execdSrv.URL)
	store := NewInMemoryPoolStateStore()

	// MaxIdle 0 and a long interval keep the pool RUNNING: no reconcile tick
	// fires to observe the fence and stop it.
	pool := newTestPool(t, lifecycle.srv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour).
			EmptyBehavior(AcquirePolicyDirectCreate)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = pool.Shutdown(context.Background(), false) })

	// Sanity check: before the destroy, direct create is the expected behavior.
	sb, err := pool.Acquire(ctx, AcquireOptions{})
	if err != nil {
		t.Fatalf("Acquire before destroy failed: %v", err)
	}
	_ = sb.Close()
	if got := lifecycle.created.Load(); got != 1 {
		t.Fatalf("created = %d before destroy, want 1", got)
	}

	manager := newTestPoolManager(t, store, lifecycle.srv.URL)
	if _, err := manager.Destroy(ctx, "test-pool", PoolDestroyOptions{}); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	snapshot, err := pool.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if snapshot.LifecycleState != PoolLifecycleRunning {
		t.Fatalf("pool state = %s, want RUNNING (the test needs a live peer)", snapshot.LifecycleState)
	}

	_, err = pool.Acquire(ctx, AcquireOptions{})
	var destroyed *PoolDestroyedError
	if !errors.As(err, &destroyed) {
		t.Fatalf("Acquire after destroy = %v, want *PoolDestroyedError", err)
	}
	if got := lifecycle.created.Load(); got != 1 {
		t.Errorf("created = %d after destroy, want 1 (no sandbox may be minted into a retired namespace)", got)
	}
}

// TestPool_Acquire_KillsSandboxFencedMidCreate covers a destroy that lands while
// a direct create is already in flight.
func TestPool_Acquire_KillsSandboxFencedMidCreate(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycle := newCountingLifecycleServer(t, execdSrv.URL)

	// The namespace is checked at Start and again before the acquire; both must
	// see ACTIVE. The third check is the post-create one, and that is the one
	// this test wants fenced.
	store := &scriptedDestroyStateStore{
		InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
		activeCalls:            2,
	}

	pool := newTestPool(t, lifecycle.srv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = pool.Shutdown(context.Background(), false) })

	_, err := pool.Acquire(ctx, AcquireOptions{})
	var destroyed *PoolDestroyedError
	if !errors.As(err, &destroyed) {
		t.Fatalf("Acquire = %v, want *PoolDestroyedError", err)
	}
	if got := lifecycle.created.Load(); got != 1 {
		t.Fatalf("created = %d, want 1", got)
	}

	// The orphaned sandbox is killed asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for lifecycle.deleted.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sandbox created before the fence was never killed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPool_Acquire_KillsIdleSandboxFencedMidAcquire covers the fence landing
// between the preflight check and the idle take. TryTakeIdle is unfenced so the
// destroy manager can drain, which means the ID is already out of the store by
// then and a concurrent Destroy can no longer reach it: the acquire has to kill
// it rather than hand back a sandbox from a retired namespace.
func TestPool_Acquire_KillsIdleSandboxFencedMidAcquire(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycle := newCountingLifecycleServer(t, execdSrv.URL)

	// Start and the acquire preflight both see ACTIVE; the post-connect check
	// is the third call and sees the fence.
	store := &scriptedDestroyStateStore{
		InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
		activeCalls:            2,
	}

	pool := newTestPool(t, lifecycle.srv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = pool.Shutdown(context.Background(), false) })

	if err := store.PutIdle(ctx, "test-pool", "sbx-idle-fenced"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}

	sb, err := pool.Acquire(ctx, AcquireOptions{})
	var destroyed *PoolDestroyedError
	if !errors.As(err, &destroyed) {
		if sb != nil {
			_ = sb.Close()
		}
		t.Fatalf("Acquire = %v, want *PoolDestroyedError", err)
	}
	if got := lifecycle.created.Load(); got != 0 {
		t.Errorf("created = %d, want 0 (the idle candidate must not be replaced)", got)
	}

	// The idle sandbox is no longer tracked anywhere, so the acquire must kill it.
	deadline := time.Now().Add(5 * time.Second)
	for lifecycle.deleted.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle sandbox taken before the fence was never killed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPool_Acquire_IdlePathFenceCheckIsFailClosed pins the deliberate asymmetry
// with the direct-create path: an idle sandbox is already out of the store, so an
// unreachable store cannot be assumed ACTIVE the way direct create may.
func TestPool_Acquire_IdlePathFenceCheckIsFailClosed(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycle := newCountingLifecycleServer(t, execdSrv.URL)

	inner := NewInMemoryPoolStateStore()
	if err := inner.PutIdle(ctx, "test-pool", "sbx-idle-outage"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}

	// Report ACTIVE for Start and the preflight, then fail. DIRECT_CREATE would
	// degrade and keep going; the idle path must not.
	store := &outageAfterNCallsStore{
		InMemoryPoolStateStore: inner,
		okCalls:                2,
	}

	pool := newTestPool(t, lifecycle.srv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour).
			EmptyBehavior(AcquirePolicyDirectCreate)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = pool.Shutdown(context.Background(), false) })

	sb, err := pool.Acquire(ctx, AcquireOptions{})
	if err == nil {
		_ = sb.Close()
		t.Fatal("Acquire succeeded with an unconfirmable namespace, want an error")
	}
	var unavailable *PoolStateStoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("Acquire = %v, want *PoolStateStoreUnavailableError", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for lifecycle.deleted.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle sandbox was never killed after the fail-closed check")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// outageAfterNCallsStore answers GetDestroyState normally for the first okCalls
// calls and then reports the store as unreachable.
type outageAfterNCallsStore struct {
	*InMemoryPoolStateStore

	okCalls int32
	calls   atomic.Int32
}

func (s *outageAfterNCallsStore) GetDestroyState(ctx context.Context, poolName string) (PoolDestroyState, error) {
	if s.calls.Add(1) <= s.okCalls {
		return s.InMemoryPoolStateStore.GetDestroyState(ctx, poolName)
	}
	return PoolDestroyStateActive, &PoolStateStoreUnavailableError{
		Operation: "GetDestroyState",
		Cause:     errors.New("redis is down"),
	}
}

// TestPool_Acquire_NamespaceCheckDegradesOnStoreOutage keeps a store outage from
// making direct-create policies less available than the OSEP-0005 matrix
// documents, while fail-closed policies still surface it.
func TestPool_Acquire_NamespaceCheckDegradesOnStoreOutage(t *testing.T) {
	tests := []struct {
		name        string
		policy      AcquirePolicy
		wantCreated int32
		wantErr     bool
	}{
		{name: "direct create degrades", policy: AcquirePolicyDirectCreate, wantCreated: 1},
		{name: "retry then create degrades", policy: AcquirePolicyRetryNextIdleThenCreate, wantCreated: 1},
		{name: "fail fast surfaces the outage", policy: AcquirePolicyFailFast, wantErr: true},
		{name: "retry next idle surfaces the outage", policy: AcquirePolicyRetryNextIdle, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			execdSrv := newMockExecdServer(t)
			lifecycle := newCountingLifecycleServer(t, execdSrv.URL)
			store := &scriptedDestroyStateStore{
				InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
				err:                    errors.New("redis is down"),
			}

			pool := newTestPool(t, lifecycle.srv.URL, func(b *SandboxPoolBuilder) {
				b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour).
					EmptyBehavior(tt.policy)
			})
			if err := pool.Start(ctx); err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			t.Cleanup(func() { _ = pool.Shutdown(context.Background(), false) })

			sb, err := pool.Acquire(ctx, AcquireOptions{})
			if tt.wantErr {
				var unavailable *PoolStateStoreUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("Acquire = %v, want *PoolStateStoreUnavailableError", err)
				}
			} else {
				if err != nil {
					t.Fatalf("Acquire failed: %v", err)
				}
				_ = sb.Close()
			}
			if got := lifecycle.created.Load(); got != tt.wantCreated {
				t.Errorf("created = %d, want %d", got, tt.wantCreated)
			}
		})
	}
}

func TestPool_Start_RefusesDestroyedNamespace(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)
	store := &scriptedDestroyStateStore{
		InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
	}

	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour)
	})

	err := pool.Start(ctx)
	var destroyed *PoolDestroyedError
	if !errors.As(err, &destroyed) {
		t.Fatalf("Start = %v, want *PoolDestroyedError", err)
	}

	snapshot, err := pool.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if snapshot.LifecycleState != PoolLifecycleNotStarted {
		t.Errorf("state after refused Start = %s, want NOT_STARTED", snapshot.LifecycleState)
	}
}

func TestInMemoryPoolStateStore_FenceRejectsWrites(t *testing.T) {
	ctx := context.Background()

	for _, state := range []PoolDestroyState{PoolDestroyStateDestroying, PoolDestroyStateDestroyed} {
		t.Run(state.String(), func(t *testing.T) {
			store := NewInMemoryPoolStateStore()
			if err := store.BeginDestroy(ctx, "test-pool", "owner-1"); err != nil {
				t.Fatalf("BeginDestroy failed: %v", err)
			}
			if state == PoolDestroyStateDestroyed {
				if err := store.MarkDestroyed(ctx, "test-pool", "owner-1", time.Hour); err != nil {
					t.Fatalf("MarkDestroyed failed: %v", err)
				}
			}

			writes := map[string]func() error{
				"PutIdle":         func() error { return store.PutIdle(ctx, "test-pool", "sbx-1") },
				"SetMaxIdle":      func() error { return store.SetMaxIdle(ctx, "test-pool", 5) },
				"SetIdleEntryTTL": func() error { return store.SetIdleEntryTTL(ctx, "test-pool", time.Minute) },
			}
			for name, write := range writes {
				var destroyed *PoolDestroyedError
				if err := write(); !errors.As(err, &destroyed) {
					t.Errorf("%s error = %v, want *PoolDestroyedError", name, err)
				} else if destroyed.State != state {
					t.Errorf("%s error state = %s, want %s", name, destroyed.State, state)
				}
			}

			acquired, err := store.TryAcquirePrimaryLock(ctx, "test-pool", "owner-2", time.Minute)
			if err != nil {
				t.Fatalf("TryAcquirePrimaryLock failed: %v", err)
			}
			if acquired {
				t.Error("TryAcquirePrimaryLock succeeded on a fenced namespace, want false")
			}

			renewed, err := store.RenewPrimaryLock(ctx, "test-pool", "owner-2", time.Minute)
			if err != nil {
				t.Fatalf("RenewPrimaryLock failed: %v", err)
			}
			if renewed {
				t.Error("RenewPrimaryLock succeeded on a fenced namespace, want false")
			}
		})
	}
}

func TestInMemoryPoolStateStore_BeginDestroyRejectsTombstoned(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()

	if err := store.BeginDestroy(ctx, "test-pool", "owner-1"); err != nil {
		t.Fatalf("BeginDestroy failed: %v", err)
	}
	// Re-entrant while DESTROYING, so a retrying owner can make progress.
	if err := store.BeginDestroy(ctx, "test-pool", "owner-1"); err != nil {
		t.Fatalf("second BeginDestroy on a DESTROYING namespace failed: %v", err)
	}

	if err := store.MarkDestroyed(ctx, "test-pool", "owner-1", time.Hour); err != nil {
		t.Fatalf("MarkDestroyed failed: %v", err)
	}

	var destroyed *PoolDestroyedError
	if err := store.BeginDestroy(ctx, "test-pool", "owner-2"); !errors.As(err, &destroyed) {
		t.Fatalf("BeginDestroy on a tombstoned namespace = %v, want *PoolDestroyedError", err)
	}
}

func TestInMemoryPoolStateStore_ClearPoolStateKeepsFence(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()

	if err := store.SetMaxIdle(ctx, "test-pool", 7); err != nil {
		t.Fatalf("SetMaxIdle failed: %v", err)
	}
	if err := store.PutIdle(ctx, "test-pool", "sbx-1"); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}
	if err := store.BeginDestroy(ctx, "test-pool", "owner-1"); err != nil {
		t.Fatalf("BeginDestroy failed: %v", err)
	}
	if err := store.ClearPoolState(ctx, "test-pool"); err != nil {
		t.Fatalf("ClearPoolState failed: %v", err)
	}

	counters, err := store.SnapshotCounters(ctx, "test-pool")
	if err != nil {
		t.Fatalf("SnapshotCounters failed: %v", err)
	}
	if counters.IdleCount != 0 {
		t.Errorf("idle count = %d, want 0", counters.IdleCount)
	}
	maxIdle, err := store.GetMaxIdle(ctx, "test-pool")
	if err != nil {
		t.Fatalf("GetMaxIdle failed: %v", err)
	}
	if maxIdle != 0 {
		t.Errorf("maxIdle = %d, want 0", maxIdle)
	}
	state, err := store.GetDestroyState(ctx, "test-pool")
	if err != nil {
		t.Fatalf("GetDestroyState failed: %v", err)
	}
	if state != PoolDestroyStateDestroying {
		t.Errorf("state = %s, want DESTROYING (ClearPoolState must not lift the fence)", state)
	}
}

func TestInMemoryPoolStateStore_MarkDestroyedRejectsBlankOwnerAndNegativeTTL(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()

	if err := store.MarkDestroyed(ctx, "test-pool", "", time.Hour); err == nil {
		t.Error("MarkDestroyed with a blank owner succeeded, want error")
	}
	if err := store.MarkDestroyed(ctx, "test-pool", "owner-1", -time.Second); err == nil {
		t.Error("MarkDestroyed with a negative TTL succeeded, want error")
	}
	if err := store.BeginDestroy(ctx, "test-pool", ""); err == nil {
		t.Error("BeginDestroy with a blank owner succeeded, want error")
	}
}

// ---------- Builder Tests ----------

func TestSandboxPoolManagerBuilder_Validation(t *testing.T) {
	tests := []struct {
		name    string
		build   func() (*SandboxPoolManager, error)
		wantErr bool
	}{
		{
			name: "missing state store",
			build: func() (*SandboxPoolManager, error) {
				return NewSandboxPoolManagerBuilder().
					ConnectionConfig(ConnectionConfig{Domain: "localhost:8080"}).
					Build()
			},
			wantErr: true,
		},
		{
			name: "missing connection config",
			build: func() (*SandboxPoolManager, error) {
				return NewSandboxPoolManagerBuilder().
					StateStore(NewInMemoryPoolStateStore()).
					Build()
			},
			wantErr: true,
		},
		{
			name: "blank owner ID",
			build: func() (*SandboxPoolManager, error) {
				return NewSandboxPoolManagerBuilder().
					StateStore(NewInMemoryPoolStateStore()).
					ConnectionConfig(ConnectionConfig{Domain: "localhost:8080"}).
					OwnerID("   ").
					Build()
			},
			wantErr: true,
		},
		{
			name: "defaults the owner ID",
			build: func() (*SandboxPoolManager, error) {
				return NewSandboxPoolManagerBuilder().
					StateStore(NewInMemoryPoolStateStore()).
					ConnectionConfig(ConnectionConfig{Domain: "localhost:8080"}).
					Build()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, err := tt.build()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Build succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Build failed: %v", err)
			}
			if manager.ownerID == "" {
				t.Error("ownerID is empty, want a generated value")
			}
		})
	}
}

// waitForIdleCount blocks until the store reports want idle entries.
func waitForIdleCount(t *testing.T, store PoolStateStore, poolName string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		counters, err := store.SnapshotCounters(context.Background(), poolName)
		if err != nil {
			t.Fatalf("SnapshotCounters failed: %v", err)
		}
		if counters.IdleCount >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d idle entries in pool %q", want, poolName)
}
