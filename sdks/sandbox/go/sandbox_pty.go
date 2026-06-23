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

package opensandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// PtyCreateRequest is the optional body for creating a PTY session.
type PtyCreateRequest struct {
	// Cwd is the working directory for the shell.
	Cwd string `json:"cwd,omitempty"`
	// Command runs instead of the default login shell when set.
	Command string `json:"command,omitempty"`
}

// PtySession identifies a created PTY session. The shell starts on the first
// WebSocket attach, not at creation time.
type PtySession struct {
	SessionID string `json:"session_id"`
}

// PtySessionStatus is the status of a PTY session.
type PtySessionStatus struct {
	SessionID string `json:"session_id"`
	Running   bool   `json:"running"`
	// OutputOffset is the byte offset of buffered output; pass it as `since`
	// on reconnect to replay scrollback from that point.
	OutputOffset int64 `json:"output_offset"`
}

// CreatePtySession creates a new interactive PTY session.
func (e *ExecdClient) CreatePtySession(ctx context.Context, req PtyCreateRequest) (*PtySession, error) {
	var result PtySession
	if err := e.client.doRequest(ctx, http.MethodPost, "/pty", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetPtySession returns the status of a PTY session.
func (e *ExecdClient) GetPtySession(ctx context.Context, sessionID string) (*PtySessionStatus, error) {
	var result PtySessionStatus
	path := "/pty/" + url.PathEscape(sessionID)
	if err := e.client.doRequest(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeletePtySession tears down a PTY session on the server side.
func (e *ExecdClient) DeletePtySession(ctx context.Context, sessionID string) error {
	return e.client.doRequest(ctx, http.MethodDelete, "/pty/"+url.PathEscape(sessionID), nil, nil)
}

// CreatePtySession creates a new interactive PTY session on the sandbox. The
// interactive stream itself is a WebSocket and is driven separately.
func (s *Sandbox) CreatePtySession(ctx context.Context, req PtyCreateRequest) (*PtySession, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.CreatePtySession(ctx, req)
}

// GetPtySession returns the status of a PTY session on the sandbox.
func (s *Sandbox) GetPtySession(ctx context.Context, sessionID string) (*PtySessionStatus, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.GetPtySession(ctx, sessionID)
}

// DeletePtySession tears down a PTY session on the sandbox.
func (s *Sandbox) DeletePtySession(ctx context.Context, sessionID string) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.DeletePtySession(ctx, sessionID)
}
