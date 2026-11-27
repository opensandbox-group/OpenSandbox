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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
		"density-ramp",
		"dataplane-microbench",
		"noisy-neighbor",
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
	pullCtxErr    error
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

func (m *mockRemoteRunner) PullBundle(ctx context.Context, _, _, _, localOutputDir string) error {
	m.pulledDir = localOutputDir
	m.pullCtxErr = ctx.Err()
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

	outDir := filepath.Join(t.TempDir(), "partial-bundle")
	cmd := app.RootCmd()
	cmd.SetArgs([]string{"run", "--profile", profFile, "--suite", "schedule-burst", "--worker-binary", binPath, "--output", outDir})
	err = cmd.ExecuteContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, mock.deleted)
	require.NoError(t, mock.deleteCtxErr, "deferred runner cleanup must receive a live context")
	require.Equal(t, outDir, mock.pulledDir)
	require.NoError(t, mock.pullCtxErr, "PullBundle must receive an independent live context on cancellation")
	require.Contains(t, mock.workerArgs, "opensandbox-server.custom-osb-sys.svc:80")
}

type leakingDownProvisioner struct {
	downCalled bool
}

func (p *leakingDownProvisioner) Name() string { return profile.RuntimeFastSandboxOverlayBD }
func (p *leakingDownProvisioner) Preflight(_ context.Context, _ *profile.BenchmarkProfile, _ provisioner.Options) (*report.EnvironmentFingerprint, error) {
	return &report.EnvironmentFingerprint{}, nil
}
func (p *leakingDownProvisioner) Up(_ context.Context, _ *profile.BenchmarkProfile, _ provisioner.Options) error {
	return nil
}
func (p *leakingDownProvisioner) Down(_ context.Context, _ *profile.BenchmarkProfile, _ provisioner.Options) (*report.LeakAudit, error) {
	p.downCalled = true
	return &report.LeakAudit{
		Passed:             false,
		ResidualSandboxCRs: 1,
		Failures:           []string{"1 residual Sandbox CR"},
	}, nil
}

func TestCLIRunTeardownFailsOnLeakAuditFailure(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "worker-linux")
	require.NoError(t, os.WriteFile(binPath, []byte("fake"), 0o755))

	prov := &leakingDownProvisioner{}
	reg := provisioner.NewRegistry()
	reg.MustRegister(prov)

	app := NewDefaultApp()
	app.Provisioners = reg
	app.NewRemoteRunner = func(_ string) (runner.RemoteRunner, *runner.KubeClients, error) {
		return &mockRemoteRunner{}, nil, nil
	}

	var stdout bytes.Buffer
	cmd := app.RootCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"run", "--suite", "schedule-burst", "--worker-binary", binPath, "--teardown"})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "post-teardown leak audit failed")
	require.True(t, prov.downCalled)
	require.Contains(t, stdout.String(), "Leak audit: FAILED")
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
		"high-density-soak":    suite.NewHighDensitySoakSuite(),
		"density-ramp":         suite.NewDensityRampSuite(),
		"dataplane-microbench": suite.NewDataplaneMicrobenchSuite(),
		"noisy-neighbor":       suite.NewNoisyNeighborSuite(),
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

	t.Run("exits non-zero when suite status is failed with nil runErr while still writing bundle", func(t *testing.T) {
		outDir := t.TempDir()
		stub := &stubWorkerSuite{
			name: "schedule-burst",
			result: &report.SuiteResult{
				Name:         "schedule-burst",
				Status:       "failed",
				TotalOps:     10,
				SucceededOps: 9,
				FailedOps:    1,
			},
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
			"--run-id", "worker-status-fail",
		})
		err := cmd.Execute()
		require.Error(t, err)
		require.Contains(t, err.Error(), `suite "schedule-burst" failed`)

		rep, loadErr := report.Load(filepath.Join(outDir, "report.json"))
		require.NoError(t, loadErr)
		require.Equal(t, "failed", rep.Suites["schedule-burst"].Status)
		_, statErr := os.Stat(filepath.Join(outDir, "summary.md"))
		require.NoError(t, statErr)
	})

	t.Run("exits non-zero when post-run leak audit fails while still writing bundle", func(t *testing.T) {
		outDir := t.TempDir()
		stub := &stubWorkerSuite{
			name: "schedule-burst",
			result: &report.SuiteResult{
				Name:         "schedule-burst",
				Status:       "passed",
				TotalOps:     5,
				SucceededOps: 5,
			},
		}
		reg := suite.NewRegistry()
		reg.MustRegister(stub)

		scheme := runtime.NewScheme()
		dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			scheme,
			map[schema.GroupVersionResource]string{
				collector.SandboxGVRV1Alpha2:         "SandboxList",
				collector.SandboxGVRV1Alpha1:         "SandboxList",
				collector.SandboxSnapshotGVRV1Alpha2: "SandboxSnapshotList",
				collector.SandboxSnapshotGVRV1Alpha1: "SandboxSnapshotList",
			},
		)
		leaked := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "sandbox.fast.io/v1alpha2",
				"kind":       "Sandbox",
				"metadata": map[string]any{
					"name":      "leaked-sbx",
					"namespace": "opensandbox-dataplane",
				},
			},
		}
		_, err := dynClient.Resource(collector.SandboxGVRV1Alpha2).Namespace("opensandbox-dataplane").Create(context.Background(), leaked, metav1.CreateOptions{})
		require.NoError(t, err)

		app := NewDefaultApp()
		app.Suites = reg
		app.NewCollectorClients = func() (*collector.Clients, error) {
			return &collector.Clients{
				Clientset:     fake.NewSimpleClientset(),
				DynamicClient: dynClient,
			}, nil
		}

		cmd := app.RootCmd()
		cmd.SetArgs([]string{
			"worker", "run",
			"--suite", "schedule-burst",
			"--output", outDir,
			"--run-id", "worker-leak-fail",
			"--leak-audit-grace", "0s",
		})
		err = cmd.Execute()
		require.Error(t, err)
		require.Contains(t, err.Error(), "post-run leak audit failed")

		rep, loadErr := report.Load(filepath.Join(outDir, "report.json"))
		require.NoError(t, loadErr)
		require.NotNil(t, rep.LeakAudit)
		require.False(t, rep.LeakAudit.Passed)
		require.Equal(t, 1, rep.LeakAudit.ResidualSandboxCRs)
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

func TestCollectorNodeResolver_FastletPodLookup(t *testing.T) {
	const ns = "opensandbox-dataplane"
	scheme := runtime.NewScheme()

	sbxViaFastlet := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      "sbx-placement",
				"namespace": ns,
			},
			"status": map[string]any{
				"placement": map[string]any{
					"fastletName": "fastlet-pool-0",
				},
			},
		},
	}
	sbxDirectNode := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":      "sbx-direct",
				"namespace": ns,
			},
			"status": map[string]any{
				"nodeName": "worker-direct-1",
			},
		},
	}

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			collector.SandboxGVRV1Alpha2: "SandboxList",
			collector.SandboxGVRV1Alpha1: "SandboxList",
		},
	)
	_, err := dynClient.Resource(collector.SandboxGVRV1Alpha2).Namespace(ns).Create(context.Background(), sbxViaFastlet, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = dynClient.Resource(collector.SandboxGVRV1Alpha2).Namespace(ns).Create(context.Background(), sbxDirectNode, metav1.CreateOptions{})
	require.NoError(t, err)
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fastlet-pool-0",
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-node-2",
		},
	})

	resolver := &collectorNodeResolver{
		clients: &collector.Clients{
			Clientset:     cs,
			DynamicClient: dynClient,
		},
		resourceNS: ns,
	}

	node, err := resolver.SandboxNode(context.Background(), "sbx-placement")
	require.NoError(t, err)
	require.Equal(t, "worker-node-2", node)

	directNode, err := resolver.SandboxNode(context.Background(), "sbx-direct")
	require.NoError(t, err)
	require.Equal(t, "worker-direct-1", directNode)
}

func TestCollectorSoakSamplerIncludesPodCgroupOOMAndPeakActive(t *testing.T) {
	ls := collector.NewLoadSampler(collector.DefaultConfig(), nil, time.Second)
	ls.RecordSample(collector.LoadSample{
		ActiveSandboxes: 4,
		Hosts:           []collector.HostSample{{NodeName: "n1", OOMKillTotal: 2, MemAvailableMiB: 4000}},
		Components: map[string]collector.ComponentSample{
			collector.ComponentFSBFastlet: {
				ExtraMetrics: map[string]float64{"admission_slots_deleting": 2},
				PodCgroup:    &collector.PodCgroupSample{MemoryEvents: collector.CgroupMemoryEvents{OOMKill: 1}},
			},
		},
	})
	ls.RecordSample(collector.LoadSample{
		ActiveSandboxes: 6,
		Hosts:           []collector.HostSample{{NodeName: "n1", OOMKillTotal: 3, MemAvailableMiB: 3500}},
		Components: map[string]collector.ComponentSample{
			collector.ComponentFSBFastlet: {
				ExtraMetrics: map[string]float64{"admission_slots_deleting": 1},
				PodCgroup:    &collector.PodCgroupSample{MemoryEvents: collector.CgroupMemoryEvents{OOMKill: 3}},
			},
		},
	})

	soakSampler := newCollectorSoakSampler(ls, nil)
	sm, err := soakSampler.Sample(context.Background())
	require.NoError(t, err)
	// Host OOM delta = 1 (3-2) + PodCgroup OOM delta = 2 (3-1) => 3 total.
	require.Equal(t, 3, sm.OOMKills)
	require.Equal(t, 1, sm.DeletingSlots)

	peak := resolvePeakActiveSandboxes(&report.SuiteResult{SucceededOps: 100}, nil, ls, nil)
	require.Equal(t, 6, peak)
}

func TestCLIRunForwardsCyclesAndLeakAuditGrace(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "worker-linux")
	require.NoError(t, os.WriteFile(binPath, []byte("fake"), 0o755))

	mock := &mockRemoteRunner{}
	app := NewDefaultApp()
	app.NewRemoteRunner = func(_ string) (runner.RemoteRunner, *runner.KubeClients, error) {
		return mock, nil, nil
	}

	cmd := app.RootCmd()
	cmd.SetArgs([]string{
		"run",
		"--suite", "density-ramp",
		"--worker-binary", binPath,
		"--cycles", "3",
		"--leak-audit-grace", "45s",
	})
	require.NoError(t, cmd.Execute())
	require.Contains(t, mock.workerArgs, "--cycles")
	require.Contains(t, mock.workerArgs, "3")
	require.Contains(t, mock.workerArgs, "--leak-audit-grace")
	require.Contains(t, mock.workerArgs, "45s")
}

type multiCycleStubSuite struct {
	name    string
	calls   int
	results []*report.SuiteResult
}

func (s *multiCycleStubSuite) Name() string               { return s.name }
func (s *multiCycleStubSuite) Description() string        { return "multi-cycle stub" }
func (s *multiCycleStubSuite) SupportedRuntimes() []string { return nil }
func (s *multiCycleStubSuite) Run(_ context.Context, _ suite.RunContext) (*report.SuiteResult, []suite.OperationSample, error) {
	idx := s.calls
	s.calls++
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	return s.results[idx], nil, nil
}

func TestCLIWorkerRunMultiCycleWritesPerCycleAndAggregateBundle(t *testing.T) {
	outDir := t.TempDir()
	stub := &multiCycleStubSuite{
		name: "schedule-burst",
		results: []*report.SuiteResult{
			{
				Name:          "schedule-burst",
				Status:        "passed",
				TotalOps:      10,
				SucceededOps:  10,
				ThroughputRPS: 12.0,
				Latencies: map[string]report.LatencyDist{
					"t_api_ack":    {Count: 10, P50Ms: 10, P95Ms: 20, P99Ms: 30},
					"t_first_ping": {Count: 10, P50Ms: 100, P95Ms: 200, P99Ms: 300},
				},
			},
			{
				Name:          "schedule-burst",
				Status:        "passed",
				TotalOps:      10,
				SucceededOps:  10,
				ThroughputRPS: 18.0,
				Latencies: map[string]report.LatencyDist{
					"t_api_ack":    {Count: 10, P50Ms: 20, P95Ms: 30, P99Ms: 40},
					"t_first_ping": {Count: 10, P50Ms: 120, P95Ms: 220, P99Ms: 320},
				},
			},
			{
				Name:          "schedule-burst",
				Status:        "failed",
				TotalOps:      10,
				SucceededOps:  8,
				FailedOps:     2,
				ThroughputRPS: 6.0,
				Latencies: map[string]report.LatencyDist{
					"t_api_ack":    {Count: 8, P50Ms: 30, P95Ms: 40, P99Ms: 50},
					"t_first_ping": {Count: 8, P50Ms: 140, P95Ms: 240, P99Ms: 340},
				},
			},
		},
	}

	reg := suite.NewRegistry()
	reg.MustRegister(stub)

	app := NewDefaultApp()
	app.Suites = reg
	app.NewCollectorClients = func() (*collector.Clients, error) {
		return nil, errors.New("sdk-only")
	}

	cmd := app.RootCmd()
	cmd.SetArgs([]string{
		"worker", "run",
		"--suite", "schedule-burst",
		"--output", outDir,
		"--run-id", "multi-run",
		"--cycles", "3",
	})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 of 3 cycles failed")
	require.Equal(t, 3, stub.calls)

	// Per-cycle artifacts must exist.
	for _, c := range []string{"cycle-1", "cycle-2", "cycle-3"} {
		_, statErr := os.Stat(filepath.Join(outDir, c, "report.json"))
		require.NoError(t, statErr)
		_, statErr = os.Stat(filepath.Join(outDir, c, "summary.md"))
		require.NoError(t, statErr)
	}

	// Top-level aggregate report and summary must exist.
	rawAgg, readErr := os.ReadFile(filepath.Join(outDir, "cycles-report.json"))
	require.NoError(t, readErr)
	var agg report.CycleSummaryReport
	require.NoError(t, json.Unmarshal(rawAgg, &agg))
	require.Equal(t, 3, agg.TotalCycles)
	require.Equal(t, 2, agg.PassedCycles)
	require.Equal(t, 1, agg.FailedCycles)
	require.InDelta(t, 12.0, agg.Suites["schedule-burst"].MeanRPS, 1e-6)
	require.InDelta(t, 6.0, agg.Suites["schedule-burst"].MinRPS, 1e-6)
	require.InDelta(t, 18.0, agg.Suites["schedule-burst"].MaxRPS, 1e-6)

	summaryBytes, readErr := os.ReadFile(filepath.Join(outDir, "summary.md"))
	require.NoError(t, readErr)
	require.Contains(t, string(summaryBytes), "2 / 3")
	require.Contains(t, string(summaryBytes), "Per-Cycle Breakdown")
}

func TestCLIPlotSubcommand(t *testing.T) {
	dir := t.TempDir()
	rep := report.New(report.Provenance{RunID: "cli-plot-test", Runtime: "fast-sandbox-overlaybd"})
	rep.Suites["schedule-burst"] = report.SuiteResult{
		Name:         "schedule-burst",
		Status:       "passed",
		TotalOps:     10,
		SucceededOps: 10,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":    {Count: 10, MinMs: 50, P50Ms: 100, P90Ms: 140, P95Ms: 150, P99Ms: 180, MaxMs: 190},
			"t_first_ping": {Count: 10, MinMs: 60, P50Ms: 110, P90Ms: 150, P95Ms: 160, P99Ms: 190, MaxMs: 200},
		},
	}
	require.NoError(t, rep.WriteBundle(dir))

	outHTML := filepath.Join(dir, "custom.html")
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"plot", filepath.Join(dir, "report.json"), "--output", outHTML, "--title", "CLI Plot Smoke"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), outHTML)

	raw, err := os.ReadFile(outHTML)
	require.NoError(t, err)
	require.Contains(t, string(raw), "CLI Plot Smoke")
	require.Contains(t, string(raw), "t_api_ack")
	require.Contains(t, string(raw), "t_first_ping")
	require.NotContains(t, string(raw), "t_first_exec")
}
