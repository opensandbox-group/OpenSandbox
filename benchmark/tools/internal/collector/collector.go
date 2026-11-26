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

package collector

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

const (
	DefaultSystemNamespace   = "opensandbox-system"
	DefaultResourceNamespace = "opensandbox-dataplane"
	DefaultBenchNamespace    = "opensandbox-bench"

	DefaultStateRoot   = "/var/lib/fast-sandbox/firecracker"
	DefaultUblkdSocket = "/run/fast-sandbox/overlaybd/ublkd.sock"

	DefaultFirecrackerRuntimeSelector = "app=fast-sandbox,component=firecracker-runtime"
	DefaultFirecrackerAgentContainer  = "firecracker-agent"
	DefaultUblkdContainer             = "overlaybd-ublkd"
	DefaultJanitorContainer           = "janitor"

	FastSandboxCRDGroup = "sandbox.fast.io"
)

var (
	// Primary fast-sandbox CRD GVRs (v1alpha2), with v1alpha1 fallback for older clusters.
	SandboxGVRV1Alpha2 = schema.GroupVersionResource{
		Group:    FastSandboxCRDGroup,
		Version:  "v1alpha2",
		Resource: "sandboxes",
	}
	SandboxGVRV1Alpha1 = schema.GroupVersionResource{
		Group:    FastSandboxCRDGroup,
		Version:  "v1alpha1",
		Resource: "sandboxes",
	}
	SandboxSnapshotGVRV1Alpha2 = schema.GroupVersionResource{
		Group:    FastSandboxCRDGroup,
		Version:  "v1alpha2",
		Resource: "sandboxsnapshots",
	}
	SandboxSnapshotGVRV1Alpha1 = schema.GroupVersionResource{
		Group:    FastSandboxCRDGroup,
		Version:  "v1alpha1",
		Resource: "sandboxsnapshots",
	}
)

// HTTPDoer abstracts http.Client for unit testing Prometheus and HTTP probes.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config configures namespace targets, paths, and timeouts for all collector components.
type Config struct {
	SystemNamespace   string
	ResourceNamespace string
	BenchNamespace    string
	StateRoot         string
	UblkdSocketPath   string
	PoolLow           int
	ScrapeTimeout     time.Duration
	ExecTimeout       time.Duration
}

// ConfigFromProfile derives a collector Config from a BenchmarkProfile with safe defaults.
func ConfigFromProfile(prof *profile.BenchmarkProfile) Config {
	cfg := DefaultConfig()
	if prof == nil {
		return cfg
	}
	if prof.Environment.SystemNamespace != "" {
		cfg.SystemNamespace = prof.Environment.SystemNamespace
	}
	if prof.Environment.ResourceNamespace != "" {
		cfg.ResourceNamespace = prof.Environment.ResourceNamespace
	}
	if prof.Environment.BenchNamespace != "" {
		cfg.BenchNamespace = prof.Environment.BenchNamespace
	}
	if prof.Stack.FastSandbox.Runtime.OCI.PoolLow > 0 {
		cfg.PoolLow = prof.Stack.FastSandbox.Runtime.OCI.PoolLow
	}
	return cfg
}

// DefaultConfig returns sensible Phase 1 OverlayBD defaults.
func DefaultConfig() Config {
	return Config{
		SystemNamespace:   DefaultSystemNamespace,
		ResourceNamespace: DefaultResourceNamespace,
		BenchNamespace:    DefaultBenchNamespace,
		StateRoot:         DefaultStateRoot,
		UblkdSocketPath:   DefaultUblkdSocket,
		PoolLow:           8,
		ScrapeTimeout:     5 * time.Second,
		ExecTimeout:       15 * time.Second,
	}
}

func (c *Config) applyDefaults() {
	def := DefaultConfig()
	if c.SystemNamespace == "" {
		c.SystemNamespace = def.SystemNamespace
	}
	if c.ResourceNamespace == "" {
		c.ResourceNamespace = def.ResourceNamespace
	}
	if c.BenchNamespace == "" {
		c.BenchNamespace = def.BenchNamespace
	}
	if c.StateRoot == "" {
		c.StateRoot = def.StateRoot
	}
	if c.UblkdSocketPath == "" {
		c.UblkdSocketPath = def.UblkdSocketPath
	}
	if c.ScrapeTimeout <= 0 {
		c.ScrapeTimeout = def.ScrapeTimeout
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = def.ExecTimeout
	}
}

// Clients bundles Kubernetes, dynamic, pod-exec, and HTTP clients shared across collector components.
type Clients struct {
	Clientset     kubernetes.Interface
	DynamicClient dynamic.Interface
	Execer        runner.PodExecer
	HTTPClient    HTTPDoer
	Logger        *slog.Logger
}

// NewClientsFromConfig builds production Clients from a Kubernetes rest.Config.
func NewClientsFromConfig(restCfg *rest.Config, log *slog.Logger) (*Clients, error) {
	if restCfg == nil {
		return nil, fmt.Errorf("rest.Config must be non-nil")
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Clients{
		Clientset:     cs,
		DynamicClient: dyn,
		Execer: &runner.SPDYPodExecer{
			RestConfig: restCfg,
			Clientset:  cs,
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Logger:     log,
	}, nil
}

func (c *Clients) logger() *slog.Logger {
	if c != nil && c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Clients) httpClient(timeout time.Duration) HTTPDoer {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// execCommand runs a command inside a pod container and returns stdout, stderr, and any error.
func execCommand(ctx context.Context, execer runner.PodExecer, namespace, podName, container string, cmd []string, timeout time.Duration) (string, string, error) {
	if execer == nil {
		return "", "", fmt.Errorf("pod execer is not configured")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var stdout, stderr bytes.Buffer
	err := execer.Exec(ctx, runner.ExecRequest{
		Namespace: namespace,
		PodName:   podName,
		Container: container,
		Command:   cmd,
		Stdout:    &stdout,
		Stderr:    &stderr,
	})
	return stdout.String(), stderr.String(), err
}

// listFirecrackerRuntimePods discovers running firecracker-runtime DaemonSet pods.
func listFirecrackerRuntimePods(ctx context.Context, cs kubernetes.Interface, namespaces ...string) ([]corev1.Pod, error) {
	if cs == nil {
		return nil, fmt.Errorf("kubernetes clientset is not configured")
	}
	seen := make(map[string]struct{})
	var pods []corev1.Pod
	for _, ns := range namespaces {
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: DefaultFirecrackerRuntimeSelector,
		})
		if err != nil {
			return nil, fmt.Errorf("list firecracker-runtime pods in %q: %w", ns, err)
		}
		for _, p := range list.Items {
			if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
				pods = append(pods, p)
			}
		}
		if len(pods) > 0 {
			return pods, nil
		}
	}
	return pods, nil
}

func hasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func pickRuntimeContainer(pod *corev1.Pod, preferred ...string) string {
	for _, name := range preferred {
		if hasContainer(pod, name) {
			return name
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

func podMatchesKeywords(pod *corev1.Pod, keywords ...string) bool {
	name := strings.ToLower(pod.Name)
	for _, kw := range keywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	for k, v := range pod.Labels {
		lv := strings.ToLower(v)
		lk := strings.ToLower(k)
		for _, kw := range keywords {
			if strings.Contains(lv, kw) || strings.Contains(lk, kw) {
				return true
			}
		}
	}
	return false
}
