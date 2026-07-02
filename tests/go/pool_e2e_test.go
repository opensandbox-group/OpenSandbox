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
	"os"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPool_AcquireFromPreWarmedIdle(t *testing.T) {
	if os.Getenv("RUN_POOL_E2E") != "true" {
		t.Skip("Set RUN_POOL_E2E=true to run pool e2e tests")
	}

	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store := opensandbox.NewInMemoryPoolStateStore()
	pool := opensandbox.NewSandboxPool(opensandbox.PoolConfig{
		PoolName:         "e2e-test-pool",
		MaxIdle:          2,
		ConnectionConfig: config,
		CreationSpec: opensandbox.PoolCreationSpec{
			Image:   getSandboxImage(),
			Timeout: 300,
			Env:     map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s"},
		},
		StateStore:        store,
		ReconcileInterval: 5 * time.Second,
		IdleTimeout:       5 * time.Minute,
	})

	// Start the pool and wait for warmup
	err := pool.Start(ctx)
	require.NoError(t, err)
	defer pool.Shutdown(context.Background())

	// Wait for reconciler to warm up at least 1 sandbox
	var snap opensandbox.PoolSnapshot
	for i := 0; i < 60; i++ {
		snap = pool.Snapshot()
		if snap.IdleCount > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	require.Greater(t, snap.IdleCount, 0, "pool should have warmed up at least 1 sandbox")

	// Acquire a sandbox from the pool
	sb, err := pool.Acquire(ctx, opensandbox.WithSandboxTimeout(2*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, sb)
	defer sb.Kill(context.Background())

	// Verify the sandbox is functional
	assert.True(t, sb.IsHealthy(ctx))

	// Run a command to confirm it works
	exec, err := sb.RunCommand(ctx, "echo pool-test-ok", nil)
	require.NoError(t, err)
	require.NotNil(t, exec.ExitCode)
	assert.Equal(t, 0, *exec.ExitCode)
	assert.Contains(t, exec.Text(), "pool-test-ok")
}

func TestPool_AcquireFailFastWhenEmpty(t *testing.T) {
	if os.Getenv("RUN_POOL_E2E") != "true" {
		t.Skip("Set RUN_POOL_E2E=true to run pool e2e tests")
	}

	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	store := opensandbox.NewInMemoryPoolStateStore()
	pool := opensandbox.NewSandboxPool(opensandbox.PoolConfig{
		PoolName:         "e2e-failfast-pool",
		MaxIdle:          0, // no warmup
		ConnectionConfig: config,
		CreationSpec: opensandbox.PoolCreationSpec{
			Image:   getSandboxImage(),
			Timeout: 300,
		},
		StateStore:        store,
		ReconcileInterval: time.Hour, // don't auto-reconcile
	})

	err := pool.Start(ctx)
	require.NoError(t, err)
	defer pool.Shutdown(context.Background())

	// FailFast should return error immediately when pool is empty
	_, err = pool.Acquire(ctx, opensandbox.WithAcquirePolicy(opensandbox.FailFast))
	require.Error(t, err)

	var poolEmpty *opensandbox.PoolEmptyError
	require.ErrorAs(t, err, &poolEmpty)
}

func TestPool_AcquireDirectCreateFallback(t *testing.T) {
	if os.Getenv("RUN_POOL_E2E") != "true" {
		t.Skip("Set RUN_POOL_E2E=true to run pool e2e tests")
	}

	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	store := opensandbox.NewInMemoryPoolStateStore()
	pool := opensandbox.NewSandboxPool(opensandbox.PoolConfig{
		PoolName:         "e2e-directcreate-pool",
		MaxIdle:          0, // no warmup
		ConnectionConfig: config,
		CreationSpec: opensandbox.PoolCreationSpec{
			Image:   getSandboxImage(),
			Timeout: 300,
			Env:     map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s"},
		},
		StateStore:        store,
		ReconcileInterval: time.Hour,
	})

	err := pool.Start(ctx)
	require.NoError(t, err)
	defer pool.Shutdown(context.Background())

	// DirectCreate (default) should create a new sandbox on-demand
	sb, err := pool.Acquire(ctx, opensandbox.WithSandboxTimeout(2*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, sb)
	defer sb.Kill(context.Background())

	assert.True(t, sb.IsHealthy(ctx))
}

func TestPool_ResizeAndSnapshot(t *testing.T) {
	if os.Getenv("RUN_POOL_E2E") != "true" {
		t.Skip("Set RUN_POOL_E2E=true to run pool e2e tests")
	}

	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store := opensandbox.NewInMemoryPoolStateStore()
	pool := opensandbox.NewSandboxPool(opensandbox.PoolConfig{
		PoolName:         "e2e-resize-pool",
		MaxIdle:          1,
		ConnectionConfig: config,
		CreationSpec: opensandbox.PoolCreationSpec{
			Image:   getSandboxImage(),
			Timeout: 300,
			Env:     map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s"},
		},
		StateStore:        store,
		ReconcileInterval: 5 * time.Second,
		IdleTimeout:       5 * time.Minute,
	})

	err := pool.Start(ctx)
	require.NoError(t, err)
	defer pool.Shutdown(context.Background())

	// Wait for initial warmup
	for i := 0; i < 60; i++ {
		if pool.Snapshot().IdleCount >= 1 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	snap := pool.Snapshot()
	assert.Equal(t, opensandbox.PoolStateRunning, snap.State)
	assert.GreaterOrEqual(t, snap.IdleCount, 1)

	// Resize to 0 (disable replenishment)
	err = pool.Resize(ctx, 0)
	require.NoError(t, err)

	// Release all idle
	released, err := pool.ReleaseAllIdle(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, released, 1)

	// Verify pool is empty after release
	snap = pool.Snapshot()
	assert.Equal(t, 0, snap.IdleCount)
}

func TestPool_GracefulShutdown(t *testing.T) {
	if os.Getenv("RUN_POOL_E2E") != "true" {
		t.Skip("Set RUN_POOL_E2E=true to run pool e2e tests")
	}

	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store := opensandbox.NewInMemoryPoolStateStore()
	pool := opensandbox.NewSandboxPool(opensandbox.PoolConfig{
		PoolName:         "e2e-shutdown-pool",
		MaxIdle:          1,
		ConnectionConfig: config,
		CreationSpec: opensandbox.PoolCreationSpec{
			Image:   getSandboxImage(),
			Timeout: 300,
			Env:     map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s"},
		},
		StateStore:        store,
		ReconcileInterval: 5 * time.Second,
		IdleTimeout:       5 * time.Minute,
		DrainTimeout:      10 * time.Second,
	})

	err := pool.Start(ctx)
	require.NoError(t, err)

	// Wait for warmup
	for i := 0; i < 60; i++ {
		if pool.Snapshot().IdleCount >= 1 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Shutdown should clean up idle sandboxes
	err = pool.Shutdown(ctx)
	require.NoError(t, err)

	snap := pool.Snapshot()
	assert.Equal(t, opensandbox.PoolStateStopped, snap.State)
	assert.Equal(t, 0, snap.IdleCount)

	// Acquire after shutdown should fail
	_, err = pool.Acquire(ctx)
	require.Error(t, err)
	var notRunning *opensandbox.PoolNotRunningError
	require.ErrorAs(t, err, &notRunning)
}
