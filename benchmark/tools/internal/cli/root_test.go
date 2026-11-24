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

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func TestCLIProfileValidateAndSuites(t *testing.T) {
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"profile", "validate"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "fast-sandbox-overlaybd-ref")
	require.Contains(t, out.String(), "is valid")

	out.Reset()
	cmd = NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"suites"})
	require.NoError(t, cmd.Execute())
	for _, expected := range []string{
		"schedule-burst",
		"steady-churn",
		"pause-resume",
		"rootfs-only-restore",
		"wake-storm",
		"high-density-soak",
	} {
		require.Contains(t, out.String(), expected)
	}
}

type mockRemoteRunner struct {
	ensured       bool
	deleted       bool
	injectedBin   string
	injectedBytes []byte
	workerArgs    []string
	pulledDir     string
}

func (m *mockRemoteRunner) EnsureRunnerPod(_ context.Context, _ *profile.BenchmarkProfile) (string, error) {
	m.ensured = true
	return runner.DefaultRunnerPodName, nil
}

func (m *mockRemoteRunner) DeleteRunnerPod(_ context.Context, _, _ string) error {
	m.deleted = true
	return nil
}

func (m *mockRemoteRunner) DetectClusterArch(_ context.Context, _, _ string) (string, error) {
	return "amd64", nil
}

func (m *mockRemoteRunner) InjectBinary(_ context.Context, _, _, localBinaryPath, _ string) error {
	m.injectedBin = localBinaryPath
	return nil
}

func (m *mockRemoteRunner) InjectBytes(_ context.Context, _, _ string, data []byte, _ string, _ int64) error {
	m.injectedBytes = append([]byte(nil), data...)
	return nil
}

func (m *mockRemoteRunner) ExecWorker(_ context.Context, _, _, _ string, args []string, stdout, _ io.Writer) error {
	m.workerArgs = append([]string(nil), args...)
	_, _ = io.WriteString(stdout, "worker executed\n")
	return nil
}

func (m *mockRemoteRunner) PullBundle(_ context.Context, _, _, _, localOutputDir string) error {
	m.pulledDir = localOutputDir
	return nil
}

func TestCLIRunOrchestratesRemoteRunner(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "worker-linux")
	require.NoError(t, os.WriteFile(binPath, []byte("fake"), 0o755))
	outDir := filepath.Join(t.TempDir(), "bundle")

	mock := &mockRemoteRunner{}
	app := NewDefaultApp()
	app.NewRemoteRunner = func(_ string) (runner.RemoteRunner, *runner.KubeClients, error) {
		return mock, nil, nil
	}

	var stdout bytes.Buffer
	cmd := app.RootCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"run",
		"--suite", "schedule-burst",
		"--worker-binary", binPath,
		"--output", outDir,
		"--run-id", "run-123",
	})
	require.NoError(t, cmd.Execute())

	require.True(t, mock.ensured)
	require.True(t, mock.deleted)
	require.Equal(t, binPath, mock.injectedBin)
	require.Contains(t, string(mock.injectedBytes), "fast-sandbox-overlaybd-ref")
	require.Contains(t, mock.workerArgs, "schedule-burst")
	require.Contains(t, mock.workerArgs, "run-123")
	require.Equal(t, outDir, mock.pulledDir)
	require.Contains(t, stdout.String(), "worker executed")
}
