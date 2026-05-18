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
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// RunCommand executes a shell command and returns the structured result.
func (s *Sandbox) RunCommand(ctx context.Context, command string, handlers *ExecutionHandlers) (*Execution, error) {
	return s.RunCommandWithOpts(ctx, RunCommandRequest{Command: command}, handlers)
}

const maxResumeRetries = 3

// isNetworkError reports whether err is a transient network error that should
// trigger SSE resume. Context cancellation and deadline errors are not retryable.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	for {
		var netErr net.Error
		if errors.As(err, &netErr) {
			return true
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return true
		}
		unwrapped := errors.Unwrap(err)
		if unwrapped == nil {
			break
		}
		err = unwrapped
	}
	return false
}

// RunCommandWithOpts executes a command with full options.
// Automatically resumes the SSE stream on transient network disconnects.
func (s *Sandbox) RunCommandWithOpts(ctx context.Context, req RunCommandRequest, handlers *ExecutionHandlers) (*Execution, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}

	exec := &Execution{}
	var commandID string

	for attempt := 0; attempt <= maxResumeRetries; attempt++ {
		var streamErr error
		if attempt == 0 {
			streamErr = s.execd.RunCommand(ctx, req, func(event StreamEvent) error {
				return processStreamEvent(exec, event, handlers)
			})
		} else {
			streamErr = s.execd.ResumeCommand(ctx, commandID, exec.LastEid, func(event StreamEvent) error {
				return processStreamEvent(exec, event, handlers)
			})
		}

		if streamErr == nil {
			return exec, nil
		}

		if exec.ID != "" {
			commandID = exec.ID
		}

		var apiErr *APIError
		if errors.As(streamErr, &apiErr) {
			if apiErr.StatusCode == 409 && attempt < maxResumeRetries {
				if err := retrySleep(ctx, 1*time.Second); err != nil {
					return exec, err
				}
				continue
			}
			if apiErr.StatusCode == 404 && attempt > 0 {
				return exec, nil
			}
			return exec, streamErr
		}

		if commandID == "" || attempt >= maxResumeRetries || !isNetworkError(streamErr) {
			return exec, streamErr
		}
	}

	return exec, nil
}

// ExecuteCode executes code in a context and streams output via SSE.
func (s *Sandbox) ExecuteCode(ctx context.Context, req RunCodeRequest, handlers *ExecutionHandlers) (*Execution, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	exec := &Execution{}
	err := s.execd.ExecuteCode(ctx, req, func(event StreamEvent) error {
		return processStreamEvent(exec, event, handlers)
	})
	return exec, err
}

// CreateContext creates a code execution context.
func (s *Sandbox) CreateContext(ctx context.Context, req CreateContextRequest) (*CodeContext, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.CreateContext(ctx, req)
}

// ListContexts lists active code execution contexts for a language.
func (s *Sandbox) ListContexts(ctx context.Context, language string) ([]CodeContext, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.ListContexts(ctx, language)
}

// DeleteContext deletes a code execution context.
func (s *Sandbox) DeleteContext(ctx context.Context, contextID string) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.DeleteContext(ctx, contextID)
}

// CreateSession creates a new bash session.
func (s *Sandbox) CreateSession(ctx context.Context) (*Session, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.CreateSession(ctx)
}

// RunInSession executes a command in an existing session with structured output.
func (s *Sandbox) RunInSession(ctx context.Context, sessionID string, req RunInSessionRequest, handlers *ExecutionHandlers) (*Execution, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	exec := &Execution{}
	err := s.execd.RunInSession(ctx, sessionID, req, func(event StreamEvent) error {
		return processStreamEvent(exec, event, handlers)
	})
	return exec, err
}

// DeleteSession deletes a bash session.
func (s *Sandbox) DeleteSession(ctx context.Context, sessionID string) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.DeleteSession(ctx, sessionID)
}

// GetMetrics returns current system resource metrics from the sandbox.
func (s *Sandbox) GetMetrics(ctx context.Context) (*Metrics, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.GetMetrics(ctx)
}
