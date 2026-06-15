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

// NoopRecycler is a RecycleHandler that skips any in-pod cleanup (no exec, no restart)
// but still deletes the pod so that a fresh replacement is created by the pool controller.
// This ensures no data from a previous sandbox run persists in a recycled pod.
type NoopRecycler struct{}

// NewNoopRecycler creates a new NoopRecycler.
func NewNoopRecycler() *NoopRecycler {
	return &NoopRecycler{}
}

// TryRecycle drives the delete recycle state machine without any in-pod cleanup.
// When the pod still exists, it returns Recycling with NeedDelete=true so the caller
// deletes the pod and the pool controller creates a fresh replacement.
// When the pod is already gone, it returns Succeeded.
// A nil pod is considered succeeded since there is nothing to do.
func (n *NoopRecycler) TryRecycle(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, spec *Spec) (*Status, error) {
	if pod == nil {
		return &Status{
			State:   StateSucceeded,
			Message: "noop recycler: pod is gone",
		}, nil
	}
	// Pod still exists but deletion has been requested; wait for it to disappear
	// before reporting success so the pool does not re-allocate a terminating pod.
	if pod.DeletionTimestamp != nil {
		return &Status{
			State:   StateRecycling,
			Message: "noop recycler: waiting for pod termination",
		}, nil
	}
	return &Status{
		State:      StateRecycling,
		Message:    "noop recycler: pod marked for deletion",
		NeedDelete: true,
	}, nil
}
