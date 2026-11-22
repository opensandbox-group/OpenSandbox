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

package task_executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSetWithOwnerAssertsPodIdentity(t *testing.T) {
	var gotPodName, gotPodUID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPodName = r.Header.Get(HeaderTaskOwnerPodName)
		gotPodUID = r.Header.Get(HeaderTaskOwnerPodUID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Task{{Name: "task-1"}})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.SetWithOwner(context.Background(), &Task{Name: "task-1"},
		TaskOwner{PodName: "pod-a", PodUID: "uid-a"})
	if err != nil {
		t.Fatalf("SetWithOwner returned error: %v", err)
	}
	if gotPodName != "pod-a" || gotPodUID != "uid-a" {
		t.Fatalf("owner headers podName=%q podUID=%q, want pod-a/uid-a", gotPodName, gotPodUID)
	}
}

// Senders that do not know the receiver's identity must keep behaving like
// before: no owner headers, older executors are unaffected.
func TestClientSetWithoutOwnerKeepsLegacyRequest(t *testing.T) {
	var gotPodName, gotPodUID string
	var releasePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releasePath = r.URL.Path
		gotPodName = r.Header.Get(HeaderTaskOwnerPodName)
		gotPodUID = r.Header.Get(HeaderTaskOwnerPodUID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Task{})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	if _, err := client.Set(context.Background(), &Task{Name: "task-1"}); err != nil {
		t.Fatalf("Set returned error: %v", err)
	}
	if gotPodName != "" || gotPodUID != "" {
		t.Fatalf("Set asserted owner headers podName=%q podUID=%q, want none", gotPodName, gotPodUID)
	}

	if _, err := client.Set(context.Background(), nil); err != nil {
		t.Fatalf("Set(nil) returned error: %v", err)
	}
	if releasePath != "/setTasks" {
		t.Fatalf("release request path = %q, want /setTasks", releasePath)
	}
}
