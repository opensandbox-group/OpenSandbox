// Copyright 2025 Alibaba Group Holding Ltd.
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

package restart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// DefaultRestartCommand is the default command used to send SIGTERM to PID 1 inside a
// container. Containers using the Restart recycle strategy must have a PID 1 process that
// handles SIGTERM and exits gracefully (e.g., a real application server, not bare
// "sleep"). When PID 1 exits, the kubelet restarts the container per its restartPolicy.
var DefaultRestartCommand = []string{"kill", "1"}

const (
	// DefaultRetryInterval is the default minimum time between consecutive restart attempts.
	DefaultRetryInterval = 30 * time.Second
	// DefaultMaxRetries is the default maximum number of restart attempts.
	DefaultMaxRetries int32 = 3
	// DefaultExecTimeout is the default timeout for executing the restart command inside a container.
	DefaultExecTimeout = 10 * time.Second
)

// restartConfig is the implementation-specific configuration parsed from Pool annotations.
type restartConfig struct {
	// Blacklist contains container names to exclude from restart.
	Blacklist []string `json:"blacklist,omitempty"`
	// RetryInterval is the minimum duration between consecutive restart attempts.
	RetryInterval string `json:"retryInterval,omitempty"`
	// MaxRetries is the maximum number of restart attempts before marking the pod as failed.
	MaxRetries int32 `json:"maxRetries,omitempty"`
	// RestartCommand overrides the default command used to restart a container.
	// If empty, DefaultRestartCommand is used.
	RestartCommand []string `json:"restartCommand,omitempty"`
}

// parseConfig parses restart configuration from Pool annotations.
// Missing or unparseable fields fall back to defaults.
func parseConfig(ctx context.Context, annotations map[string]string) restartConfig {
	log := logf.FromContext(ctx)
	var cfg restartConfig
	if raw, ok := annotations[AnnoRestartConfigKey]; ok {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			log.Error(err, "Failed to parse restart config annotation, falling back to defaults",
				"annotation", AnnoRestartConfigKey, "value", raw)
		}
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if _, err := time.ParseDuration(cfg.RetryInterval); err != nil || cfg.RetryInterval == "" {
		cfg.RetryInterval = DefaultRetryInterval.String()
	}
	if len(cfg.RestartCommand) == 0 {
		cfg.RestartCommand = DefaultRestartCommand
	}
	return cfg
}

// restartInfo is stored in the pod annotation and updated by TryRestart.
// It only contains state that must survive across reconcile calls: the sandbox
// identity, the pre-restart container IDs for change detection, and retry
// bookkeeping. Configuration (retryInterval, maxRetries, restartCommand) is
// derived from Pool annotations on every call and is not persisted here.
type restartInfo struct {
	ID            string            `json:"id"`
	StartTime     string            `json:"startTime"`
	Containers    map[string]string `json:"containers"`
	LastRetryTime string            `json:"lastRetryTime,omitempty"`
	RetryCount    int               `json:"retryCount,omitempty"`
}

// containerExec abstracts pod exec so that TryRestart can be unit-tested without
// a real Kubernetes API server.
type containerExec interface {
	exec(ctx context.Context, pod *corev1.Pod, containerName string, command []string) error
}

// defaultRestartHandler implements Handler using pod exec to execute a configurable
// restart command inside each container.
type defaultRestartHandler struct {
	client      client.Client
	execTimeout time.Duration
	exec        containerExec
}

// spdyContainerExec is the production containerExec that streams commands via SPDY.
type spdyContainerExec struct {
	kubeClient  kubernetes.Interface
	restConfig  *rest.Config
	execTimeout time.Duration
}

func (e *spdyContainerExec) exec(ctx context.Context, pod *corev1.Pod, containerName string, command []string) error {
	ctx, cancel := context.WithTimeout(ctx, e.execTimeout)
	defer cancel()

	req := e.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, k8sscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create spdy executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("failed to exec restart command in container %s (stdout=%q stderr=%q): %w",
			containerName, stdout.String(), stderr.String(), err)
	}
	return nil
}

// NewDefaultRestartHandler creates a RestartHandler that restarts containers by
// executing a configurable command inside the container via pod exec.
// execTimeout caps the time allowed for each exec call; use DefaultExecTimeout if unsure.
func NewDefaultRestartHandler(c client.Client, restConfig *rest.Config, execTimeout time.Duration) (Handler, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("restConfig is required for defaultRestartHandler")
	}
	if execTimeout <= 0 {
		execTimeout = DefaultExecTimeout
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	return &defaultRestartHandler{
		client:      c,
		execTimeout: execTimeout,
		exec:        &spdyContainerExec{kubeClient: kubeClient, restConfig: restConfig, execTimeout: execTimeout},
	}, nil
}

// TryRestart initiates or drives forward the restart state machine for the given pool and pod.
// On the first call (no annotation present), it initializes the restart record using opts
// and issues the first restart attempt.
// On subsequent calls, opts is ignored and the existing record drives the state machine.
// Configuration (retryInterval, maxRetries, restartCommand) is always read fresh from
// pool annotations so that operator changes take effect without re-creating the record.
func (h *defaultRestartHandler) TryRestart(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, opts *Spec) (*Status, error) {
	if pod.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("pod %s/%s is not running (phase: %s)", pod.Namespace, pod.Name, pod.Status.Phase)
	}

	var annotations map[string]string
	if pool != nil {
		annotations = pool.GetAnnotations()
	}
	cfg := parseConfig(ctx, annotations)

	// If no annotation exists yet, or the existing record belongs to a different sandbox,
	// initialize a new restart record and issue the first restart attempt.
	info, err := h.loadInfo(pod)
	if err != nil || info.ID != opts.ID {
		info = h.initInfo(cfg, pod, opts)
		if err := h.persistInfo(ctx, pod, info); err != nil {
			return nil, fmt.Errorf("failed to initialize restart record: %w", err)
		}
	}

	if h.evalState(pod, info) == StateSucceeded {
		return &Status{
			StartTime:  &info.StartTime,
			RetryCount: info.RetryCount,
			State:      StateSucceeded,
			Message:    "restart already succeeded",
		}, nil
	}

	maxRetries := cfg.MaxRetries
	if int32(info.RetryCount) >= maxRetries {
		return &Status{
			StartTime:  &info.StartTime,
			RetryCount: info.RetryCount,
			State:      StateFailed,
			Message:    fmt.Sprintf("max retries (%d) exceeded", maxRetries),
		}, nil
	}

	retryInterval, _ := time.ParseDuration(cfg.RetryInterval)

	if info.LastRetryTime != "" {
		if lastRetry, err := time.Parse(time.RFC3339, info.LastRetryTime); err == nil {
			if time.Since(lastRetry) < retryInterval {
				return &Status{
					StartTime:  &info.StartTime,
					RetryCount: info.RetryCount,
					State:      StateRestarting,
					Message:    fmt.Sprintf("retry interval not elapsed, next retry after %s", lastRetry.Add(retryInterval).Format(time.RFC3339)),
				}, nil
			}
		}
	}

	return h.doRetry(ctx, pod, info, cfg)
}

// initInfo builds the initial restart record from the parsed config, pod status, and options.
func (h *defaultRestartHandler) initInfo(cfg restartConfig, pod *corev1.Pod, opts *Spec) *restartInfo {
	var blacklist sets.Set[string]
	id := ""

	if opts != nil {
		id = opts.ID
	}
	if len(cfg.Blacklist) > 0 {
		blacklist = sets.New(cfg.Blacklist...)
	}

	var containers []string
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil && !blacklist.Has(cs.Name) {
			containers = append(containers, cs.Name)
		}
	}
	return h.buildInfo(pod, id, containers)
}

func (h *defaultRestartHandler) doRetry(ctx context.Context, pod *corev1.Pod, info *restartInfo, cfg restartConfig) (*Status, error) {
	if len(info.Containers) == 0 {
		return &Status{
			StartTime: &info.StartTime,
			State:     StateSucceeded,
			Message:   "no containers to restart",
		}, nil
	}
	// Execute the restart command in each container.  Exec errors are logged but do not
	// abort the retry: RetryCount and LastRetryTime are always persisted so that
	// retryInterval and maxRetries are enforced even when exec fails.
	log := logf.FromContext(ctx)
	for containerName := range info.Containers {
		if err := h.exec.exec(ctx, pod, containerName, cfg.RestartCommand); err != nil {
			log.Error(err, "Failed to exec restart command in container",
				"pod", pod.Name, "container", containerName)
		}
	}

	info.LastRetryTime = time.Now().Format(time.RFC3339)
	info.RetryCount++
	if err := h.persistInfo(ctx, pod, info); err != nil {
		return nil, fmt.Errorf("failed to persist retry metadata: %w", err)
	}

	return &Status{
		StartTime:  &info.StartTime,
		RetryCount: info.RetryCount,
		State:      StateRestarting,
		Message:    fmt.Sprintf("retry #%d sent to %d container(s), waiting for restart", info.RetryCount, len(info.Containers)),
	}, nil
}

func (h *defaultRestartHandler) evalState(pod *corev1.Pod, info *restartInfo) State {
	currentStatuses := make(map[string]corev1.ContainerStatus)
	for _, cs := range pod.Status.ContainerStatuses {
		currentStatuses[cs.Name] = cs
	}
	for containerName, containerID := range info.Containers {
		current, ok := currentStatuses[containerName]
		if !ok {
			return StateRestarting
		}
		if current.ContainerID == containerID || current.State.Running == nil {
			return StateRestarting
		}
	}
	return StateSucceeded
}

func (h *defaultRestartHandler) buildInfo(pod *corev1.Pod, id string, containers []string) *restartInfo {
	statusIndex := make(map[string]corev1.ContainerStatus)
	for _, cs := range pod.Status.ContainerStatuses {
		statusIndex[cs.Name] = cs
	}
	containerIds := make(map[string]string, len(containers))
	for _, name := range containers {
		containerIds[name] = statusIndex[name].ContainerID
	}
	return &restartInfo{
		ID:         id,
		StartTime:  time.Now().Format(time.RFC3339),
		Containers: containerIds,
	}
}

func (h *defaultRestartHandler) persistInfo(ctx context.Context, pod *corev1.Pod, info *restartInfo) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal restart info: %w", err)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				AnnoRestartRecordKey: string(raw),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}
	obj := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name}}
	return h.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

func (h *defaultRestartHandler) loadInfo(pod *corev1.Pod) (*restartInfo, error) {
	if pod.Annotations == nil {
		return nil, fmt.Errorf("pod %s/%s has no restart info annotation", pod.Namespace, pod.Name)
	}
	raw, ok := pod.Annotations[AnnoRestartRecordKey]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s has no restart info annotation", pod.Namespace, pod.Name)
	}
	var info restartInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal restart info: %w", err)
	}
	return &info, nil
}
