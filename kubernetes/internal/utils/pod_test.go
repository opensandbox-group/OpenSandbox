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

package utils

import (
	"slices"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsPodReady(t *testing.T) {
	readyCondition := v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionTrue}
	notReadyCondition := v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionFalse}

	tests := []struct {
		name string
		pod  *v1.Pod
		want bool
	}{
		{
			name: "running and ready",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase:      v1.PodRunning,
				Conditions: []v1.PodCondition{readyCondition},
			}},
			want: true,
		},
		{
			name: "running but not ready",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase:      v1.PodRunning,
				Conditions: []v1.PodCondition{notReadyCondition},
			}},
			want: false,
		},
		{
			name: "running but no ready condition",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase: v1.PodRunning,
			}},
			want: false,
		},
		{
			name: "pending with stale ready=true",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase:      v1.PodPending,
				Conditions: []v1.PodCondition{readyCondition},
			}},
			want: false,
		},
		{
			name: "unknown phase with stale ready=true",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase:      v1.PodUnknown,
				Conditions: []v1.PodCondition{readyCondition},
			}},
			want: false,
		},
		{
			name: "failed phase",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase: v1.PodFailed,
			}},
			want: false,
		},
		{
			name: "succeeded phase",
			pod: &v1.Pod{Status: v1.PodStatus{
				Phase: v1.PodSucceeded,
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPodReady(tt.pod)
			if got != tt.want {
				t.Errorf("IsPodReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithPodIndexSorter(t *testing.T) {
	tests := []struct {
		name     string
		podIndex map[string]int
		podA     *v1.Pod
		podB     *v1.Pod
		want     int
	}{
		{
			name: "a index < b index",
			podIndex: map[string]int{
				"pod-a": 1,
				"pod-b": 2,
			},
			podA: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want: -1,
		},
		{
			name: "a index > b index",
			podIndex: map[string]int{
				"pod-a": 5,
				"pod-b": 3,
			},
			podA: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want: 1,
		},
		{
			name: "a index == b index",
			podIndex: map[string]int{
				"pod-a": 2,
				"pod-b": 2,
			},
			podA: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want: 0,
		},
		{
			name: "a has no index, b has index - a should be last",
			podIndex: map[string]int{
				"pod-b": 1,
			},
			podA: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want: 1,
		},
		{
			name: "a has index, b has no index - b should be last",
			podIndex: map[string]int{
				"pod-a": 1,
			},
			podA: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want: -1,
		},
		{
			name:     "both have no index",
			podIndex: map[string]int{},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     0,
		},
		{
			name: "index 0 vs index 1",
			podIndex: map[string]int{
				"pod-a": 0,
				"pod-b": 1,
			},
			podA: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sorter := WithPodIndexSorter(tt.podIndex)
			got := sorter(tt.podA, tt.podB)
			if got != tt.want {
				t.Errorf("WithPodIndexSorter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMultiPodSorter(t *testing.T) {
	tests := []struct {
		name     string
		sorters  MultiPodSorter
		podA     *v1.Pod
		podB     *v1.Pod
		want     int
		wantDesc string
	}{
		{
			name: "first sorter decides - a < b",
			sorters: MultiPodSorter{
				func(a, b *v1.Pod) int {
					if a.Name < b.Name {
						return -1
					} else if a.Name > b.Name {
						return 1
					}
					return 0
				},
			},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     -1,
			wantDesc: "pod-a should come before pod-b",
		},
		{
			name: "first sorter equal, second sorter decides",
			sorters: MultiPodSorter{
				func(a, b *v1.Pod) int {
					return 0
				},
				func(a, b *v1.Pod) int {
					if a.Name < b.Name {
						return -1
					} else if a.Name > b.Name {
						return 1
					}
					return 0
				},
			},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     -1,
			wantDesc: "first sorter returns 0, second sorter decides",
		},
		{
			name: "all sorters return equal",
			sorters: MultiPodSorter{
				func(a, b *v1.Pod) int { return 0 },
				func(a, b *v1.Pod) int { return 0 },
				func(a, b *v1.Pod) int { return 0 },
			},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     0,
			wantDesc: "all sorters return 0",
		},
		{
			name: "index sorter then name sorter - decided by index",
			sorters: MultiPodSorter{
				WithPodIndexSorter(map[string]int{
					"pod-b": 0,
					"pod-a": 1,
				}),
				PodNameSorter,
			},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     1,
			wantDesc: "pod-b has lower index (0) than pod-a (1), so pod-a > pod-b",
		},
		{
			name: "index sorter then name sorter - decided by name",
			sorters: MultiPodSorter{
				WithPodIndexSorter(map[string]int{
					"pod-a": 1,
					"pod-b": 1,
				}),
				PodNameSorter,
			},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     -1,
			wantDesc: "same index, fallback to name comparison",
		},
		{
			name:     "empty sorters list",
			sorters:  MultiPodSorter{},
			podA:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
			podB:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
			want:     0,
			wantDesc: "no sorters, should return 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.sorters.Sort(tt.podA, tt.podB)
			if got != tt.want {
				t.Errorf("MultiPodSorter.Sort() = %v, want %v (%s)", got, tt.want, tt.wantDesc)
			}
		})
	}
}

func TestMultiPodSorter_Integration(t *testing.T) {
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-c"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-d"}},
	}

	podIndex := map[string]int{
		"pod-a": 2,
		"pod-b": 0,
		"pod-c": 1,
	}

	sorter := MultiPodSorter{
		WithPodIndexSorter(podIndex),
		PodNameSorter,
	}

	slices.SortStableFunc(pods, sorter.Sort)

	expectedOrder := []string{"pod-b", "pod-c", "pod-a", "pod-d"}

	for i, pod := range pods {
		if pod.Name != expectedOrder[i] {
			t.Errorf("pod at index %d: got %s, want %s", i, pod.Name, expectedOrder[i])
		}
	}
}

func TestComparePodsForDeletion(t *testing.T) {
	now := metav1.Now()
	older := metav1.NewTime(now.Add(-1 * time.Hour))
	newer := metav1.NewTime(now.Add(-30 * time.Minute))

	tests := []struct {
		name string
		p1   *v1.Pod
		p2   *v1.Pod
		want bool
	}{
		{
			name: "unassigned < assigned",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unassigned"}, Spec: v1.PodSpec{NodeName: ""}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "assigned"}, Spec: v1.PodSpec{NodeName: "node-1"}},
			want: true,
		},
		{
			name: "assigned > unassigned",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "assigned"}, Spec: v1.PodSpec{NodeName: "node-1"}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unassigned"}, Spec: v1.PodSpec{NodeName: ""}},
			want: false,
		},
		{
			name: "both unassigned - tie-break by name",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: v1.PodSpec{NodeName: ""}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1.PodSpec{NodeName: ""}},
			want: true,
		},
		{
			name: "both assigned - tie-break by name",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: v1.PodSpec{NodeName: "node-1"}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1.PodSpec{NodeName: "node-2"}},
			want: true,
		},
		{
			name: "pending < running",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending"}, Status: v1.PodStatus{Phase: v1.PodPending}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running"}, Status: v1.PodStatus{Phase: v1.PodRunning}},
			want: true,
		},
		{
			name: "running > pending",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running"}, Status: v1.PodStatus{Phase: v1.PodRunning}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending"}, Status: v1.PodStatus{Phase: v1.PodPending}},
			want: false,
		},
		{
			name: "unknown < running",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unknown"}, Status: v1.PodStatus{Phase: v1.PodUnknown}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running"}, Status: v1.PodStatus{Phase: v1.PodRunning}},
			want: true,
		},
		{
			name: "pending < unknown",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending"}, Status: v1.PodStatus{Phase: v1.PodPending}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unknown"}, Status: v1.PodStatus{Phase: v1.PodUnknown}},
			want: true,
		},
		{
			name: "not ready < ready",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-ready"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{}}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ready"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			want: true,
		},
		{
			name: "ready > not ready",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ready"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-ready"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{}}},
			want: false,
		},
		{
			name: "shorter ready time < longer ready time",
			p1: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shorter"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: older},
			}}},
			p2: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "longer"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: newer},
			}}},
			want: true,
		},
		{
			name: "longer ready time > shorter ready time",
			p1: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "longer"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: newer},
			}}},
			p2: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shorter"}, Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: older},
			}}},
			want: false,
		},
		{
			name: "higher restarts < lower restarts",
			p1: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "high-restarts"}, Status: v1.PodStatus{Phase: v1.PodRunning, ContainerStatuses: []v1.ContainerStatus{
				{RestartCount: 5},
			}}},
			p2: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "low-restarts"}, Status: v1.PodStatus{Phase: v1.PodRunning, ContainerStatuses: []v1.ContainerStatus{
				{RestartCount: 1},
			}}},
			want: true,
		},
		{
			name: "lower restarts > higher restarts",
			p1: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "low-restarts"}, Status: v1.PodStatus{Phase: v1.PodRunning, ContainerStatuses: []v1.ContainerStatus{
				{RestartCount: 1},
			}}},
			p2: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "high-restarts"}, Status: v1.PodStatus{Phase: v1.PodRunning, ContainerStatuses: []v1.ContainerStatus{
				{RestartCount: 5},
			}}},
			want: false,
		},
		{
			name: "newer < older",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "newer", CreationTimestamp: newer}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "older", CreationTimestamp: older}},
			want: true,
		},
		{
			name: "older > newer",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "older", CreationTimestamp: older}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "newer", CreationTimestamp: newer}},
			want: false,
		},
		{
			name: "equal pods",
			p1:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "same", CreationTimestamp: now}, Spec: v1.PodSpec{NodeName: "node-1"}, Status: v1.PodStatus{Phase: v1.PodRunning}},
			p2:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "same", CreationTimestamp: now}, Spec: v1.PodSpec{NodeName: "node-1"}, Status: v1.PodStatus{Phase: v1.PodRunning}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComparePodsForDeletion(tt.p1, tt.p2)
			if got != tt.want {
				t.Errorf("ComparePodsForDeletion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComparePodsForDeletion_SortIntegration(t *testing.T) {
	now := metav1.Now()
	older := metav1.NewTime(now.Add(-2 * time.Hour))
	newer := metav1.NewTime(now.Add(-1 * time.Hour))

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "assigned-ready", CreationTimestamp: newer},
			Spec:       v1.PodSpec{NodeName: "node-1"},
			Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: older},
			}},
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

	slices.SortStableFunc(pods, func(a, b *v1.Pod) int {
		if ComparePodsForDeletion(a, b) {
			return -1
		}
		if ComparePodsForDeletion(b, a) {
			return 1
		}
		return 0
	})

	expectedOrder := []string{"unassigned-pending", "assigned-not-ready", "assigned-ready"}
	for i, pod := range pods {
		if pod.Name != expectedOrder[i] {
			t.Errorf("pod at index %d: got %s, want %s", i, pod.Name, expectedOrder[i])
		}
	}
}
