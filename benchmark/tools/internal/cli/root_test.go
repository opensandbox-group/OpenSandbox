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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/provisioner"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
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

type injectedFile struct {
	data []byte
	mode int64
}

type mockRemoteRunner struct {
	ensured       bool
	deleted       bool
	deleteCtxErr  error
	execCancel    context.CancelFunc
	injectedBin   string
	injectedFiles map[string]injectedFile
	workerArgs    []string
	pulledDir     string
}

func (m *mockRemoteRunner) EnsureRunnerPod(_ context.Context, _ *profile.BenchmarkProfile) (string, error) {
	m.ensured = true
	return runner.DefaultRunnerPodName + "-test", nil
}

func (m *mockRemoteRunner) DeleteRunnerPod(ctx context.Context, _, _ string) error {
	m.deleted = true
	m.deleteCtxErr = ctx.Err()
	return nil
}

func (m *mockRemoteRunner) DetectClusterArch(_ context.Context, _, _ string) (string, error) {
	return "amd64", nil
}

func (m *mockRemoteRunner) InjectBinary(_ context.Context, _, _, localBinaryPath, _ string) error {
	m.injectedBin = localBinaryPath
	return nil
}

func (m *mockRemoteRunner) InjectBytes(_ context.Context, _, _ string, data []byte, remotePath string, mode int64) error {
	if m.injectedFiles == nil {
		m.injectedFiles = make(map[string]injectedFile)
	}
	m.injectedFiles[remotePath] = injectedFile{
		data: append([]byte(nil), data...),
		mode: mode,
	}
	return nil
}

func (m *mockRemoteRunner) ExecWorker(_ context.Context, _, _, _ string, args []string, stdout, _ io.Writer) error {
	m.workerArgs = append([]string(nil), args...)
	if m.execCancel != nil {
		m.execCancel()
		return context.Canceled
	}
	_, _ = io.WriteString(stdout, "worker executed\n")
	return nil
}

func (m *mockRemoteRunner) PullBundle(_ context.Context, _, _, _, localOutputDir string) error {
	m.pulledDir = localOutputDir
	return nil
}

func TestCLIRunOrchestratesRemoteRunnerAndProtectsAPIKey(t *testing.T) {
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
		"--api-key", "super-secret-token",
		"--run-id", "run-123",
	})
	require.NoError(t, cmd.Execute())

	require.True(t, mock.ensured)
	require.True(t, mock.deleted)
	require.Equal(t, binPath, mock.injectedBin)
	require.Contains(t, string(mock.injectedFiles[runner.DefaultRemoteProfilePath].data), "fast-sandbox-overlaybd-ref")
	require.Equal(t, []byte("super-secret-token"), mock.injectedFiles[runner.DefaultRemoteAPIKeyPath].data)
	require.Equal(t, int64(0o600), mock.injectedFiles[runner.DefaultRemoteAPIKeyPath].mode)
	require.Contains(t, mock.workerArgs, "--api-key-file")
	require.Contains(t, mock.workerArgs, runner.DefaultRemoteAPIKeyPath)
	require.NotContains(t, mock.workerArgs, "super-secret-token")
	require.Contains(t, mock.workerArgs, "--server")
	require.Contains(t, mock.workerArgs, "opensandbox-server.opensandbox-system.svc:8080")
	require.Contains(t, mock.workerArgs, "schedule-burst")
	require.Contains(t, mock.workerArgs, "run-123")
	require.Equal(t, outDir, mock.pulledDir)
	require.Contains(t, stdout.String(), "worker executed")
}

func TestCLIRunUsesLiveContextForCleanupAndDerivesServerNamespace(t *testing.T) {
	raw, err := profile.EmbeddedRaw(profile.DefaultProfileName)
	require.NoError(t, err)
	customYAML := bytes.ReplaceAll(raw, []byte("systemNamespace: opensandbox-system"), []byte("systemNamespace: custom-osb-sys"))
	profFile := filepath.Join(t.TempDir(), "custom-profile.yaml")
	require.NoError(t, os.WriteFile(profFile, customYAML, 0o644))

	binPath := filepath.Join(t.TempDir(), "worker-linux")
	require.NoError(t, os.WriteFile(binPath, []byte("fake"), 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	mock := &mockRemoteRunner{execCancel: cancel}
	app := NewDefaultApp()
	app.NewRemoteRunner = func(_ string) (runner.RemoteRunner, *runner.KubeClients, error) {
		return mock, nil, nil
	}

	cmd := app.RootCmd()
	cmd.SetArgs([]string{"run", "--profile", profFile, "--suite", "schedule-burst", "--worker-binary", binPath})
	err = cmd.ExecuteContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, mock.deleted)
	require.NoError(t, mock.deleteCtxErr, "deferred runner cleanup must receive a live context")
	require.Contains(t, mock.workerArgs, "opensandbox-server.custom-osb-sys.svc:8080")
}

type failingUpProvisioner struct {
	downCalled bool
}

func (p *failingUpProvisioner) Name() string { return profile.RuntimeFastSandboxOverlayBD }
func (p *failingUpProvisioner) Preflight(_ context.Context, _ *profile.BenchmarkProfile, _ provisioner.Options) (*report.EnvironmentFingerprint, error) {
	return &report.EnvironmentFingerprint{}, nil
}
func (p *failingUpProvisioner) Up(_ context.Context, _ *profile.BenchmarkProfile, _ provisioner.Options) error {
	return errors.New("partial deploy failure")
}
func (p *failingUpProvisioner) Down(_ context.Context, _ *profile.BenchmarkProfile, _ provisioner.Options) (*report.LeakAudit, error) {
	p.downCalled = true
	return nil, errors.New("down cleanup warning")
}

func TestCLIRunTearsDownOnPartialProvisionFailure(t *testing.T) {
	prov := &failingUpProvisioner{}
	reg := provisioner.NewRegistry()
	reg.MustRegister(prov)

	app := NewDefaultApp()
	app.Provisioners = reg
	app.NewRemoteRunner = func(_ string) (runner.RemoteRunner, *runner.KubeClients, error) {
		return &mockRemoteRunner{}, nil, nil
	}

	cmd := app.RootCmd()
	cmd.SetArgs([]string{"run", "--suite", "schedule-burst", "--provision", "--teardown"})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "partial deploy failure")
	require.True(t, prov.downCalled, "Down must run even when Up fails midway")
}
