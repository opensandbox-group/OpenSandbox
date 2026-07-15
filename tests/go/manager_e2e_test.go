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

func TestManager_ListSandboxes(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := opensandbox.NewSandboxManager(config)
	defer mgr.Close()

	result, err := mgr.ListSandboxInfos(ctx, opensandbox.ListOptions{
		Page:     1,
		PageSize: 10,
	})
	require.NoError(t, err)

	t.Logf("Listed %d sandboxes (page %d/%d)",
		len(result.Items), result.Pagination.Page, result.Pagination.TotalPages)
}

func TestManager_ListByState(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Metadata: map[string]string{
			"test": "go-e2e-manager",
		},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	mgr := opensandbox.NewSandboxManager(config)
	defer mgr.Close()

	result, err := mgr.ListSandboxInfos(ctx, opensandbox.ListOptions{
		States: []opensandbox.SandboxState{opensandbox.StateRunning},
	})
	require.NoError(t, err)

	require.NotEmpty(t, result.Items, "expected at least one Running sandbox")

	for _, item := range result.Items {
		require.Equal(t, opensandbox.StateRunning, item.Status.State, "sandbox %s", item.ID)
	}
	t.Logf("Found %d Running sandboxes", len(result.Items))
}

func TestManager_GetAndKill(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	team := "t1"
	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:    getSandboxImage(),
		Env:      map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Metadata: map[string]string{"team": team, "env": "prod"},
	})
	require.NoError(t, err)

	mgr := opensandbox.NewSandboxManager(config)
	defer mgr.Close()

	info, err := mgr.GetSandboxInfo(ctx, sb.ID())
	require.NoError(t, err)
	require.Equal(t, sb.ID(), info.ID)
	t.Logf("Got sandbox %s via manager (state=%s)", info.ID, info.Status.State)

	stage := "stage"
	patched, err := mgr.PatchSandboxMetadata(ctx, sb.ID(), opensandbox.MetadataPatch{
		"env":  &stage,
		"team": nil,
	})
	require.NoError(t, err)
	require.Equal(t, "stage", patched.Metadata["env"])
	require.NotContains(t, patched.Metadata, "team")

	refreshed, err := mgr.GetSandboxInfo(ctx, sb.ID())
	require.NoError(t, err)
	require.Equal(t, "stage", refreshed.Metadata["env"])
	require.NotContains(t, refreshed.Metadata, "team")

	require.NoError(t, mgr.KillSandbox(ctx, sb.ID()))
	t.Log("Killed sandbox via manager")
}

func TestManager_PauseAndResume(t *testing.T) {
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

	mgr := opensandbox.NewSandboxManager(config)
	defer mgr.Close()

	if err := mgr.PauseSandbox(ctx, sb.ID()); err != nil {
		t.Skipf("PauseSandbox not supported in this environment: %v", err)
	}

	paused := false
	for i := 0; i < 20; i++ {
		info, infoErr := mgr.GetSandboxInfo(ctx, sb.ID())
		require.NoError(t, infoErr)
		if info.Status.State == opensandbox.StatePaused {
			paused = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, paused, "sandbox did not reach Paused state")

	require.NoError(t, mgr.ResumeSandbox(ctx, sb.ID()))

	resumed := false
	for i := 0; i < 20; i++ {
		info, infoErr := mgr.GetSandboxInfo(ctx, sb.ID())
		require.NoError(t, infoErr)
		if info.Status.State == opensandbox.StateRunning {
			resumed = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, resumed, "sandbox did not return to Running state")
}

func TestManager_RenewSandbox(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	mgr := opensandbox.NewSandboxManager(config)
	defer mgr.Close()

	resp, err := mgr.RenewSandbox(ctx, sb.ID(), 30*time.Minute)
	if err != nil {
		t.Skipf("RenewSandbox not supported in this environment: %v", err)
	}
	require.NotNil(t, resp)
	require.False(t, resp.ExpiresAt.IsZero(), "renew response should include expiresAt")
}
