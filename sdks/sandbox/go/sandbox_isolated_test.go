// Copyright 2026 Alibaba Group Holding Ltd.
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

package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCreateIsolatedSessionRequest_BindsWireFormat verifies that binds and
// uid_mode serialize to the expected execd wire format.
func TestCreateIsolatedSessionRequest_BindsWireFormat(t *testing.T) {
	req := CreateIsolatedSessionRequest{
		Workspace: IsolatedWorkspaceSpec{Path: "/workspace", Mode: "rw"},
		Binds: []BindMount{
			{Source: "/data/in", Dest: "/mnt/in", ReadOnly: true},
			{Source: "/data/out"},
		},
		UidMode: "userns",
	}

	b, err := json.Marshal(req)
	require.NoError(t, err)
	s := string(b)

	assert.Contains(t, s, `"binds":[`)
	assert.Contains(t, s, `"source":"/data/in"`)
	assert.Contains(t, s, `"dest":"/mnt/in"`)
	assert.Contains(t, s, `"readonly":true`)
	assert.Contains(t, s, `"uid_mode":"userns"`)
	// Empty dest/readonly must be omitted for the second bind.
	require.True(t, strings.Contains(s, `{"source":"/data/out"}`),
		"bind with only source should omit dest/readonly: %s", s)
}

// TestCreateIsolatedSessionRequest_BindsOmittedWhenEmpty verifies binds and
// uid_mode are omitted when unset (backward compatible with existing callers).
func TestCreateIsolatedSessionRequest_BindsOmittedWhenEmpty(t *testing.T) {
	req := CreateIsolatedSessionRequest{
		Workspace: IsolatedWorkspaceSpec{Path: "/workspace"},
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	s := string(b)
	require.True(t, !strings.Contains(s, "binds"), "binds should be omitted: %s", s)
	require.True(t, !strings.Contains(s, "uid_mode"), "uid_mode should be omitted: %s", s)
}

func TestIsolationRunOnce_CreatesRunsDeletes(t *testing.T) {
	var (
		createCalled int32
		runCalled    int32
		deleteCalled int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session":
			atomic.AddInt32(&createCalled, 1)
			jsonResponse(w, http.StatusCreated, map[string]string{
				"session_id": "sess-test",
				"created_at": "2026-01-01T00:00:00Z",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session/sess-test/run":
			atomic.AddInt32(&runCalled, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: {\"type\":\"complete\"}\n\n")
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/isolated/session/sess-test":
			atomic.AddInt32(&deleteCalled, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	req := CreateIsolatedSessionRequest{
		Workspace: IsolatedWorkspaceSpec{Path: "/workspace", Mode: "overlay"},
	}
	run := IsolatedRunRequest{Code: "echo hello"}

	_, err := sb.IsolationRunOnce(context.Background(), req, run, nil)
	require.NoError(t, err)

	if atomic.LoadInt32(&createCalled) != 1 {
		assert.Fail(t, "create should be called once")
	}
	if atomic.LoadInt32(&runCalled) != 1 {
		assert.Fail(t, "run should be called once")
	}
	if atomic.LoadInt32(&deleteCalled) != 1 {
		assert.Fail(t, "delete should be called once")
	}
}

func TestIsolationListSessions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/isolated/sessions":
			jsonResponse(w, http.StatusOK, map[string]any{
				"sessions": []map[string]any{
					{
						"session_id":             "sess-1",
						"status":                 "active",
						"created_at":             "2026-01-01T00:00:00Z",
						"last_run_at":            "2026-01-01T00:01:00Z",
						"idle_remaining_seconds": 30,
					},
					{
						"session_id":  "sess-2",
						"status":      "dead",
						"created_at":  "2026-01-01T00:00:00Z",
						"last_run_at": "2026-01-01T00:00:00Z",
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	sessions, err := sb.IsolationListSessions(context.Background())
	require.NoError(t, err)
	require.Len(t, sessions, 2)

	require.Equal(t, "sess-1", sessions[0].SessionID)
	require.Equal(t, "active", sessions[0].Status)
	require.NotNil(t, sessions[0].IdleRemainingSeconds)
	require.Equal(t, 30, *sessions[0].IdleRemainingSeconds)

	require.Equal(t, "sess-2", sessions[1].SessionID)
	require.Equal(t, "dead", sessions[1].Status)
	require.True(t, sessions[1].IdleRemainingSeconds == nil)
}

func TestIsolationListSessions_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/isolated/sessions" {
			jsonResponse(w, http.StatusOK, map[string]any{"sessions": []any{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	sessions, err := sb.IsolationListSessions(context.Background())
	require.NoError(t, err)
	require.Len(t, sessions, 0)
}

func TestIsolationRunOnce_DeletesOnRunError(t *testing.T) {
	var deleteCalled int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session":
			jsonResponse(w, http.StatusCreated, map[string]string{
				"session_id": "sess-fail",
				"created_at": "2026-01-01T00:00:00Z",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session/sess-fail/run":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/isolated/session/sess-fail":
			atomic.AddInt32(&deleteCalled, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	req := CreateIsolatedSessionRequest{
		Workspace: IsolatedWorkspaceSpec{Path: "/workspace"},
	}
	run := IsolatedRunRequest{Code: "bad cmd"}

	_, err := sb.IsolationRunOnce(context.Background(), req, run, nil)
	if err == nil {
		assert.Fail(t, "expected error from run")
	}

	if atomic.LoadInt32(&deleteCalled) != 1 {
		assert.Fail(t, "delete should still be called on run failure")
	}
}

func TestIsolationWithSession_CallbackAndCleanup(t *testing.T) {
	var (
		callbackCalled int32
		deleteCalled   int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session":
			jsonResponse(w, http.StatusCreated, map[string]string{
				"session_id": "sess-with",
				"created_at": "2026-01-01T00:00:00Z",
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/isolated/session/sess-with":
			atomic.AddInt32(&deleteCalled, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	req := CreateIsolatedSessionRequest{
		Workspace: IsolatedWorkspaceSpec{Path: "/workspace"},
	}

	err := sb.IsolationWithSession(context.Background(), req, func(s *IsolationSession) error {
		atomic.AddInt32(&callbackCalled, 1)
		if s.SessionID() != "sess-with" {
			return fmt.Errorf("unexpected session id: %s", s.SessionID())
		}
		return nil
	})
	require.NoError(t, err)

	if atomic.LoadInt32(&callbackCalled) != 1 {
		assert.Fail(t, "callback should be called once")
	}
	if atomic.LoadInt32(&deleteCalled) != 1 {
		assert.Fail(t, "delete should be called once")
	}
}

func TestIsolationWithSession_DeletesOnCallbackError(t *testing.T) {
	var deleteCalled int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session":
			jsonResponse(w, http.StatusCreated, map[string]string{
				"session_id": "sess-err",
				"created_at": "2026-01-01T00:00:00Z",
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/isolated/session/sess-err":
			atomic.AddInt32(&deleteCalled, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	req := CreateIsolatedSessionRequest{
		Workspace: IsolatedWorkspaceSpec{Path: "/workspace"},
	}

	err := sb.IsolationWithSession(context.Background(), req, func(s *IsolationSession) error {
		return fmt.Errorf("callback error")
	})
	if err == nil {
		assert.Fail(t, "expected error from callback")
	}

	if atomic.LoadInt32(&deleteCalled) != 1 {
		assert.Fail(t, "delete should still be called on callback error")
	}
}
