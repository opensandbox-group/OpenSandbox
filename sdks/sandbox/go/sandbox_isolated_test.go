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

// TestIsolationAttach_PopulatesFullInfo verifies that IsolationAttach
// populates IsolatedSessionInfo with all creation-parameter echoes when
// the execd server returns them in the SessionState response.
func TestIsolationAttach_PopulatesFullInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/isolated/session/sess-full" {
			jsonResponse(w, http.StatusOK, map[string]any{
				"status":                 "active",
				"created_at":             "2026-01-01T00:00:00Z",
				"last_run_at":            "2026-01-01T00:01:00Z",
				"idle_remaining_seconds": 42,
				"profile":                "python",
				"workspace":              map[string]any{"path": "/workspace", "mode": "overlay"},
				"extra_writable":         []string{"/tmp", "/var/tmp"},
				"binds": []map[string]any{
					{"source": "/data/in", "dest": "/mnt/in", "readonly": true},
					{"source": "/data/out"},
				},
				"share_net": true,
				"env_passthrough": map[string]any{
					"mode": "allow",
					"keys": []string{"HTTP_PROXY", "HTTPS_PROXY"},
				},
				"uid":                  1000,
				"gid":                  1000,
				"uid_mode":             "userns",
				"idle_timeout_seconds": 600,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	session, err := sb.IsolationAttach(context.Background(), "sess-full")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, "sess-full", session.SessionID())

	info := session.Info()
	require.NotNil(t, info)
	require.Equal(t, "sess-full", info.SessionID)
	require.Equal(t, "python", info.Profile)

	require.NotNil(t, info.Workspace)
	require.Equal(t, "/workspace", info.Workspace.Path)
	require.Equal(t, "overlay", info.Workspace.Mode)

	require.Len(t, info.ExtraWritable, 2)
	require.Equal(t, "/tmp", info.ExtraWritable[0])
	require.Equal(t, "/var/tmp", info.ExtraWritable[1])

	require.Len(t, info.Binds, 2)
	require.Equal(t, "/data/in", info.Binds[0].Source)
	require.Equal(t, "/mnt/in", info.Binds[0].Dest)
	require.True(t, info.Binds[0].ReadOnly)
	require.Equal(t, "/data/out", info.Binds[1].Source)
	require.Equal(t, "", info.Binds[1].Dest)
	require.True(t, !info.Binds[1].ReadOnly)

	require.NotNil(t, info.ShareNet)
	require.Equal(t, true, *info.ShareNet)

	require.NotNil(t, info.EnvPassthrough)
	require.Equal(t, "allow", info.EnvPassthrough.Mode)
	require.Len(t, info.EnvPassthrough.Keys, 2)
	require.Equal(t, "HTTP_PROXY", info.EnvPassthrough.Keys[0])

	require.NotNil(t, info.Uid)
	require.Equal(t, uint32(1000), *info.Uid)
	require.NotNil(t, info.Gid)
	require.Equal(t, uint32(1000), *info.Gid)
	require.Equal(t, "userns", info.UidMode)
	require.NotNil(t, info.IdleTimeoutSeconds)
	require.Equal(t, 600, *info.IdleTimeoutSeconds)

	require.NotNil(t, session.Files())
}

// TestIsolationAttach_ToleratesMissingCreationParams verifies that
// IsolationAttach still constructs a working handle when the execd server
// returns a minimal SessionState with no creation-parameter echoes (older
// execd builds). It also proves that Run/Get/Delete route through the
// handle correctly using only the sessionID.
func TestIsolationAttach_ToleratesMissingCreationParams(t *testing.T) {
	var (
		getCalled    int32
		runCalled    int32
		deleteCalled int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/isolated/session/sess-min":
			atomic.AddInt32(&getCalled, 1)
			jsonResponse(w, http.StatusOK, map[string]any{
				"status":      "active",
				"created_at":  "2026-01-01T00:00:00Z",
				"last_run_at": "2026-01-01T00:01:00Z",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/isolated/session/sess-min/run":
			atomic.AddInt32(&runCalled, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: {\"type\":\"complete\"}\n\n")
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/isolated/session/sess-min":
			atomic.AddInt32(&deleteCalled, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	session, err := sb.IsolationAttach(context.Background(), "sess-min")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, "sess-min", session.SessionID())

	// All echo fields should be zero-valued when the server did not echo them.
	info := session.Info()
	require.Equal(t, "", info.Profile)
	require.True(t, info.Workspace == nil)
	require.Len(t, info.ExtraWritable, 0)
	require.Len(t, info.Binds, 0)
	require.True(t, info.ShareNet == nil)
	require.True(t, info.EnvPassthrough == nil)
	require.True(t, info.Uid == nil)
	require.True(t, info.Gid == nil)
	require.Equal(t, "", info.UidMode)
	require.True(t, info.IdleTimeoutSeconds == nil, "older execd omitted idle_timeout_seconds — expect nil, not zero-value")

	// Run/Get/Delete must still work through the handle (they only need the sessionID).
	state, err := session.Get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, "active", state.Status)

	_, err = session.Run(context.Background(), IsolatedRunRequest{Code: "echo hi"}, nil)
	require.NoError(t, err)

	err = session.Delete(context.Background())
	require.NoError(t, err)

	// getCalled == 2: once for attach, once for session.Get.
	if atomic.LoadInt32(&getCalled) != 2 {
		assert.Fail(t, fmt.Sprintf("expected 2 GET calls, got %d", atomic.LoadInt32(&getCalled)))
	}
	if atomic.LoadInt32(&runCalled) != 1 {
		assert.Fail(t, "run should be called once")
	}
	if atomic.LoadInt32(&deleteCalled) != 1 {
		assert.Fail(t, "delete should be called once")
	}
}

// TestIsolationAttach_NotFound verifies that a 404 from the server surfaces
// as an *APIError with StatusCode 404, matching the error type used by
// IsolatedGet for a missing session.
// TestIsolationAttach_PreservesIdleTimeoutZero verifies that a session
// created with idle_timeout_seconds=0 (idle GC disabled — the long-window
// stateless-recovery configuration) round-trips through attach as a
// non-nil pointer to 0, distinct from nil (which means "older execd
// omitted the field").
func TestIsolationAttach_PreservesIdleTimeoutZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/isolated/session/sess-zero-idle" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{
				"status": "active",
				"created_at": "2026-07-14T00:00:00Z",
				"last_run_at": "2026-07-14T00:00:00Z",
				"profile": "strict",
				"workspace": {"path": "/workspace", "mode": "rw"},
				"idle_timeout_seconds": 0
			}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	session, err := sb.IsolationAttach(context.Background(), "sess-zero-idle")
	require.NoError(t, err)
	require.NotNil(t, session)

	info := session.Info()
	require.NotNil(t, info.IdleTimeoutSeconds,
		"idle_timeout_seconds:0 must decode as non-nil (explicit no-idle-GC), not as nil")
	require.Equal(t, 0, *info.IdleTimeoutSeconds)
}

func TestIsolationAttach_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/isolated/session/missing" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":"NotFound","message":"session missing not found"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	session, err := sb.IsolationAttach(context.Background(), "missing")
	require.Error(t, err)
	require.True(t, session == nil)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	require.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}

// TestIsolationAttach_EmptySessionID verifies that IsolationAttach rejects
// empty session IDs without making an HTTP call.
func TestIsolationAttach_EmptySessionID(t *testing.T) {
	execd := NewExecdClient("http://unused.invalid", "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}

	session, err := sb.IsolationAttach(context.Background(), "")
	require.Error(t, err)
	require.True(t, session == nil)

	var invalid *InvalidArgumentError
	require.ErrorAs(t, err, &invalid)
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
