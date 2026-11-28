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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/manager"
	executorserver "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/server"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// syncRecordingTaskManager only counts Sync calls: the regression only needs to
// observe whether a stale task list reached the task manager.
type syncRecordingTaskManager struct {
	manager.TaskManager
	syncCalls int
}

func (m *syncRecordingTaskManager) Sync(ctx context.Context, desired []*types.Task) ([]*types.Task, error) {
	m.syncCalls++
	return desired, nil
}

// A task node recovered at controller startup must keep asserting the UID of
// the Pod it is bound to. Otherwise, deleting the original Pod degrades the
// claim to its name — and when a same-name/same-IP replacement Pod is visible
// in the snapshot, the scheduler silently asserts the replacement's UID — so
// the replacement executor accepts the stale task list and stops its own task.
func Test_defaultTaskScheduler_RecoveredBindingRejectsReplacementPod(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	const (
		originalUID    = "uid-original"
		replacementUID = "uid-replacement"
	)
	originalPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", UID: originalUID},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	tNode := &taskNode{
		ObjectMeta: metav1.ObjectMeta{Name: "task-0"},
		Spec: taskSpec{
			Process: &api.Process{Command: []string{"sleep", "3600"}},
		},
	}
	runningTask := &api.Task{
		Name:          "task-0",
		Process:       tNode.Spec.Process,
		ProcessStatus: &api.ProcessStatus{Running: &api.Running{}},
	}

	collector := NewMocktaskStatusCollector(ctl)
	collector.EXPECT().Collect(gomock.Any(), []string{"10.0.0.1"}).
		Return(map[string]*api.Task{"10.0.0.1": runningTask}, nil).Times(1)

	sch := &defaultTaskScheduler{
		allPods:             []*corev1.Pod{originalPod},
		taskNodes:           []*taskNode{tNode},
		taskNodeByNameIndex: map[string]*taskNode{"task-0": tNode},
		taskStatusCollector: collector,
		logger:              testLogger,
	}

	// Controller restart: recovery rebinds the task node from the live Pod.
	if err := sch.recoverTaskNodesStatus(); err != nil {
		t.Fatalf("recoverTaskNodesStatus() error = %v", err)
	}
	if tNode.PodUID != originalUID {
		t.Fatalf("recovered binding asserts PodUID %q, want %q", tNode.PodUID, originalUID)
	}

	// The original Pod is deleted and replaced under the same name; the CNI
	// hands the same IP back to the replacement.
	replacementPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", UID: replacementUID},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	sch.UpdatePods([]*corev1.Pod{replacementPod})

	// The replacement executor knows its own identity.
	mgr := &syncRecordingTaskManager{}
	srv := httptest.NewServer(executorserver.NewRouter(executorserver.NewHandler(mgr,
		&config.Config{PodName: "pod-1", PodUID: replacementUID})))
	defer srv.Close()

	// The stale scheduler resends its desired task list through the real
	// client, asserting the identity recovered before the Pod was deleted.
	staleClient := api.NewClient(srv.URL)
	_, err := setTask(staleClient, &api.Task{
		Name:    tNode.Name,
		Process: tNode.Spec.Process,
	}, sch.taskOwnerFor(tNode), testLogger)
	if err == nil {
		t.Fatal("stale task list accepted by the replacement executor, want owner-fencing rejection")
	}
	if !strings.Contains(err.Error(), "status=409") {
		t.Fatalf("setTask error = %v, want a 409 owner-mismatch rejection", err)
	}
	if mgr.syncCalls != 0 {
		t.Fatalf("manager.Sync calls = %d, want 0: a rejected task list must not reach the task manager", mgr.syncCalls)
	}

	// Control: the same request asserting the replacement's own identity is
	// applied, proving the rejection above came from the owner fence.
	sameOwnerClient := api.NewClient(srv.URL)
	if _, err := setTask(sameOwnerClient, &api.Task{
		Name:    tNode.Name,
		Process: tNode.Spec.Process,
	}, api.TaskOwner{PodName: "pod-1", PodUID: replacementUID}, testLogger); err != nil {
		t.Fatalf("setTask with the replacement's own identity failed: %v", err)
	}
	if mgr.syncCalls != 1 {
		t.Fatalf("manager.Sync calls = %d, want 1 for a same-owner task list", mgr.syncCalls)
	}
}
