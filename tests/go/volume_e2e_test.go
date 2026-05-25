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
	"strings"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func getHostVolumeDir() string {
	if v := os.Getenv("OPENSANDBOX_TEST_HOST_VOLUME_DIR"); v != "" {
		return v
	}
	return "/tmp/opensandbox-e2e/host-volume-test"
}

func getPVCName() string {
	if v := os.Getenv("OPENSANDBOX_TEST_PVC_NAME"); v != "" {
		return v
	}
	return "opensandbox-e2e-pvc-test"
}

func TestVolume_HostMount(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hostDir := getHostVolumeDir()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:        getSandboxImage(),
		Env:          map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		ReadyTimeout: 60 * time.Second,
		Volumes: []opensandbox.Volume{
			{
				Name:      "test-host-vol",
				Host:      &opensandbox.Host{Path: hostDir},
				MountPath: "/mnt/host-data",
			},
		},
	})
	if err != nil {
		t.Logf("CreateSandbox with host volume: %v (host volumes may not be allowed)", err)
		t.Skip("Host volume mount not supported in this environment")
	}
	defer sb.Kill(context.Background())

	// Retry: execd SSE endpoint may return empty stream before fully ready
	var exec *opensandbox.Execution
	for attempt := 0; attempt < 3; attempt++ {
		exec, err = sb.RunCommand(ctx, `echo "host-mount-test" > /mnt/host-data/go-e2e.txt`, nil)
		if err == nil || !strings.Contains(err.Error(), "empty sse stream") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	if exec.ExitCode != nil {
		require.Equal(t, 0, *exec.ExitCode, "write exit code")
	}

	for attempt := 0; attempt < 3; attempt++ {
		exec, err = sb.RunCommand(ctx, "cat /mnt/host-data/go-e2e.txt", nil)
		if err == nil || !strings.Contains(err.Error(), "empty sse stream") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "host-mount-test")
	t.Log("Host volume mount read/write passed")
}

func TestVolume_HostMountReadOnly(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hostDir := getHostVolumeDir()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Volumes: []opensandbox.Volume{
			{
				Name:      "test-host-ro",
				Host:      &opensandbox.Host{Path: hostDir},
				MountPath: "/mnt/host-ro",
				ReadOnly:  true,
			},
		},
	})
	if err != nil {
		t.Logf("CreateSandbox with ro host volume: %v", err)
		t.Skip("Host volume mount not supported")
	}
	defer sb.Kill(context.Background())

	exec, err := sb.RunCommand(ctx, `echo "should-fail" > /mnt/host-ro/fail.txt 2>&1; echo "EXIT_CODE=$?"`, nil)
	require.NoError(t, err)
	output := exec.Text()
	require.NotContains(t, output, "EXIT_CODE=0", "write to read-only mount unexpectedly succeeded (exit code 0)")
	hasROError := strings.Contains(output, "Read-only") || strings.Contains(output, "read-only") ||
		strings.Contains(output, "Permission denied") || strings.Contains(output, "EXIT_CODE=1")
	require.True(t, hasROError, "expected read-only / permission denied / non-zero exit, got: %q", output)
	t.Log("Host volume read-only mount verified")
}

func TestVolume_PVCMount(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pvcName := getPVCName()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env:   map[string]string{"EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
		Volumes: []opensandbox.Volume{
			{
				Name:      "test-pvc-vol",
				PVC:       &opensandbox.PVC{ClaimName: pvcName},
				MountPath: "/mnt/pvc-data",
			},
		},
	})
	if err != nil {
		t.Logf("CreateSandbox with PVC: %v (PVC %s may not exist)", err, pvcName)
		t.Skip("PVC volume mount not available")
	}
	defer sb.Kill(context.Background())

	// Retry: execd SSE endpoint may return empty stream before fully ready
	var exec *opensandbox.Execution
	for attempt := 0; attempt < 3; attempt++ {
		exec, err = sb.RunCommand(ctx, `echo "pvc-test-data" > /mnt/pvc-data/go-e2e.txt`, nil)
		if err == nil || !strings.Contains(err.Error(), "empty sse stream") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	if exec.ExitCode != nil {
		require.Equal(t, 0, *exec.ExitCode, "write exit code")
	}

	for attempt := 0; attempt < 3; attempt++ {
		exec, err = sb.RunCommand(ctx, "cat /mnt/pvc-data/go-e2e.txt", nil)
		if err == nil || !strings.Contains(err.Error(), "empty sse stream") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "pvc-test-data")
	t.Log("PVC volume mount read/write passed")
}
