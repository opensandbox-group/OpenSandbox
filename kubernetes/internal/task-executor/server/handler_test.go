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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// MockTaskManager implements manager.TaskManager for testing
type MockTaskManager struct {
	tasks map[string]*types.Task
	err   error
}

func NewMockTaskManager() *MockTaskManager {
	return &MockTaskManager{
		tasks: make(map[string]*types.Task),
	}
}

func (m *MockTaskManager) Create(ctx context.Context, task *types.Task) (*types.Task, error) {
	if m.err != nil {
		return nil, m.err
	}
	m.tasks[task.Name] = task
	return task, nil
}

func (m *MockTaskManager) Sync(ctx context.Context, desired []*types.Task) ([]*types.Task, error) {
	if m.err != nil {
		return nil, m.err
	}
	m.tasks = make(map[string]*types.Task)
	var result []*types.Task
	for _, t := range desired {
		m.tasks[t.Name] = t
		result = append(result, t)
	}
	return result, nil
}

func (m *MockTaskManager) Get(ctx context.Context, id string) (*types.Task, error) {
	if m.err != nil {
		return nil, m.err
	}
	if t, ok := m.tasks[id]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("not found")
}

func (m *MockTaskManager) List(ctx context.Context) ([]*types.Task, error) {
	if m.err != nil {
		return nil, m.err
	}
	var list []*types.Task
	for _, t := range m.tasks {
		list = append(list, t)
	}
	return list, nil
}

func (m *MockTaskManager) Delete(ctx context.Context, id string) error {
	if m.err != nil {
		return m.err
	}
	delete(m.tasks, id)
	return nil
}

func (m *MockTaskManager) Start(ctx context.Context) {}
func (m *MockTaskManager) Stop()                     {}

func TestHandler_Health(t *testing.T) {
	cfg := &config.Config{}
	h := NewHandler(NewMockTaskManager(), cfg)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	h.Health(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Health returned status %d", w.Code)
	}
}

func TestHandler_CreateTask(t *testing.T) {
	mgr := NewMockTaskManager()
	cfg := &config.Config{}
	h := NewHandler(mgr, cfg)

	task := api.Task{
		Name: "test-task",
		Process: &api.Process{
			Command: []string{"echo"},
		},
	}
	body, _ := json.Marshal(task)

	req := httptest.NewRequest("POST", "/tasks", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.CreateTask(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("CreateTask returned status %d", w.Code)
	}

	if _, ok := mgr.tasks["test-task"]; !ok {
		t.Error("Task was not created in manager")
	}
}

func TestHandler_GetTask(t *testing.T) {
	mgr := NewMockTaskManager()
	mgr.tasks["test-task"] = &types.Task{Name: "test-task"}
	cfg := &config.Config{}
	h := NewHandler(mgr, cfg)

	router := NewRouter(h)
	req := httptest.NewRequest("GET", "/tasks/test-task", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GetTask returned status %d", w.Code)
	}

	var resp api.Task
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Name != "test-task" {
		t.Errorf("GetTask returned name %s", resp.Name)
	}
}

func TestHandler_DeleteTask(t *testing.T) {
	mgr := NewMockTaskManager()
	mgr.tasks["test-task"] = &types.Task{Name: "test-task"}
	cfg := &config.Config{}
	h := NewHandler(mgr, cfg)
	router := NewRouter(h)

	req := httptest.NewRequest("DELETE", "/tasks/test-task", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("DeleteTask returned status %d", w.Code)
	}

	if _, ok := mgr.tasks["test-task"]; ok {
		t.Error("Task was not deleted from manager")
	}
}

func TestHandler_ListTasks(t *testing.T) {
	mgr := NewMockTaskManager()
	mgr.tasks["task-1"] = &types.Task{Name: "task-1"}
	mgr.tasks["task-2"] = &types.Task{Name: "task-2"}
	cfg := &config.Config{}
	h := NewHandler(mgr, cfg)

	req := httptest.NewRequest("GET", "/getTasks", nil)
	w := httptest.NewRecorder()

	h.ListTasks(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ListTasks returned status %d", w.Code)
	}

	var resp []api.Task
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp) != 2 {
		t.Errorf("ListTasks returned %d tasks, want 2", len(resp))
	}
}

func TestHandler_SyncTasks(t *testing.T) {
	mgr := NewMockTaskManager()
	cfg := &config.Config{}
	h := NewHandler(mgr, cfg)

	tasks := []api.Task{
		{Name: "task-1", Process: &api.Process{}},
	}
	body, _ := json.Marshal(tasks)

	req := httptest.NewRequest("POST", "/setTasks", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.SyncTasks(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("SyncTasks returned status %d", w.Code)
	}

	if _, ok := mgr.tasks["task-1"]; !ok {
		t.Error("Task was not synced to manager")
	}
}

func TestHandler_Errors(t *testing.T) {
	mgr := NewMockTaskManager()
	mgr.err = errors.New("mock error")
	cfg := &config.Config{}
	h := NewHandler(mgr, cfg)

	// Create fail
	task := api.Task{Name: "fail"}
	body, _ := json.Marshal(task)
	req := httptest.NewRequest("POST", "/tasks", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateTask(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("CreateTask should fail with 500, got %d", w.Code)
	}
}

func TestConvertInternalToAPITask(t *testing.T) {
	now := time.Now()

	t.Run("Process Task", func(t *testing.T) {
		task := &types.Task{
			Name:    "proc-task",
			Process: &api.Process{Command: []string{"ls"}},
			Status: types.Status{
				State: types.TaskStateSucceeded,
				SubStatuses: []types.SubStatus{
					{
						ExitCode:   0,
						Reason:     "Completed",
						FinishedAt: &now,
					},
				},
			},
		}

		apiTask := convertInternalToAPITask(task)
		assert.NotNil(t, apiTask.ProcessStatus)
		assert.NotNil(t, apiTask.ProcessStatus.Terminated)
		assert.Equal(t, int32(0), apiTask.ProcessStatus.Terminated.ExitCode)
		assert.Nil(t, apiTask.PodStatus)
	})

	t.Run("Pod Task - Partially Ready", func(t *testing.T) {
		task := &types.Task{
			Name:            "pod-task-partial",
			PodTemplateSpec: &corev1.PodTemplateSpec{},
			Status: types.Status{
				State: types.TaskStateRunning,
				SubStatuses: []types.SubStatus{
					{
						Name:      "c1",
						StartedAt: &now,
					},
					{
						Name:   "c2",
						Reason: "Pending",
					},
				},
			},
		}

		apiTask := convertInternalToAPITask(task)
		assert.NotNil(t, apiTask.PodStatus)
		assert.Equal(t, corev1.PodRunning, apiTask.PodStatus.Phase)
		assert.Len(t, apiTask.PodStatus.ContainerStatuses, 2)
		assert.True(t, apiTask.PodStatus.ContainerStatuses[0].Ready)
		assert.False(t, apiTask.PodStatus.ContainerStatuses[1].Ready)
		assert.False(t, utils.IsPodReadyConditionTrue(*apiTask.PodStatus))

		var podReady, containersReady *corev1.PodCondition
		for i := range apiTask.PodStatus.Conditions {
			c := &apiTask.PodStatus.Conditions[i]
			if c.Type == corev1.PodReady {
				podReady = c
			} else if c.Type == corev1.ContainersReady {
				containersReady = c
			}
		}
		assert.NotNil(t, podReady)
		assert.Equal(t, corev1.ConditionFalse, podReady.Status)
		assert.NotNil(t, containersReady)
		assert.Equal(t, corev1.ConditionFalse, containersReady.Status)
		assert.Equal(t, now.Unix(), podReady.LastTransitionTime.Unix())
	})

	t.Run("Pod Task - Fully Ready", func(t *testing.T) {
		later := now.Add(time.Minute)
		task := &types.Task{
			Name:            "pod-task-ready",
			PodTemplateSpec: &corev1.PodTemplateSpec{},
			Status: types.Status{
				State: types.TaskStateRunning,
				SubStatuses: []types.SubStatus{
					{
						Name:      "c1",
						StartedAt: &now,
					},
					{
						Name:      "c2",
						StartedAt: &later,
					},
				},
			},
		}

		apiTask := convertInternalToAPITask(task)
		assert.NotNil(t, apiTask.PodStatus)

		var podReady, containersReady *corev1.PodCondition
		for i := range apiTask.PodStatus.Conditions {
			c := &apiTask.PodStatus.Conditions[i]
			if c.Type == corev1.PodReady {
				podReady = c
			} else if c.Type == corev1.ContainersReady {
				containersReady = c
			}
		}
		assert.NotNil(t, podReady)
		assert.Equal(t, corev1.ConditionTrue, podReady.Status)
		assert.NotNil(t, containersReady)
		assert.Equal(t, corev1.ConditionTrue, containersReady.Status)
		// Should use the latest timestamp (later)
		assert.Equal(t, later.Unix(), podReady.LastTransitionTime.Unix())
		assert.True(t, utils.IsPodReadyConditionTrue(*apiTask.PodStatus))
	})
}

func TestConvertInternalToAPITask_Timeout(t *testing.T) {
	now := time.Now()
	timeoutSec := int64(60)

	t.Run("Process Task Timeout", func(t *testing.T) {
		task := &types.Task{
			Name: "timeout-task",
			Process: &api.Process{
				Command:        []string{"sleep", "100"},
				TimeoutSeconds: &timeoutSec,
			},
			Status: types.Status{
				State: types.TaskStateTimeout,
				SubStatuses: []types.SubStatus{
					{
						Reason:     "TaskTimeout",
						Message:    "Task exceeded timeout of 60 seconds",
						StartedAt:  &now,
						FinishedAt: nil, // Not finished yet
					},
				},
			},
		}

		apiTask := convertInternalToAPITask(task)

		// Should map to Terminated with exit code 137
		assert.NotNil(t, apiTask.ProcessStatus)
		assert.NotNil(t, apiTask.ProcessStatus.Terminated)
		assert.Nil(t, apiTask.ProcessStatus.Running)
		assert.Nil(t, apiTask.ProcessStatus.Waiting)
		assert.Equal(t, int32(137), apiTask.ProcessStatus.Terminated.ExitCode)
		assert.Equal(t, "TaskTimeout", apiTask.ProcessStatus.Terminated.Reason)
		assert.Equal(t, "Task exceeded timeout of 60 seconds", apiTask.ProcessStatus.Terminated.Message)
		assert.Equal(t, now.Unix(), apiTask.ProcessStatus.Terminated.StartedAt.Unix())
		// FinishedAt should be set to "now" for timeout
		assert.False(t, apiTask.ProcessStatus.Terminated.FinishedAt.IsZero())
		assert.Nil(t, apiTask.PodStatus)
	})

	t.Run("Timeout After Completion", func(t *testing.T) {
		later := now.Add(2 * time.Minute)
		task := &types.Task{
			Name:    "completed-task",
			Process: &api.Process{Command: []string{"ls"}},
			Status: types.Status{
				State: types.TaskStateFailed, // After stop, it becomes Failed
				SubStatuses: []types.SubStatus{
					{
						ExitCode:   137,
						Reason:     "Killed",
						StartedAt:  &now,
						FinishedAt: &later,
					},
				},
			},
		}

		apiTask := convertInternalToAPITask(task)

		// Should be Terminated with actual exit code
		assert.NotNil(t, apiTask.ProcessStatus.Terminated)
		assert.Equal(t, int32(137), apiTask.ProcessStatus.Terminated.ExitCode)
		assert.Equal(t, now.Unix(), apiTask.ProcessStatus.Terminated.StartedAt.Unix())
		assert.Equal(t, later.Unix(), apiTask.ProcessStatus.Terminated.FinishedAt.Unix())
	})
}
