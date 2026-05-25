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
	"context"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// State represents the state of a container restart operation.
type State string

const (
	// StateRestarting indicates that the pod has been marked for restart.
	StateRestarting State = "Restarting"
	// StateSucceeded indicates that all targeted containers have restarted.
	StateSucceeded State = "Succeeded"
	// StateFailed indicates that the restart failed after max retries.
	StateFailed State = "Failed"
)

// Status represents the current state of a restart operation on a pod.
type Status struct {
	// StartTime is the timestamp when the restart operation was initiated.
	StartTime *string `json:"startTime,omitempty"`
	// RetryCount is the number of restart attempts that have been issued so far.
	RetryCount int `json:"retryCount,omitempty"`
	// State is the current phase of the restart operation.
	State State `json:"state"`
	// Message contains human-readable details about the current state.
	Message string `json:"message,omitempty"`
}

// Spec contains options for the restart operation.
type Spec struct {
	// ID is the sandbox identifier used to correlate restart records.
	ID string
}

const (
	// AnnoRestartRecordKey is the annotation key for storing restart info on a Pod.
	AnnoRestartRecordKey = "sandbox.opensandbox.io/restart-record"
	// AnnoRestartConfigKey is the annotation key on a Pool object for restart configuration.
	AnnoRestartConfigKey = "sandbox.opensandbox.io/restart-config"
)

// Handler provides in-place container restart operations.
// Multiple implementations can exist (e.g., kill-based, exec-based, etc.).
type Handler interface {
	// TryRestart initiates or drives forward the restart state machine for the given pool and pod.
	// On the first call (no annotation present), it initializes the restart record using opts
	// and issues the first restart attempt.
	// On subsequent calls, opts is ignored and the existing record drives the state machine.
	// It is re-entrant: safe to call multiple times until Succeeded or Failed is returned.
	TryRestart(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, opts *Spec) (*Status, error)
}
