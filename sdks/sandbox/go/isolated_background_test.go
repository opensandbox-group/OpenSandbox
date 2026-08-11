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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newIsolationSessionForTest(sb *Sandbox, sessionID string) *IsolationSession {
	return sb.newIsolationSession(&IsolatedSessionInfo{SessionID: sessionID})
}

// TestIsolationRunBackground_BodyConstruction verifies that RunBackground
// posts code/envs with background:true and never sends timeout_seconds.
func TestIsolationRunBackground_BodyConstruction(t *testing.T) {
	var (
		called    int32
		reqBody   string
		reqMethod string
		reqPath   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		reqMethod = r.Method
		reqPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		reqBody = string(body)
		jsonResponse(w, http.StatusAccepted, map[string]any{
			"session_id": "sess-1",
			"run_id":     "run-1",
			"started_at": "2026-01-02T03:04:05Z",
		})
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	run, err := session.RunBackground(context.Background(), "echo hi", IsolatedRunOpts{
		Envs:           map[string]string{"A": "b"},
		TimeoutSeconds: 30, // must be deliberately dropped for background
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&called))
	require.Equal(t, http.MethodPost, reqMethod)
	require.Equal(t, "/v1/isolated/session/sess-1/run", reqPath)

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(reqBody), &body))
	require.Equal(t, "echo hi", body["code"])
	require.Equal(t, true, body["background"])
	require.True(t, strings.Contains(reqBody, `"envs":{"A":"b"}`), "envs should be sent: %s", reqBody)
	require.True(t, !strings.Contains(reqBody, "timeout_seconds"), "timeout_seconds must not be sent for background: %s", reqBody)

	require.Equal(t, "sess-1", run.SessionID)
	require.Equal(t, "run-1", run.RunID)
	require.Equal(t, "2026-01-02T03:04:05Z", run.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
}

// TestIsolationRunBackground_DefaultOpts verifies RunBackground works with no
// opts and that the response handle parses from a 202.
func TestIsolationRunBackground_DefaultOpts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		require.True(t, !strings.Contains(string(body), "envs"), "envs should be omitted when unset: %s", body)
		jsonResponse(w, http.StatusAccepted, map[string]any{
			"session_id": "sess-1",
			"run_id":     "run-2",
		})
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	run, err := session.RunBackground(context.Background(), "echo hi")
	require.NoError(t, err)
	require.Equal(t, "run-2", run.RunID)
	require.True(t, run.StartedAt.IsZero(), "absent started_at should decode as zero time")
}

// TestIsolationRunBackground_NotFound verifies a 404 surfaces as an *APIError.
func TestIsolationRunBackground_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"SESSION_NOT_FOUND","message":"no such session"}`)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	_, err := session.RunBackground(context.Background(), "echo hi")
	require.Error(t, err)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}

// TestIsolationGetRunStatus_ParsesRunningAndFinished verifies status parsing
// for both a running and a finished background run.
func TestIsolationGetRunStatus_ParsesRunningAndFinished(t *testing.T) {
	var called int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/isolated/session/sess-1/runs/run-1", r.URL.Path)
		if atomic.LoadInt32(&called) == 1 {
			jsonResponse(w, http.StatusOK, map[string]any{
				"session_id": "sess-1",
				"run_id":     "run-1",
				"running":    true,
				"started_at": "2026-01-02T03:04:05Z",
			})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{
			"session_id":  "sess-1",
			"run_id":      "run-1",
			"running":     false,
			"exit_code":   7,
			"started_at":  "2026-01-02T03:04:05Z",
			"finished_at": "2026-01-02T03:04:09Z",
		})
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	running, err := session.GetRunStatus(context.Background(), "run-1")
	require.NoError(t, err)
	require.Equal(t, true, running.Running)
	require.True(t, running.ExitCode == nil)
	require.True(t, running.FinishedAt == nil)

	finished, err := session.GetRunStatus(context.Background(), "run-1")
	require.NoError(t, err)
	require.Equal(t, false, finished.Running)
	require.NotNil(t, finished.ExitCode)
	require.Equal(t, 7, *finished.ExitCode)
	require.NotNil(t, finished.FinishedAt)
	require.Equal(t, "2026-01-02T03:04:09Z", finished.FinishedAt.Format("2006-01-02T15:04:05Z07:00"))
}

// TestIsolationGetRunStatus_TerminatedSession verifies a run whose session
// died mid-flight reports running=false with an error message.
func TestIsolationGetRunStatus_TerminatedSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, map[string]any{
			"session_id": "sess-1",
			"run_id":     "run-1",
			"running":    false,
			"error":      "session terminated",
			"started_at": "2026-01-02T03:04:05Z",
		})
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	status, err := session.GetRunStatus(context.Background(), "run-1")
	require.NoError(t, err)
	require.Equal(t, false, status.Running)
	require.Equal(t, "session terminated", status.Error)
}

// TestIsolationGetRunStatus_NotFound verifies a 404 surfaces as an *APIError.
func TestIsolationGetRunStatus_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"RUN_NOT_FOUND","message":"no such run"}`)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	_, err := session.GetRunStatus(context.Background(), "no-such-run")
	require.Error(t, err)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}

// TestIsolationGetRunLogs_HeaderCursor verifies the cursor query parameter and
// the EXECD-ISOLATED-TAIL-CURSOR header parsing.
func TestIsolationGetRunLogs_HeaderCursor(t *testing.T) {
	var (
		reqPath  string
		reqQuery string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath = r.URL.Path
		reqQuery = r.URL.RawQuery
		w.Header().Set(execdIsolatedTailCursorHeader, "12")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "line1\nline2\n")
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	text, nextCursor, err := session.GetRunLogs(context.Background(), "run-1", 4)
	require.NoError(t, err)
	require.Equal(t, "/v1/isolated/session/sess-1/runs/run-1/logs", reqPath)
	require.Equal(t, "cursor=4", reqQuery)
	require.Equal(t, "line1\nline2\n", text)
	require.Equal(t, int64(12), nextCursor)
}

// TestIsolationGetRunLogs_FallbackCursor verifies that without the header the
// cursor advances by the number of bytes returned, and that cursor=0 omits the
// query parameter.
func TestIsolationGetRunLogs_FallbackCursor(t *testing.T) {
	var reqQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	text, nextCursor, err := session.GetRunLogs(context.Background(), "run-1", 0)
	require.NoError(t, err)
	require.Equal(t, "", reqQuery, "cursor=0 should omit the query parameter")
	require.Equal(t, "hello", text)
	require.Equal(t, int64(5), nextCursor)
}

// TestIsolationGetRunLogs_InvalidHeaderFallsBack verifies an unparseable
// EXECD-ISOLATED-TAIL-CURSOR header falls back to cursor + len(text).
func TestIsolationGetRunLogs_InvalidHeaderFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(execdIsolatedTailCursorHeader, "not-a-number")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	_, nextCursor, err := session.GetRunLogs(context.Background(), "run-1", 3)
	require.NoError(t, err)
	require.Equal(t, int64(8), nextCursor)
}

// TestIsolationGetRunLogs_NotFound verifies a 404 surfaces as an *APIError.
func TestIsolationGetRunLogs_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"RUN_NOT_FOUND","message":"no such run"}`)
	}))
	defer srv.Close()

	execd := NewExecdClient(srv.URL, "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	_, _, err := session.GetRunLogs(context.Background(), "no-such-run", 0)
	require.Error(t, err)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}

// TestIsolationBackground_Validation verifies argument validation without any
// HTTP call.
func TestIsolationBackground_Validation(t *testing.T) {
	execd := NewExecdClient("http://unused.invalid", "test-key")
	sb := &Sandbox{id: "sbx-test", execd: execd}
	session := newIsolationSessionForTest(sb, "sess-1")

	_, err := session.RunBackground(context.Background(), "")
	require.Error(t, err)
	var invalid *InvalidArgumentError
	require.ErrorAs(t, err, &invalid)

	_, err = session.GetRunStatus(context.Background(), "")
	require.Error(t, err)
	require.ErrorAs(t, err, &invalid)

	_, _, err = session.GetRunLogs(context.Background(), "", 0)
	require.Error(t, err)
	require.ErrorAs(t, err, &invalid)

	_, _, err = session.GetRunLogs(context.Background(), "run-1", -1)
	require.Error(t, err)
	require.ErrorAs(t, err, &invalid)
}
