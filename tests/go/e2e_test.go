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
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func getServerURL() string {
	if u := os.Getenv("OPENSANDBOX_URL"); u != "" {
		return u
	}
	return "http://localhost:8080"
}

func getDefaultImage() string {
	if img := os.Getenv("OPENSANDBOX_SANDBOX_DEFAULT_IMAGE"); img != "" {
		return img
	}
	return "opensandbox/code-interpreter:latest"
}

func TestE2E_FullLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := opensandbox.NewLifecycleClient(getServerURL()+"/v1", "")

	// 1. List sandboxes
	list, err := client.ListSandboxes(ctx, opensandbox.ListOptions{Page: 1, PageSize: 10})
	require.NoError(t, err)
	t.Logf("Initial sandbox count: %d", list.Pagination.TotalItems)

	// 2. Create a sandbox
	sb, err := client.CreateSandbox(ctx, opensandbox.CreateSandboxRequest{
		Image: &opensandbox.ImageSpec{
			URI: getDefaultImage(),
		},
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		ResourceLimits: map[string]string{
			"cpu":    "500m",
			"memory": "256Mi",
		},
		Env: map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Metadata: map[string]string{
			"test": "go-e2e",
		},
	})
	require.NoError(t, err)
	t.Logf("Created sandbox: %s (state: %s)", sb.ID, sb.Status.State)

	require.NotEmpty(t, sb.ID)

	defer func() {
		t.Log("Cleaning up: deleting sandbox")
		_ = client.DeleteSandbox(context.Background(), sb.ID)
	}()

	// 3. Wait for Running state
	var running *opensandbox.SandboxInfo
	for i := 0; i < 30; i++ {
		running, err = client.GetSandbox(ctx, sb.ID)
		require.NoError(t, err)
		t.Logf("  Poll %d: state=%s", i+1, running.Status.State)
		if running.Status.State == opensandbox.StateRunning {
			break
		}
		if running.Status.State == opensandbox.StateFailed || running.Status.State == opensandbox.StateTerminated {
			require.FailNow(t, fmt.Sprintf("Sandbox entered terminal state: %s (reason: %s, message: %s)",
				running.Status.State, running.Status.Reason, running.Status.Message))
		}
		time.Sleep(2 * time.Second)
	}

	require.NotNil(t, running)
	require.Equal(t, opensandbox.StateRunning, running.Status.State, "sandbox did not reach Running state within timeout")
	t.Logf("Sandbox is Running: %s", running.ID)

	// 4. Get execd endpoint (default execd port: 44772)
	endpoint, err := client.GetEndpoint(ctx, sb.ID, 44772, nil)
	require.NoError(t, err)
	t.Logf("Execd endpoint: %s", endpoint.Endpoint)

	require.NotEmpty(t, endpoint.Endpoint)

	// 5. Test Execd — ping
	execdURL := endpoint.Endpoint
	if !strings.HasPrefix(execdURL, "http") {
		execdURL = "http://" + execdURL
	}
	execdURL = strings.Replace(execdURL, "host.docker.internal", "localhost", 1)
	t.Logf("Normalized execd URL: %s", execdURL)

	execToken := ""
	if endpoint.Headers != nil {
		execToken = endpoint.Headers["X-EXECD-ACCESS-TOKEN"]
	}
	execClient := opensandbox.NewExecdClient(execdURL, execToken)

	// This test bypasses the SDK's high-level CreateSandbox helper (which calls
	// WaitUntilReady) and pings execd directly through the server-side proxy.
	// The state-Running flag is satisfied as soon as the container is up, but
	// execd's HTTP routes may register a few ms later and the proxy can drop
	// the very first connection it sees ("connection reset by peer"). Poll
	// until ping succeeds — real users go through CreateSandbox which already
	// handles this.
	require.Eventually(t, func() bool {
		return execClient.Ping(ctx) == nil
	}, 30*time.Second, 500*time.Millisecond, "execd ping never succeeded")
	t.Log("Execd ping: OK")

	// 6. Test Execd — run a command with SSE streaming
	var output strings.Builder
	err = execClient.RunCommand(ctx, opensandbox.RunCommandRequest{
		Command: "echo hello-from-go-e2e && python3 --version",
	}, func(event opensandbox.StreamEvent) error {
		t.Logf("  SSE event: type=%s data=%s", event.Event, event.Data)
		output.WriteString(event.Data)
		return nil
	})
	require.NoError(t, err)

	// 7. Test Execd — file operations
	fileInfoMap, err := execClient.GetFileInfo(ctx, "/etc/os-release")
	require.NoError(t, err)
	for path, fi := range fileInfoMap {
		t.Logf("File info: path=%s size=%d", path, fi.Size)
	}

	// 8. Test Execd — metrics
	metrics, err := execClient.GetMetrics(ctx)
	require.NoError(t, err)
	t.Logf("Metrics: cpu_count=%.0f mem_total=%.0fMiB", metrics.CPUCount, metrics.MemTotalMB)

	// 9. Test Egress — get policy (if available, default egress port: 18080)
	egressEndpoint, err := client.GetEndpoint(ctx, sb.ID, 18080, nil)
	if err != nil {
		t.Logf("GetEndpoint(egress): %v (skipping egress tests)", err)
	} else {
		egressURL := egressEndpoint.Endpoint
		if !strings.HasPrefix(egressURL, "http") {
			egressURL = "http://" + egressURL
		}
		egressURL = strings.Replace(egressURL, "host.docker.internal", "localhost", 1)

		egressToken := ""
		if egressEndpoint.Headers != nil {
			egressToken = egressEndpoint.Headers["OPENSANDBOX-EGRESS-AUTH"]
		}
		egressClient := opensandbox.NewEgressClient(egressURL, egressToken)

		policy, err := egressClient.GetPolicy(ctx)
		if err != nil {
			t.Logf("GetPolicy: %v (egress sidecar might not be ready)", err)
		} else {
			t.Logf("Egress policy: mode=%s defaultAction=%s rules=%d",
				policy.Mode, policy.Policy.DefaultAction, len(policy.Policy.Egress))
		}
	}

	// 10. Delete sandbox
	err = client.DeleteSandbox(ctx, sb.ID)
	require.NoError(t, err)
	t.Log("Sandbox deleted successfully")

	// 11. Verify deletion
	deleted, err := client.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Logf("GetSandbox after delete: %v (expected)", err)
	} else {
		t.Logf("GetSandbox after delete: state=%s", deleted.Status.State)
	}

	fmt.Println("\n=== GO E2E TEST PASSED ===")
	fmt.Println("Lifecycle: create → poll → Running → execd ping → run command (SSE) → file info → metrics → egress → delete")
}

func TestE2E_PauseResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	config := opensandbox.ConnectionConfig{
		Domain:   strings.TrimPrefix(strings.TrimPrefix(getServerURL(), "http://"), "https://"),
		Protocol: "http",
		APIKey:   os.Getenv("OPENSANDBOX_API_KEY"),
	}

	// 1. Create sandbox via high-level API
	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:    getDefaultImage(),
		Env:      map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Metadata: map[string]string{"test": "go-e2e-pause-resume"},
	})
	require.NoError(t, err)
	t.Logf("Created sandbox: %s", sb.ID())
	defer func() { _ = sb.Kill(context.Background()) }()

	// 2. Verify sandbox is healthy
	require.True(t, sb.IsHealthy(ctx), "sandbox not healthy after creation")
	t.Log("Sandbox is healthy")

	// 3. Run a command before pause
	exec, err := sb.RunCommand(ctx, "echo before-pause", nil)
	require.NoError(t, err)
	t.Logf("Pre-pause output: %s", exec.Text())

	// 4. Pause
	require.NoError(t, sb.Pause(ctx))
	t.Log("Sandbox paused")

	// 5. Verify paused state
	info, err := sb.GetInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, opensandbox.StatePaused, info.Status.State)
	t.Logf("Confirmed state: %s", info.Status.State)

	// 6. Resume via package-level function
	resumed, err := opensandbox.ResumeSandbox(ctx, config, sb.ID())
	require.NoError(t, err)
	t.Log("Sandbox resumed")

	// 7. Verify resumed sandbox is healthy and functional
	require.True(t, resumed.IsHealthy(ctx), "sandbox not healthy after resume")

	exec2, err := resumed.RunCommand(ctx, "echo after-resume", nil)
	require.NoError(t, err)
	t.Logf("Post-resume output: %s", exec2.Text())

	// 8. Also test instance method Resume: pause again and resume via method
	require.NoError(t, resumed.Pause(ctx))
	t.Log("Sandbox paused again")

	resumed2, err := resumed.Resume(ctx)
	require.NoError(t, err)

	exec3, err := resumed2.RunCommand(ctx, "echo instance-resume", nil)
	require.NoError(t, err)
	t.Logf("Instance resume output: %s", exec3.Text())

	// Cleanup
	require.NoError(t, resumed2.Kill(ctx))
	t.Log("Sandbox killed — pause/resume e2e passed")
}

func TestE2E_ManualCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	config := opensandbox.ConnectionConfig{
		Domain:   strings.TrimPrefix(strings.TrimPrefix(getServerURL(), "http://"), "https://"),
		Protocol: "http",
		APIKey:   os.Getenv("OPENSANDBOX_API_KEY"),
	}

	// 1. Create sandbox with ManualCleanup (no auto-expiration)
	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:         getDefaultImage(),
		Env:           map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		ManualCleanup: true,
		Metadata:      map[string]string{"test": "go-e2e-manual-cleanup"},
	})
	require.NoError(t, err)
	t.Logf("Created sandbox: %s", sb.ID())
	defer func() { _ = sb.Kill(context.Background()) }()

	// 2. Verify sandbox has no expiration set
	info, err := sb.GetInfo(ctx)
	require.NoError(t, err)

	require.Nil(t, info.ExpiresAt, "ManualCleanup sandbox must omit ExpiresAt")
	if info.ExpiresAt == nil {
		t.Log("Confirmed: ExpiresAt is nil (no auto-expiration)")
	}

	// 3. Verify sandbox is functional
	exec, err := sb.RunCommand(ctx, "echo manual-cleanup-works", nil)
	require.NoError(t, err)
	t.Logf("Output: %s", exec.Text())

	// 4. Compare with a normal sandbox that should have an expiration
	sbWithTimeout, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:    getDefaultImage(),
		Env:      map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Metadata: map[string]string{"test": "go-e2e-with-timeout"},
	})
	require.NoError(t, err)
	defer func() { _ = sbWithTimeout.Kill(context.Background()) }()

	infoWithTimeout, err := sbWithTimeout.GetInfo(ctx)
	require.NoError(t, err)

	if infoWithTimeout.ExpiresAt == nil {
		t.Log("Warning: default sandbox also has nil ExpiresAt — server may not populate this field")
	} else {
		t.Logf("Default sandbox ExpiresAt: %v (confirms manual cleanup sandbox correctly omits it)", infoWithTimeout.ExpiresAt)
	}

	t.Log("Manual cleanup e2e passed")
}
