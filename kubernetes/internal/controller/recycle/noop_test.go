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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// TestNoopRecycler verifies that NoopRecycler deletes the pod on release (NeedDelete=true)
// so that a fresh replacement is created by the pool controller and no stale data persists.
func TestNoopRecycler(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name           string
		pod            *corev1.Pod
		wantState      string
		wantNeedDelete bool
	}{
		{
			name:           "NilPod_Succeeded",
			pod:            nil,
			wantState:      StateSucceeded,
			wantNeedDelete: false,
		},
		{
			// Pod is terminating but not yet gone; report Recycling so the pool
			// does not re-allocate it before it fully disappears.
			name:           "PodWithDeletionTimestamp_Recycling",
			pod:            &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}},
			wantState:      StateRecycling,
			wantNeedDelete: false,
		},
		{
			name:           "PodStillExists_NeedDelete",
			pod:            &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
			wantState:      StateRecycling,
			wantNeedDelete: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := NewNoopRecycler()
			status, err := n.TryRecycle(context.Background(), &sandboxv1alpha1.Pool{}, tt.pod, &Spec{ID: "sbx1"})
			assert.NoError(t, err)
			assert.Equal(t, tt.wantState, status.State)
			assert.Equal(t, tt.wantNeedDelete, status.NeedDelete)
		})
	}
}
