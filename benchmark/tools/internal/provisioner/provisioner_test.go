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

package provisioner

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testProfile(t *testing.T) *profile.BenchmarkProfile {
	t.Helper()
	prof, err := profile.LoadEmbedded(profile.DefaultProfileName)
	require.NoError(t, err)
	return prof
}

type mockCmdCall struct {
	Name       string
	Args       []string
	ValuesYAML map[string]any
}

type mockCommandRunner struct {
	mu    sync.Mutex
	calls []mockCmdCall
	fn    func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (m *mockCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := mockCmdCall{Name: name, Args: append([]string(nil), args...)}
	// Capture temporary helm values YAML file before caller deletes it.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" {
			if raw, err := os.ReadFile(args[i+1]); err == nil {
				var parsed map[string]any
				if yaml.Unmarshal(raw, &parsed) == nil {
					call.ValuesYAML = parsed
				}
			}
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	if m.fn != nil {
		return m.fn(ctx, name, args...)
	}
	return []byte("ok"), nil
}

type mockPodExecer struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(ctx context.Context, namespace, podName, container string, command []string) (string, string, error)
}

func (m *mockPodExecer) Exec(ctx context.Context, namespace, podName, container string, command []string) (string, string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, append([]string{namespace, podName, container}, command...))
	m.mu.Unlock()
	if m.fn != nil {
		return m.fn(ctx, namespace, podName, container, command)
	}
	return "", "", nil
}

type mockLogReader struct {
	logsByPod map[string]string
}

func (m *mockLogReader) ReadLogs(_ context.Context, _ kubernetes.Interface, _, podName, _ string) (string, error) {
	if out, ok := m.logsByPod[podName]; ok {
		return out, nil
	}
	return "", fmt.Errorf("no mock logs for pod %q", podName)
}

func makeReadyNode(name, arch string, firecrackerReady bool) *corev1.Node {
	labels := map[string]string{
		corev1.LabelArchStable: arch,
	}
	conds := []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
	}
	if firecrackerReady {
		labels[firecrackerNodeLabelKey] = "true"
		conds = append(conds, corev1.NodeCondition{
			Type:   firecrackerReadyCondition,
			Status: corev1.ConditionTrue,
		})
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Status: corev1.NodeStatus{
			Conditions: conds,
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("32"),
				corev1.ResourceMemory: resource.MustParse("128Gi"),
			},
			NodeInfo: corev1.NodeSystemInfo{
				KernelVersion:           "6.8.0-45-generic",
				OSImage:                 "Ubuntu 24.04 LTS",
				ContainerRuntimeVersion: "containerd://1.7.20",
				Architecture:            arch,
			},
		},
	}
}

func newFakeDynamicClient() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxGVR:         "SandboxList",
		sandboxSnapshotGVR: "SandboxSnapshotList",
		sandboxPoolGVR:     "SandboxPoolList",
		sandboxTemplateGVR: "SandboxTemplateList",
	})
}

func TestGenerateRegistryCertBundle(t *testing.T) {
	bundle, err := GenerateRegistryCertBundle(ephemeralRegistryName, "opensandbox-bench", 24*time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, bundle.CACertPEM)
	require.NotEmpty(t, bundle.ServerCertPEM)
	require.NotEmpty(t, bundle.ServerKeyPEM)

	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(bundle.CACertPEM))

	block, _ := pem.Decode(bundle.ServerCertPEM)
	require.NotNil(t, block)
	serverCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	_, err = serverCert.Verify(x509.VerifyOptions{
		DNSName: "opensandbox-bench-registry.opensandbox-bench.svc",
		Roots:   caPool,
	})
	require.NoError(t, err)

	_, err = serverCert.Verify(x509.VerifyOptions{
		DNSName: "opensandbox-bench-registry.opensandbox-bench.svc.cluster.local",
		Roots:   caPool,
	})
	require.NoError(t, err)
}

func TestBuildHelmValues(t *testing.T) {
	prof := testProfile(t)
	prof.Stack.HelmValues = map[string]any{
		"fast-sandbox": map[string]any{
			"controller": map[string]any{
				"kubeClientQPS": 50,
			},
		},
	}

	vals := BuildHelmValues(prof)

	// Tracing must stay disabled.
	tracing := vals["tracing"].(map[string]any)
	assert.Equal(t, false, tracing["enabled"])

	// Base chart must not recreate namespaces pre-created by Provisioner.Up.
	baseVals := vals["base"].(map[string]any)
	assert.Equal(t, true, baseVals["enabled"])
	baseNS := baseVals["fastSandbox"].(map[string]any)["namespaces"].(map[string]any)
	assert.Equal(t, false, baseNS["createSystem"])
	assert.Equal(t, false, baseNS["create"])
	assert.Equal(t, "opensandbox-system", baseNS["system"])
	assert.Equal(t, "opensandbox-dataplane", baseNS["resources"])

	// Fast-sandbox OCI keys must match manifests/charts/fast-sandbox/values.yaml.
	fsb := vals["fast-sandbox"].(map[string]any)
	assert.Equal(t, true, fsb["enabled"])
	assert.Equal(t, "opensandbox-system", fsb["systemNamespace"])
	assert.Equal(t, "opensandbox-dataplane", fsb["resourceNamespace"])
	_, hasPool := fsb["pool"]
	assert.False(t, hasPool, "fast-sandbox chart has no top-level pool key")

	rt := fsb["runtime"].(map[string]any)
	assert.Equal(t, agentRegistrySecretName, rt["registrySecret"])
	_, hasCABundle := rt["registryCABundleSecret"]
	assert.False(t, hasCABundle)

	oci := rt["oci"].(map[string]any)
	assert.Equal(t, true, oci["enabled"])
	assert.Equal(t, 64, oci["cacheSizeGB"])
	assert.Equal(t, 32, oci["poolLow"])
	assert.Equal(t, 64, oci["poolHigh"])
	assert.Equal(t, 2, oci["poolSizeGB"])

	// Merged HelmValues override preserved alongside fastletProxy Guaranteed resources.
	ctrl := fsb["controller"].(map[string]any)
	assert.Equal(t, 50, ctrl["kubeClientQPS"])
	proxyRes := ctrl["fastletProxy"].(map[string]any)["resources"].(map[string]any)
	assert.Equal(t, map[string]any{"cpu": "2", "memory": "4Gi"}, proxyRes["requests"])
	assert.Equal(t, map[string]any{"cpu": "2", "memory": "4Gi"}, proxyRes["limits"])

	// Ingress & Redis wiring.
	ig := vals["ingress-gateway"].(map[string]any)
	assert.Equal(t, true, ig["enabled"])
	gw := ig["gateway"].(map[string]any)
	assert.Equal(t, "opensandbox-dataplane", gw["dataplaneNamespace"])
	assert.Equal(t, true, gw["activity"].(map[string]any)["enabled"])
	assert.Equal(t, true, gw["wake"].(map[string]any)["enabled"])
	assert.Equal(t, secureAccessSecretName, gw["secureAccess"].(map[string]any)["existingSecret"])

	// Server configToml includes template_oci_publish_secret and idle_pause redis, plus secureAccess secret.
	srv := vals["opensandbox-server"].(map[string]any)
	srvSpec := srv["server"].(map[string]any)
	srvEnv := srvSpec["env"].([]any)
	require.Len(t, srvEnv, 1)
	assert.Equal(t, "OPENSANDBOX_INSECURE_SERVER", srvEnv[0].(map[string]any)["name"])
	assert.Equal(t, "YES", srvEnv[0].(map[string]any)["value"])
	srvGW := srvSpec["gateway"].(map[string]any)
	assert.Equal(t, secureAccessSecretName, srvGW["secureAccess"].(map[string]any)["existingSecret"])
	toml := srv["configToml"].(string)
	assert.Contains(t, toml, `execd_image = "`)
	assert.Contains(t, toml, `template_oci_publish_secret = "template-oci-publish-secret"`)
	assert.Contains(t, toml, `workload_provider = "batchsandbox"`)
	assert.Contains(t, toml, `[egress]`)
	assert.Contains(t, toml, `image = "`)
	assert.Contains(t, toml, `[automation.idle_pause]`)
}

func TestPreflightSuccessAndCleanup(t *testing.T) {
	prof := testProfile(t)
	cs := k8sfake.NewSimpleClientset(
		makeReadyNode("node-a", "amd64", false),
		makeReadyNode("node-b", "amd64", false),
	)
	// Mark created preflight probe pods as Succeeded immediately.
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})

	prov := NewOverlayBDProvisioner()
	prov.PollInterval = time.Millisecond
	prov.ReadyTimeout = 5 * time.Second
	prov.LogReader = &mockLogReader{
		logsByPod: map[string]string{
			"osb-preflight-node-a": "KVM_READY=true\nCGROUP_V2=true\nUBLK_READY=true\nSTATEROOT_FS=xfs\nREFLINK_SUPPORTED=true\nSPILL_DIR_AVAILABLE=true\nCPU_IDENTITY=GenuineIntel:6:106:6\n",
			"osb-preflight-node-b": "KVM_READY=true\nCGROUP_V2=true\nUBLK_READY=true\nSTATEROOT_FS=xfs\nREFLINK_SUPPORTED=true\nSPILL_DIR_AVAILABLE=true\nCPU_IDENTITY=GenuineIntel:6:106:6\n",
		},
	}

	fp, err := prov.Preflight(context.Background(), prof, Options{
		Client: cs,
		Logger: testLogger(),
	})
	require.NoError(t, err)
	require.NotNil(t, fp)
	assert.Equal(t, "amd64", fp.ClusterArch)
	assert.Equal(t, profile.PoolModeResourceAware, fp.PoolMode)
	require.Len(t, fp.Nodes, 2)
	assert.True(t, fp.Nodes[0].KVMReady)
	assert.True(t, fp.Nodes[0].UblkReady)
	assert.Equal(t, "xfs", fp.Nodes[0].StateRootFS)
	assert.True(t, fp.Nodes[0].ReflinkSupported)
	assert.True(t, fp.Nodes[0].SpillDirAvailable)
	assert.Equal(t, "GenuineIntel:6:106:6", fp.Nodes[0].CPUIdentity)

	// Verify inspection pods were cleaned up.
	pods, err := cs.CoreV1().Pods(prof.Environment.BenchNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pods.Items)
}

func TestPreflightFailsFastOnMissingKVMOrUblk(t *testing.T) {
	prof := testProfile(t)
	cs := k8sfake.NewSimpleClientset(makeReadyNode("node-bad", "amd64", false))
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})

	prov := NewOverlayBDProvisioner()
	prov.PollInterval = time.Millisecond
	prov.ReadyTimeout = 5 * time.Second
	prov.LogReader = &mockLogReader{
		logsByPod: map[string]string{
			"osb-preflight-node-bad": "KVM_READY=false\nCGROUP_V2=true\nUBLK_READY=false\nSTATEROOT_FS=ext4\nREFLINK_SUPPORTED=false\nSPILL_DIR_AVAILABLE=false\nCPU_IDENTITY=AuthenticAMD:25:1:1\n",
		},
	}

	fp, err := prov.Preflight(context.Background(), prof, Options{
		Client: cs,
		Logger: testLogger(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing /dev/kvm")
	assert.Contains(t, err.Error(), "missing ublk_drv")
	require.NotNil(t, fp)
	require.Len(t, fp.Nodes, 1)
	assert.False(t, fp.Nodes[0].KVMReady)
	assert.False(t, fp.Nodes[0].UblkReady)

	// Probe pod still cleaned up on failure.
	pods, listErr := cs.CoreV1().Pods(prof.Environment.BenchNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, pods.Items)
}

func TestUpFullFlow(t *testing.T) {
	prof := testProfile(t)
	prof.Stack.FastSandbox.Pool.MinFastlets = 2
	prof.Stack.FastSandbox.Runtime.OCI.PoolLow = 4
	systemNS := prof.Environment.SystemNamespace
	resourceNS := prof.Environment.ResourceNamespace
	benchNS := prof.Environment.BenchNamespace

	cs := k8sfake.NewSimpleClientset(makeReadyNode("node-1", "amd64", true))

	// Auto-complete prepare-node Jobs.
	cs.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		return false, nil, nil
	})
	// Auto-ready Deployments created by infra stage.
	cs.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		dep := action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		return false, nil, nil
	})

	dyn := newFakeDynamicClient()
	// Mark created SandboxTemplate as Succeeded with oci:// manifestRef.
	dyn.PrependReactor("create", "sandboxtemplates", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		_ = unstructured.SetNestedField(obj.Object, "Succeeded", "status", "phase")
		_ = unstructured.SetNestedField(obj.Object, "oci://opensandbox-bench-registry.opensandbox-bench.svc:5000/opensandbox/templates@sha256:deadbeef", "status", "manifestRef")
		return false, nil, nil
	})
	// When SandboxPool is created, seed ready fastlet+egress pods in resourceNS.
	dyn.PrependReactor("create", "sandboxpools", func(action k8stesting.Action) (bool, runtime.Object, error) {
		for i := 0; i < 2; i++ {
			_, _ = cs.CoreV1().Pods(resourceNS).Create(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("fastlet-%d", i),
					Namespace: resourceNS,
					Labels:    map[string]string{"app": "sandbox-fastlet"},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "fastlet", Ready: true},
						{Name: "egress", Ready: true},
					},
				},
			}, metav1.CreateOptions{})
		}
		return false, nil, nil
	})

	cmdRunner := &mockCommandRunner{
		fn: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			// Simulate Helm installing control plane deployments + firecracker-runtime DaemonSet + runtime pod.
			if name == "helm" && len(args) > 0 && args[0] == "upgrade" {
				for _, depName := range []string{"opensandbox-server", "opensandbox-ingress-gateway", "fast-sandbox-controller"} {
					_, _ = cs.AppsV1().Deployments(systemNS).Create(ctx, &appsv1.Deployment{
						ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: systemNS},
						Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
						Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
					}, metav1.CreateOptions{})
				}
				_, _ = cs.AppsV1().DaemonSets(systemNS).Create(ctx, &appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Name: "firecracker-runtime", Namespace: systemNS},
					Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1},
				}, metav1.CreateOptions{})
				_, _ = cs.CoreV1().Pods(systemNS).Create(ctx, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "firecracker-runtime-abc",
						Namespace: systemNS,
						Labels:    map[string]string{"app": "fast-sandbox", "component": "firecracker-runtime"},
					},
					Status: corev1.PodStatus{
						Phase:      corev1.PodRunning,
						Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
					},
				}, metav1.CreateOptions{})
			}
			return []byte("Release installed"), nil
		},
	}

	execer := &mockPodExecer{
		fn: func(_ context.Context, _, _, container string, _ []string) (string, string, error) {
			if container == "overlaybd-ublkd" {
				return `{"idle":4,"active":0,"hits":0,"misses":0}`, "", nil
			}
			return "", "", nil
		},
	}

	prov := NewOverlayBDProvisioner()
	prov.DynamicClient = dyn
	prov.CmdRunner = cmdRunner
	prov.PodExecer = execer
	prov.PollInterval = time.Millisecond
	prov.ReadyTimeout = 5 * time.Second

	err := prov.Up(context.Background(), prof, Options{
		Kubeconfig:   "/tmp/fake-kubeconfig",
		PrepareNodes: true,
		Client:       cs,
		Logger:       testLogger(),
	})
	require.NoError(t, err)

	// Verify Runner RBAC created with required cross-node isolation permissions.
	_, err = cs.CoreV1().ServiceAccounts(benchNS).Get(context.Background(), runnerServiceAccountName, metav1.GetOptions{})
	require.NoError(t, err)
	cr, err := cs.RbacV1().ClusterRoles().Get(context.Background(), runnerClusterRoleName, metav1.GetOptions{})
	require.NoError(t, err)
	hasNodePatch := false
	hasPodDelete := false
	for _, rule := range cr.Rules {
		for _, res := range rule.Resources {
			if res == "nodes" {
				for _, v := range rule.Verbs {
					if v == "patch" {
						hasNodePatch = true
					}
				}
			}
			if res == "pods" {
				for _, v := range rule.Verbs {
					if v == "delete" {
						hasPodDelete = true
					}
				}
			}
		}
	}
	assert.True(t, hasNodePatch, "runner ClusterRole must grant patch on nodes for cross-node isolation")
	assert.True(t, hasPodDelete, "runner ClusterRole must grant delete on pods for fastlet isolation")
	_, err = cs.RbacV1().ClusterRoleBindings().Get(context.Background(), runnerClusterRoleName, metav1.GetOptions{})
	require.NoError(t, err)

	// Verify fast-sandbox-logging ConfigMap mirrors logs to container stderr/stdout in both namespaces.
	for _, ns := range []string{systemNS, resourceNS} {
		cm, cmErr := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), fastSandboxLoggingConfigMapName, metav1.GetOptions{})
		require.NoError(t, cmErr)
		assert.Contains(t, cm.Data["logging.yaml"], "alsoStderr: true")
	}

	// Verify OSEP-0011 secure-access signing Secret created in systemNS.
	saSec, err := cs.CoreV1().Secrets(systemNS).Get(context.Background(), secureAccessSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "a", saSec.StringData["active-key"])
	assert.True(t, strings.HasPrefix(saSec.StringData["keys"], "a="))

	// Verify fast-sandbox-registry ConfigMap created in resourceNS for fastlet OCI pulls/pushes.
	regCM, err := cs.CoreV1().ConfigMaps(resourceNS).Get(context.Background(), fastSandboxRegistryConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, regCM.Data["registries.yaml"], ephemeralRegistryHost(benchNS))
	assert.Contains(t, regCM.Data["registries.yaml"], ociPublishSecretName)

	// Verify ephemeral registry + Redis + secrets created.
	_, err = cs.AppsV1().Deployments(benchNS).Get(context.Background(), ephemeralRegistryName, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Services(benchNS).Get(context.Background(), ephemeralRegistryName, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Secrets(systemNS).Get(context.Background(), agentRegistrySecretName, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Secrets(resourceNS).Get(context.Background(), ociPublishSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Secrets(systemNS).Get(context.Background(), ephemeralRegistryCASecret, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = cs.AppsV1().Deployments(benchNS).Get(context.Background(), ephemeralRedisName, metav1.GetOptions{})
	require.NoError(t, err)

	// Verify Helm called with kubeconfig and tracing disabled.
	require.Len(t, cmdRunner.calls, 1)
	assert.Equal(t, "helm", cmdRunner.calls[0].Name)
	assert.Contains(t, strings.Join(cmdRunner.calls[0].Args, " "), "--kubeconfig /tmp/fake-kubeconfig")
	require.NotNil(t, cmdRunner.calls[0].ValuesYAML)
	assert.Equal(t, false, cmdRunner.calls[0].ValuesYAML["tracing"].(map[string]any)["enabled"])

	// Verify SandboxTemplate and ResourceAware SandboxPool CRs created.
	tmpl, err := dyn.Resource(sandboxTemplateGVR).Namespace(resourceNS).Get(context.Background(), defaultTemplateName, metav1.GetOptions{})
	require.NoError(t, err)
	format, _, _ := unstructured.NestedString(tmpl.Object, "spec", "output", "format")
	assert.Equal(t, "overlaybd", format)

	poolCR, err := dyn.Resource(sandboxPoolGVR).Namespace(resourceNS).Get(context.Background(), defaultPoolName, metav1.GetOptions{})
	require.NoError(t, err)
	mode, _, _ := unstructured.NestedString(poolCR.Object, "spec", "sandboxResources", "mode")
	assert.Equal(t, profile.PoolModeResourceAware, mode)
}

func TestPoolAndTemplateCustomOverrides(t *testing.T) {
	prof := testProfile(t)
	prof.Stack.FastSandbox.Pool.FastletImage = "acr.example.com/opensandbox/fsb-fastlet:custom"
	prof.Stack.FastSandbox.Pool.EgressImage = "acr.example.com/opensandbox/egress:custom"
	prof.Stack.FastSandbox.Pool.ImagePullSecrets = []string{"acr-opensandbox-20261019"}
	prof.Stack.Template.ExecdImage = "acr.example.com/opensandbox/execd:custom"
	prof.Stack.Template.Publish = "oci://acr.example.com/opensandbox/templates"
	prof.Stack.Template.PublishSecretRef = "custom-publish-secret"

	tmpl := buildGoldenTemplateCR(prof, "opensandbox-dataplane", "opensandbox-bench")
	execdImg, _, _ := unstructured.NestedString(tmpl.Object, "spec", "execd")
	assert.Equal(t, "acr.example.com/opensandbox/execd:custom", execdImg)
	publish, _, _ := unstructured.NestedString(tmpl.Object, "spec", "output", "publish")
	assert.Equal(t, "oci://acr.example.com/opensandbox/templates", publish)
	pubSecret, _, _ := unstructured.NestedString(tmpl.Object, "spec", "output", "publishSecretRef", "name")
	assert.Equal(t, "custom-publish-secret", pubSecret)

	poolCR := buildResourceAwarePoolCR(prof, "opensandbox-dataplane")
	maxPerPod, _, _ := unstructured.NestedInt64(poolCR.Object, "spec", "maxSandboxesPerPod")
	assert.Equal(t, int64(500), maxPerPod)
	sbxCPU, _, _ := unstructured.NestedString(poolCR.Object, "spec", "sandboxResources", "cpu")
	sbxMem, _, _ := unstructured.NestedString(poolCR.Object, "spec", "sandboxResources", "memory")
	assert.Equal(t, "2", sbxCPU)
	assert.Equal(t, "4096Mi", sbxMem)
	ocCPU, _, _ := unstructured.NestedString(poolCR.Object, "spec", "sandboxResources", "overcommitRatio", "cpu")
	ocMem, _, _ := unstructured.NestedString(poolCR.Object, "spec", "sandboxResources", "overcommitRatio", "memory")
	assert.Equal(t, "8", ocCPU)
	assert.Equal(t, "4", ocMem)

	reqAntiAffinity, foundAntiAffinity, _ := unstructured.NestedSlice(poolCR.Object, "spec", "fastletTemplate", "spec", "affinity", "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution")
	require.True(t, foundAntiAffinity)
	require.Len(t, reqAntiAffinity, 1)
	term := reqAntiAffinity[0].(map[string]any)
	assert.Equal(t, "kubernetes.io/hostname", term["topologyKey"])
	assert.Equal(t, map[string]any{"app": "sandbox-fastlet"}, term["labelSelector"].(map[string]any)["matchLabels"])

	containers, _, _ := unstructured.NestedSlice(poolCR.Object, "spec", "fastletTemplate", "spec", "containers")
	require.Len(t, containers, 2)
	fastletC := containers[0].(map[string]any)
	assert.Equal(t, "acr.example.com/opensandbox/fsb-fastlet:custom", fastletC["image"])
	_, hasRes := fastletC["resources"]
	assert.False(t, hasRes, "ResourceAware fastlet container must not set explicit resources")
	egressC := containers[1].(map[string]any)
	assert.Equal(t, "acr.example.com/opensandbox/egress:custom", egressC["image"])
	egressRes := egressC["resources"].(map[string]any)
	assert.Equal(t, map[string]any{"cpu": "2", "memory": "4Gi"}, egressRes["requests"])
	assert.Equal(t, map[string]any{"cpu": "4", "memory": "16Gi"}, egressRes["limits"])
	egressMounts := egressC["volumeMounts"].([]any)
	hasFCStateMount := false
	for _, m := range egressMounts {
		mm := m.(map[string]any)
		if mm["name"] == "firecracker-state" && mm["mountPath"] == defaultStateRoot {
			hasFCStateMount = true
		}
	}
	assert.True(t, hasFCStateMount, "egress sidecar must mount controller-injected firecracker-state volume for VAULT_EXPORT/VAULT_IMPORT")
	pullSecrets, _, _ := unstructured.NestedSlice(poolCR.Object, "spec", "fastletTemplate", "spec", "imagePullSecrets")
	require.Len(t, pullSecrets, 1)
	assert.Equal(t, "acr-opensandbox-20261019", pullSecrets[0].(map[string]any)["name"])

	// Verify podAntiAffinity=false omits affinity.
	disableAntiAffinity := false
	prof.Stack.FastSandbox.Pool.PodAntiAffinity = &disableAntiAffinity
	noAffinityCR := buildResourceAwarePoolCR(prof, "opensandbox-dataplane")
	_, hasAffinity, _ := unstructured.NestedMap(noAffinityCR.Object, "spec", "fastletTemplate", "spec", "affinity")
	assert.False(t, hasAffinity)

	// Verify LegacySlot mode omits overcommitRatio.
	prof.Stack.FastSandbox.Pool.Mode = profile.PoolModeLegacySlot
	legacyCR := buildResourceAwarePoolCR(prof, "opensandbox-dataplane")
	legacyMode, _, _ := unstructured.NestedString(legacyCR.Object, "spec", "sandboxResources", "mode")
	assert.Equal(t, profile.PoolModeLegacySlot, legacyMode)
	_, hasOvercommit, _ := unstructured.NestedMap(legacyCR.Object, "spec", "sandboxResources", "overcommitRatio")
	assert.False(t, hasOvercommit)

	// Verify ensureAgentRegistrySecretIfMissing creates fallback secret when absent and preserves existing.
	cs := k8sfake.NewSimpleClientset()
	prov := NewOverlayBDProvisioner()
	require.NoError(t, prov.ensureAgentRegistrySecretIfMissing(context.Background(), cs, "opensandbox-system"))
	sec, err := cs.CoreV1().Secrets("opensandbox-system").Get(context.Background(), agentRegistrySecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, sec.StringData["registry.json"], `"registries":[]`)
}

func TestUpRejectsNonOCIManifestRef(t *testing.T) {
	prof := testProfile(t)
	prof.Environment.Registry.EphemeralInCluster = false
	prof.Environment.Redis.Enabled = false
	prof.Suites.WakeStorm = nil
	prof.Stack.FastSandbox.Runtime.OCI.PoolLow = 0
	systemNS := prof.Environment.SystemNamespace

	cs := k8sfake.NewSimpleClientset(
		makeReadyNode("node-1", "amd64", true),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "opensandbox-server", Namespace: systemNS},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "opensandbox-ingress-gateway", Namespace: systemNS},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "fast-sandbox-controller", Namespace: systemNS},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "firecracker-runtime", Namespace: systemNS},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1},
		},
	)
	dyn := newFakeDynamicClient()
	dyn.PrependReactor("create", "sandboxtemplates", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		_ = unstructured.SetNestedField(obj.Object, "Succeeded", "status", "phase")
		_ = unstructured.SetNestedField(obj.Object, "s3://wrong-bucket/manifest.json", "status", "manifestRef")
		return false, nil, nil
	})

	prov := NewOverlayBDProvisioner()
	prov.DynamicClient = dyn
	prov.CmdRunner = &mockCommandRunner{}
	prov.PollInterval = time.Millisecond
	prov.ReadyTimeout = 5 * time.Second

	err := prov.Up(context.Background(), prof, Options{Client: cs, Logger: testLogger()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-OCI manifestRef")
}

func TestDownCleanAndLeakDetection(t *testing.T) {
	prof := testProfile(t)
	systemNS := prof.Environment.SystemNamespace
	resourceNS := prof.Environment.ResourceNamespace
	benchNS := prof.Environment.BenchNamespace

	t.Run("clean teardown passes leak audit", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(
			makeReadyNode("node-1", "amd64", true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: benchNS}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ephemeralRegistryName, Namespace: benchNS}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ephemeralRegistryName, Namespace: benchNS}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agentRegistrySecretName, Namespace: systemNS}},
		)
		cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			pod.Status.Phase = corev1.PodSucceeded
			return false, nil, nil
		})

		dyn := newFakeDynamicClient()
		_, err := dyn.Resource(sandboxGVR).Namespace(resourceNS).Create(context.Background(), &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "sandbox.fast.io/v1alpha2",
				"kind":       "Sandbox",
				"metadata":   map[string]any{"name": "sbx-1", "namespace": resourceNS},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		cmdRunner := &mockCommandRunner{}
		prov := NewOverlayBDProvisioner()
		prov.DynamicClient = dyn
		prov.CmdRunner = cmdRunner
		prov.PollInterval = time.Millisecond
		prov.ReadyTimeout = 5 * time.Second
		prov.LogReader = &mockLogReader{
			logsByPod: map[string]string{
				"osb-leakaudit-node-1": "ORPHANED_UBLK=0\nLEAKED_FC_PIDS=0\nHOST_OOM_KILLS=0\nSTATEROOT_FREE_KB=52428800\n",
			},
		}

		audit, err := prov.Down(context.Background(), prof, Options{Client: cs, Logger: testLogger()})
		require.NoError(t, err)
		require.NotNil(t, audit)
		assert.True(t, audit.Passed)
		assert.Equal(t, 0, audit.ResidualSandboxCRs)
		assert.Equal(t, 0, audit.OrphanedUblkDevices)
		assert.Equal(t, 0, audit.LeakedFirecrackerPIDs)
		assert.InDelta(t, 50.0, audit.StateRootFreeHeadroomG, 0.1)

		// Verify helm uninstall was called and registry deployment deleted.
		require.Len(t, cmdRunner.calls, 1)
		assert.Equal(t, "helm", cmdRunner.calls[0].Name)
		assert.Equal(t, "uninstall", cmdRunner.calls[0].Args[0])

		_, err = cs.AppsV1().Deployments(benchNS).Get(context.Background(), ephemeralRegistryName, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("orphaned ublk and leaked firecracker PIDs fail audit", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(
			makeReadyNode("node-1", "amd64", true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: benchNS}},
		)
		cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			pod.Status.Phase = corev1.PodSucceeded
			return false, nil, nil
		})

		prov := NewOverlayBDProvisioner()
		prov.DynamicClient = newFakeDynamicClient()
		prov.CmdRunner = &mockCommandRunner{}
		prov.PollInterval = time.Millisecond
		prov.ReadyTimeout = 5 * time.Second
		prov.LogReader = &mockLogReader{
			logsByPod: map[string]string{
				"osb-leakaudit-node-1": "ORPHANED_UBLK=2\nLEAKED_FC_PIDS=1\nHOST_OOM_KILLS=0\nSTATEROOT_FREE_KB=20971520\n",
			},
		}

		audit, err := prov.Down(context.Background(), prof, Options{Client: cs, Logger: testLogger()})
		require.NoError(t, err)
		require.NotNil(t, audit)
		assert.False(t, audit.Passed)
		assert.Equal(t, 2, audit.OrphanedUblkDevices)
		assert.Equal(t, 1, audit.LeakedFirecrackerPIDs)
		assert.Len(t, audit.Failures, 2)
	})
}
