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
)

// IsolationSession is a handle to a single isolated bash session.
type IsolationSession struct {
	info    *IsolatedSessionInfo
	sandbox *Sandbox
	files   *ExecdClient // file operations scoped to this session
}

// SessionID returns the session identifier.
func (s *IsolationSession) SessionID() string { return s.info.SessionID }

// Info returns the session creation info.
func (s *IsolationSession) Info() *IsolatedSessionInfo { return s.info }

// Files returns an ExecdClient scoped to this session's file endpoints.
// File operations (GetFileInfo, UploadFile, DownloadFile, etc.) are
// automatically routed to /v1/isolated/session/{id}/files/*.
func (s *IsolationSession) Files() *ExecdClient { return s.files }

// Run executes code in this isolated session.
func (s *IsolationSession) Run(ctx context.Context, req IsolatedRunRequest, handlers *ExecutionHandlers) (*Execution, error) {
	if s.sandbox.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	exec := &Execution{}
	err := s.sandbox.execd.IsolatedRun(ctx, s.info.SessionID, req, func(event StreamEvent) error {
		return processStreamEvent(exec, event, handlers)
	})
	return exec, err
}

// Get retrieves the current state of this session.
func (s *IsolationSession) Get(ctx context.Context) (*IsolatedSessionState, error) {
	if s.sandbox.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.sandbox.execd.IsolatedGet(ctx, s.info.SessionID)
}

// Delete deletes this isolated session.
func (s *IsolationSession) Delete(ctx context.Context) error {
	if s.sandbox.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.sandbox.execd.IsolatedDelete(ctx, s.info.SessionID)
}

// IsolationCreate creates an isolated bash session and returns a session handle.
func (s *Sandbox) IsolationCreate(ctx context.Context, req CreateIsolatedSessionRequest) (*IsolationSession, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	info, err := s.execd.IsolatedCreate(ctx, req)
	if err != nil {
		return nil, err
	}
	sessionBaseURL := s.execd.client.baseURL + "/v1/isolated/session/" + info.SessionID
	var filesOpts []Option
	if len(s.execd.client.headers) > 0 {
		filesOpts = append(filesOpts, WithHeaders(s.execd.client.headers))
	}
	filesClient := NewExecdClient(sessionBaseURL, s.execd.client.apiKey, filesOpts...)
	return &IsolationSession{info: info, sandbox: s, files: filesClient}, nil
}

// IsolationCapabilities retrieves isolation capabilities.
func (s *Sandbox) IsolationCapabilities(ctx context.Context) (*IsolatedCapabilities, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.IsolatedCapabilities(ctx)
}

// IsolationListSessions lists all active isolated sessions.
func (s *Sandbox) IsolationListSessions(ctx context.Context) ([]IsolatedSessionSummary, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.IsolatedList(ctx)
}

// Deprecated: Use IsolationCreate instead.
func (s *Sandbox) IsolatedCreate(ctx context.Context, req CreateIsolatedSessionRequest) (*IsolatedSessionInfo, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.IsolatedCreate(ctx, req)
}

// Deprecated: Use IsolationSession.Get instead.
func (s *Sandbox) IsolatedGet(ctx context.Context, sessionID string) (*IsolatedSessionState, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.IsolatedGet(ctx, sessionID)
}

// Deprecated: Use IsolationSession.Run instead.
func (s *Sandbox) IsolatedRun(ctx context.Context, sessionID string, req IsolatedRunRequest, handlers *ExecutionHandlers) (*Execution, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	exec := &Execution{}
	err := s.execd.IsolatedRun(ctx, sessionID, req, func(event StreamEvent) error {
		return processStreamEvent(exec, event, handlers)
	})
	return exec, err
}

// Deprecated: Use IsolationSession.Delete instead.
func (s *Sandbox) IsolatedDelete(ctx context.Context, sessionID string) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.IsolatedDelete(ctx, sessionID)
}

// IsolationRunOnce creates an isolated session, runs the given code, and deletes the session.
// It is a convenience wrapper for the create → run → delete lifecycle.
func (s *Sandbox) IsolationRunOnce(ctx context.Context, req CreateIsolatedSessionRequest, run IsolatedRunRequest, handlers *ExecutionHandlers) (*Execution, error) {
	session, err := s.IsolationCreate(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = session.Delete(context.Background())
	}()
	return session.Run(ctx, run, handlers)
}

// IsolationWithSession creates an isolated session, invokes fn, and deletes the session afterwards.
// The session is always deleted regardless of whether fn returns an error.
func (s *Sandbox) IsolationWithSession(ctx context.Context, req CreateIsolatedSessionRequest, fn func(*IsolationSession) error) error {
	session, err := s.IsolationCreate(ctx, req)
	if err != nil {
		return err
	}
	defer func() {
		_ = session.Delete(context.Background())
	}()
	return fn(session)
}

// Deprecated: Use IsolationCapabilities instead.
func (s *Sandbox) IsolatedCapabilities(ctx context.Context) (*IsolatedCapabilities, error) {
	return s.IsolationCapabilities(ctx)
}
