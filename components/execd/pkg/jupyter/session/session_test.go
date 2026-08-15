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

package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListSessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected request method GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/sessions" {
			t.Errorf("expected request path /api/sessions, got %s", r.URL.Path)
		}

		response := `[
			{
				"id": "session-1",
				"path": "/path/to/notebook1.ipynb",
				"name": "Session 1",
				"type": "notebook",
				"kernel": {
					"id": "kernel-1",
					"name": "python3",
					"last_activity": "2023-01-01T00:00:00Z",
					"execution_state": "idle",
					"connections": 1
				}
			},
			{
				"id": "session-2",
				"path": "/path/to/notebook2.ipynb",
				"name": "Session 2",
				"type": "notebook",
				"kernel": {
					"id": "kernel-2",
					"name": "python3",
					"last_activity": "2023-01-01T00:00:00Z",
					"execution_state": "idle",
					"connections": 1
				}
			}
		]`

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(response))
	}))
	defer server.Close()

	client := NewClient(server.URL, &http.Client{})

	sessions, err := client.ListSessions()
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}

	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}

	if sessions[0].ID != "session-1" {
		t.Errorf("expected session ID 'session-1', got '%s'", sessions[0].ID)
	}
	if sessions[0].Name != "Session 1" {
		t.Errorf("expected session name 'Session 1', got '%s'", sessions[0].Name)
	}
	if sessions[0].Path != "/path/to/notebook1.ipynb" {
		t.Errorf("expected session path '/path/to/notebook1.ipynb', got '%s'", sessions[0].Path)
	}
	if sessions[0].Type != "notebook" {
		t.Errorf("expected session type 'notebook', got '%s'", sessions[0].Type)
	}

	if sessions[0].Kernel.ID != "kernel-1" {
		t.Errorf("expected kernel ID 'kernel-1', got '%s'", sessions[0].Kernel.ID)
	}
	if sessions[0].Kernel.Name != "python3" {
		t.Errorf("expected kernel name 'python3', got '%s'", sessions[0].Kernel.Name)
	}
}

func TestCreateSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected request method POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/sessions" {
			t.Errorf("expected request path /api/sessions, got %s", r.URL.Path)
		}

		var requestBody SessionCreateRequest
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&requestBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		// Validate request params
		if requestBody.Name != "Test Session" {
			t.Errorf("expected session name 'Test Session', got '%s'", requestBody.Name)
		}
		if requestBody.Path != "/path/to/notebook.ipynb" {
			t.Errorf("expected session path '/path/to/notebook.ipynb', got '%s'", requestBody.Path)
		}
		if requestBody.Type != "notebook" {
			t.Errorf("expected session type 'notebook', got '%s'", requestBody.Type)
		}
		if requestBody.Kernel.Name != "python3" {
			t.Errorf("expected kernel name 'python3', got '%s'", requestBody.Kernel.Name)
		}

		response := `{
			"id": "new-session-id",
			"path": "/path/to/notebook.ipynb",
			"name": "Test Session",
			"type": "notebook",
			"kernel": {
				"id": "new-kernel-id",
				"name": "python3",
				"last_activity": "2023-01-01T00:00:00Z",
				"execution_state": "idle",
				"connections": 0
			}
		}`

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(response))
	}))
	defer server.Close()

	client := NewClient(server.URL, &http.Client{})

	newSession, err := client.CreateSession("Test Session", "/path/to/notebook.ipynb", "python3")
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	if newSession.ID != "new-session-id" {
		t.Errorf("expected session ID 'new-session-id', got '%s'", newSession.ID)
	}
	if newSession.Name != "Test Session" {
		t.Errorf("expected session name 'Test Session', got '%s'", newSession.Name)
	}
	if newSession.Path != "/path/to/notebook.ipynb" {
		t.Errorf("expected session path '/path/to/notebook.ipynb', got '%s'", newSession.Path)
	}
	if newSession.Kernel.ID != "new-kernel-id" {
		t.Errorf("expected kernel ID 'new-kernel-id', got '%s'", newSession.Kernel.ID)
	}
}

func TestGetSession(t *testing.T) {
	sessionID := "test-session-id"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected request method GET, got %s", r.Method)
		}

		expectedPath := "/api/sessions/" + sessionID
		if r.URL.Path != expectedPath {
			t.Errorf("expected request path '%s', got '%s'", expectedPath, r.URL.Path)
		}

		response := `{
			"id": "test-session-id",
			"path": "/path/to/notebook.ipynb",
			"name": "Test Session",
			"type": "notebook",
			"kernel": {
				"id": "test-kernel-id",
				"name": "python3",
				"last_activity": "2023-01-01T00:00:00Z",
				"execution_state": "idle",
				"connections": 1
			}
		}`

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(response))
	}))
	defer server.Close()

	client := NewClient(server.URL, &http.Client{})

	session, err := client.GetSession(sessionID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}

	if session.ID != sessionID {
		t.Errorf("expected session ID '%s', got '%s'", sessionID, session.ID)
	}
	if session.Name != "Test Session" {
		t.Errorf("expected session name 'Test Session', got '%s'", session.Name)
	}
	if session.Kernel.ID != "test-kernel-id" {
		t.Errorf("expected kernel ID 'test-kernel-id', got '%s'", session.Kernel.ID)
	}
}
