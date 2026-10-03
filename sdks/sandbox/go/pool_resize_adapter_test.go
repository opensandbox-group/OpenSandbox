// Copyright 2026 The OpenSandbox Authors
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

// ---------- test doubles ----------

// resizeAdapterTestPool is a PoolResizeTarget that records Resize calls and can
// fail a fixed number of Snapshot or Resize calls. A failure count of 0 means
// the corresponding error never fires; -1 means it always fires.
type resizeAdapterTestPool struct {
	mu          sync.Mutex
	snapshot    PoolSnapshot
	resizeCalls []int

	snapshotErr      error
	snapshotFailures int
	resizeErr        error
	resizeFailures   int
}

func (p *resizeAdapterTestPool) Snapshot(ctx context.Context) (*PoolSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snapshotErr != nil && p.snapshotFailures != 0 {
		if p.snapshotFailures > 0 {
			p.snapshotFailures--
		}
		return nil, p.snapshotErr
	}
	snapshot := p.snapshot
	return &snapshot, nil
}

func (p *resizeAdapterTestPool) Resize(ctx context.Context, maxIdle int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resizeErr != nil && p.resizeFailures != 0 {
		if p.resizeFailures > 0 {
			p.resizeFailures--
		}
		return p.resizeErr
	}
	p.resizeCalls = append(p.resizeCalls, maxIdle)
	p.snapshot.MaxIdle = maxIdle
	return nil
}

func (p *resizeAdapterTestPool) resizeCallValues() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.resizeCalls...)
}

// blockingResizePool blocks inside Resize so a test can observe that a
// concurrent Apply does not overlap an in-flight Run write.
type blockingResizePool struct {
	*resizeAdapterTestPool
	entries   chan struct{}
	release   chan struct{}
	active    int32
	maxActive int32
}

func (p *blockingResizePool) Resize(ctx context.Context, maxIdle int) error {
	active := atomic.AddInt32(&p.active, 1)
	defer atomic.AddInt32(&p.active, -1)
	for {
		observed := atomic.LoadInt32(&p.maxActive)
		if active <= observed || atomic.CompareAndSwapInt32(&p.maxActive, observed, active) {
			break
		}
	}
	p.entries <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.resizeAdapterTestPool.Resize(ctx, maxIdle)
}

// staticResizePolicy always returns the same target.
type staticResizePolicy struct {
	target   int
	interval time.Duration
	err      error

	mu    sync.Mutex
	calls int
}

func (p *staticResizePolicy) NextMaxIdle(_ context.Context, _ PoolSnapshot) (int, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.err != nil {
		return 0, p.err
	}
	return p.target, nil
}

func (p *staticResizePolicy) Interval() time.Duration { return p.interval }

func (p *staticResizePolicy) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// cancelingResizePolicy cancels a context on a chosen call, so a Run loop can be
// stopped deterministically instead of by sleeping.
type cancelingResizePolicy struct {
	staticResizePolicy
	cancel   context.CancelFunc
	cancelAt int
}

func (p *cancelingResizePolicy) NextMaxIdle(ctx context.Context, snapshot PoolSnapshot) (int, error) {
	target, err := p.staticResizePolicy.NextMaxIdle(ctx, snapshot)
	if p.callCount() == p.cancelAt {
		p.cancel()
	}
	return target, err
}

// ---------- adapter construction ----------

func TestNewPoolResizeAdapter_RejectsInvalidArguments(t *testing.T) {
	pool := &resizeAdapterTestPool{}
	tests := []struct {
		name   string
		pool   PoolResizeTarget
		policy PoolResizePolicy
	}{
		{name: "nil pool", policy: &staticResizePolicy{target: 1, interval: time.Minute}},
		{name: "nil policy", pool: pool},
		{name: "zero interval", pool: pool, policy: &staticResizePolicy{target: 1}},
		{name: "negative interval", pool: pool, policy: &staticResizePolicy{target: 1, interval: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, err := NewPoolResizeAdapter(tt.pool, tt.policy)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if adapter != nil {
				t.Error("expected a nil adapter alongside the error")
			}
		})
	}
}

func TestNewPoolResizeAdapter_AcceptsValidArguments(t *testing.T) {
	adapter, err := NewPoolResizeAdapter(
		&resizeAdapterTestPool{},
		&staticResizePolicy{target: 1, interval: time.Minute},
	)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if adapter.Stopped() {
		t.Error("a fresh adapter must not report stopped")
	}
	if adapter.LastError() != nil {
		t.Errorf("LastError = %v, want nil", adapter.LastError())
	}
}

// ---------- Apply ----------

func TestPoolResizeAdapter_ApplyWritesPolicyTarget(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 1}}
	policy := &staticResizePolicy{target: 7, interval: time.Minute}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if got := pool.resizeCallValues(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("resize calls = %v, want [7]", got)
	}
	if adapter.Stopped() {
		t.Error("a successful Apply must not stop the adapter")
	}
}

// A target that already matches the snapshot is still written, because the
// write is the only way to observe a destroy that raced with the evaluation.
func TestPoolResizeAdapter_ApplyWritesEvenWhenTargetIsUnchanged(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 4}}
	policy := &staticResizePolicy{target: 4, interval: time.Minute}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if got := pool.resizeCallValues(); len(got) != 1 || got[0] != 4 {
		t.Fatalf("resize calls = %v, want one unconditional write of 4", got)
	}
}

func TestPoolResizeAdapter_ApplyRejectsNegativeTarget(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning}}
	policy := &staticResizePolicy{target: -1, interval: time.Minute}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	err = adapter.Apply(context.Background())
	if err == nil {
		t.Fatal("expected an error for a negative target")
	}
	if !strings.Contains(err.Error(), "negative maxIdle") {
		t.Errorf("error = %v, want it to mention a negative maxIdle", err)
	}
	if calls := pool.resizeCallValues(); len(calls) != 0 {
		t.Errorf("resize calls = %v, want none", calls)
	}
	if !errors.Is(adapter.LastError(), err) {
		t.Errorf("LastError = %v, want the Apply error", adapter.LastError())
	}
}

func TestPoolResizeAdapter_ApplyPropagatesPolicyError(t *testing.T) {
	policyErr := errors.New("policy exploded")
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning}}
	policy := &staticResizePolicy{interval: time.Minute, err: policyErr}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	err = adapter.Apply(context.Background())
	if !errors.Is(err, policyErr) {
		t.Fatalf("Apply error = %v, want policy error", err)
	}
	if !errors.Is(adapter.LastError(), policyErr) {
		t.Fatalf("LastError = %v, want policy error", adapter.LastError())
	}
	if calls := pool.resizeCallValues(); len(calls) != 0 {
		t.Errorf("resize calls = %v, want none", calls)
	}
}

func TestPoolResizeAdapter_ApplyRejectsNilSnapshot(t *testing.T) {
	// A pool that reports a nil snapshot with no error is a contract violation.
	// Guard against a nil dereference rather than trusting the caller.
	adapter, err := NewPoolResizeAdapter(&nilSnapshotPool{}, &staticResizePolicy{target: 1, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err == nil {
		t.Fatal("expected an error for a nil snapshot")
	} else if !strings.Contains(err.Error(), "nil snapshot") {
		t.Errorf("error = %v, want it to mention a nil snapshot", err)
	}
}

type nilSnapshotPool struct{}

func (p *nilSnapshotPool) Snapshot(context.Context) (*PoolSnapshot, error) { return nil, nil }
func (p *nilSnapshotPool) Resize(context.Context, int) error               { return nil }

// The not-running error satisfies both the adapter sentinel and the pool's own
// typed error, so existing errors.As handling keeps working.
func TestPoolResizeAdapter_NotRunningErrorIsAlsoATypedPoolError(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleStarting, MaxIdle: 1}}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	err = adapter.Apply(context.Background())
	if !errors.Is(err, ErrPoolResizePoolNotRunning) {
		t.Fatalf("Apply error = %v, want ErrPoolResizePoolNotRunning", err)
	}
	var notRunning *PoolNotRunningError
	if !errors.As(err, &notRunning) {
		t.Fatalf("Apply error = %v, want it to unwrap to *PoolNotRunningError", err)
	}
	if notRunning.State != PoolLifecycleStarting {
		t.Errorf("State = %s, want STARTING", notRunning.State)
	}
}

// A destroy stops the adapter without failing Apply, but the cause stays
// observable through LastError so it is not confused with a local drain.
func TestPoolResizeAdapter_RecordsTerminalStopCause(t *testing.T) {
	pool := &resizeAdapterTestPool{
		snapshot:       PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 1},
		resizeErr:      &PoolDestroyedError{PoolName: "test-pool", State: PoolDestroyStateDestroying},
		resizeFailures: -1,
	}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply = %v, want nil after a terminal stop", err)
	}
	var destroyed *PoolDestroyedError
	if !errors.As(adapter.LastError(), &destroyed) {
		t.Fatalf("LastError = %v, want the PoolDestroyedError that ended the adapter", adapter.LastError())
	}
}

func TestPoolResizeAdapter_RecordsTerminalStopCauseForDrainingPool(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleDraining, MaxIdle: 1}}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply = %v, want nil after a terminal stop", err)
	}
	if !adapter.Stopped() {
		t.Fatal("adapter did not stop for a draining pool")
	}
	if err := adapter.LastError(); err == nil || !strings.Contains(err.Error(), "DRAINING") {
		t.Errorf("LastError = %v, want it to name the DRAINING state", err)
	}
}

// A store outage raised by Resize (not Snapshot) is retryable, so Run keeps
// going instead of returning it.
func TestPoolResizeAdapter_RunRetriesResizeStoreOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unavailable := &PoolStateStoreUnavailableError{Operation: "SetMaxIdle", Cause: errors.New("redis blip")}
	pool := &resizeAdapterTestPool{
		snapshot:       PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 0},
		resizeErr:      unavailable,
		resizeFailures: 1,
	}
	policy := &cancelingResizePolicy{
		staticResizePolicy: staticResizePolicy{target: 1, interval: 10 * time.Millisecond},
		cancel:             cancel,
		cancelAt:           2,
	}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled after retrying the resize outage", err)
	}
	if got := pool.resizeCallValues(); len(got) != 1 {
		t.Errorf("successful resize calls = %v, want exactly one after the retry", got)
	}
	// The successful retry clears the outage.
	if err := adapter.LastError(); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("LastError = %v, want the outage cleared or the cancellation", err)
	}
}

// A policy that returns a non-positive interval must not turn Run into a spin
// loop; the adapter falls back to its default period instead of re-evaluating
// with no delay.
func TestPoolResizeAdapter_RunDoesNotSpinOnNonPositiveInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 0}}
	policy := &staticResizePolicy{target: 1, interval: time.Hour}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	// Bypass the constructor's validation to model a policy that starts returning
	// a zero interval after construction.
	adapter.policy = &flakyIntervalPolicy{PoolResizePolicy: policy, interval: 0}

	done := make(chan error, 1)
	go func() { done <- adapter.Run(ctx) }()
	// One evaluation happens immediately, then the adapter must wait out the
	// default period. A spin loop would produce thousands inside this window.
	const probe = 250 * time.Millisecond
	if defaultPoolResizePolicyInterval <= probe {
		t.Skipf("default interval %s is too short to distinguish a spin loop", defaultPoolResizePolicyInterval)
	}
	time.Sleep(probe)
	if got := policy.callCount(); got > 2 {
		t.Fatalf("policy calls after %s = %d, want at most 2 (no spin loop)", probe, got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

type flakyIntervalPolicy struct {
	PoolResizePolicy
	interval time.Duration
}

func (p *flakyIntervalPolicy) Interval() time.Duration { return p.interval }

func TestPoolResizeAdapter_StopsWhenPoolIsNotRunning(t *testing.T) {
	tests := []struct {
		state   PoolLifecycleState
		wantErr bool
	}{
		{state: PoolLifecycleNotStarted, wantErr: true},
		{state: PoolLifecycleStarting, wantErr: true},
		{state: PoolLifecycleDraining},
		{state: PoolLifecycleStopped},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: tt.state, MaxIdle: 1}}
			policy := &staticResizePolicy{target: 4, interval: time.Minute}
			adapter, err := NewPoolResizeAdapter(pool, policy)
			if err != nil {
				t.Fatalf("NewPoolResizeAdapter failed: %v", err)
			}
			err = adapter.Apply(context.Background())
			if tt.wantErr {
				if !errors.Is(err, ErrPoolResizePoolNotRunning) {
					t.Fatalf("Apply for %s pool error = %v, want ErrPoolResizePoolNotRunning", tt.state, err)
				}
				if adapter.Stopped() {
					t.Error("adapter stopped on a transient not-running state")
				}
			} else if err != nil {
				t.Fatalf("Apply for %s pool failed: %v", tt.state, err)
			} else if !adapter.Stopped() {
				t.Error("adapter did not stop for a terminal pool state")
			}
			if policy.callCount() != 0 {
				t.Errorf("policy calls = %d, want 0", policy.callCount())
			}
			if calls := pool.resizeCallValues(); len(calls) != 0 {
				t.Errorf("resize calls = %v, want none", calls)
			}
		})
	}
}

// A NOT_STARTED pool is a caller error, not a terminal state, so the same
// adapter must work once the pool is started.
func TestPoolResizeAdapter_RecoversAfterPoolStarts(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleNotStarted, MaxIdle: 1}}
	policy := &staticResizePolicy{target: 3, interval: time.Minute}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); !errors.Is(err, ErrPoolResizePoolNotRunning) {
		t.Fatalf("Apply before start = %v, want ErrPoolResizePoolNotRunning", err)
	}
	pool.mu.Lock()
	pool.snapshot.LifecycleState = PoolLifecycleRunning
	pool.mu.Unlock()
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply after start failed: %v", err)
	}
	if got := pool.resizeCallValues(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("resize calls = %v, want [3]", got)
	}
}

func TestPoolResizeAdapter_StopsOnDestroyedError(t *testing.T) {
	destroyed := &PoolDestroyedError{PoolName: "test-pool", State: PoolDestroyStateDestroyed}
	pool := &resizeAdapterTestPool{
		snapshot:       PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 1},
		resizeErr:      destroyed,
		resizeFailures: -1,
	}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply against a destroyed namespace returned %v, want nil", err)
	}
	if !adapter.Stopped() {
		t.Error("adapter did not stop after observing PoolDestroyedError")
	}
	// A stopped adapter must never write again.
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply after stop returned %v, want nil", err)
	}
	if got := pool.resizeCallValues(); len(got) != 0 {
		t.Errorf("resize calls = %v, want none after the destroy fence", got)
	}
}

func TestPoolResizeAdapter_StopsOnDestroyedSnapshotError(t *testing.T) {
	pool := &resizeAdapterTestPool{
		snapshotErr:      &PoolDestroyedError{PoolName: "test-pool", State: PoolDestroyStateDestroying},
		snapshotFailures: 1,
	}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply returned %v, want nil after a terminal stop", err)
	}
	if !adapter.Stopped() {
		t.Error("adapter did not stop after a destroyed snapshot error")
	}
}

func TestPoolResizeAdapter_PropagatesStoreOutageFromApply(t *testing.T) {
	unavailable := &PoolStateStoreUnavailableError{Operation: "SnapshotCounters", Cause: errors.New("redis blip")}
	pool := &resizeAdapterTestPool{
		snapshot:         PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 1},
		snapshotErr:      unavailable,
		snapshotFailures: -1,
	}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); !errors.Is(err, unavailable) {
		t.Fatalf("Apply error = %v, want the store outage", err)
	}
	if adapter.Stopped() {
		t.Error("a store outage must not stop the adapter")
	}
	if !errors.Is(adapter.LastError(), unavailable) {
		t.Errorf("LastError = %v, want the store outage", adapter.LastError())
	}
}

func TestPoolResizeAdapter_ApplyHonoursCancelledContext(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning}}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.Apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
	if got := pool.resizeCallValues(); len(got) != 0 {
		t.Errorf("resize calls = %v, want none", got)
	}
}

// The state store's destroy fence is the final authority: even a RUNNING pool
// rejects the write once a destroy has begun.
func TestPoolResizeAdapter_RealStateStoreFenceStopsStableTarget(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycleSrv := newMockLifecycleServer(t, execdSrv.URL)
	store := NewInMemoryPoolStateStore()
	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.StateStore(store).MaxIdle(0).ReconcileInterval(time.Hour)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(context.Background(), false)
	if err := pool.Resize(ctx, 2); err != nil {
		t.Fatalf("initial Resize failed: %v", err)
	}
	if err := store.BeginDestroy(ctx, "test-pool", "test-owner"); err != nil {
		t.Fatalf("BeginDestroy failed: %v", err)
	}

	policy := &staticResizePolicy{target: 2, interval: time.Minute}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(ctx); err != nil {
		t.Fatalf("Apply against fenced pool returned %v, want nil after terminal stop", err)
	}
	if !adapter.Stopped() {
		t.Error("adapter did not stop after the real store rejected SetMaxIdle")
	}
	if got, err := store.GetMaxIdle(ctx, "test-pool"); err != nil || got != 2 {
		t.Fatalf("maxIdle after fenced write = (%d, %v), want (2, nil)", got, err)
	}
}

// The adapter's target and the reconciler's job stay separate: a resize changes
// the idle target, and a borrowed sandbox is never returned to the idle buffer
// or killed as part of a scale-up.
func TestPoolResizeAdapter_ResizeDoesNotReclaimBorrowedSandboxes(t *testing.T) {
	ctx := context.Background()
	execdSrv := newMockExecdServer(t)
	lifecycleSrv, deletedIDs := newResizeTrackingLifecycleServer(t, execdSrv.URL)
	// MaxIdle(0) keeps Start from firing an immediate warmup tick, so the only
	// idle entry is the one this test plants and Acquire cannot race the
	// reconciler for the head of the queue.
	pool := newTestPool(t, lifecycleSrv.URL, func(b *SandboxPoolBuilder) {
		b.MaxIdle(0).ReconcileInterval(time.Hour)
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer pool.Shutdown(context.Background(), false)

	// Borrow a sandbox out of the idle buffer.
	const sandboxID = "sbx-borrowed-1"
	if err := pool.config.StateStore.PutIdle(ctx, "test-pool", sandboxID); err != nil {
		t.Fatalf("PutIdle failed: %v", err)
	}
	borrowed, err := pool.Acquire(ctx, AcquireOptions{SkipHealthCheck: true})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if borrowed.ID() != sandboxID {
		t.Fatalf("acquired sandbox = %q, want %q", borrowed.ID(), sandboxID)
	}

	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 3, interval: time.Minute})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(ctx); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if got, err := pool.config.StateStore.GetMaxIdle(ctx, "test-pool"); err != nil || got != 3 {
		t.Fatalf("maxIdle after Apply = (%d, %v), want (3, nil)", got, err)
	}
	assertSandboxNotIdle(t, pool, sandboxID)
	if deletedIDs()[sandboxID] {
		t.Error("scaling up killed a borrowed sandbox")
	}
}

// newResizeTrackingLifecycleServer is newMockLifecycleServer plus a record of
// every sandbox the pool asked the server to delete.
func newResizeTrackingLifecycleServer(t *testing.T, execdURL string) (*httptest.Server, func() map[string]bool) {
	t.Helper()
	var mu sync.Mutex
	deleted := map[string]bool{}
	record := func(id string) {
		mu.Lock()
		deleted[id] = true
		mu.Unlock()
	}
	deletedIDs := func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(deleted))
		for id, seen := range deleted {
			out[id] = seen
		}
		return out
	}

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
			record(parts[len(parts)-1])
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/renew-expiration"):
			jsonResponse(w, http.StatusOK, RenewExpirationResponse{
				ExpiresAt: time.Now().Add(time.Hour).UTC(),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, deletedIDs
}

func assertSandboxNotIdle(t *testing.T, pool *DefaultSandboxPool, sandboxID string) {
	t.Helper()
	entries, err := pool.config.StateStore.SnapshotIdleEntries(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("SnapshotIdleEntries failed: %v", err)
	}
	for _, entry := range entries {
		if entry.SandboxID == sandboxID {
			t.Fatalf("borrowed sandbox %q reappeared in the idle buffer", sandboxID)
		}
	}
}

// ---------- Run ----------

func TestPoolResizeAdapter_RunStopsWhenDestroyed(t *testing.T) {
	pool := &resizeAdapterTestPool{
		snapshot:       PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 1},
		resizeErr:      &PoolDestroyedError{PoolName: "test-pool", State: PoolDestroyStateDestroying},
		resizeFailures: -1,
	}
	policy := &staticResizePolicy{target: 4, interval: time.Hour}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Run(context.Background()); err != nil {
		t.Fatalf("Run after destroy returned %v, want nil", err)
	}
}

func TestPoolResizeAdapter_RunEvaluatesOnInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 0}}
	policy := &cancelingResizePolicy{
		staticResizePolicy: staticResizePolicy{target: 1, interval: time.Millisecond},
		cancel:             cancel,
		cancelAt:           2,
	}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}

	if err := adapter.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if got := policy.callCount(); got < 2 {
		t.Errorf("policy calls = %d, want at least 2", got)
	}
}

func TestPoolResizeAdapter_RunRetriesTransientStoreOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &resizeAdapterTestPool{
		snapshot:         PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 0},
		snapshotErr:      &PoolStateStoreUnavailableError{Operation: "SnapshotCounters", Cause: errors.New("redis blip")},
		snapshotFailures: 1,
	}
	policy := &cancelingResizePolicy{
		staticResizePolicy: staticResizePolicy{target: 1, interval: 10 * time.Millisecond},
		cancel:             cancel,
		cancelAt:           1,
	}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}

	if err := adapter.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled after retrying the store outage", err)
	}
	if got := policy.callCount(); got < 1 {
		t.Errorf("policy calls = %d, want at least 1 after the store recovered", got)
	}
}

func TestPoolResizeAdapter_RunReturnsNotRunningBeforeStart(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleStarting, MaxIdle: 1}}
	adapter, err := NewPoolResizeAdapter(pool, &staticResizePolicy{target: 2, interval: time.Hour})
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Run(context.Background()); !errors.Is(err, ErrPoolResizePoolNotRunning) {
		t.Fatalf("Run error = %v, want ErrPoolResizePoolNotRunning", err)
	}
	if adapter.Stopped() {
		t.Error("a starting pool must not stop the adapter")
	}
}

func TestPoolResizeAdapter_SerializesConcurrentApplyAndRun(t *testing.T) {
	pool := &blockingResizePool{
		resizeAdapterTestPool: &resizeAdapterTestPool{
			snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 0},
		},
		entries: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	policy := &staticResizePolicy{target: 1, interval: time.Hour}
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- adapter.Run(ctx) }()

	select {
	case <-pool.entries:
	case <-time.After(time.Second):
		t.Fatal("Run did not reach the blocking Resize call")
	}

	applyDone := make(chan error, 1)
	go func() { applyDone <- adapter.Apply(context.Background()) }()
	select {
	case <-pool.entries:
		t.Fatal("concurrent Apply entered Resize while Run was in progress")
	case <-time.After(50 * time.Millisecond):
	}

	stoppedDone := make(chan bool, 1)
	go func() { stoppedDone <- adapter.Stopped() }()
	select {
	case <-stoppedDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Stopped blocked behind an in-flight Resize")
	}

	close(pool.release)
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) && err != nil {
		t.Fatalf("Run error = %v, want context.Canceled or nil", err)
	}
	if err := <-applyDone; !errors.Is(err, context.Canceled) && err != nil {
		t.Fatalf("Apply error = %v, want context.Canceled or nil", err)
	}
	if got := atomic.LoadInt32(&pool.maxActive); got != 1 {
		t.Errorf("max concurrent Resize calls = %d, want 1", got)
	}
}

// ---------- time window policy ----------

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("timezone %s is unavailable: %v", name, err)
	}
	return location
}

func newFixedClockPolicy(t *testing.T, windows []PoolResizeWindow, location *time.Location, defaultMaxIdle int, at time.Time) *TimeWindowPoolResizePolicy {
	t.Helper()
	policy, err := NewTimeWindowPoolResizePolicy(windows, location, defaultMaxIdle, time.Minute)
	if err != nil {
		t.Fatalf("NewTimeWindowPoolResizePolicy failed: %v", err)
	}
	policy.now = func() time.Time { return at }
	return policy
}

func TestTimeWindowPoolResizePolicy_HonoursWindowBoundaries(t *testing.T) {
	location := time.UTC
	windows := []PoolResizeWindow{
		{Name: "business", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9},
	}
	tests := []struct {
		name string
		at   time.Time
		want int
	}{
		{name: "one minute before start", at: time.Date(2024, time.January, 2, 8, 59, 0, 0, location), want: 1},
		{name: "at start is inclusive", at: time.Date(2024, time.January, 2, 9, 0, 0, 0, location), want: 9},
		{name: "inside", at: time.Date(2024, time.January, 2, 12, 0, 0, 0, location), want: 9},
		{name: "at end is exclusive", at: time.Date(2024, time.January, 2, 17, 0, 0, 0, location), want: 1},
		{name: "after end", at: time.Date(2024, time.January, 2, 23, 30, 0, 0, location), want: 1},
		{name: "midnight", at: time.Date(2024, time.January, 2, 0, 0, 0, 0, location), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newFixedClockPolicy(t, windows, location, 1, tt.at)
			got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{})
			if err != nil {
				t.Fatalf("NextMaxIdle failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("NextMaxIdle = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTimeWindowPoolResizePolicy_WrapsOvernightWindow(t *testing.T) {
	location := time.UTC
	windows := []PoolResizeWindow{
		{Name: "overnight", Start: 22 * time.Hour, End: 6 * time.Hour, MaxIdle: 4},
	}
	tests := []struct {
		name string
		at   time.Time
		want int
	}{
		{name: "late evening", at: time.Date(2024, time.January, 2, 23, 0, 0, 0, location), want: 4},
		{name: "just before midnight", at: time.Date(2024, time.January, 2, 23, 59, 0, 0, location), want: 4},
		{name: "just after midnight", at: time.Date(2024, time.January, 3, 0, 1, 0, 0, location), want: 4},
		{name: "at end is exclusive", at: time.Date(2024, time.January, 3, 6, 0, 0, 0, location), want: 0},
		{name: "midday", at: time.Date(2024, time.January, 3, 12, 0, 0, 0, location), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newFixedClockPolicy(t, windows, location, 0, tt.at)
			got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{})
			if err != nil {
				t.Fatalf("NextMaxIdle failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("NextMaxIdle = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTimeWindowPoolResizePolicy_EvaluatesInConfiguredLocation(t *testing.T) {
	tokyo := mustLoadLocation(t, "Asia/Tokyo")
	// 09:00 in Tokyo is 00:00 UTC, so a 09:00-17:00 Tokyo window must not match
	// a UTC clock reading of 12:00.
	windows := []PoolResizeWindow{
		{Name: "tokyo business", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9},
	}
	at := time.Date(2024, time.January, 2, 12, 0, 0, 0, tokyo)
	policy := newFixedClockPolicy(t, windows, tokyo, 1, at)
	got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{})
	if err != nil {
		t.Fatalf("NextMaxIdle failed: %v", err)
	}
	if got != 9 {
		t.Errorf("NextMaxIdle = %d, want 9 inside the Tokyo window", got)
	}

	utcPolicy := newFixedClockPolicy(t, windows, time.UTC, 1, at.UTC())
	got, err = utcPolicy.NextMaxIdle(context.Background(), PoolSnapshot{})
	if err != nil {
		t.Fatalf("NextMaxIdle failed: %v", err)
	}
	if got != 1 {
		t.Errorf("NextMaxIdle in UTC = %d, want the default 1", got)
	}
}

func TestTimeWindowPoolResizePolicy_NilLocationIsUTC(t *testing.T) {
	windows := []PoolResizeWindow{
		{Name: "business", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9},
	}
	at := time.Date(2024, time.January, 2, 10, 0, 0, 0, time.UTC)
	policy := newFixedClockPolicy(t, windows, nil, 1, at)
	got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{})
	if err != nil {
		t.Fatalf("NextMaxIdle failed: %v", err)
	}
	if got != 9 {
		t.Errorf("NextMaxIdle = %d, want 9", got)
	}
}

// A daylight-saving transition must not move a wall-clock window. The test
// reaches both 01:30 instants on the fall-back day explicitly, because
// time.Date silently resolves the ambiguous local time to the first one.
func TestTimeWindowPoolResizePolicy_WindowSurvivesDSTTransitions(t *testing.T) {
	location := mustLoadLocation(t, "America/Los_Angeles")
	windows := []PoolResizeWindow{
		{Name: "early", Start: 1 * time.Hour, End: 3 * time.Hour, MaxIdle: 9},
	}
	// 2024-11-03: 01:30 happens twice, at 08:30Z (PDT) and 09:30Z (PST).
	fallBack := []time.Time{
		time.Date(2024, time.November, 3, 8, 30, 0, 0, time.UTC).In(location),
		time.Date(2024, time.November, 3, 9, 30, 0, 0, time.UTC).In(location),
	}
	tests := []struct {
		name string
		now  time.Time
	}{
		// 2024-03-10 is the US spring-forward date; 01:30 is before the gap.
		{name: "spring forward", now: time.Date(2024, time.March, 10, 1, 30, 0, 0, location)},
		{name: "fall back first pass", now: fallBack[0]},
		{name: "fall back second pass", now: fallBack[1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newFixedClockPolicy(t, windows, location, 1, tt.now)
			if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 9 {
				t.Fatalf("window at DST transition = (%d, %v), want (9, nil)", got, err)
			}
		})
	}
}

// A narrow window around the repeated hour must contain the wall-clock time on
// both passes, which only holds if the offset is read from the clock fields.
func TestTimeWindowPoolResizePolicy_NarrowWindowCoversRepeatedHour(t *testing.T) {
	location := mustLoadLocation(t, "America/Los_Angeles")
	windows := []PoolResizeWindow{
		{Name: "first pass only", Start: 1 * time.Hour, End: 2 * time.Hour, MaxIdle: 5},
	}
	tests := []struct {
		name string
		now  time.Time
	}{
		{name: "first 01:30", now: time.Date(2024, time.November, 3, 8, 30, 0, 0, time.UTC).In(location)},
		{name: "second 01:30", now: time.Date(2024, time.November, 3, 9, 30, 0, 0, time.UTC).In(location)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newFixedClockPolicy(t, windows, location, 1, tt.now)
			if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 5 {
				t.Fatalf("narrow window at repeated hour = (%d, %v), want (5, nil)", got, err)
			}
		})
	}
	// A window that does not cover the repeated hour must not match it.
	policy := newFixedClockPolicy(t, []PoolResizeWindow{
		{Name: "after the gap", Start: 2 * time.Hour, End: 3 * time.Hour, MaxIdle: 5},
	}, location, 1, time.Date(2024, time.November, 3, 9, 30, 0, 0, time.UTC).In(location))
	if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 1 {
		t.Fatalf("02:00-03:00 window at 01:30 = (%d, %v), want the default (1, nil)", got, err)
	}
}

// The spring-forward direction discriminates too: 02:00-03:00 does not exist as
// wall-clock time, so 03:30 must fall outside it. Elapsed-time arithmetic puts
// 03:30 at 2h30m on the spring-forward day and would match it wrongly.
func TestTimeWindowPoolResizePolicy_SpringForwardSkipsTheMissingHour(t *testing.T) {
	location := mustLoadLocation(t, "America/Los_Angeles")
	// 2024-03-10 03:30 PDT is 10:30Z; local midnight is 08:00Z.
	now := time.Date(2024, time.March, 10, 10, 30, 0, 0, time.UTC).In(location)
	policy := newFixedClockPolicy(t, []PoolResizeWindow{
		{Name: "skipped hour", Start: 2 * time.Hour, End: 3 * time.Hour, MaxIdle: 5},
	}, location, 1, now)
	if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 1 {
		t.Fatalf("02:00-03:00 window at 03:30 = (%d, %v), want the default (1, nil)", got, err)
	}
	// A window that does contain 03:30 must still match.
	policy = newFixedClockPolicy(t, []PoolResizeWindow{
		{Name: "afternoon", Start: 3 * time.Hour, End: 4 * time.Hour, MaxIdle: 5},
	}, location, 1, now)
	if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 5 {
		t.Fatalf("03:00-04:00 window at 03:30 = (%d, %v), want (5, nil)", got, err)
	}
}

// A window that wraps midnight is attributed to the day it starts on, so a
// Friday-night window also covers the small hours of Saturday.
func TestTimeWindowPoolResizePolicy_WrappedWindowWeekdayFollowsStartDay(t *testing.T) {
	location := time.UTC
	windows := []PoolResizeWindow{
		{Name: "friday night", Start: 22 * time.Hour, End: 6 * time.Hour, MaxIdle: 4,
			Weekdays: []time.Weekday{time.Friday}},
	}
	// 2024-01-05 is a Friday, 2024-01-06 a Saturday, 2024-01-08 a Monday.
	tests := []struct {
		name string
		now  time.Time
		want int
	}{
		{name: "friday evening", now: time.Date(2024, time.January, 5, 23, 0, 0, 0, location), want: 4},
		{name: "saturday small hours continue the friday window", now: time.Date(2024, time.January, 6, 2, 0, 0, 0, location), want: 4},
		{name: "saturday evening is not friday", now: time.Date(2024, time.January, 6, 23, 0, 0, 0, location), want: 0},
		{name: "sunday small hours are not friday", now: time.Date(2024, time.January, 7, 2, 0, 0, 0, location), want: 0},
		{name: "monday small hours are not friday", now: time.Date(2024, time.January, 8, 2, 0, 0, 0, location), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := newFixedClockPolicy(t, windows, location, 0, tt.now)
			if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != tt.want {
				t.Fatalf("NextMaxIdle = (%d, %v), want (%d, nil)", got, err, tt.want)
			}
		})
	}
}

// A wrapped Friday-night window and a Saturday-morning window really do overlap,
// so the constructor must reject the pair.
func TestNewTimeWindowPoolResizePolicy_RejectsWrapAcrossDayBoundary(t *testing.T) {
	_, err := NewTimeWindowPoolResizePolicy([]PoolResizeWindow{
		{Name: "friday night", Start: 22 * time.Hour, End: 6 * time.Hour, MaxIdle: 4,
			Weekdays: []time.Weekday{time.Friday}},
		{Name: "saturday morning", Start: 1 * time.Hour, End: 3 * time.Hour, MaxIdle: 2,
			Weekdays: []time.Weekday{time.Saturday}},
	}, time.UTC, 0, time.Minute)
	if err == nil {
		t.Fatal("expected an overlap error for windows that meet across midnight")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("error = %q, want it to mention the overlap", err.Error())
	}
}

// Pairs that share a weekday *or* share a wall-clock range, but never both on
// the same day, resolve deterministically and must be accepted. A validator that
// intersects the weekday filter and the offset independently would wrongly
// reject every one of these.
func TestNewTimeWindowPoolResizePolicy_AllowsNonOverlappingWrapPairs(t *testing.T) {
	tests := []struct {
		name    string
		windows []PoolResizeWindow
	}{
		{
			// Friday 22:00-02:00 and Saturday 21:00-23:00 never coincide: the
			// wrapped window is off on Saturday evening and the other is off on
			// Friday night.
			name: "wrapped friday night vs saturday evening",
			windows: []PoolResizeWindow{
				{Name: "friday night", Start: 22 * time.Hour, End: 2 * time.Hour, MaxIdle: 4,
					Weekdays: []time.Weekday{time.Friday}},
				{Name: "saturday evening", Start: 21 * time.Hour, End: 23 * time.Hour, MaxIdle: 7,
					Weekdays: []time.Weekday{time.Saturday}},
			},
		},
		{
			// Friday 22:00-02:00 covers Friday night and Saturday small hours;
			// Saturday 23:00-01:00 covers Saturday night and Sunday small hours.
			// Saturday 00:30 has only the first, Sunday 00:30 only the second.
			name: "two wrapped windows on consecutive days",
			windows: []PoolResizeWindow{
				{Name: "friday night", Start: 22 * time.Hour, End: 2 * time.Hour, MaxIdle: 4,
					Weekdays: []time.Weekday{time.Friday}},
				{Name: "saturday night", Start: 23 * time.Hour, End: 1 * time.Hour, MaxIdle: 7,
					Weekdays: []time.Weekday{time.Saturday}},
			},
		},
		{
			// A wrapped window never reaches Saturday afternoon.
			name: "wrapped friday night vs saturday afternoon",
			windows: []PoolResizeWindow{
				{Name: "friday night", Start: 22 * time.Hour, End: 2 * time.Hour, MaxIdle: 4,
					Weekdays: []time.Weekday{time.Friday}},
				{Name: "saturday afternoon", Start: 12 * time.Hour, End: 18 * time.Hour, MaxIdle: 7,
					Weekdays: []time.Weekday{time.Saturday}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTimeWindowPoolResizePolicy(tt.windows, time.UTC, 0, time.Minute); err != nil {
				t.Fatalf("deterministic window pair rejected: %v", err)
			}
		})
	}
}

// The overlap check must not be fooled in the other direction either: a pair
// that genuinely co-occurs on a wrapped segment is still rejected.
func TestNewTimeWindowPoolResizePolicy_StillRejectsGenuineWrapOverlap(t *testing.T) {
	// Friday 22:00-02:00 covers Saturday 00:00-02:00, and Saturday 01:00-03:00
	// covers Saturday 01:00-03:00, so they meet on Saturday 01:00-02:00.
	_, err := NewTimeWindowPoolResizePolicy([]PoolResizeWindow{
		{Name: "friday night", Start: 22 * time.Hour, End: 2 * time.Hour, MaxIdle: 4,
			Weekdays: []time.Weekday{time.Friday}},
		{Name: "saturday early", Start: 1 * time.Hour, End: 3 * time.Hour, MaxIdle: 7,
			Weekdays: []time.Weekday{time.Saturday}},
	}, time.UTC, 0, time.Minute)
	if err == nil {
		t.Fatal("expected an overlap error for windows that meet on a wrapped segment")
	}
}

func TestTimeWindowPoolResizePolicy_FiltersByWeekday(t *testing.T) {
	location := time.UTC
	// 2024-01-06 is a Saturday, 2024-01-08 a Monday.
	windows := []PoolResizeWindow{
		{Name: "weekdays", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9,
			Weekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}},
	}
	saturday := time.Date(2024, time.January, 6, 10, 0, 0, 0, location)
	policy := newFixedClockPolicy(t, windows, location, 1, saturday)
	if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 1 {
		t.Fatalf("Saturday target = (%d, %v), want (1, nil)", got, err)
	}
	monday := time.Date(2024, time.January, 8, 10, 0, 0, 0, location)
	policy = newFixedClockPolicy(t, windows, location, 1, monday)
	if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 9 {
		t.Fatalf("Monday target = (%d, %v), want (9, nil)", got, err)
	}
}

func TestTimeWindowPoolResizePolicy_FirstMatchWins(t *testing.T) {
	location := time.UTC
	// Overlapping windows are rejected by the constructor, so a "first match
	// wins" policy is expressed with adjacent, non-overlapping windows.
	windows := []PoolResizeWindow{
		{Name: "morning", Start: 9 * time.Hour, End: 12 * time.Hour, MaxIdle: 2},
		{Name: "afternoon", Start: 12 * time.Hour, End: 17 * time.Hour, MaxIdle: 5},
	}
	at := time.Date(2024, time.January, 2, 13, 0, 0, 0, location)
	policy := newFixedClockPolicy(t, windows, location, 0, at)
	if got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{}); err != nil || got != 5 {
		t.Fatalf("NextMaxIdle = (%d, %v), want (5, nil)", got, err)
	}
}

func TestNewTimeWindowPoolResizePolicy_RejectsInvalidConfiguration(t *testing.T) {
	valid := PoolResizeWindow{Name: "business", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9}
	tests := []struct {
		name         string
		windows      []PoolResizeWindow
		defaultMax   int
		interval     time.Duration
		wantContains string
	}{
		{
			name:         "zero interval",
			windows:      []PoolResizeWindow{valid},
			interval:     0,
			wantContains: "interval must be positive",
		},
		{
			name:         "negative interval",
			windows:      []PoolResizeWindow{valid},
			interval:     -time.Minute,
			wantContains: "interval must be positive",
		},
		{
			name:         "negative default",
			windows:      []PoolResizeWindow{valid},
			defaultMax:   -1,
			interval:     time.Minute,
			wantContains: "default maxIdle must be >= 0",
		},
		{
			name:         "negative start",
			windows:      []PoolResizeWindow{{Name: "bad", Start: -time.Hour, End: time.Hour}},
			interval:     time.Minute,
			wantContains: "start",
		},
		{
			name:         "start at end of day",
			windows:      []PoolResizeWindow{{Name: "bad", Start: poolResizeLocalDay, End: poolResizeLocalDay}},
			interval:     time.Minute,
			wantContains: "outside a local day",
		},
		{
			name:         "zero end",
			windows:      []PoolResizeWindow{{Name: "bad", Start: time.Hour}},
			interval:     time.Minute,
			wantContains: "end",
		},
		{
			name:         "end past midnight",
			windows:      []PoolResizeWindow{{Name: "bad", Start: time.Hour, End: poolResizeLocalDay + time.Hour}},
			interval:     time.Minute,
			wantContains: "outside a local day",
		},
		{
			name:         "empty window",
			windows:      []PoolResizeWindow{{Name: "bad", Start: 9 * time.Hour, End: 9 * time.Hour, MaxIdle: 2}},
			interval:     time.Minute,
			wantContains: "is empty",
		},
		{
			name:         "negative maxIdle",
			windows:      []PoolResizeWindow{{Name: "bad", Start: time.Hour, End: 2 * time.Hour, MaxIdle: -1}},
			interval:     time.Minute,
			wantContains: "maxIdle must be >= 0",
		},
		{
			name:         "overlapping windows",
			windows:      []PoolResizeWindow{valid, {Name: "clash", Start: 16 * time.Hour, End: 20 * time.Hour, MaxIdle: 2}},
			interval:     time.Minute,
			wantContains: "overlaps",
		},
		{
			name: "overlapping wrapped window",
			windows: []PoolResizeWindow{
				{Name: "overnight", Start: 22 * time.Hour, End: 6 * time.Hour, MaxIdle: 4},
				{Name: "early", Start: 3 * time.Hour, End: 9 * time.Hour, MaxIdle: 2},
			},
			interval:     time.Minute,
			wantContains: "overlaps",
		},
		{
			name: "duplicate weekday",
			windows: []PoolResizeWindow{{Name: "bad", Start: time.Hour, End: 2 * time.Hour,
				Weekdays: []time.Weekday{time.Monday, time.Monday}}},
			interval:     time.Minute,
			wantContains: "repeats weekday",
		},
		{
			name: "invalid weekday",
			windows: []PoolResizeWindow{{Name: "bad", Start: time.Hour, End: 2 * time.Hour,
				Weekdays: []time.Weekday{time.Weekday(9)}}},
			interval:     time.Minute,
			wantContains: "invalid weekday",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := NewTimeWindowPoolResizePolicy(tt.windows, time.UTC, tt.defaultMax, tt.interval)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if policy != nil {
				t.Error("expected a nil policy alongside the error")
			}
			if !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantContains)
			}
		})
	}
}

// Windows that only share wall-clock time on disjoint weekdays are legal.
func TestNewTimeWindowPoolResizePolicy_AllowsDisjointWeekdayOverlap(t *testing.T) {
	_, err := NewTimeWindowPoolResizePolicy([]PoolResizeWindow{
		{Name: "weekdays", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9,
			Weekdays: []time.Weekday{time.Monday, time.Tuesday}},
		{Name: "weekends", Start: 10 * time.Hour, End: 14 * time.Hour, MaxIdle: 2,
			Weekdays: []time.Weekday{time.Saturday, time.Sunday}},
	}, time.UTC, 0, time.Minute)
	if err != nil {
		t.Fatalf("disjoint weekday windows rejected: %v", err)
	}
}

func TestTimeWindowPoolResizePolicy_ZeroValueIntervalFallsBackToDefault(t *testing.T) {
	// A struct literal must stay usable without the constructor.
	policy := &TimeWindowPoolResizePolicy{
		Windows: []PoolResizeWindow{{Name: "always", Start: 0, End: poolResizeLocalDay, MaxIdle: 3}},
	}
	if got := policy.Interval(); got != defaultPoolResizePolicyInterval {
		t.Errorf("Interval = %s, want %s", got, defaultPoolResizePolicyInterval)
	}
	got, err := policy.NextMaxIdle(context.Background(), PoolSnapshot{})
	if err != nil {
		t.Fatalf("NextMaxIdle failed: %v", err)
	}
	if got != 3 {
		t.Errorf("NextMaxIdle = %d, want 3", got)
	}
}

// The policy satisfies the interface the adapter consumes.
func TestTimeWindowPoolResizePolicy_ImplementsPoolResizePolicy(t *testing.T) {
	policy, err := NewTimeWindowPoolResizePolicy(nil, time.UTC, 0, time.Minute)
	if err != nil {
		t.Fatalf("NewTimeWindowPoolResizePolicy failed: %v", err)
	}
	var _ PoolResizePolicy = policy
}

// A policy built by the constructor drives a real pool end to end.
func TestTimeWindowPoolResizePolicy_DrivesAdapterTarget(t *testing.T) {
	pool := &resizeAdapterTestPool{snapshot: PoolSnapshot{LifecycleState: PoolLifecycleRunning, MaxIdle: 1}}
	policy, err := NewTimeWindowPoolResizePolicy([]PoolResizeWindow{
		{Name: "business", Start: 9 * time.Hour, End: 17 * time.Hour, MaxIdle: 9},
	}, time.UTC, 1, time.Minute)
	if err != nil {
		t.Fatalf("NewTimeWindowPoolResizePolicy failed: %v", err)
	}
	policy.now = func() time.Time { return time.Date(2024, time.January, 2, 10, 0, 0, 0, time.UTC) }
	adapter, err := NewPoolResizeAdapter(pool, policy)
	if err != nil {
		t.Fatalf("NewPoolResizeAdapter failed: %v", err)
	}
	if err := adapter.Apply(context.Background()); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if got := pool.resizeCallValues(); len(got) != 1 || got[0] != 9 {
		t.Fatalf("resize calls = %v, want [9]", got)
	}
}
