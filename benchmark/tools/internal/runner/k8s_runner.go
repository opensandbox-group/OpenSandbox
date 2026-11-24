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

package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

const (
	DefaultRunnerPodName       = "opensandbox-bench-runner"
	DefaultRunnerContainerName = "runner"
	DefaultRemoteBinaryPath    = "/tmp/opensandbox-bench"
	DefaultRemoteProfilePath   = "/tmp/bench-profile.yaml"
	DefaultRemoteBundleDir     = "/tmp/run-bundle"
	FirecrackerNodeLabelKey    = "fast-sandbox.io/firecracker-node"
)

// ExecRequest describes a single command executed inside a container via pods/exec.
type ExecRequest struct {
	Namespace string
	PodName   string
	Container string
	Command   []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

// PodExecer abstracts Kubernetes pods/exec streaming so K8sRunner can be unit-tested without a live API server.
type PodExecer interface {
	Exec(ctx context.Context, req ExecRequest) error
}

// SPDYPodExecer implements PodExecer using client-go SPDY remotecommand.
type SPDYPodExecer struct {
	RestConfig *rest.Config
	Clientset  kubernetes.Interface
}

func (e *SPDYPodExecer) Exec(ctx context.Context, req ExecRequest) error {
	if e.RestConfig == nil || e.Clientset == nil {
		return fmt.Errorf("SPDYPodExecer requires non-nil RestConfig and Clientset")
	}
	postReq := e.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(req.PodName).
		Namespace(req.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: req.Container,
			Command:   req.Command,
			Stdin:     req.Stdin != nil,
			Stdout:    req.Stdout != nil,
			Stderr:    req.Stderr != nil,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.RestConfig, http.MethodPost, postReq.URL())
	if err != nil {
		return fmt.Errorf("create SPDY executor: %w", err)
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  req.Stdin,
		Stdout: req.Stdout,
		Stderr: req.Stderr,
	})
}

// K8sRunner implements RemoteRunner using client-go and pods/exec tar streaming.
type K8sRunner struct {
	Clientset    kubernetes.Interface
	Execer       PodExecer
	Logger       *slog.Logger
	PollInterval time.Duration
	ReadyTimeout time.Duration
}

func NewK8sRunner(kc *KubeClients, log *slog.Logger) *K8sRunner {
	if log == nil {
		log = slog.Default()
	}
	return &K8sRunner{
		Clientset: kc.Clientset,
		Execer: &SPDYPodExecer{
			RestConfig: kc.RestConfig,
			Clientset:  kc.Clientset,
		},
		Logger:       log,
		PollInterval: 500 * time.Millisecond,
		ReadyTimeout: 2 * time.Minute,
	}
}

// EnsureRunnerPod ensures the benchmark namespace and Ready Runner Pod exist,
// reusing an already-running pod or recreating a terminated one.
func (r *K8sRunner) EnsureRunnerPod(ctx context.Context, prof *profile.BenchmarkProfile) (string, error) {
	ns := prof.Environment.BenchNamespace
	if ns == "" {
		ns = "opensandbox-bench"
	}
	if err := r.ensureNamespace(ctx, ns); err != nil {
		return "", err
	}

	podName := DefaultRunnerPodName
	existing, err := r.Clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		switch existing.Status.Phase {
		case corev1.PodRunning:
			if isPodReady(existing) {
				r.Logger.Info("reusing existing runner pod", "namespace", ns, "pod", podName)
				return podName, nil
			}
			// Pod is Running but not yet Ready; wait for readiness below.
		case corev1.PodPending:
			// Wait for readiness below.
		default:
			// Failed / Succeeded / Unknown: recreate cleanly.
			r.Logger.Info("recreating terminated runner pod", "namespace", ns, "pod", podName, "phase", existing.Status.Phase)
			if delErr := r.DeleteRunnerPod(ctx, ns, podName); delErr != nil {
				return "", delErr
			}
			if createErr := r.createRunnerPod(ctx, ns, podName, prof); createErr != nil {
				return "", createErr
			}
		}
	} else if apierrors.IsNotFound(err) {
		if createErr := r.createRunnerPod(ctx, ns, podName, prof); createErr != nil {
			return "", createErr
		}
	} else {
		return "", fmt.Errorf("get runner pod %s/%s: %w", ns, podName, err)
	}

	if err := r.waitForPodReady(ctx, ns, podName); err != nil {
		return "", err
	}
	return podName, nil
}

// DeleteRunnerPod removes the Runner Pod if it exists.
func (r *K8sRunner) DeleteRunnerPod(ctx context.Context, namespace, podName string) error {
	grace := int64(0)
	err := r.Clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete runner pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// DetectClusterArch determines the GOARCH (amd64 / arm64) of the node hosting the Runner Pod.
func (r *K8sRunner) DetectClusterArch(ctx context.Context, namespace, podName string) (string, error) {
	pod, err := r.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get runner pod %s/%s: %w", namespace, podName, err)
	}
	if pod.Spec.NodeName == "" {
		return "amd64", nil
	}
	node, err := r.Clientset.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get node %q for runner pod: %w", pod.Spec.NodeName, err)
	}
	arch := node.Labels[corev1.LabelArchStable]
	if arch == "" {
		arch = node.Status.NodeInfo.Architecture
	}
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "arm64", "aarch64":
		return "arm64", nil
	case "amd64", "x86_64", "":
		return "amd64", nil
	default:
		return arch, nil
	}
}

// InjectBinary streams a local static binary into the Runner Pod at remotePath with 0755 permissions.
func (r *K8sRunner) InjectBinary(ctx context.Context, namespace, podName, localBinaryPath, remotePath string) error {
	return r.injectTarStream(ctx, namespace, podName, remotePath, func(w io.Writer) error {
		return WriteSingleFileTar(w, localBinaryPath, path.Base(remotePath), 0o755)
	})
}

// InjectBytes streams in-memory content (e.g. BenchmarkProfile YAML) into the Runner Pod at remotePath.
func (r *K8sRunner) InjectBytes(ctx context.Context, namespace, podName string, data []byte, remotePath string, mode int64) error {
	return r.injectTarStream(ctx, namespace, podName, remotePath, func(w io.Writer) error {
		return WriteBytesTar(w, path.Base(remotePath), data, mode)
	})
}

func (r *K8sRunner) injectTarStream(ctx context.Context, namespace, podName, remotePath string, writeTar func(io.Writer) error) error {
	remoteDir := path.Dir(remotePath)
	if remoteDir == "" || remoteDir == "." {
		remoteDir = "/tmp"
	}

	var tarBuf bytes.Buffer
	if err := writeTar(&tarBuf); err != nil {
		return err
	}

	var stderr bytes.Buffer
	cmd := []string{"sh", "-c", fmt.Sprintf("mkdir -p %s && tar -xf - -C %s", shellQuote(remoteDir), shellQuote(remoteDir))}
	err := r.Execer.Exec(ctx, ExecRequest{
		Namespace: namespace,
		PodName:   podName,
		Container: DefaultRunnerContainerName,
		Command:   cmd,
		Stdin:     &tarBuf,
		Stderr:    &stderr,
	})
	if err != nil {
		return fmt.Errorf("inject %q into %s/%s: %w (stderr: %s)", remotePath, namespace, podName, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ExecWorker runs the injected opensandbox-bench binary inside the Runner Pod, streaming stdout/stderr.
func (r *K8sRunner) ExecWorker(ctx context.Context, namespace, podName, remoteBinaryPath string, args []string, stdout, stderr io.Writer) error {
	cmd := append([]string{remoteBinaryPath}, args...)
	return r.Execer.Exec(ctx, ExecRequest{
		Namespace: namespace,
		PodName:   podName,
		Container: DefaultRunnerContainerName,
		Command:   cmd,
		Stdout:    stdout,
		Stderr:    stderr,
	})
}

// PullBundle streams remoteBundleDir out of the Runner Pod as a tar archive and unpacks it to localOutputDir.
func (r *K8sRunner) PullBundle(ctx context.Context, namespace, podName, remoteBundleDir, localOutputDir string) error {
	var tarOut bytes.Buffer
	var stderr bytes.Buffer

	cmd := []string{"tar", "-cf", "-", "-C", remoteBundleDir, "."}
	err := r.Execer.Exec(ctx, ExecRequest{
		Namespace: namespace,
		PodName:   podName,
		Container: DefaultRunnerContainerName,
		Command:   cmd,
		Stdout:    &tarOut,
		Stderr:    &stderr,
	})
	if err != nil {
		return fmt.Errorf("pull bundle %q from %s/%s: %w (stderr: %s)", remoteBundleDir, namespace, podName, err, strings.TrimSpace(stderr.String()))
	}
	return ExtractTar(&tarOut, localOutputDir)
}

func (r *K8sRunner) ensureNamespace(ctx context.Context, ns string) error {
	_, err := r.Clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get namespace %q: %w", ns, err)
	}
	_, err = r.Clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "opensandbox-bench",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %q: %w", ns, err)
	}
	return nil
}

func (r *K8sRunner) createRunnerPod(ctx context.Context, ns, podName string, prof *profile.BenchmarkProfile) error {
	pod, err := BuildRunnerPodSpec(ns, podName, prof)
	if err != nil {
		return err
	}
	r.Logger.Info("creating runner pod", "namespace", ns, "pod", podName, "image", pod.Spec.Containers[0].Image)
	_, err = r.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create runner pod %s/%s: %w", ns, podName, err)
	}
	return nil
}

// BuildRunnerPodSpec constructs the in-cluster Bench Bastion/Runner Pod specification.
// It prefers scheduling off fast-sandbox Firecracker runtime nodes to avoid contending with microVMs.
func BuildRunnerPodSpec(namespace, podName string, prof *profile.BenchmarkProfile) (*corev1.Pod, error) {
	image := prof.Environment.Runner.BaseImage
	if image == "" {
		image = "alpine:3.20"
	}
	limits := corev1.ResourceList{}
	requests := corev1.ResourceList{}
	if prof.Environment.Runner.CPULimit != "" {
		q, err := resource.ParseQuantity(prof.Environment.Runner.CPULimit)
		if err != nil {
			return nil, fmt.Errorf("parse runner.cpuLimit %q: %w", prof.Environment.Runner.CPULimit, err)
		}
		limits[corev1.ResourceCPU] = q
		requests[corev1.ResourceCPU] = q
	}
	if prof.Environment.Runner.MemoryLimit != "" {
		q, err := resource.ParseQuantity(prof.Environment.Runner.MemoryLimit)
		if err != nil {
			return nil, fmt.Errorf("parse runner.memoryLimit %q: %w", prof.Environment.Runner.MemoryLimit, err)
		}
		limits[corev1.ResourceMemory] = q
		requests[corev1.ResourceMemory] = q
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "opensandbox-bench-runner",
				"app.kubernetes.io/managed-by": "opensandbox-bench",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
						{
							Weight: 100,
							Preference: corev1.NodeSelectorTerm{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      FirecrackerNodeLabelKey,
										Operator: corev1.NodeSelectorOpNotIn,
										Values:   []string{"true"},
									},
								},
							},
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    DefaultRunnerContainerName,
					Image:   image,
					Command: []string{"sh", "-c", "trap 'exit 0' TERM INT; while true; do sleep 3600; done"},
					Resources: corev1.ResourceRequirements{
						Requests: requests,
						Limits:   limits,
					},
				},
			},
		},
	}, nil
}

func (r *K8sRunner) waitForPodReady(ctx context.Context, ns, podName string) error {
	timeout := r.ReadyTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		pod, err := r.Clientset.CoreV1().Pods(ns).Get(waitCtx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("poll runner pod %s/%s: %w", ns, podName, err)
		}
		if pod.Status.Phase == corev1.PodRunning && isPodReady(pod) {
			return nil
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("runner pod %s/%s exited prematurely in phase %s", ns, podName, pod.Status.Phase)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for runner pod %s/%s to become Ready: %w", ns, podName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
