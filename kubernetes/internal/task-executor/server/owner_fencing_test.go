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

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/klog/v2"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func TestOwnerMismatch(t *testing.T) {
	tests := []struct {
		name    string
		local   api.TaskOwner
		claimed api.TaskOwner
		want    bool
	}{
		{
			name:    "same pod uid",
			local:   api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"},
			claimed: api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"},
			want:    false,
		},
		{
			name:    "different pod uid wins over a matching name",
			local:   api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"},
			claimed: api.TaskOwner{PodName: "pod-a", PodUID: "uid-b"},
			want:    true,
		},
		{
			name:    "different pod uid on the receiver's old IP",
			local:   api.TaskOwner{PodName: "replacement", PodUID: "uid-new"},
			claimed: api.TaskOwner{PodName: "stale", PodUID: "uid-old"},
			want:    true,
		},
		{
			name:    "names compared when no uid is available",
			local:   api.TaskOwner{PodName: "pod-a"},
			claimed: api.TaskOwner{PodName: "pod-b"},
			want:    true,
		},
		{
			name:    "same names when no uid is available",
			local:   api.TaskOwner{PodName: "pod-a"},
			claimed: api.TaskOwner{PodName: "pod-a"},
			want:    false,
		},
		{
			name:    "names compared when the sender does not know the uid",
			local:   api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"},
			claimed: api.TaskOwner{PodName: "pod-b"},
			want:    true,
		},
		{
			name:    "receiver has no identity, fencing disabled",
			local:   api.TaskOwner{},
			claimed: api.TaskOwner{PodName: "pod-b", PodUID: "uid-b"},
			want:    false,
		},
		{
			name:    "sender asserts no identity, legacy sender",
			local:   api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"},
			claimed: api.TaskOwner{},
			want:    false,
		},
		{
			name:    "sender asserts only a uid the receiver cannot compare",
			local:   api.TaskOwner{PodName: "pod-a"},
			claimed: api.TaskOwner{PodUID: "uid-b"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownerMismatch(tt.local, tt.claimed); got != tt.want {
				t.Fatalf("ownerMismatch(%+v, %+v) = %v, want %v", tt.local, tt.claimed, got, tt.want)
			}
		})
	}
}

func syncRequest(t *testing.T, tasks []api.Task, claimed api.TaskOwner) *http.Request {
	t.Helper()
	body, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("failed to marshal tasks: %v", err)
	}
	req := httptest.NewRequest("POST", "/setTasks", bytes.NewReader(body))
	if claimed.PodName != "" {
		req.Header.Set(api.HeaderTaskOwnerPodName, claimed.PodName)
	}
	if claimed.PodUID != "" {
		req.Header.Set(api.HeaderTaskOwnerPodUID, claimed.PodUID)
	}
	return req
}

func TestHandler_SyncTasksOwnerFencing(t *testing.T) {
	local := api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"}
	tasks := []api.Task{{Name: "task-1", Process: &api.Process{}}}

	tests := []struct {
		name       string
		local      api.TaskOwner
		claimed    api.TaskOwner
		wantStatus int
		wantSync   bool
	}{
		{
			name:       "matching pod uid",
			local:      local,
			claimed:    api.TaskOwner{PodName: "pod-a", PodUID: "uid-a"},
			wantStatus: http.StatusOK,
			wantSync:   true,
		},
		{
			name:       "matching pod name without uids",
			local:      api.TaskOwner{PodName: "pod-a"},
			claimed:    api.TaskOwner{PodName: "pod-a"},
			wantStatus: http.StatusOK,
			wantSync:   true,
		},
		{
			name:       "foreign owner pod uid is rejected",
			local:      local,
			claimed:    api.TaskOwner{PodName: "pod-a", PodUID: "uid-b"},
			wantStatus: http.StatusConflict,
			wantSync:   false,
		},
		{
			name:       "task list of a deleted pod is rejected by the replacement",
			local:      api.TaskOwner{PodName: "replacement", PodUID: "uid-new"},
			claimed:    api.TaskOwner{PodName: "stale", PodUID: "uid-old"},
			wantStatus: http.StatusConflict,
			wantSync:   false,
		},
		{
			name:       "foreign owner pod name is rejected",
			local:      api.TaskOwner{PodName: "pod-a"},
			claimed:    api.TaskOwner{PodName: "pod-b"},
			wantStatus: http.StatusConflict,
			wantSync:   false,
		},
		{
			name:       "legacy sender without identity is accepted",
			local:      local,
			claimed:    api.TaskOwner{},
			wantStatus: http.StatusOK,
			wantSync:   true,
		},
		{
			name:       "executor without identity accepts any sender",
			local:      api.TaskOwner{},
			claimed:    api.TaskOwner{PodName: "pod-b", PodUID: "uid-b"},
			wantStatus: http.StatusOK,
			wantSync:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewMockTaskManager()
			h := NewHandler(mgr, &config.Config{PodName: tt.local.PodName, PodUID: tt.local.PodUID})

			req := syncRequest(t, tasks, tt.claimed)
			w := httptest.NewRecorder()
			h.syncTasks(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("syncTasks returned status %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if gotSync := mgr.syncCalls > 0; gotSync != tt.wantSync {
				t.Fatalf("manager.Sync called = %v, want %v", gotSync, tt.wantSync)
			}
		})
	}
}

// A rejected sync must not disturb the executor: the task it is running keeps
// running, and completed tasks stay readable with their outcome.
func TestHandler_RejectedForeignSyncPreservesCompletedOutcome(t *testing.T) {
	var logBuf bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logBuf)
	t.Cleanup(func() {
		klog.LogToStderr(true)
	})

	mgr := NewMockTaskManager()
	finishedAt := time.Now()
	mgr.tasks["sandbox-b-0"] = &types.Task{
		Name:    "sandbox-b-0",
		Process: &api.Process{Command: []string{"run"}},
		Status: types.Status{
			State: types.TaskStateSucceeded,
			SubStatuses: []types.SubStatus{{
				Reason:     "Completed",
				ExitCode:   0,
				FinishedAt: &finishedAt,
			}},
		},
	}
	h := NewHandler(mgr, &config.Config{PodName: "replacement", PodUID: "uid-new"})

	// A stale scheduler bound to the deleted Pod still pushes its task list.
	req := syncRequest(t, []api.Task{{Name: "stale-sandbox-0", Process: &api.Process{}}},
		api.TaskOwner{PodName: "stale", PodUID: "uid-old"})
	w := httptest.NewRecorder()
	h.syncTasks(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("syncTasks returned status %d, want %d", w.Code, http.StatusConflict)
	}
	if mgr.syncCalls != 0 {
		t.Fatalf("manager.Sync calls = %d, want 0: a foreign task list must not touch local tasks", mgr.syncCalls)
	}
	if _, ok := mgr.tasks["stale-sandbox-0"]; ok {
		t.Fatal("foreign task was created despite the rejection")
	}

	// The completed task is still readable with its outcome.
	getReq := httptest.NewRequest("GET", "/tasks/sandbox-b-0", nil)
	getReq.SetPathValue("id", "sandbox-b-0")
	getW := httptest.NewRecorder()
	h.getTask(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("getTask returned status %d, want %d", getW.Code, http.StatusOK)
	}
	var got api.Task
	if err := json.NewDecoder(getW.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode task response: %v", err)
	}
	if got.ProcessStatus == nil || got.ProcessStatus.Terminated == nil {
		t.Fatal("completed task lost its terminal process status")
	}
	if got.ProcessStatus.Terminated.ExitCode != 0 {
		t.Fatalf("completed task exit code = %d, want 0", got.ProcessStatus.Terminated.ExitCode)
	}

	listReq := httptest.NewRequest("GET", "/getTasks", nil)
	listW := httptest.NewRecorder()
	h.listTasks(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("listTasks returned status %d, want %d", listW.Code, http.StatusOK)
	}
	var list []api.Task
	if err := json.NewDecoder(listW.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode task list response: %v", err)
	}
	if len(list) != 1 || list[0].Name != "sandbox-b-0" {
		t.Fatalf("task list = %+v, want only the completed task sandbox-b-0", list)
	}

	// The rejection is logged, not silent: it names the task and both owners.
	klog.Flush()
	logged := logBuf.String()
	for _, want := range []string{"rejected task sync from a different pod owner", "stale-sandbox-0", "stale", "replacement"} {
		if !bytes.Contains([]byte(logged), []byte(want)) {
			t.Fatalf("rejection log does not contain %q:\n%s", want, logged)
		}
	}
}
