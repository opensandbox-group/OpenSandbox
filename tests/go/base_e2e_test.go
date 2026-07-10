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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func getConnectionConfig(t *testing.T) opensandbox.ConnectionConfig {
	t.Helper()

	domain := os.Getenv("OPENSANDBOX_TEST_DOMAIN")
	if domain == "" {
		domain = "localhost:8080"
	}

	protocol := os.Getenv("OPENSANDBOX_TEST_PROTOCOL")
	if protocol == "" {
		protocol = "http"
	}

	apiKey := os.Getenv("OPENSANDBOX_TEST_API_KEY")
	if apiKey == "" {
		apiKey = "e2e-test"
	}

	useProxy := os.Getenv("OPENSANDBOX_TEST_USE_SERVER_PROXY") == "true"

	config := opensandbox.ConnectionConfig{
		Domain:         domain,
		Protocol:       protocol,
		APIKey:         apiKey,
		UseServerProxy: useProxy,
	}

	if useProxy {
		config.AuthHeader = "X-API-Key"
	}

	return config
}

func connectionConfigForStreaming(t *testing.T) opensandbox.ConnectionConfig {
	t.Helper()
	c := getConnectionConfig(t)
	c.RequestTimeout = 3 * time.Minute
	return c
}

func getSandboxImage() string {
	if img := os.Getenv("OPENSANDBOX_SANDBOX_DEFAULT_IMAGE"); img != "" {
		return img
	}
	return "python:3.11-slim"
}

func createTestSandbox(t *testing.T) (context.Context, *opensandbox.Sandbox) {
	t.Helper()
	config := connectionConfigForStreaming(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { sb.Kill(context.Background()) })
	return ctx, sb
}

func newExecdClientForSandbox(t *testing.T, ctx context.Context, sb *opensandbox.Sandbox) *opensandbox.ExecdClient {
	t.Helper()

	endpoint, err := sb.GetEndpoint(ctx, opensandbox.DefaultExecdPort)
	require.NoError(t, err)
	require.NotEmpty(t, endpoint.Endpoint)

	execdURL := endpoint.Endpoint
	if !strings.HasPrefix(execdURL, "http") {
		execdURL = "http://" + execdURL
	}
	execdURL = strings.Replace(execdURL, "host.docker.internal", "localhost", 1)

	token := ""
	if endpoint.Headers != nil {
		token = endpoint.Headers["X-EXECD-ACCESS-TOKEN"]
	}
	return opensandbox.NewExecdClient(execdURL, token)
}

// ---------- Pool E2E helpers ----------

// Pool test constants aligned with Python E2E tests.
const (
	poolMaxIdle           = 2
	poolReconcileInterval = 1 * time.Second
	poolPrimaryLockTTL    = 4 * time.Second
	poolDrainTimeout      = 300 * time.Millisecond
	poolAwaitTimeout      = 2 * time.Minute
)

func getE2eSandboxResource() opensandbox.ResourceLimits {
	cpu := os.Getenv("OPENSANDBOX_E2E_SANDBOX_CPU")
	if cpu == "" {
		cpu = "1"
	}
	memory := os.Getenv("OPENSANDBOX_E2E_SANDBOX_MEMORY")
	if memory == "" {
		memory = "2Gi"
	}
	return opensandbox.ResourceLimits{"cpu": cpu, "memory": memory}
}

func poolTag(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

type poolCreateOpts struct {
	warmupConcurrency    int
	warmupSandboxPreparer func(ctx context.Context, sb *opensandbox.Sandbox) error
	connectionConfig     *opensandbox.ConnectionConfig
	degradedThreshold    int
	warmupReadyTimeout   time.Duration
	acquireReadyTimeout  time.Duration
	primaryLockTTL       time.Duration
	reconcileInterval    time.Duration
}

func createTestPool(
	t *testing.T,
	poolName, ownerID string,
	store opensandbox.PoolStateStore,
	tag string,
	maxIdle int,
	opts *poolCreateOpts,
) *opensandbox.DefaultSandboxPool {
	t.Helper()

	connCfg := connectionConfigForStreaming(t)
	warmupConc := 1
	degradedThresh := 3
	warmupReady := 30 * time.Second
	acquireReady := 30 * time.Second
	lockTTL := poolPrimaryLockTTL
	reconcileInt := poolReconcileInterval
	var preparer func(ctx context.Context, sb *opensandbox.Sandbox) error

	if opts != nil {
		if opts.warmupConcurrency > 0 {
			warmupConc = opts.warmupConcurrency
		}
		if opts.degradedThreshold > 0 {
			degradedThresh = opts.degradedThreshold
		}
		if opts.warmupReadyTimeout > 0 {
			warmupReady = opts.warmupReadyTimeout
		}
		if opts.acquireReadyTimeout > 0 {
			acquireReady = opts.acquireReadyTimeout
		}
		if opts.primaryLockTTL > 0 {
			lockTTL = opts.primaryLockTTL
		}
		if opts.reconcileInterval > 0 {
			reconcileInt = opts.reconcileInterval
		}
		if opts.connectionConfig != nil {
			connCfg = *opts.connectionConfig
		}
		preparer = opts.warmupSandboxPreparer
	}

	builder := opensandbox.NewSandboxPoolBuilder().
		PoolName(poolName).
		OwnerID(ownerID).
		MaxIdle(maxIdle).
		WarmupConcurrency(warmupConc).
		StateStore(store).
		ConnectionConfig(connCfg).
		CreationSpec(opensandbox.PoolCreationSpec{
			Image:      getSandboxImage(),
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			Metadata:   map[string]string{"tag": tag, "suite": "sandbox-pool-go-e2e"},
			Env: map[string]string{
				"E2E_TEST":                         "true",
				"EXECD_API_GRACE_SHUTDOWN":          "3s",
				"EXECD_JUPYTER_IDLE_POLL_INTERVAL":  "1s",
			},
			ResourceLimits: getE2eSandboxResource(),
		}).
		ReconcileInterval(reconcileInt).
		PrimaryLockTTL(lockTTL).
		DrainTimeout(poolDrainTimeout).
		DegradedThreshold(degradedThresh).
		WarmupReadyTimeout(warmupReady).
		AcquireReadyTimeout(acquireReady)

	if preparer != nil {
		builder = builder.WarmupSandboxPreparer(preparer)
	}

	pool, err := builder.Build()
	require.NoError(t, err, "pool build failed")
	return pool
}

func eventually(t *testing.T, description string, condition func() bool, timeout, interval time.Duration) {
	t.Helper()
	if timeout == 0 {
		timeout = poolAwaitTimeout
	}
	if interval == 0 {
		interval = 1 * time.Second
	}
	deadline := time.After(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", description)
		case <-ticker.C:
			if condition() {
				return
			}
		}
	}
}

func cleanupPool(pool *opensandbox.DefaultSandboxPool) {
	ctx := context.Background()
	_ = pool.Resize(ctx, 0)
	_, _ = pool.ReleaseAllIdle(ctx)
	// Graceful shutdown first: waits for in-flight warmup to finish so
	// orphan sandboxes are tracked and cleaned. Fall back to non-graceful.
	_ = pool.Shutdown(ctx, true)
	_ = pool.Shutdown(ctx, false)
}

func cleanupBorrowed(sandboxes []*opensandbox.Sandbox) {
	for _, sb := range sandboxes {
		_ = sb.Kill(context.Background())
		_ = sb.Close()
	}
}

func cleanupTaggedSandboxes(t *testing.T, tag string) {
	t.Helper()
	mgr := opensandbox.NewSandboxManager(getConnectionConfig(t))
	defer mgr.Close()
	ctx := context.Background()
	// Brief wait for any in-flight sandbox creation to land before scanning.
	time.Sleep(500 * time.Millisecond)
	for attempt := 0; attempt < 5; attempt++ {
		result, err := mgr.ListSandboxInfos(ctx, opensandbox.ListOptions{
			Metadata: map[string]string{"tag": tag},
			PageSize: 50,
		})
		if err != nil || len(result.Items) == 0 {
			return
		}
		for _, info := range result.Items {
			_ = mgr.KillSandbox(ctx, info.ID)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func countTaggedSandboxes(t *testing.T, tag string) int {
	t.Helper()
	mgr := opensandbox.NewSandboxManager(getConnectionConfig(t))
	defer mgr.Close()
	result, err := mgr.ListSandboxInfos(context.Background(), opensandbox.ListOptions{
		Metadata: map[string]string{"tag": tag},
		PageSize: 50,
	})
	if err != nil {
		t.Logf("countTaggedSandboxes: list error: %v", err)
		return 0
	}
	return len(result.Items)
}

func brokenConnectionConfig() opensandbox.ConnectionConfig {
	return opensandbox.ConnectionConfig{
		Domain:         "127.0.0.1:9",
		Protocol:       "http",
		APIKey:         "broken-e2e-test",
		RequestTimeout: 1 * time.Second,
	}
}

func newPoolManager(t *testing.T) *opensandbox.SandboxManager {
	t.Helper()
	mgr := opensandbox.NewSandboxManager(getConnectionConfig(t))
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

func snapshotIdle(pool *opensandbox.DefaultSandboxPool) int {
	snap, err := pool.Snapshot(context.Background())
	if err != nil {
		return -1
	}
	return snap.IdleCount
}

func snapshotState(pool *opensandbox.DefaultSandboxPool) string {
	snap, err := pool.Snapshot(context.Background())
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return snap.HealthState.String()
}

func snapshotLifecycle(pool *opensandbox.DefaultSandboxPool) string {
	snap, err := pool.Snapshot(context.Background())
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return snap.LifecycleState.String()
}
