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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/manager"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type handler struct {
	manager manager.TaskManager
	config  *config.Config
}

func NewHandler(mgr manager.TaskManager, cfg *config.Config) *handler {
	if mgr == nil {
		klog.Warning("TaskManager is nil, handler may not work properly")
	}
	if cfg == nil {
		klog.Warning("Config is nil, handler may not work properly")
	}
	return &handler{
		manager: mgr,
		config:  cfg,
	}
}

func (h *handler) createTask(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		writeError(w, http.StatusInternalServerError, "task manager not initialized")
		return
	}

	var apiTask api.Task
	if err := json.NewDecoder(r.Body).Decode(&apiTask); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	if apiTask.Name == "" {
		writeError(w, http.StatusBadRequest, "task name is required")
		return
	}

	task := h.convertAPIToInternalTask(&apiTask)
	if task == nil {
		writeError(w, http.StatusBadRequest, "failed to convert task")
		return
	}

	created, err := h.manager.Create(r.Context(), task)
	if err != nil {
		klog.ErrorS(err, "failed to create task", "name", apiTask.Name)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create task: %v", err))
		return
	}

	response := convertInternalToAPITask(created)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(response)

	klog.InfoS("task created via API", "name", apiTask.Name)
}

func (h *handler) syncTasks(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		writeError(w, http.StatusInternalServerError, "task manager not initialized")
		return
	}

	var apiTasks []api.Task
	if err := json.NewDecoder(r.Body).Decode(&apiTasks); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	desired := make([]*types.Task, 0, len(apiTasks))
	for i := range apiTasks {
		if apiTasks[i].Name == "" {
			continue
		}
		task := h.convertAPIToInternalTask(&apiTasks[i])
		if task != nil {
			desired = append(desired, task)
		}
	}

	current, err := h.manager.Sync(r.Context(), desired)
	if err != nil {
		klog.ErrorS(err, "failed to sync tasks")
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to sync tasks: %v", err))
		return
	}

	response := make([]api.Task, 0, len(current))
	for _, task := range current {
		if task != nil {
			response = append(response, *convertInternalToAPITask(task))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	klog.V(1).InfoS("tasks synced via API", "count", len(response))
}

func (h *handler) getTask(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		writeError(w, http.StatusInternalServerError, "task manager not initialized")
		return
	}

	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	task, err := h.manager.Get(r.Context(), taskID)
	if err != nil {
		klog.ErrorS(err, "failed to get task", "id", taskID)
		writeError(w, http.StatusNotFound, fmt.Sprintf("task not found: %v", err))
		return
	}

	response := convertInternalToAPITask(task)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *handler) listTasks(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		writeError(w, http.StatusInternalServerError, "task manager not initialized")
		return
	}

	tasks, err := h.manager.List(r.Context())
	if err != nil {
		klog.ErrorS(err, "failed to list tasks")
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
		return
	}

	response := make([]api.Task, 0, len(tasks))
	for _, task := range tasks {
		if task != nil {
			response = append(response, *convertInternalToAPITask(task))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	response := map[string]string{
		"status": "healthy",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		writeError(w, http.StatusInternalServerError, "task manager not initialized")
		return
	}

	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	err := h.manager.Delete(r.Context(), taskID)
	if err != nil {
		klog.ErrorS(err, "failed to delete task", "id", taskID)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete task: %v", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
	klog.InfoS("task deleted via API", "id", taskID)
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{
		Code:    http.StatusText(code),
		Message: message,
	})
}

func (h *handler) convertAPIToInternalTask(apiTask *api.Task) *types.Task {
	if apiTask == nil {
		return nil
	}
	task := &types.Task{
		Name:            apiTask.Name,
		Process:         apiTask.Process,
		PodTemplateSpec: apiTask.PodTemplateSpec,
	}
	task.Status = types.Status{
		State: types.TaskStatePending,
	}

	return task
}

func convertInternalToAPITask(task *types.Task) *api.Task {
	if task == nil {
		return nil
	}

	apiTask := &api.Task{
		Name:            task.Name,
		Process:         task.Process,
		PodTemplateSpec: task.PodTemplateSpec,
	}

	if task.Process != nil && len(task.Status.SubStatuses) > 0 {
		sub := task.Status.SubStatuses[0]
		apiStatus := &api.ProcessStatus{}

		if task.Status.State == types.TaskStateTimeout {
			term := &api.Terminated{
				ExitCode: 137,
				Reason:   sub.Reason,
				Message:  sub.Message,
			}
			if sub.StartedAt != nil {
				term.StartedAt = metav1.NewTime(*sub.StartedAt)
			}
			term.FinishedAt = metav1.Now()
			apiStatus.Terminated = term
		} else if sub.FinishedAt != nil {
			term := &api.Terminated{
				ExitCode: int32(sub.ExitCode),
				Reason:   sub.Reason,
				Message:  sub.Message,
			}
			term.FinishedAt = metav1.NewTime(*sub.FinishedAt)
			if sub.StartedAt != nil {
				term.StartedAt = metav1.NewTime(*sub.StartedAt)
			}
			apiStatus.Terminated = term
		} else if sub.StartedAt != nil {
			apiStatus.Running = &api.Running{
				StartedAt: metav1.NewTime(*sub.StartedAt),
			}
		} else {
			apiStatus.Waiting = &api.Waiting{
				Reason:  sub.Reason,
				Message: sub.Message,
			}
		}
		apiTask.ProcessStatus = apiStatus
	}

	if task.PodTemplateSpec != nil {
		podStatus := &corev1.PodStatus{
			Phase: corev1.PodUnknown,
		}

		switch task.Status.State {
		case types.TaskStatePending:
			podStatus.Phase = corev1.PodPending
		case types.TaskStateRunning:
			podStatus.Phase = corev1.PodRunning
		case types.TaskStateSucceeded:
			podStatus.Phase = corev1.PodSucceeded
		case types.TaskStateFailed:
			podStatus.Phase = corev1.PodFailed
		}

		for _, sub := range task.Status.SubStatuses {
			cs := corev1.ContainerStatus{
				Name: sub.Name,
			}
			if sub.FinishedAt != nil {
				cs.State.Terminated = &corev1.ContainerStateTerminated{
					ExitCode:   int32(sub.ExitCode),
					Reason:     sub.Reason,
					Message:    sub.Message,
					FinishedAt: metav1.NewTime(*sub.FinishedAt),
				}
				if sub.StartedAt != nil {
					cs.State.Terminated.StartedAt = metav1.NewTime(*sub.StartedAt)
				}
			} else if sub.StartedAt != nil {
				cs.State.Running = &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(*sub.StartedAt),
				}
				cs.Ready = true
			} else {
				cs.State.Waiting = &corev1.ContainerStateWaiting{
					Reason:  sub.Reason,
					Message: sub.Message,
				}
			}
			podStatus.ContainerStatuses = append(podStatus.ContainerStatuses, cs)
		}

		allReady := len(podStatus.ContainerStatuses) > 0
		for _, cs := range podStatus.ContainerStatuses {
			if !cs.Ready {
				allReady = false
				break
			}
		}
		readyStatus := corev1.ConditionFalse
		if allReady {
			readyStatus = corev1.ConditionTrue
		}

		var latestTransition time.Time
		for _, sub := range task.Status.SubStatuses {
			if sub.StartedAt != nil && sub.StartedAt.After(latestTransition) {
				latestTransition = *sub.StartedAt
			}
			if sub.FinishedAt != nil && sub.FinishedAt.After(latestTransition) {
				latestTransition = *sub.FinishedAt
			}
		}
		ltt := metav1.NewTime(latestTransition)
		if latestTransition.IsZero() {
			ltt = metav1.Now()
		}

		podStatus.Conditions = append(podStatus.Conditions,
			corev1.PodCondition{
				Type:               corev1.PodReady,
				Status:             readyStatus,
				LastTransitionTime: ltt,
			},
			corev1.PodCondition{
				Type:               corev1.ContainersReady,
				Status:             readyStatus,
				LastTransitionTime: ltt,
			},
		)

		apiTask.PodStatus = podStatus
	}

	return apiTask
}
