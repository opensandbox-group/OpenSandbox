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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// TestPoolBatchSandboxUpdateReplicas verifies replicas are compared by value:
// equal values at different addresses do not trigger, value changes trigger,
// nil on both sides does not trigger, and both directions between nil and an
// explicit zero value trigger.
func TestPoolBatchSandboxUpdateReplicas(t *testing.T) {
	cases := []struct {
		name                     string
		oldReplicas, newReplicas *int32
		want                     bool
	}{
		{name: "equal-values-at-different-addresses", oldReplicas: ptr.To(int32(1)), newReplicas: ptr.To(int32(1))},
		{name: "changed-values", oldReplicas: ptr.To(int32(1)), newReplicas: ptr.To(int32(2)), want: true},
		{name: "both-nil"},
		{name: "nil-to-zero", newReplicas: ptr.To(int32(0)), want: true},
		{name: "zero-to-nil", oldReplicas: ptr.To(int32(0)), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: "warm-pool", Replicas: tc.oldReplicas},
			}
			newObj := oldObj.DeepCopy()
			newObj.Spec.Replicas = tc.newReplicas
			got := shouldReconcilePoolForBatchSandboxUpdate(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
			if got != tc.want {
				t.Errorf("update predicate = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestPoolBatchSandboxUpdateFilters verifies unrelated updates are still
// filtered after a deep copy: status, ordinary annotation, and already-deleting
// updates do not trigger; release annotation changes and first transition into
// deleting still trigger; missing pool reference and wrong types are filtered.
func TestPoolBatchSandboxUpdateFilters(t *testing.T) {
	cases := []struct {
		name   string
		update func(*event.UpdateEvent)
		want   bool
	}{
		{
			name: "status-only",
			update: func(e *event.UpdateEvent) {
				e.ObjectNew.(*sandboxv1alpha1.BatchSandbox).Status.Ready = 1
			},
		},
		{
			name: "unrelated-annotation",
			update: func(e *event.UpdateEvent) {
				e.ObjectNew.SetAnnotations(map[string]string{"example.com/note": "updated"})
			},
		},
		{
			name: "release-annotation",
			update: func(e *event.UpdateEvent) {
				e.ObjectNew.SetAnnotations(map[string]string{annoAllocReleaseKey: `{"pods":["warm-pod"]}`})
			},
			want: true,
		},
		{
			name: "pool-ref-auto-assign-resolved",
			update: func(e *event.UpdateEvent) {
				e.ObjectOld.(*sandboxv1alpha1.BatchSandbox).Spec.PoolRef = "*"
			},
			want: true,
		},
		{
			name: "pool-ref-reassigned",
			update: func(e *event.UpdateEvent) {
				e.ObjectNew.(*sandboxv1alpha1.BatchSandbox).Spec.PoolRef = "other-pool"
			},
			want: true,
		},
		{
			name: "entering-terminating-state",
			update: func(e *event.UpdateEvent) {
				timestamp := metav1.Now()
				e.ObjectNew.SetDeletionTimestamp(&timestamp)
			},
			want: true,
		},
		{
			name: "already-terminating",
			update: func(e *event.UpdateEvent) {
				timestamp := metav1.Now()
				e.ObjectOld.SetDeletionTimestamp(&timestamp)
				e.ObjectNew.SetDeletionTimestamp(&timestamp)
			},
		},
		{
			name: "no-pool-reference",
			update: func(e *event.UpdateEvent) {
				newObj := e.ObjectNew.(*sandboxv1alpha1.BatchSandbox)
				newObj.Spec.PoolRef = ""
				newObj.Spec.Replicas = ptr.To(int32(2))
			},
		},
		{
			name:   "invalid-old-object",
			update: func(e *event.UpdateEvent) { e.ObjectOld = &corev1.Pod{} },
		},
		{
			name:   "invalid-new-object",
			update: func(e *event.UpdateEvent) { e.ObjectNew = &corev1.Pod{} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: "warm-pool", Replicas: ptr.To(int32(1))},
			}
			// DeepCopy gives the new object its own Replicas pointer, as on an unrelated update.
			update := event.UpdateEvent{ObjectOld: oldObj, ObjectNew: oldObj.DeepCopy()}
			tc.update(&update)
			if got := shouldReconcilePoolForBatchSandboxUpdate(update); got != tc.want {
				t.Errorf("update predicate = %t, want %t", got, tc.want)
			}
		})
	}
}
