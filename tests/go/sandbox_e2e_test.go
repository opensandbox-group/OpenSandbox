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
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func TestSandbox_CreateAndKill(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:      getSandboxImage(),
		Env:        map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		ResourceLimits: opensandbox.ResourceLimits{
			"cpu":    "500m",
			"memory": "256Mi",
		},
		Metadata: map[string]string{
			"test": "go-e2e-create",
		},
	})
	require.NoError(t, err)
	t.Logf("Created sandbox: %s", sb.ID())

	defer func() {
		if err := sb.Kill(context.Background()); err != nil {
			t.Logf("Kill cleanup: %v", err)
		}
	}()

	require.True(t, sb.IsHealthy(ctx), "sandbox should be healthy after creation")

	info, err := sb.GetInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, sb.ID(), info.ID)
	require.Equal(t, opensandbox.StateRunning, info.Status.State)
	t.Logf("Info: state=%s, created=%s", info.Status.State, info.CreatedAt)

	metrics, err := sb.GetMetrics(ctx)
	require.NoError(t, err)
	require.NotZero(t, metrics.CPUCount, "expected non-zero CPU count")
	t.Logf("Metrics: cpu=%.0f, mem=%.0fMiB", metrics.CPUCount, metrics.MemTotalMB)

	require.NoError(t, sb.Kill(ctx))
	t.Log("Sandbox killed successfully")
}

func TestSandbox_Renew(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	_, err := sb.Renew(ctx, 30*time.Minute)
	if err != nil {
		t.Logf("Renew: %v (may not be supported)", err)
	} else {
		t.Log("Renewed expiration: +30m")
	}
}

func TestSandbox_GetEndpoint(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	endpoint, err := sb.GetEndpoint(ctx, opensandbox.DefaultExecdPort)
	require.NoError(t, err)
	require.NotEmpty(t, endpoint.Endpoint)
	t.Logf("Endpoint: %s", endpoint.Endpoint)
}

func TestSandbox_ConnectToExisting(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb1, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	defer sb1.Kill(context.Background())

	sb2, err := opensandbox.ConnectSandbox(ctx, config, sb1.ID(), opensandbox.ReadyOptions{})
	require.NoError(t, err)

	require.Equal(t, sb1.ID(), sb2.ID())

	exec, err := sb2.RunCommand(ctx, "echo connected", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "connected")
	t.Log("ConnectSandbox works")
}

func TestSandbox_Session(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	session, err := sb.CreateSession(ctx)
	require.NoError(t, err)
	t.Logf("Created session: %s", session.ID)

	sb.RunInSession(ctx, session.ID, opensandbox.RunInSessionRequest{
		Command: "export MY_VAR=hello_session",
	}, nil)

	exec, err := sb.RunInSession(ctx, session.ID, opensandbox.RunInSessionRequest{
		Command: "echo $MY_VAR",
	}, nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "hello_session")
	t.Log("Session state persists across commands")

	err = sb.DeleteSession(ctx, session.ID)
	require.NoError(t, err)
	t.Log("Session deleted")
}

func TestSandbox_ManualCleanup(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	info, err := sb.GetInfo(ctx)
	require.NoError(t, err)
	t.Logf("Sandbox %s created (expiresAt=%v)", info.ID, info.ExpiresAt)
}

func TestSandbox_NetworkPolicyCreate(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		NetworkPolicy: &opensandbox.NetworkPolicy{
			DefaultAction: "deny",
			Egress: []opensandbox.NetworkRule{
				{Action: "allow", Target: "pypi.org"},
				{Action: "allow", Target: "*.python.org"},
			},
		},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	require.True(t, sb.IsHealthy(ctx), "sandbox with network policy should be healthy")
	t.Log("Sandbox created with deny-default network policy + 2 allow rules")
}

func TestSandbox_PauseAndResume(t *testing.T) {
	t.Skip("skip pause/resume e2e test")
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	err = sb.Pause(ctx)
	if err != nil {
		t.Logf("Pause: %v (may not be supported by runtime)", err)
		t.Skip("Pause not supported")
	}
	t.Log("Pause requested")

	reachedPaused := false
	for i := 0; i < 30; i++ {
		info, err := sb.GetInfo(ctx)
		require.NoError(t, err)
		t.Logf("  Poll %d: state=%s", i+1, info.Status.State)
		if info.Status.State == opensandbox.StatePaused {
			t.Log("Sandbox is Paused")
			reachedPaused = true
			break
		}
		if info.Status.State == opensandbox.StateFailed {
			require.FailNowf(t, "sandbox failed: %s", info.Status.Reason)
		}
		time.Sleep(2 * time.Second)
	}
	require.True(t, reachedPaused, "sandbox did not reach Paused state within timeout")

	mgr := opensandbox.NewSandboxManager(config)
	err = mgr.ResumeSandbox(ctx, sb.ID())
	require.NoError(t, err)
	t.Log("Resume requested")

	for i := 0; i < 30; i++ {
		info, err := sb.GetInfo(ctx)
		require.NoError(t, err)
		t.Logf("  Poll %d: state=%s", i+1, info.Status.State)
		if info.Status.State == opensandbox.StateRunning {
			t.Log("Sandbox is Running again after resume")
			return
		}
		time.Sleep(2 * time.Second)
	}
	require.FailNow(t, "sandbox did not resume to Running state")
}
