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
	"sync/atomic"
	"testing"
	"time"
)

func TestPool_ReconcileLoop_MaintainsIdleDuringBackoff(t *testing.T) {
	for _, nearExpiry := range []bool{false, true} {
		name := "shrink"
		if nearExpiry {
			name = "reap without replenishing"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewInMemoryPoolStateStore()
			cfg := defaultTestPoolConfig(store)
			cfg.Logger = noopPoolLogger{}
			target := 0
			if nearExpiry {
				target = 1
				if err := store.SetIdleEntryTTL(ctx, cfg.PoolName, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SetMaxIdle(ctx, cfg.PoolName, target); err != nil {
				t.Fatal(err)
			}
			if err := store.PutIdle(ctx, cfg.PoolName, "idle"); err != nil {
				t.Fatal(err)
			}
			rec := newKillRecorder(t)
			cfg.ConnectionConfig = ConnectionConfig{Domain: rec.srv.URL, Protocol: "http", DisableMetrics: true}
			var creates atomic.Int32
			cfg.SandboxCreator = &mockSandboxCreator{createFn: func(context.Context, PooledSandboxCreateContext) (*Sandbox, error) {
				creates.Add(1)
				return nil, errors.New("creation must remain throttled")
			}}
			state := newReconcileState(1)
			state.recordFailure(errors.New("backend unavailable"))
			ticks := make(chan time.Time)
			pool := &DefaultSandboxPool{
				config: cfg, manager: NewSandboxManager(cfg.ConnectionConfig), reconciler: state,
				ticker: &time.Ticker{C: ticks}, done: make(chan struct{}),
			}
			pool.wg.Add(1)
			go pool.reconcileLoop(ctx)
			// An unbuffered tick guarantees the loop selects this iteration before
			// shutdown. Waiting for the loop then also waits for its maintenance work.
			ticks <- time.Now()
			close(pool.done)
			pool.wg.Wait()
			counters, err := store.SnapshotCounters(ctx, cfg.PoolName)
			if err != nil {
				t.Fatal(err)
			}
			if counters.IdleCount != 0 {
				t.Errorf("idle count = %d, want 0 after maintenance", counters.IdleCount)
			}
			if got := rec.deleted.Load(); got != 1 {
				t.Errorf("kill attempts = %d, want 1", got)
			}
			if got := creates.Load(); got != 0 {
				t.Errorf("create attempts = %d during backoff, want 0", got)
			}
			if !state.shouldBackoff() {
				t.Error("maintenance cleared creation backoff")
			}
		})
	}
}

func TestReconciler_LockOutageDoesNotExtendActiveBackoff(t *testing.T) {
	ctx := context.Background()
	lockAttempts := 0
	store := &mockLockStore{
		InMemoryPoolStateStore: NewInMemoryPoolStateStore(),
		acquireLockFn: func(context.Context, string, string, time.Duration) (bool, error) {
			lockAttempts++
			return false, errors.New("store unavailable")
		},
	}
	cfg := defaultTestPoolConfig(store)
	state := newReconcileState(cfg.DegradedThreshold)
	if err := store.SetMaxIdle(ctx, cfg.PoolName, 1); err != nil {
		t.Fatal(err)
	}
	var creates atomic.Int32
	tick := func() {
		reconcileTick(ctx, cfg, store, state, testLogger, func(context.Context, PooledSandboxCreateReason) (string, error) {
			creates.Add(1)
			return "replacement", nil
		}, func(string) {})
	}
	for i := 0; i < cfg.DegradedThreshold; i++ {
		tick()
	}
	deadline := state.backoffUntil
	if !state.shouldBackoff() {
		t.Fatal("expected creation backoff after lock failures")
	}
	for i := 0; i < 15; i++ {
		tick()
	}
	if lockAttempts != cfg.DegradedThreshold+15 || state.backoffUntil != deadline {
		t.Fatalf("lock attempts = %d, deadline moved by %v", lockAttempts, state.backoffUntil.Sub(deadline))
	}

	store.acquireLockFn = nil
	tick()
	if creates.Load() != 0 {
		t.Fatal("warmup ran before the deadline")
	}
	state.backoffUntil = time.Now().Add(-time.Second)
	tick()
	if creates.Load() != 1 || state.backoffAttempts != 0 {
		t.Fatal("expected warmup to resume and clear backoff after recovery")
	}
}
