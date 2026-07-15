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
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/recycle/restart"
)

// RestartRecycler is a RecycleHandler that restarts containers in the pod.
type RestartRecycler struct {
	handler restart.Handler
}

// NewRestartRecycler creates a new RestartRecycler with the given restart handler.
func NewRestartRecycler(handler restart.Handler) *RestartRecycler {
	return &RestartRecycler{handler: handler}
}

// TryRecycle initiates or drives forward the restart recycle operation.
// It is re-entrant: delegates directly to RestartHandler.TryRestart.
// A nil pod (already deleted) is considered succeeded since the pod is gone.
// When restart fails (max retries exceeded), it falls back to deletion via NeedDelete.
func (r *RestartRecycler) TryRecycle(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, spec *Spec) (*Status, error) {
	if pod == nil {
		return &Status{
			State:   StateSucceeded,
			Message: "restart recycler: pod is deleted",
		}, nil
	}
	opts := &restart.Spec{ID: spec.ID}
	status, err := r.handler.TryRestart(ctx, pool, pod, opts)
	if err != nil {
		return nil, err
	}

	switch status.State {
	case restart.StateSucceeded:
		return &Status{
			State:   StateSucceeded,
			Message: status.Message,
		}, nil
	case restart.StateFailed:
		return &Status{
			State:      StateFailed,
			Message:    status.Message,
			NeedDelete: true,
		}, nil
	default:
		return &Status{
			State:   StateRecycling,
			Message: status.Message,
		}, nil
	}
}
