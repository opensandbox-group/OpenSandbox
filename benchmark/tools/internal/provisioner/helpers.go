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
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

const (
	defaultHelmReleaseName = "opensandbox"
	defaultPollInterval    = 2 * time.Second
	defaultReadyTimeout    = 10 * time.Minute
	defaultStateRoot       = "/var/lib/fast-sandbox/firecracker"
	defaultUblkdSocketPath = "/run/fast-sandbox/overlaybd/ublkd.sock"
	defaultProbeImage      = "alpine:3.20"

	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "opensandbox-bench"

	firecrackerNodeLabelKey    = "fast-sandbox.io/firecracker-node"
	firecrackerReadyCondition  = corev1.NodeConditionType("FirecrackerReady")
	runnerServiceAccountName   = RunnerServiceAccountName
	runnerClusterRoleName      = RunnerServiceAccountName
	ephemeralRegistryName      = "opensandbox-bench-registry"
	ephemeralRegistryPort      = 5000
	ephemeralRegistryTLSSecret = "opensandbox-bench-registry-tls"
	ephemeralRegistryCASecret  = "opensandbox-bench-registry-ca"
	agentRegistrySecretName    = "fast-sandbox-agent-registry"
	ociPublishSecretName       = "template-oci-publish-secret"
	ephemeralRedisName               = "opensandbox-bench-redis"
	ephemeralRedisPort               = 6379
	otelObsNamespace                 = "opensandbox-bench-obs"
	otelJaegerName                   = "opensandbox-bench-jaeger"
	otelJaegerImage                  = "jaegertracing/jaeger:2.4.0"
	otelJaegerOTLPGRPCPort           = 4317
	otelJaegerQueryHTTPPort          = 16686
	otelCollectorName                = "opensandbox-bench-otel-collector"
	otelCollectorConfigMapName       = "opensandbox-bench-otel-collector-config"
	otelCollectorImage               = "otel/opentelemetry-collector-contrib:0.120.0"
	otelCollectorOTLPGRPCPort        = 4317
	otelCollectorOTLPHTTPPort        = 4318
	otelCollectorPrometheusPort      = 8889
	defaultTemplateName              = "opensandbox-bench-golden"
	defaultPoolName                  = "opensandbox-bench-pool"
	fastSandboxLoggingConfigMapName  = "fast-sandbox-logging"
	fastSandboxRegistryConfigMapName = "fast-sandbox-registry"
	secureAccessSecretName           = "opensandbox-bench-secure-access"
)

var (
	sandboxGVR = schema.GroupVersionResource{
		Group:    "sandbox.fast.io",
		Version:  "v1alpha2",
		Resource: "sandboxes",
	}
	sandboxSnapshotGVR = schema.GroupVersionResource{
		Group:    "sandbox.fast.io",
		Version:  "v1alpha2",
		Resource: "sandboxsnapshots",
	}
	sandboxPoolGVR = schema.GroupVersionResource{
		Group:    "sandbox.fast.io",
		Version:  "v1alpha2",
		Resource: "sandboxpools",
	}
	sandboxTemplateGVR = schema.GroupVersionResource{
		Group:    "sandbox.fast.io",
		Version:  "v1alpha2",
		Resource: "sandboxtemplates",
	}
)

// CommandRunner abstracts external CLI invocations (e.g. helm) for unit testing.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// PodExecer abstracts Kubernetes pods/exec commands so runtime probes can be unit tested.
type PodExecer interface {
	Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout, stderr string, err error)
}

type spdyPodExecer struct {
	restConfig *rest.Config
	clientset  kubernetes.Interface
}

func (e *spdyPodExecer) Exec(ctx context.Context, namespace, podName, container string, command []string) (string, string, error) {
	if e.restConfig == nil || e.clientset == nil {
		return "", "", fmt.Errorf("SPDY pod execer requires non-nil restConfig and clientset")
	}
	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.restConfig, http.MethodPost, req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create SPDY executor for %s/%s: %w", namespace, podName, err)
	}
	var outBuf, errBuf bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	return outBuf.String(), errBuf.String(), err
}

// PodLogReader abstracts reading pod container logs so preflight/audit pods can be unit tested with fake clients.
type PodLogReader interface {
	ReadLogs(ctx context.Context, client kubernetes.Interface, namespace, podName, container string) (string, error)
}

type defaultPodLogReader struct{}

func (defaultPodLogReader) ReadLogs(ctx context.Context, client kubernetes.Interface, namespace, podName, container string) (string, error) {
	opts := &corev1.PodLogOptions{Container: container}
	stream, err := client.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for %s/%s: %w", namespace, podName, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("read logs for %s/%s: %w", namespace, podName, err)
	}
	return string(data), nil
}

type resolvedClients struct {
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	execer PodExecer
	logs   PodLogReader
	cmd    CommandRunner
	logger *slog.Logger
}

func (p *OverlayBDProvisioner) resolveClients(opts Options) (*resolvedClients, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	kube := opts.Client
	dyn := p.DynamicClient
	execer := p.PodExecer

	restCfg := opts.RestConfig
	needRestCfg := restCfg == nil && (kube == nil || dyn == nil || execer == nil)
	if needRestCfg {
		cfg, err := loadRestConfig(opts.Kubeconfig)
		if err == nil {
			restCfg = cfg
		} else if kube == nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}

	if kube == nil {
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, fmt.Errorf("create kubernetes clientset: %w", err)
		}
		kube = cs
	}
	if dyn == nil && restCfg != nil {
		dc, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			return nil, fmt.Errorf("create dynamic client: %w", err)
		}
		dyn = dc
	}
	if execer == nil && restCfg != nil {
		execer = &spdyPodExecer{restConfig: restCfg, clientset: kube}
	}

	cmdRunner := p.CmdRunner
	if cmdRunner == nil {
		cmdRunner = execCommandRunner{}
	}
	logReader := p.LogReader
	if logReader == nil {
		logReader = defaultPodLogReader{}
	}

	return &resolvedClients{
		kube:   kube,
		dyn:    dyn,
		execer: execer,
		logs:   logReader,
		cmd:    cmdRunner,
		logger: logger,
	}, nil
}

func loadRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if envKC := os.Getenv("KUBECONFIG"); envKC != "" {
		return clientcmd.BuildConfigFromFlags("", envKC)
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	home, _ := os.UserHomeDir()
	return clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
}

func (p *OverlayBDProvisioner) pollInterval() time.Duration {
	if p.PollInterval > 0 {
		return p.PollInterval
	}
	return defaultPollInterval
}

func (p *OverlayBDProvisioner) readyTimeout() time.Duration {
	if p.ReadyTimeout > 0 {
		return p.ReadyTimeout
	}
	return defaultReadyTimeout
}

func (p *OverlayBDProvisioner) helmReleaseName() string {
	if strings.TrimSpace(p.HelmRelease) != "" {
		return p.HelmRelease
	}
	return defaultHelmReleaseName
}

func resolveNamespaces(prof *profile.BenchmarkProfile) (systemNS, resourceNS, benchNS string) {
	systemNS = prof.Environment.SystemNamespace
	if systemNS == "" {
		systemNS = "opensandbox-system"
	}
	resourceNS = prof.Environment.ResourceNamespace
	if resourceNS == "" {
		resourceNS = "opensandbox-dataplane"
	}
	benchNS = prof.Environment.BenchNamespace
	if benchNS == "" {
		benchNS = "opensandbox-bench"
	}
	return systemNS, resourceNS, benchNS
}

func resolveProbeImage(prof *profile.BenchmarkProfile) string {
	img := defaultProbeImage
	if prof != nil && strings.TrimSpace(prof.Environment.Runner.BaseImage) != "" {
		img = prof.Environment.Runner.BaseImage
	}
	return applyImageMirror(prof, img)
}

func applyImageMirror(prof *profile.BenchmarkProfile, image string) string {
	if prof == nil {
		return image
	}
	mirror := strings.TrimRight(strings.TrimSpace(prof.Environment.ImageRegistryMirror), "/")
	if mirror == "" || strings.HasPrefix(image, mirror+"/") {
		return image
	}
	// Only prefix unqualified / docker.io images; leave explicit third-party registries untouched.
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		return image
	}
	return mirror + "/" + image
}

func ensureNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	_, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get namespace %q: %w", name, err)
	}
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %q: %w", name, err)
	}
	return nil
}

func upsertConfigMap(ctx context.Context, client kubernetes.Interface, cm *corev1.ConfigMap) error {
	existing, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create configmap %s/%s: %w", cm.Namespace, cm.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get configmap %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	cm.ResourceVersion = existing.ResourceVersion
	if _, err = client.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update configmap %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	return nil
}

func upsertSecret(ctx context.Context, client kubernetes.Interface, secret *corev1.Secret) error {
	existing, err := client.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	secret.ResourceVersion = existing.ResourceVersion
	_, err = client.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}

func upsertServiceAccount(ctx context.Context, client kubernetes.Interface, sa *corev1.ServiceAccount) error {
	_, err := client.CoreV1().ServiceAccounts(sa.Namespace).Get(ctx, sa.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(sa.Namespace).Create(ctx, sa, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create serviceaccount %s/%s: %w", sa.Namespace, sa.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get serviceaccount %s/%s: %w", sa.Namespace, sa.Name, err)
	}
	return nil
}

func upsertClusterRole(ctx context.Context, client kubernetes.Interface, cr *rbacv1.ClusterRole) error {
	existing, err := client.RbacV1().ClusterRoles().Get(ctx, cr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create clusterrole %q: %w", cr.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get clusterrole %q: %w", cr.Name, err)
	}
	cr.ResourceVersion = existing.ResourceVersion
	_, err = client.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update clusterrole %q: %w", cr.Name, err)
	}
	return nil
}

func upsertClusterRoleBinding(ctx context.Context, client kubernetes.Interface, crb *rbacv1.ClusterRoleBinding) error {
	existing, err := client.RbacV1().ClusterRoleBindings().Get(ctx, crb.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create clusterrolebinding %q: %w", crb.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get clusterrolebinding %q: %w", crb.Name, err)
	}
	crb.ResourceVersion = existing.ResourceVersion
	_, err = client.RbacV1().ClusterRoleBindings().Update(ctx, crb, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update clusterrolebinding %q: %w", crb.Name, err)
	}
	return nil
}

func upsertDeployment(ctx context.Context, client kubernetes.Interface, dep *appsv1.Deployment) error {
	existing, err := client.AppsV1().Deployments(dep.Namespace).Get(ctx, dep.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.AppsV1().Deployments(dep.Namespace).Create(ctx, dep, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create deployment %s/%s: %w", dep.Namespace, dep.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get deployment %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	dep.ResourceVersion = existing.ResourceVersion
	_, err = client.AppsV1().Deployments(dep.Namespace).Update(ctx, dep, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update deployment %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	return nil
}

func upsertService(ctx context.Context, client kubernetes.Interface, svc *corev1.Service) error {
	existing, err := client.CoreV1().Services(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().Services(svc.Namespace).Create(ctx, svc, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	svc.ResourceVersion = existing.ResourceVersion
	svc.Spec.ClusterIP = existing.Spec.ClusterIP
	_, err = client.CoreV1().Services(svc.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return nil
}

func ensurePVC(ctx context.Context, client kubernetes.Interface, pvc *corev1.PersistentVolumeClaim) error {
	_, err := client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get pvc %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}
	_, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pvc %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}
	return nil
}

func nonFirecrackerNodeAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 100,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      firecrackerNodeLabelKey,
								Operator: corev1.NodeSelectorOpNotIn,
								Values:   []string{"true"},
							},
						},
					},
				},
			},
		},
	}
}

func boolPtr(v bool) *bool    { return &v }
func int32Ptr(v int32) *int32 { return &v }
func int64Ptr(v int64) *int64 { return &v }

func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isContainerReady(pod *corev1.Pod, containerName string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName {
			return cs.Ready
		}
	}
	return false
}

func selectCandidateNodes(nodes []corev1.Node) []corev1.Node {
	var ready []corev1.Node
	var firecracker []corev1.Node
	for _, n := range nodes {
		if !isNodeReady(&n) {
			continue
		}
		ready = append(ready, n)
		if n.Labels[firecrackerNodeLabelKey] == "true" {
			firecracker = append(firecracker, n)
		}
	}
	if len(firecracker) > 0 {
		return firecracker
	}
	// Exclude control-plane-only tainted nodes when worker nodes exist.
	var workers []corev1.Node
	for _, n := range ready {
		if !hasNoScheduleControlPlaneTaint(&n) {
			workers = append(workers, n)
		}
	}
	if len(workers) > 0 {
		return workers
	}
	return ready
}

func hasNoScheduleControlPlaneTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if (t.Key == "node-role.kubernetes.io/control-plane" || t.Key == "node-role.kubernetes.io/master") &&
			t.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func normalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "arm64", "aarch64":
		return "arm64"
	case "amd64", "x86_64", "":
		return "amd64"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}
