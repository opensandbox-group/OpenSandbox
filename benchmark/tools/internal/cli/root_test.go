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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/collector"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/provisioner"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/suite"
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
	require.Contains(t, mock.workerArgs, "opensandbox-server.opensandbox-system.svc:80")
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
	require.Contains(t, mock.workerArgs, "opensandbox-server.custom-osb-sys.svc:80")
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

type stubWorkerSuite struct {
	name      string
	result    *report.SuiteResult
	runErr    error
	sawHooks  bool
	sawNodeMB int
}

func (s *stubWorkerSuite) Name() string        { return s.name }
func (s *stubWorkerSuite) Description() string { return "stub suite for worker test" }
func (s *stubWorkerSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}
func (s *stubWorkerSuite) Run(ctx context.Context, _ suite.RunContext) (*report.SuiteResult, []suite.OperationSample, error) {
	hooks := suite.BenchHooksFromContext(ctx)
	s.sawHooks = hooks.CacheEvictor != nil && hooks.NodeIsolator != nil && hooks.WakeResumeCounter != nil
	s.sawNodeMB = hooks.SoakNodeMemoryMiB
	return s.result, nil, s.runErr
}

func TestDefaultRegistryRegistersRealSuites(t *testing.T) {
	reg := suite.DefaultRegistry()
	expected := map[string]suite.Suite{
		"schedule-burst":      suite.NewScheduleBurstSuite(),
		"steady-churn":        suite.NewSteadyChurnSuite(),
		"pause-resume":        suite.NewPauseResumeSuite(),
		"rootfs-only-restore": suite.NewRootfsOnlyRestoreSuite(),
		"wake-storm":          suite.NewWakeStormSuite(),
		"high-density-soak":   suite.NewHighDensitySoakSuite(),
	}
	require.Len(t, reg.Names(), len(expected))
	for name, proto := range expected {
		s, ok := reg.Get(name)
		require.True(t, ok, "missing suite %q", name)
		require.IsType(t, proto, s)
	}
}

func TestCLIWorkerRunWritesBundleAndPreservesPartialDiagnostics(t *testing.T) {
	t.Run("writes report.json and summary.md with collector hooks wired", func(t *testing.T) {
		outDir := t.TempDir()
		stub := &stubWorkerSuite{
			name: "schedule-burst",
			result: &report.SuiteResult{
				Name:            "schedule-burst",
				Status:          "passed",
				StartedAt:       time.Now().UTC(),
				DurationSeconds: 1.5,
				ThroughputRPS:   10,
				TotalOps:        15,
				SucceededOps:    15,
				Latencies: map[string]report.LatencyDist{
					"t_first_ping": {Count: 15, P50Ms: 120, P95Ms: 220, P99Ms: 310},
				},
			},
		}
		reg := suite.NewRegistry()
		reg.MustRegister(stub)

		fakeCS := fake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "fc-node-1",
				Labels: map[string]string{corev1.LabelArchStable: "amd64", "fast-sandbox.io/firecracker-node": "true"},
			},
			Status: corev1.NodeStatus{
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("32"),
					corev1.ResourceMemory: resource.MustParse("64Gi"),
				},
				Allocatable: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("60Gi"),
				},
			},
		})

		app := NewDefaultApp()
		app.Suites = reg
		app.NewCollectorClients = func() (*collector.Clients, error) {
			return &collector.Clients{Clientset: fakeCS}, nil
		}

		cmd := app.RootCmd()
		cmd.SetArgs([]string{
			"worker", "run",
			"--suite", "schedule-burst",
			"--output", outDir,
			"--run-id", "worker-test-1",
			"--opensandbox-commit", "abc123",
			"--opensandbox-tag", "v1.3.0",
			"--fast-sandbox-commit", "def456",
		})
		require.NoError(t, cmd.Execute())
		require.True(t, stub.sawHooks, "expected cluster BenchHooks to be attached to suite context")
		require.Equal(t, 60*1024, stub.sawNodeMB)

		rep, err := report.Load(filepath.Join(outDir, "report.json"))
		require.NoError(t, err)
		require.Equal(t, report.SchemaVersion, rep.SchemaVersion)
		require.Equal(t, "worker-test-1", rep.Provenance.RunID)
		require.Equal(t, "abc123", rep.Provenance.OpenSandboxCommit)
		require.Equal(t, "def456", rep.Provenance.FastSandboxCommit)
		require.Equal(t, "amd64", rep.Environment.ClusterArch)
		require.Len(t, rep.Environment.Nodes, 1)
		require.Contains(t, rep.Suites, "schedule-burst")
		require.NotNil(t, rep.LeakAudit)
		require.True(t, rep.LeakAudit.Passed)

		summaryBytes, err := os.ReadFile(filepath.Join(outDir, "summary.md"))
		require.NoError(t, err)
		require.Contains(t, string(summaryBytes), "schedule-burst")
		require.Contains(t, string(summaryBytes), "Leak & Stability Audit: PASS")
	})

	t.Run("writes bundle even when a suite fails in SDK-only mode", func(t *testing.T) {
		outDir := t.TempDir()
		stub := &stubWorkerSuite{
			name:   "schedule-burst",
			runErr: errors.New("simulated suite failure"),
		}
		reg := suite.NewRegistry()
		reg.MustRegister(stub)

		app := NewDefaultApp()
		app.Suites = reg
		app.NewCollectorClients = func() (*collector.Clients, error) {
			return nil, errors.New("not running in-cluster")
		}

		cmd := app.RootCmd()
		cmd.SetArgs([]string{
			"worker", "run",
			"--suite", "schedule-burst",
			"--output", outDir,
			"--run-id", "worker-fail-1",
		})
		err := cmd.Execute()
		require.Error(t, err)
		require.Contains(t, err.Error(), "simulated suite failure")

		rep, loadErr := report.Load(filepath.Join(outDir, "report.json"))
		require.NoError(t, loadErr)
		require.Equal(t, "failed", rep.Suites["schedule-burst"].Status)
		_, statErr := os.Stat(filepath.Join(outDir, "summary.md"))
		require.NoError(t, statErr)
	})
}

func TestProvenanceAndRenderHelpers(t *testing.T) {
	tmp := t.TempDir()
	commitDir := filepath.Join(tmp, "manifests", "third-party")
	require.NoError(t, os.MkdirAll(commitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(commitDir, "fast-sandbox.commit"), []byte("# comment\nrepo: https://example.com\ncommit: 5b96ba7f71e019bcce1c00fc24ddeb2b7a32ce74\n"), 0o644))
	prov := DetectLocalProvenance(tmp)
	require.Equal(t, "5b96ba7f71e019bcce1c00fc24ddeb2b7a32ce74", prov.FastSandboxCommit)

	env := &report.EnvironmentFingerprint{
		KubernetesVersion: "v1.31.0",
		ClusterArch:       "amd64",
		PoolMode:          "ResourceAware",
		Nodes: []report.NodeInfo{
			{Name: "node-1", KVMReady: true, UblkReady: true, StateRootFS: "xfs", ReflinkSupported: true, SpillDirAvailable: true, CPUIdentity: "6:143:8"},
		},
	}
	var textBuf bytes.Buffer
	require.NoError(t, renderPreflightSummary(&textBuf, env, false))
	require.Contains(t, textBuf.String(), "Preflight PASSED")
	require.Contains(t, textBuf.String(), "CPU homogeneity: homogeneous")

	var jsonBuf bytes.Buffer
	require.NoError(t, renderPreflightSummary(&jsonBuf, env, true))
	var decoded report.EnvironmentFingerprint
	require.NoError(t, json.Unmarshal(jsonBuf.Bytes(), &decoded))
	require.Equal(t, "v1.31.0", decoded.KubernetesVersion)

	var auditBuf bytes.Buffer
	renderLeakAuditSummary(&auditBuf, &report.LeakAudit{
		Passed:   false,
		Failures: []string{"1 orphaned ublk device"},
	})
	require.Contains(t, auditBuf.String(), "Leak audit: FAILED")
	require.Contains(t, auditBuf.String(), "1 orphaned ublk device")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnsureWorkerTemplateCreatesAndPollsWhenCatalogEmpty(t *testing.T) {
	prof, err := profile.LoadEmbedded(profile.DefaultProfileName)
	require.NoError(t, err)

	var createdReq opensandbox.CreateTemplateRequest
	getCalls := 0
	mockClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/v1/templates":
				return jsonHTTPResp(http.StatusOK, opensandbox.ListTemplatesResponse{}), nil
			case req.Method == http.MethodPost && req.URL.Path == "/v1/templates":
				require.NoError(t, json.NewDecoder(req.Body).Decode(&createdReq))
				return jsonHTTPResp(http.StatusAccepted, opensandbox.TemplateInfo{
					TemplateID: "tpl-bench-1",
					Image:      createdReq.Image,
					Publish:    createdReq.Publish,
					Format:     opensandbox.TemplateFormatOverlayBD,
					Status:     opensandbox.TemplateStatus{Phase: opensandbox.TemplatePhasePending},
				}), nil
			case req.Method == http.MethodGet && req.URL.Path == "/v1/templates/tpl-bench-1":
				getCalls++
				phase := opensandbox.TemplatePhaseBuilding
				if getCalls >= 2 {
					phase = opensandbox.TemplatePhaseSucceeded
				}
				return jsonHTTPResp(http.StatusOK, opensandbox.TemplateInfo{
					TemplateID: "tpl-bench-1",
					Format:     opensandbox.TemplateFormatOverlayBD,
					Status:     opensandbox.TemplateStatus{Phase: phase, ManifestRef: "oci://opensandbox-bench-registry.opensandbox-bench.svc:5000/opensandbox/templates@sha256:abc"},
				}), nil
			default:
				return jsonHTTPResp(http.StatusNotFound, map[string]string{"code": "NOT_FOUND"}), nil
			}
		}),
	}

	tplID, err := ensureWorkerTemplate(context.Background(), prof, opensandbox.ConnectionConfig{
		Domain:     "opensandbox-server.opensandbox-system.svc:80",
		Protocol:   "http",
		HTTPClient: mockClient,
	}, nil, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "tpl-bench-1", tplID)
	require.Equal(t, prof.Stack.Template.BaseImage, createdReq.Image)
	require.Equal(t, opensandbox.TemplateFormatOverlayBD, createdReq.Format)
	require.Equal(t, "oci://opensandbox-bench-registry.opensandbox-bench.svc:5000/opensandbox/templates", createdReq.Publish)
	require.GreaterOrEqual(t, getCalls, 2)
}

func jsonHTTPResp(status int, v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}
}
