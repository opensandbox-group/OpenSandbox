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
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestResolveMaxUnavailable(t *testing.T) {
	tests := []struct {
		name         string
		pool         *sandboxv1alpha1.Pool
		desiredTotal int32
		want         int32
	}{
		{
			name: "default 25% of 10 = 3 (rounded up)",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{},
			},
			desiredTotal: 10,
			want:         3,
		},
		{
			name: "default 25% of 4 = 1",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{},
			},
			desiredTotal: 4,
			want:         1,
		},
		{
			name: "custom percentage 50% of 10 = 5",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("50%"),
					},
				},
			},
			desiredTotal: 10,
			want:         5,
		},
		{
			name: "custom absolute value 3",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrIntPtr(3),
					},
				},
			},
			desiredTotal: 10,
			want:         3,
		},
		{
			name: "absolute value 0 defaults to 1",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("0"),
					},
				},
			},
			desiredTotal: 10,
			want:         1,
		},
		{
			name: "percentage rounds up - 10% of 9 = 1",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("10%"),
					},
				},
			},
			desiredTotal: 9,
			want:         1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getUpdateMaxUnavailable(tt.pool, tt.desiredTotal)
			if got != tt.want {
				t.Errorf("resolveMaxUnavailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecreateUpdateStrategy_Compute(t *testing.T) {
	ctx := context.Background()
	updateRevision := "v2"

	tests := []struct {
		name           string
		pool           *sandboxv1alpha1.Pool
		pods           []*v1.Pod
		idlePods       []string
		wantIdlePods   []string
		wantDeletePods []string
		wantSupplyNew  int32
	}{
		{
			name: "all pods already at update revision - no deletion",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("25%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v2", true, true),
				makePod("pod-2", "v2", true, true),
			},
			idlePods:       []string{"pod-1", "pod-2"},
			wantIdlePods:   []string{"pod-1", "pod-2"},
			wantDeletePods: []string{},
			wantSupplyNew:  0,
		},
		{
			name: "all idle pods at old revision - delete within budget",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("100%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", true, true),
				makePod("pod-2", "v1", true, true),
			},
			idlePods:       []string{"pod-1", "pod-2"},
			wantIdlePods:   []string{},
			wantDeletePods: []string{"pod-1", "pod-2"},
			wantSupplyNew:  2,
		},
		{
			name: "old revision pods exceed budget - partial deletion",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("25%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", true, true),
				makePod("pod-2", "v1", true, true),
				makePod("pod-3", "v1", true, true),
				makePod("pod-4", "v1", true, true),
			},
			idlePods:       []string{"pod-1", "pod-2", "pod-3", "pod-4"},
			wantIdlePods:   []string{"pod-1", "pod-2", "pod-3"},
			wantDeletePods: []string{"pod-4"},
			wantSupplyNew:  1,
		},
		{
			name: "unavailable pods don't consume budget",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("25%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", false, true),
				makePod("pod-2", "v1", true, true),
				makePod("pod-3", "v1", true, true),
				makePod("pod-4", "v1", true, true),
			},
			idlePods:       []string{"pod-1", "pod-2", "pod-3", "pod-4"},
			wantIdlePods:   []string{"pod-2", "pod-3", "pod-4"},
			wantDeletePods: []string{"pod-1"},
			wantSupplyNew:  1,
		},
		{
			name: "mixed revisions - only delete old revision",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("100%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", true, true),
				makePod("pod-2", "v2", true, true),
				makePod("pod-3", "v1", true, true),
			},
			idlePods:       []string{"pod-1", "pod-2", "pod-3"},
			wantIdlePods:   []string{"pod-2"},
			wantDeletePods: []string{"pod-1", "pod-3"},
			wantSupplyNew:  2,
		},
		{
			name: "allocated pods not in idle list are ignored",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("100%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", true, true),
				makePod("pod-2", "v1", true, false),
				makePod("pod-3", "v1", true, true),
			},
			idlePods:       []string{"pod-1", "pod-3"},
			wantIdlePods:   []string{},
			wantDeletePods: []string{"pod-1", "pod-3"},
			wantSupplyNew:  2,
		},
		{
			name: "unready pods count towards curUnavailable",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("50%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", false, false),
				makePod("pod-2", "v1", false, false),
				makePod("pod-3", "v1", true, true),
				makePod("pod-4", "v1", true, true),
			},
			idlePods:       []string{"pod-3", "pod-4"},
			wantIdlePods:   []string{"pod-3", "pod-4"},
			wantDeletePods: []string{},
			wantSupplyNew:  0,
		},
		{
			name: "empty idle pods",
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
						MaxUnavailable: intStrPtr("25%"),
					},
				},
			},
			pods: []*v1.Pod{
				makePod("pod-1", "v1", true, false),
				makePod("pod-2", "v1", true, false),
			},
			idlePods:       []string{},
			wantIdlePods:   []string{},
			wantDeletePods: []string{},
			wantSupplyNew:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetPodCounter()
			strategy := &recreateUpdateStrategy{pool: tt.pool}
			result := strategy.Compute(ctx, updateRevision, tt.pods, tt.idlePods)

			if !stringSlicesEqualUnordered(result.IdlePods, tt.wantIdlePods) {
				t.Errorf("IdlePods = %v, want %v", result.IdlePods, tt.wantIdlePods)
			}
			if !stringSlicesEqualUnordered(result.ToDeletePods, tt.wantDeletePods) {
				t.Errorf("ToDeletePods = %v, want %v", result.ToDeletePods, tt.wantDeletePods)
			}
			if result.SupplyUpdateRevision != tt.wantSupplyNew {
				t.Errorf("SupplyUpdateRevision = %v, want %v", result.SupplyUpdateRevision, tt.wantSupplyNew)
			}
		})
	}
}

func TestRecreateUpdateStrategy_Compute_Sorting(t *testing.T) {
	ctx := context.Background()
	updateRevision := "v2"

	pool := &sandboxv1alpha1.Pool{
		Spec: sandboxv1alpha1.PoolSpec{
			UpdateStrategy: &sandboxv1alpha1.UpdateStrategy{
				MaxUnavailable: intStrPtr("25%"),
			},
		},
	}

	now := metav1.Now()
	older := metav1.NewTime(now.Add(-2 * time.Hour))
	newer := metav1.NewTime(now.Add(-1 * time.Hour))

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "assigned-running", CreationTimestamp: newer},
			Spec:       v1.PodSpec{NodeName: "node-1"},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				Conditions: []v1.PodCondition{
					{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: older},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "unassigned-pending", CreationTimestamp: older},
			Spec:       v1.PodSpec{NodeName: ""},
			Status:     v1.PodStatus{Phase: v1.PodPending},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "assigned-not-ready", CreationTimestamp: now},
			Spec:       v1.PodSpec{NodeName: "node-2"},
			Status:     v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{}},
		},
	}

	for _, pod := range pods {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[LabelPoolRevision] = "v1"
	}

	strategy := &recreateUpdateStrategy{pool: pool}
	result := strategy.Compute(ctx, updateRevision, pods, []string{"assigned-running", "unassigned-pending", "assigned-not-ready"})

	wantDelete := []string{"unassigned-pending", "assigned-not-ready"}
	wantIdle := []string{"assigned-running"}

	if !stringSlicesEqual(result.ToDeletePods, wantDelete) {
		t.Errorf("ToDeletePods = %v, want %v", result.ToDeletePods, wantDelete)
	}
	if !stringSlicesEqual(result.IdlePods, wantIdle) {
		t.Errorf("IdlePods = %v, want %v", result.IdlePods, wantIdle)
	}
}

var podCreationCounter int

func makePod(name, revision string, ready, idle bool) *v1.Pod {
	podCreationCounter++
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			CreationTimestamp: metav1.NewTime(
				metav1.Now().Add(time.Duration(podCreationCounter) * time.Second),
			),
			Labels: map[string]string{
				LabelPoolRevision: revision,
			},
		},
		Spec: v1.PodSpec{
			NodeName: "node-1",
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}

	if ready {
		pod.Status.Conditions = []v1.PodCondition{
			{Type: v1.PodReady, Status: v1.ConditionTrue},
		}
	}

	if !idle {
		pod.Labels["allocated"] = "true"
	}

	return pod
}

func resetPodCounter() {
	podCreationCounter = 0
}

func intStrPtr(s string) *intstr.IntOrString {
	val := intstr.FromString(s)
	return &val
}

func intStrIntPtr(i int) *intstr.IntOrString {
	val := intstr.FromInt(i)
	return &val
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}
