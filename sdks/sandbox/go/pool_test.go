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
	"testing"
	"time"
)

func TestNewSandboxPoolValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  PoolConfig
		wantErr bool
	}{
		{
			name:    "missing pool name",
			config:  PoolConfig{MaxIdle: 5, CreationSpec: PoolCreationSpec{Image: "test"}, StateStore: NewInMemoryPoolStateStore()},
			wantErr: true,
		},
		{
			name:    "missing max idle",
			config:  PoolConfig{PoolName: "test", MaxIdle: 0, CreationSpec: PoolCreationSpec{Image: "test"}, StateStore: NewInMemoryPoolStateStore()},
			wantErr: true,
		},
		{
			name:    "missing image",
			config:  PoolConfig{PoolName: "test", MaxIdle: 5, CreationSpec: PoolCreationSpec{}, StateStore: NewInMemoryPoolStateStore()},
			wantErr: true,
		},
		{
			name:    "missing state store",
			config:  PoolConfig{PoolName: "test", MaxIdle: 5, CreationSpec: PoolCreationSpec{Image: "test"}},
			wantErr: true,
		},
		{
			name:    "valid config",
			config:  PoolConfig{PoolName: "test", MaxIdle: 5, CreationSpec: PoolCreationSpec{Image: "test"}, StateStore: NewInMemoryPoolStateStore()},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := NewSandboxPool(tt.config)
			err := pool.Start(context.Background())
			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if err == nil {
				pool.Shutdown(context.Background())
			}
		})
	}
}

func TestPoolConfigDefaults(t *testing.T) {
	config := PoolConfig{
		PoolName:     "test",
		MaxIdle:      10,
		CreationSpec: PoolCreationSpec{Image: "test"},
		StateStore:   NewInMemoryPoolStateStore(),
	}
	config.applyDefaults()

	if config.WarmupConcurrency != 2 { // ceil(10 * 0.2) = 2
		t.Errorf("WarmupConcurrency = %d, want 2", config.WarmupConcurrency)
	}
	if config.ReconcileInterval != DefaultReconcileInterval {
		t.Errorf("ReconcileInterval = %v, want %v", config.ReconcileInterval, DefaultReconcileInterval)
	}
	if config.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", config.IdleTimeout, DefaultIdleTimeout)
	}
	if config.OwnerID == "" {
		t.Error("OwnerID should be auto-generated")
	}
}

func TestInMemoryPoolStateStore(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	pool := "test-pool"

	// Initially empty
	_, ok, err := store.TryTakeIdle(ctx, pool)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if ok {
		t.Error("expected no idle sandbox")
	}

	// Put some idle
	future := time.Now().Add(time.Hour)
	if err := store.PutIdle(ctx, pool, "sb-1", future); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}
	if err := store.PutIdle(ctx, pool, "sb-2", future); err != nil {
		t.Fatalf("PutIdle error: %v", err)
	}

	// Count
	count, err := store.CountIdle(ctx, pool)
	if err != nil {
		t.Fatalf("CountIdle error: %v", err)
	}
	if count != 2 {
		t.Errorf("CountIdle = %d, want 2", count)
	}

	// Take (FIFO)
	id, ok, err := store.TryTakeIdle(ctx, pool)
	if err != nil {
		t.Fatalf("TryTakeIdle error: %v", err)
	}
	if !ok || id != "sb-1" {
		t.Errorf("TryTakeIdle = (%q, %v), want (sb-1, true)", id, ok)
	}

	// Remove specific
	if err := store.RemoveIdle(ctx, pool, "sb-2"); err != nil {
		t.Fatalf("RemoveIdle error: %v", err)
	}
	count, _ = store.CountIdle(ctx, pool)
	if count != 0 {
		t.Errorf("CountIdle after remove = %d, want 0", count)
	}
}

func TestInMemoryPoolStateStoreReapExpired(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	pool := "test-pool"

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	store.PutIdle(ctx, pool, "expired", past)
	store.PutIdle(ctx, pool, "valid", future)

	if err := store.ReapExpired(ctx, pool, time.Now()); err != nil {
		t.Fatalf("ReapExpired error: %v", err)
	}

	count, _ := store.CountIdle(ctx, pool)
	if count != 1 {
		t.Errorf("CountIdle after reap = %d, want 1", count)
	}

	id, ok, _ := store.TryTakeIdle(ctx, pool)
	if !ok || id != "valid" {
		t.Errorf("remaining entry = (%q, %v), want (valid, true)", id, ok)
	}
}

func TestInMemoryPoolStateStoreLock(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPoolStateStore()
	pool := "test-pool"

	// Acquire lock
	ok, err := store.TryAcquireLock(ctx, pool, "owner-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("TryAcquireLock = (%v, %v), want (true, nil)", ok, err)
	}

	// Another owner cannot acquire
	ok, err = store.TryAcquireLock(ctx, pool, "owner-2", time.Minute)
	if err != nil {
		t.Fatalf("TryAcquireLock error: %v", err)
	}
	if ok {
		t.Error("owner-2 should not acquire lock held by owner-1")
	}

	// Same owner can renew
	ok, _ = store.RenewLock(ctx, pool, "owner-1", time.Minute)
	if !ok {
		t.Error("owner-1 should be able to renew")
	}

	// Release
	store.ReleaseLock(ctx, pool, "owner-1")

	// Now owner-2 can acquire
	ok, _ = store.TryAcquireLock(ctx, pool, "owner-2", time.Minute)
	if !ok {
		t.Error("owner-2 should acquire after release")
	}
}

func TestReconcileState(t *testing.T) {
	rs := newReconcileState(3)

	// Not degraded initially
	if rs.isDegraded() {
		t.Error("should not be degraded initially")
	}

	// Record failures below threshold
	rs.recordFailure("err1")
	rs.recordFailure("err2")
	if rs.isDegraded() {
		t.Error("should not be degraded below threshold")
	}

	// Hit threshold
	rs.recordFailure("err3")
	if !rs.isDegraded() {
		t.Error("should be degraded at threshold")
	}
	if !rs.isBackoffActive() {
		t.Error("backoff should be active")
	}

	// Success resets
	rs.recordSuccess()
	if rs.isDegraded() {
		t.Error("should not be degraded after success")
	}
	if rs.isBackoffActive() {
		t.Error("backoff should not be active after success")
	}
}

func TestPoolAcquireNotRunning(t *testing.T) {
	pool := NewSandboxPool(PoolConfig{
		PoolName:     "test",
		MaxIdle:      5,
		CreationSpec: PoolCreationSpec{Image: "test"},
		StateStore:   NewInMemoryPoolStateStore(),
	})

	// Don't call Start
	_, err := pool.Acquire(context.Background())
	if err == nil {
		t.Error("expected error acquiring from non-started pool")
	}
	if _, ok := err.(*PoolNotRunningError); !ok {
		t.Errorf("expected PoolNotRunningError, got %T: %v", err, err)
	}
}

func TestPoolSnapshot(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	pool := NewSandboxPool(PoolConfig{
		PoolName:     "test",
		MaxIdle:      5,
		CreationSpec: PoolCreationSpec{Image: "test"},
		StateStore:   store,
	})

	snap := pool.Snapshot()
	if snap.State != PoolStateCreated {
		t.Errorf("State = %v, want CREATED", snap.State)
	}
	if snap.IdleCount != 0 {
		t.Errorf("IdleCount = %d, want 0", snap.IdleCount)
	}
}

func TestPoolAcquireFailFastEmpty(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	config := PoolConfig{
		PoolName:         "test",
		MaxIdle:          5,
		ConnectionConfig: ConnectionConfig{Domain: "localhost:9999", APIKey: "test"},
		CreationSpec:     PoolCreationSpec{Image: "test:latest"},
		StateStore:       store,
		ReconcileInterval: time.Hour, // don't auto-reconcile during test
	}
	pool := NewSandboxPool(config)
	ctx := context.Background()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer pool.Shutdown(ctx)

	// Pool is empty, FailFast should error
	_, err := pool.Acquire(ctx, WithAcquirePolicy(FailFast))
	if err == nil {
		t.Fatal("expected PoolEmptyError")
	}
	if _, ok := err.(*PoolEmptyError); !ok {
		t.Errorf("expected *PoolEmptyError, got %T: %v", err, err)
	}
}
