// Copyright 2025 The OpenSandbox Authors
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

package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func TestScheduleDoesNotContactReusedPodIP(t *testing.T) {
	originalPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "original-pod", UID: "original-uid"},
		Status:     corev1.PodStatus{PodIP: "192.0.2.1"},
	}
	replacedPod := originalPod.DeepCopy()
	replacedPod.UID = "replacement-uid"
	differentPod := replacedPod.DeepCopy()
	differentPod.Name = "replacement-pod"
	changedIPPod := originalPod.DeepCopy()
	changedIPPod.Status.PodIP = "192.0.2.2"
	terminatingPod := originalPod.DeepCopy()
	deletingSince := metav1.Now()
	terminatingPod.DeletionTimestamp = &deletingSince

	for _, binding := range []struct {
		name string
		pods []*corev1.Pod
	}{
		{name: "original pod deleted"},
		{name: "different pod reuses IP", pods: []*corev1.Pod{differentPod}},
		{name: "same name and IP with different UID", pods: []*corev1.Pod{replacedPod}},
		{name: "bound pod IP changed", pods: []*corev1.Pod{changedIPPod}},
		{name: "terminating Pod still reports original IP", pods: []*corev1.Pod{terminatingPod}},
	} {
		for _, lifecycle := range []string{"running", "deleting", "already releasing"} {
			t.Run(binding.name+"/"+lifecycle, func(t *testing.T) {
				endpoint := &bindingTestEndpoint{tasks: []*api.Task{{
					Name:          "new-sandbox-task",
					ProcessStatus: &api.ProcessStatus{Running: &api.Running{}},
				}}}
				scheduler, node := newBindingTestScheduler(t, originalPod, endpoint)
				if lifecycle == "deleting" {
					now := metav1.Now()
					node.DeletionTimestamp = &now
				} else if lifecycle == "already releasing" {
					node.sState = stateReleasing
				}
				scheduler.UpdatePods(binding.pods)

				for range 2 {
					if err := scheduler.Schedule(); err != nil {
						t.Fatalf("Schedule: %v", err)
					}
				}

				endpoint.mu.Lock()
				defer endpoint.mu.Unlock()
				if endpoint.gets != 0 || endpoint.sets != 0 {
					t.Errorf("stale binding contacted reused endpoint: GET=%d POST=%d", endpoint.gets, endpoint.sets)
				}
				if len(endpoint.tasks) != 1 || endpoint.tasks[0].Name != "new-sandbox-task" {
					t.Errorf("stale scheduler replaced the new sandbox's task: %+v", endpoint.tasks)
				}
				if node.IP != originalPod.Status.PodIP || node.PodName != originalPod.Name || node.tState != RunningTaskState {
					t.Errorf("binding loss changed assignment or task lifecycle: %+v", node)
				}
				if node.Status != nil {
					t.Errorf("binding loss retained execution status: %+v", node.Status)
				}
				if lifecycle != "running" && !node.IsResourceReleased() {
					t.Errorf("task did not finish local release: state=%q", node.sState)
				}
				if lifecycle == "running" && node.IsResourceReleased() {
					t.Error("running task was released after binding loss")
				}
			})
		}
	}
}

func TestScheduleDoesNotAssignReusedIPStatusToStaleBinding(t *testing.T) {
	originalPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "original-pod", UID: "original-uid"},
		Status:     corev1.PodStatus{PodIP: "192.0.2.1"},
	}
	replacementPod := originalPod.DeepCopy()
	replacementPod.Name = "replacement-pod"
	replacementPod.UID = "replacement-uid"
	endpoint := &bindingTestEndpoint{tasks: []*api.Task{{
		Name:          "new-sandbox-task",
		ProcessStatus: &api.ProcessStatus{Running: &api.Running{}},
	}}}
	scheduler, staleNode := newBindingTestScheduler(t, originalPod, endpoint)
	currentNode := &taskNode{
		ObjectMeta: metav1.ObjectMeta{Name: "new-sandbox-task"},
		Spec:       staleNode.Spec,
	}
	assignTaskNodes([]*taskNode{currentNode}, []*corev1.Pod{replacementPod}, testLogger)
	scheduler.taskNodes = append(scheduler.taskNodes, currentNode)
	scheduler.UpdatePods([]*corev1.Pod{replacementPod})

	if err := scheduler.Schedule(); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if staleNode.Status != nil || staleNode.tState != RunningTaskState {
		t.Errorf("replacement task status leaked to stale binding: status=%+v state=%q", staleNode.Status, staleNode.tState)
	}
	if currentNode.Status == nil || currentNode.Status.Name != "new-sandbox-task" || currentNode.tState != RunningTaskState {
		t.Errorf("valid binding lost its task status: status=%+v state=%q", currentNode.Status, currentNode.tState)
	}
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.gets != 1 || endpoint.sets != 1 || len(endpoint.tasks) != 1 || endpoint.tasks[0].Name != currentNode.Name {
		t.Errorf("only valid binding should contact executor: GET=%d POST=%d tasks=%+v", endpoint.gets, endpoint.sets, endpoint.tasks)
	}
}

func TestScheduleQueryFailureDoesNotSkipReleaseForBoundPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "original-pod", UID: "original-uid"},
		Status:     corev1.PodStatus{PodIP: "192.0.2.1"},
	}
	endpoint := &bindingTestEndpoint{queryFailure: true}
	scheduler, node := newBindingTestScheduler(t, pod, endpoint)
	now := metav1.Now()
	node.DeletionTimestamp = &now
	lastStatus := node.Status

	if err := scheduler.Schedule(); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.gets != 1 || endpoint.sets != 1 || len(endpoint.tasks) != 0 {
		t.Errorf("bound pod release: GET=%d POST=%d tasks=%+v, want one query and an empty task list", endpoint.gets, endpoint.sets, endpoint.tasks)
	}
	if node.Status != lastStatus || node.IsResourceReleased() {
		t.Errorf("query failure was treated as completed cleanup: status=%+v state=%q", node.Status, node.sState)
	}
}

func newBindingTestScheduler(t *testing.T, pod *corev1.Pod, endpoint *bindingTestEndpoint) (*defaultTaskScheduler, *taskNode) {
	t.Helper()
	server := httptest.NewServer(endpoint)
	t.Cleanup(server.Close)
	creator := func(string) taskClient { return api.NewClient(server.URL) }
	node := &taskNode{
		ObjectMeta: metav1.ObjectMeta{Name: "old-sandbox-task"},
		Spec:       taskSpec{Process: &api.Process{Command: []string{"sleep", "3600"}}},
		Status:     &api.Task{Name: "old-sandbox-task", ProcessStatus: &api.ProcessStatus{Running: &api.Running{}}},
		tState:     RunningTaskState,
	}
	assignTaskNodes([]*taskNode{node}, []*corev1.Pod{pod}, testLogger)
	return &defaultTaskScheduler{
		allPods:                   []*corev1.Pod{pod},
		taskNodes:                 []*taskNode{node},
		maxConcurrency:            1,
		taskClientCreator:         creator,
		taskStatusCollector:       newTaskStatusCollector(creator, testLogger),
		resPolicyWhenTaskComplete: sandboxv1alpha1.TaskResourcePolicyRetain,
		logger:                    testLogger,
	}, node
}

type bindingTestEndpoint struct {
	mu           sync.Mutex
	tasks        []*api.Task
	gets         int
	sets         int
	queryFailure bool
}

func (endpoint *bindingTestEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /getTasks":
		endpoint.gets++
		if endpoint.queryFailure {
			http.Error(w, "status temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	case "POST /setTasks":
		endpoint.sets++
		if err := json.NewDecoder(r.Body).Decode(&endpoint.tasks); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(endpoint.tasks)
}
