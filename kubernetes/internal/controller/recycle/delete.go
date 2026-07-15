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

// DeleteRecycler is a RecycleHandler that marks pods for deletion.
// The actual deletion is handled by the pool controller's scale logic.
type DeleteRecycler struct{}

// NewDeleteRecycler creates a new DeleteRecycler.
func NewDeleteRecycler() *DeleteRecycler {
	return &DeleteRecycler{}
}

// TryRecycle drives the delete recycle state machine.
// When the pod still exists, it returns Recycling with NeedDelete=true so the caller deletes the pod.
// When the pod is gone (DeletionTimestamp set), it returns Succeeded.
func (d *DeleteRecycler) TryRecycle(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, spec *Spec) (*Status, error) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return &Status{
			State:   StateSucceeded,
			Message: "delete recycler: pod is deleted",
		}, nil
	}
	return &Status{
		State:      StateRecycling,
		Message:    "delete recycler: pod marked for deletion",
		NeedDelete: true,
	}, nil
}
