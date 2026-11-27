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
	"context"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type identityRecordingClient struct {
	gets atomic.Int32
	sets atomic.Int32
	task *api.Task
}

func (c *identityRecordingClient) Get(context.Context) (*api.Task, error) {
	c.gets.Add(1)
	return c.task, nil
}

func (c *identityRecordingClient) Set(_ context.Context, task *api.Task) (*api.Task, error) {
	c.sets.Add(1)
	return task, nil
}

func TestSchedulerDoesNotContactReusedPodIP(t *testing.T) {
	original := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-pod", Namespace: "default", UID: "original"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	tests := []struct {
		name   string
		mutate func(*corev1.Pod) []*corev1.Pod
	}{
		{"deleted", func(*corev1.Pod) []*corev1.Pod { return nil }},
		{"different pod at old IP", func(p *corev1.Pod) []*corev1.Pod { p.Name = "other-pod"; p.UID = "other"; return []*corev1.Pod{p} }},
		{"same name with new IP", func(p *corev1.Pod) []*corev1.Pod { p.Status.PodIP = "10.0.0.2"; return []*corev1.Pod{p} }},
		{"same name and IP with new UID", func(p *corev1.Pod) []*corev1.Pod { p.UID = "replacement"; return []*corev1.Pod{p} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, deleting := range []bool{false, true} {
				name := "running"
				if deleting {
					name = "releasing"
				}
				t.Run(name, func(t *testing.T) {
					node := &taskNode{ObjectMeta: metav1.ObjectMeta{Name: "stale-task"}, tState: RunningTaskState,
						Status: &api.Task{Name: "stale-task", ProcessStatus: &api.ProcessStatus{Running: &api.Running{}}}}
					assignTaskNodes([]*taskNode{node}, []*corev1.Pod{original}, testLogger)
					if deleting {
						now := metav1.Now()
						node.DeletionTimestamp = &now
					}
					// A new executor answers successfully, so a failed-query-only
					// check would accept its status and send the stale task list.
					client := &identityRecordingClient{task: &api.Task{Name: "foreign-task", ProcessStatus: &api.ProcessStatus{Running: &api.Running{}}}}
					creator := func(string) taskClient { return client }
					sch := &defaultTaskScheduler{taskNodes: []*taskNode{node}, allPods: tt.mutate(original.DeepCopy()),
						maxConcurrency: 1, taskClientCreator: creator, taskStatusCollector: newTaskStatusCollector(creator, testLogger),
						resPolicyWhenTaskComplete: sandboxv1alpha1.TaskResourcePolicyRetain, logger: testLogger}
					for i := 0; i < 2; i++ {
						if err := sch.Schedule(); err != nil {
							t.Fatal(err)
						}
					}
					if client.gets.Load() != 0 || client.sets.Load() != 0 {
						t.Fatalf("contacted stale endpoint: GET=%d SET=%d", client.gets.Load(), client.sets.Load())
					}
					if !node.IsResourceReleased() {
						t.Fatal("missing pod's task resource was not released")
					}
					if node.GetState() != FailedTaskState {
						t.Fatalf("task state = %s, want failed", node.GetState())
					}
				})
			}
		})
	}
}

func TestSchedulerKeepsContactingBoundPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pool-pod", UID: "original"}, Status: corev1.PodStatus{PodIP: "10.0.0.1"}}
	node := &taskNode{ObjectMeta: metav1.ObjectMeta{Name: "task"}}
	assignTaskNodes([]*taskNode{node}, []*corev1.Pod{pod}, testLogger)
	client := &identityRecordingClient{task: &api.Task{Name: "task", ProcessStatus: &api.ProcessStatus{Running: &api.Running{}}}}
	creator := func(string) taskClient { return client }
	sch := &defaultTaskScheduler{taskNodes: []*taskNode{node}, allPods: []*corev1.Pod{pod.DeepCopy()}, maxConcurrency: 1,
		taskClientCreator: creator, taskStatusCollector: newTaskStatusCollector(creator, testLogger), logger: testLogger}
	if err := sch.Schedule(); err != nil {
		t.Fatal(err)
	}
	if client.gets.Load() != 1 || client.sets.Load() != 1 || node.IsResourceReleased() {
		t.Fatalf("bound pod: GET=%d SET=%d released=%v", client.gets.Load(), client.sets.Load(), node.IsResourceReleased())
	}
}

func TestRecoveredSchedulerRejectsRecreatedPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pool-pod", UID: "original"}, Status: corev1.PodStatus{PodIP: "10.0.0.1"}}
	node := &taskNode{ObjectMeta: metav1.ObjectMeta{Name: "task"}}
	client := &identityRecordingClient{task: &api.Task{Name: "task", ProcessStatus: &api.ProcessStatus{Running: &api.Running{}}}}
	creator := func(string) taskClient { return client }
	sch := &defaultTaskScheduler{taskNodes: []*taskNode{node}, taskNodeByNameIndex: map[string]*taskNode{"task": node}, allPods: []*corev1.Pod{pod},
		maxConcurrency: 1, taskClientCreator: creator, taskStatusCollector: newTaskStatusCollector(creator, testLogger), logger: testLogger}
	if err := sch.recoverTaskNodesStatus(); err != nil {
		t.Fatal(err)
	}
	client.gets.Store(0)
	replacement := pod.DeepCopy()
	replacement.UID = "replacement"
	sch.UpdatePods([]*corev1.Pod{replacement})
	if err := sch.Schedule(); err != nil {
		t.Fatal(err)
	}
	if client.gets.Load() != 0 || client.sets.Load() != 0 || !node.IsResourceReleased() {
		t.Fatalf("recovered task contacted replacement: GET=%d SET=%d released=%v", client.gets.Load(), client.sets.Load(), node.IsResourceReleased())
	}
}

func TestSchedulerPreservesCompletedOutcomeAfterPodRemoval(t *testing.T) {
	for _, state := range []TaskState{SucceedTaskState, FailedTaskState} {
		t.Run(string(state), func(t *testing.T) {
			status := &api.Task{Name: "completed", ProcessStatus: &api.ProcessStatus{Terminated: &api.Terminated{Message: "original outcome"}}}
			node := &taskNode{ObjectMeta: metav1.ObjectMeta{Name: "completed"}, IP: "10.0.0.1", PodName: "gone", Status: status, tState: state}
			creator := func(string) taskClient { t.Fatal("contacted removed pod"); return nil }
			sch := &defaultTaskScheduler{taskNodes: []*taskNode{node}, maxConcurrency: 1, taskClientCreator: creator,
				taskStatusCollector: newTaskStatusCollector(creator, testLogger), logger: testLogger}
			if err := sch.Schedule(); err != nil {
				t.Fatal(err)
			}
			if node.Status != status || node.GetState() != state || !node.IsResourceReleased() {
				t.Fatalf("completed outcome changed: status=%v state=%s released=%v", node.Status, node.GetState(), node.IsResourceReleased())
			}
		})
	}
}
