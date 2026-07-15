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
	"net/http"
	"net/url"
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
type IsolatedSessionInfo struct {
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

// IsolatedSessionState represents the current state of an isolated session.
type IsolatedSessionState struct {
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`
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

// IsolatedCapabilities reports isolation capabilities.
type IsolatedCapabilities struct {
	Available       bool   `json:"available"`
	Isolator        string `json:"isolator,omitempty"`
	Version         string `json:"version,omitempty"`
	Message         string `json:"message,omitempty"`
	CommitSupported bool   `json:"commit_supported"`
	DiffSupported   bool   `json:"diff_supported"`
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
