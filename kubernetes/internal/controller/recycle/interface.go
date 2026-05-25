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

package recycle

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const (

	// StateRecycling RecycleStateRecycling indicates the pod is being recycled.
	StateRecycling string = "Recycling"
	// StateSucceeded RecycleStateSucceeded indicates the pod has been successfully recycled.
	StateSucceeded string = "Succeeded"
	// StateFailed RecycleStateFailed indicates the recycle operation failed.
	StateFailed string = "Failed"
)

// Spec describes the sandbox being recycled.
type Spec struct {
	ID string
}

// Status represents the current state of a recycle operation on a pod.
type Status struct {
	// State is the current phase of the recycle operation.
	State string `json:"state"`
	// Message contains human-readable details about the current state.
	Message string `json:"message,omitempty"`
	// NeedDelete indicates whether the pod needs to be deleted.
	NeedDelete bool `json:"needDelete,omitempty"`
}

// Handler handles pod recycling when pods are returned to the pool.
// Different implementations provide different recycle strategies:
// - NoopRecycler: do nothing, pod is immediately available
// - DeleteRecycler: delete the pod
// - RestartRecycler: restart containers in the pod
type Handler interface {
	// TryRecycle initiates or drives forward the recycle operation for the pod.
	// It is re-entrant: safe to call multiple times until Succeeded or Failed is returned.
	TryRecycle(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, spec *Spec) (*Status, error)
}
