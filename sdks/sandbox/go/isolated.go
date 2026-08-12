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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// IsolatedWorkspaceSpec describes the workspace bind configuration.
type IsolatedWorkspaceSpec struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"` // "rw" | "overlay" | "ro"
}

// EnvPassthroughSpec controls environment variable passthrough.
type EnvPassthroughSpec struct {
	Mode string   `json:"mode,omitempty"` // "allow" | "deny"
	Keys []string `json:"keys,omitempty"`
}

// BindMount describes an explicit source-to-destination bind mount into the namespace.
type BindMount struct {
	Source   string `json:"source"`
	Dest     string `json:"dest,omitempty"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// CreateIsolatedSessionRequest is the request body for creating an isolated session.
type CreateIsolatedSessionRequest struct {
	Workspace          IsolatedWorkspaceSpec `json:"workspace"`
	Profile            string                `json:"profile,omitempty"`
	ExtraWritable      []string              `json:"extra_writable,omitempty"`
	Binds              []BindMount           `json:"binds,omitempty"`
	ShareNet           *bool                 `json:"share_net,omitempty"`
	EnvPassthrough     *EnvPassthroughSpec   `json:"env_passthrough,omitempty"`
	Uid                *uint32               `json:"uid,omitempty"`
	Gid                *uint32               `json:"gid,omitempty"`
	UidMode            string                `json:"uid_mode,omitempty"` // "setpriv" | "userns"
	IdleTimeoutSeconds int                   `json:"idle_timeout_seconds,omitempty"`
}

// IsolatedSessionInfo is the response from creating an isolated session.
//
// The creation-parameter echo fields (Profile, Workspace, ExtraWritable, Binds,
// ShareNet, EnvPassthrough, Uid, Gid, UidMode, IdleTimeoutSeconds) are populated
// only when the info is built by IsolationAttach against an execd build that
// echoes creation parameters on GET /v1/isolated/session/{id}. Older execd
// builds and the POST /v1/isolated/session create response leave them zero
// (or nil, for pointer fields).
//
// IdleTimeoutSeconds is a pointer so callers can distinguish "execd did not
// echo the field" (nil, e.g. older execd) from "session was created with
// idle GC disabled" (non-nil zero).
type IsolatedSessionInfo struct {
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`

	// Creation-parameter echoes (populated on attach when the server supports it).
	Profile            string                 `json:"profile,omitempty"`
	Workspace          *IsolatedWorkspaceSpec `json:"workspace,omitempty"`
	ExtraWritable      []string               `json:"extra_writable,omitempty"`
	Binds              []BindMount            `json:"binds,omitempty"`
	ShareNet           *bool                  `json:"share_net,omitempty"`
	EnvPassthrough     *EnvPassthroughSpec    `json:"env_passthrough,omitempty"`
	Uid                *uint32                `json:"uid,omitempty"`
	Gid                *uint32                `json:"gid,omitempty"`
	UidMode            string                 `json:"uid_mode,omitempty"`
	IdleTimeoutSeconds *int                   `json:"idle_timeout_seconds,omitempty"`
}

// IsolatedSessionState represents the current state of an isolated session.
//
// Since feat/isolated-session-attach, execd additionally echoes the
// creation parameters on GET /v1/isolated/session/{id}. The echoed fields
// are optional; older execd builds omit them and clients must tolerate
// their absence.
//
// IdleTimeoutSeconds is a pointer so callers can distinguish "execd did not
// echo the field" (nil, e.g. older execd) from "session was created with
// idle GC disabled" (non-nil zero).
type IsolatedSessionState struct {
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`

	// Creation-parameter echoes (optional; omitted by older execd builds).
	Profile            string                 `json:"profile,omitempty"`
	Workspace          *IsolatedWorkspaceSpec `json:"workspace,omitempty"`
	ExtraWritable      []string               `json:"extra_writable,omitempty"`
	Binds              []BindMount            `json:"binds,omitempty"`
	ShareNet           *bool                  `json:"share_net,omitempty"`
	EnvPassthrough     *EnvPassthroughSpec    `json:"env_passthrough,omitempty"`
	Uid                *uint32                `json:"uid,omitempty"`
	Gid                *uint32                `json:"gid,omitempty"`
	UidMode            string                 `json:"uid_mode,omitempty"`
	IdleTimeoutSeconds *int                   `json:"idle_timeout_seconds,omitempty"`
}

// IsolatedSessionSummary describes a single isolated session in a list response.
type IsolatedSessionSummary struct {
	SessionID            string    `json:"session_id"`
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`
}

// listIsolatedSessionsResponse is the wire response for listing isolated sessions.
type listIsolatedSessionsResponse struct {
	Sessions []IsolatedSessionSummary `json:"sessions"`
}

// IsolatedRunRequest is the request body for running code in an isolated session.
type IsolatedRunRequest struct {
	Code           string            `json:"code"`
	Envs           map[string]string `json:"envs,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// IsolatedRunOpts configures a run in an isolated session.
//
// Background execution is only available through IsolationSession.RunBackground;
// this opts type deliberately carries no background flag because
// IsolationSession.Run is foreground-only.
type IsolatedRunOpts struct {
	// Envs are environment variables exported into the shell before the code runs.
	Envs map[string]string
	// TimeoutSeconds is foreground-only. It is never sent for background runs:
	// a background run is not time-limited and idle GC is suspended while it is
	// active.
	TimeoutSeconds int
}

// IsolatedBackgroundRun is the handle returned when a run is started with
// background: true.
type IsolatedBackgroundRun struct {
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
}

// IsolatedRunStatus is the lifecycle state of an isolated background run.
type IsolatedRunStatus struct {
	SessionID  string     `json:"session_id"`
	RunID      string     `json:"run_id"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// execdIsolatedTailCursorHeader carries the next byte cursor for incremental
// isolated background run log reads.
const execdIsolatedTailCursorHeader = "EXECD-ISOLATED-TAIL-CURSOR"

// isolatedBackgroundRunRequest is the wire body for starting a detached
// background run. timeout_seconds is foreground-only and deliberately never
// sent for background runs.
type isolatedBackgroundRunRequest struct {
	Code       string            `json:"code"`
	Envs       map[string]string `json:"envs,omitempty"`
	Background bool              `json:"background"`
}

// IsolatedCapabilities reports isolation capabilities.
type IsolatedCapabilities struct {
	Available        bool   `json:"available"`
	Isolator         string `json:"isolator,omitempty"`
	Version          string `json:"version,omitempty"`
	Message          string `json:"message,omitempty"`
	SetprivAvailable bool   `json:"setpriv_available"`
	UsernsAvailable  bool   `json:"userns_available"`
	CommitSupported  bool   `json:"commit_supported"`
	DiffSupported    bool   `json:"diff_supported"`
}

// IsolatedCreate creates an isolated bash session.
func (e *ExecdClient) IsolatedCreate(ctx context.Context, req CreateIsolatedSessionRequest) (*IsolatedSessionInfo, error) {
	var result IsolatedSessionInfo
	err := e.client.doRequest(ctx, http.MethodPost, "/v1/isolated/session", req, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// IsolatedGet retrieves the state of an isolated session.
func (e *ExecdClient) IsolatedGet(ctx context.Context, sessionID string) (*IsolatedSessionState, error) {
	var result IsolatedSessionState
	path := "/v1/isolated/session/" + url.PathEscape(sessionID)
	err := e.client.doRequest(ctx, http.MethodGet, path, nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// IsolatedList lists all active isolated sessions.
func (e *ExecdClient) IsolatedList(ctx context.Context) ([]IsolatedSessionSummary, error) {
	var result listIsolatedSessionsResponse
	err := e.client.doRequest(ctx, http.MethodGet, "/v1/isolated/sessions", nil, &result)
	if err != nil {
		return nil, err
	}
	return result.Sessions, nil
}

// IsolatedRun runs code in an isolated session, streaming output via SSE.
func (e *ExecdClient) IsolatedRun(ctx context.Context, sessionID string, req IsolatedRunRequest, handler EventHandler) error {
	path := "/v1/isolated/session/" + url.PathEscape(sessionID) + "/run"
	return e.client.doStreamRequest(ctx, http.MethodPost, path, req, handler)
}

// IsolatedRunBackground starts a detached background run in an isolated session
// and returns the run handle. The run's combined stdout/stderr is captured to a
// log pollable via IsolatedRunLogs; its lifecycle is tracked via
// IsolatedRunStatus. timeout_seconds is foreground-only and deliberately not
// sent (background runs are not time-limited).
// Background runs require a writable log location, so sessions with a
// read-only (ro) workspace reject them with an error.
func (e *ExecdClient) IsolatedRunBackground(ctx context.Context, sessionID, code string, opts IsolatedRunOpts) (*IsolatedBackgroundRun, error) {
	if code == "" {
		return nil, &InvalidArgumentError{Field: "code", Message: "must not be empty"}
	}
	req := isolatedBackgroundRunRequest{
		Code:       code,
		Envs:       opts.Envs,
		Background: true,
	}
	var result IsolatedBackgroundRun
	path := "/v1/isolated/session/" + url.PathEscape(sessionID) + "/run"
	err := e.client.doRequest(ctx, http.MethodPost, path, req, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// IsolatedRunStatus returns the lifecycle state of a background run started
// with IsolatedRunBackground. A run whose session dies mid-flight reports
// Running=false with an error; run records vanish (404) once the session is
// deleted.
func (e *ExecdClient) IsolatedRunStatus(ctx context.Context, sessionID, runID string) (*IsolatedRunStatus, error) {
	if runID == "" {
		return nil, &InvalidArgumentError{Field: "runID", Message: "must not be empty"}
	}
	var result IsolatedRunStatus
	path := "/v1/isolated/session/" + url.PathEscape(sessionID) + "/runs/" + url.PathEscape(runID)
	err := e.client.doRequest(ctx, http.MethodGet, path, nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// IsolatedRunLogs returns the combined stdout/stderr of a background run
// started with IsolatedRunBackground, beginning at the given byte cursor.
// Pass cursor=0 to read from the start; at most 16 MiB are returned per
// request, so poll repeatedly with the returned cursor for long outputs.
// Per-run log retention is capped at 16 MiB: output beyond the cap is
// discarded when the run completes, so callers that need more than the first
// page should drain incrementally while the run is active instead of waiting
// for running=false before reading logs.
//
// The returned nextCursor is the next byte cursor for incremental reads: the
// EXECD-ISOLATED-TAIL-CURSOR response header when present and parseable,
// otherwise cursor + len(text).
func (e *ExecdClient) IsolatedRunLogs(ctx context.Context, sessionID, runID string, cursor int64) (string, int64, error) {
	if runID == "" {
		return "", 0, &InvalidArgumentError{Field: "runID", Message: "must not be empty"}
	}
	if cursor < 0 {
		return "", 0, &InvalidArgumentError{Field: "cursor", Message: "must not be negative"}
	}
	path := "/v1/isolated/session/" + url.PathEscape(sessionID) + "/runs/" + url.PathEscape(runID) + "/logs"
	if cursor != 0 {
		path += "?cursor=" + strconv.FormatInt(cursor, 10)
	}

	var text string
	nextCursor := cursor
	err := e.client.withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.client.baseURL+path, nil)
		if err != nil {
			return fmt.Errorf("opensandbox: create request: %w", err)
		}
		req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
		for k, v := range e.client.headers {
			req.Header.Set(k, v)
		}
		if e.client.apiKey != "" {
			req.Header.Set(e.client.authHeader, e.client.apiKey)
		}
		req.Header.Set("Accept", "text/plain")

		resp, err := e.client.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("opensandbox: do request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			return handleError(resp)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("opensandbox: read response: %w", err)
		}
		text = string(body)

		next := cursor + int64(len(body))
		if cursorStr := resp.Header.Get(execdIsolatedTailCursorHeader); cursorStr != "" {
			if parsed, parseErr := strconv.ParseInt(cursorStr, 10, 64); parseErr == nil {
				next = parsed
			}
		}
		nextCursor = next
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return text, nextCursor, nil
}

// IsolatedDelete deletes an isolated session.
func (e *ExecdClient) IsolatedDelete(ctx context.Context, sessionID string) error {
	path := "/v1/isolated/session/" + url.PathEscape(sessionID)
	return e.client.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// IsolatedCapabilities retrieves isolation capabilities.
func (e *ExecdClient) IsolatedCapabilities(ctx context.Context) (*IsolatedCapabilities, error) {
	var result IsolatedCapabilities
	err := e.client.doRequest(ctx, http.MethodGet, "/v1/isolated/capabilities", nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
