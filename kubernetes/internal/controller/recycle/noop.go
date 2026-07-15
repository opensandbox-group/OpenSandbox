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

// NoopRecycler is a RecycleHandler that does nothing.
// The pod is immediately available for reallocation after being returned to the pool.
type NoopRecycler struct{}

// NewNoopRecycler creates a new NoopRecycler.
func NewNoopRecycler() *NoopRecycler {
	return &NoopRecycler{}
}

// TryRecycle does nothing and returns Succeeded status immediately.
// A nil pod (already deleted) is also considered succeeded since there is nothing to do.
func (n *NoopRecycler) TryRecycle(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod, spec *Spec) (*Status, error) {
	return &Status{
		State:   StateSucceeded,
		Message: "noop recycler: no action needed",
	}, nil
}
