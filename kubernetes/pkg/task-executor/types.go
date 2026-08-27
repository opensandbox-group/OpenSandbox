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

package task_executor

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Task represents the internal local task resource (LocalTask)
// It follows the Kubernetes resource model with Metadata, Spec, and Status.
type Task struct {
	Name              string       `json:"name"`
	DeletionTimestamp *metav1.Time `json:"deletionTimestamp,omitempty"`

	Process         *Process                `json:"process,omitempty"`
	PodTemplateSpec *corev1.PodTemplateSpec `json:"podTemplateSpec,omitempty"`

	ProcessStatus *ProcessStatus    `json:"processStatus,omitempty"`
	PodStatus     *corev1.PodStatus `json:"podStatus,omitempty"`
}

// ExecMode defines where a process should be executed.
type ExecMode string

const (
	// ExecModeLocal executes in the task-executor container.
	ExecModeLocal ExecMode = "Local"
	// ExecModeRemote executes in the main container via nsenter.
	ExecModeRemote ExecMode = "Remote"
)

type Process struct {
	// Command command
	Command []string `json:"command"`
	// Arguments to the entrypoint.
	Args []string `json:"args,omitempty"`
	// List of environment variables to set in the process.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// WorkingDir process working directory.
	WorkingDir string `json:"workingDir,omitempty"`
	// TimeoutSeconds process timeout seconds.
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
	// ExecMode controls where the process runs. If empty, execution follows the
	// task-executor sidecar mode configuration.
	ExecMode ExecMode `json:"execMode,omitempty"`
	// Lifecycle defines actions to be executed before and after the main process.
	Lifecycle *ProcessLifecycle `json:"lifecycle,omitempty"`
}

// ProcessLifecycle defines lifecycle hooks for a process.
type ProcessLifecycle struct {
	PreStart *LifecycleHandler `json:"preStart,omitempty"`
	PostStop *LifecycleHandler `json:"postStop,omitempty"`
}

// LifecycleHandler defines a lifecycle action.
type LifecycleHandler struct {
	Exec *ExecAction `json:"exec,omitempty"`
	// ExecMode controls where the hook runs. If empty, the hook runs locally in
	// the task-executor container.
	ExecMode ExecMode `json:"execMode,omitempty"`
	// TimeoutSeconds is the maximum number of seconds the hook may run.
	// If the hook does not complete within this time, it is killed and the
	// enclosing operation (Start or Stop) is treated as failed.
	// If not set or zero, there is no timeout.
	// +optional
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

// ExecAction describes a "run in container" action.
type ExecAction struct {
	Command []string `json:"command"`
}

// ProcessStatus holds a possible state of process.
// Only one of its members may be specified.
// If none of them is specified, the default one is Waiting.
type ProcessStatus struct {
	// Details about a waiting process
	// +optional
	Waiting *Waiting `json:"waiting,omitempty"`
	// Details about a running process
	// +optional
	Running *Running `json:"running,omitempty"`
	// Details about a terminated process
	// +optional
	Terminated *Terminated `json:"terminated,omitempty"`
}

// Waiting is a waiting state of a process.
type Waiting struct {
	// (brief) reason the process is not yet running.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message regarding why the process is not yet running.
	// +optional
	Message string `json:"message,omitempty"`
}

// Running is a running state of a process.
type Running struct {
	// Time at which the process was last (re-)started
	// +optional
	StartedAt metav1.Time `json:"startedAt"`
}

// Terminated is a terminated state of a process.
type Terminated struct {
	// Exit status from the last termination of the process
	ExitCode int32 `json:"exitCode"`
	// Signal from the last termination of the process
	// +optional
	Signal int32 `json:"signal,omitempty"`
	// (brief) reason from the last termination of the process
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message regarding the last termination of the process
	// +optional
	Message string `json:"message,omitempty"`
	// Time at which previous execution of the process started
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`
	// Time at which the process last terminated
	// +optional
	FinishedAt metav1.Time `json:"finishedAt,omitempty"`
}
