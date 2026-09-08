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
	"time"
)

// CodeInterpreterImage is the default container image for the code interpreter.
const CodeInterpreterImage = "opensandbox/code-interpreter:latest"

// CodeInterpreterEntrypoint is the default entrypoint for the code interpreter.
var CodeInterpreterEntrypoint = []string{"/opt/code-interpreter/code-interpreter.sh"}

// CodeInterpreterRuntimeCheckCommand is the script run inside the sandbox to
// verify the code interpreter runtime (Jupyter kernel gateway) is actually
// serving. execd starts serving /ping before the entrypoint launches Jupyter,
// and the setup stage may run short-lived "jupyter kernelspec" helpers, so a
// daemon ping or a process-name grep cannot prove the runtime is ready. Probing
// the Jupyter listen port (127.0.0.1:${JUPYTER_PORT:-44771}, same default as
// the entrypoint) only passes once the server accepts connections.
const CodeInterpreterRuntimeCheckCommand = "bash -c 'exec 3<>/dev/tcp/127.0.0.1/${JUPYTER_PORT:-44771}' && exit 0 || exit 1"

// CodeInterpreterCreateOptions configures code interpreter creation.
type CodeInterpreterCreateOptions struct {
	// Image overrides the default code-interpreter image.
	Image string

	// Entrypoint overrides the default code-interpreter entrypoint.
	Entrypoint []string

	// ResourceLimits for the sandbox. Defaults to DefaultResourceLimits.
	ResourceLimits ResourceLimits

	// TimeoutSeconds is the sandbox TTL. Defaults to 900 (15 min).
	TimeoutSeconds *int

	// Env variables injected into the sandbox.
	Env map[string]string

	// Metadata for filtering and tagging.
	Metadata map[string]string

	// SkipHealthCheck skips both readiness checks: the sandbox WaitUntilReady
	// (execd /ping) call and the code interpreter runtime process check.
	SkipHealthCheck bool

	// ReadyTimeout overrides the default ready timeout.
	ReadyTimeout time.Duration

	// HealthCheckInterval overrides the default polling interval.
	HealthCheckInterval time.Duration
}

// CodeInterpreter wraps a Sandbox with code execution capabilities.
// It provides multi-language code execution with persistent contexts.
type CodeInterpreter struct {
	*Sandbox
}

// CreateCodeInterpreter creates a sandbox with the code-interpreter image
// and returns a CodeInterpreter wrapping it.
func CreateCodeInterpreter(ctx context.Context, config ConnectionConfig, opts CodeInterpreterCreateOptions) (*CodeInterpreter, error) {
	image := opts.Image
	if image == "" {
		image = CodeInterpreterImage
	}
	entrypoint := opts.Entrypoint
	if len(entrypoint) == 0 {
		entrypoint = CodeInterpreterEntrypoint
	}
	timeout := opts.TimeoutSeconds
	if timeout == nil {
		t := DefaultCodeInterpreterTimeoutSeconds
		timeout = &t
	}

	sb, err := CreateSandbox(ctx, config, SandboxCreateOptions{
		Image:               image,
		Entrypoint:          entrypoint,
		ResourceLimits:      opts.ResourceLimits,
		TimeoutSeconds:      timeout,
		Env:                 opts.Env,
		Metadata:            opts.Metadata,
		SkipHealthCheck:     opts.SkipHealthCheck,
		ReadyTimeout:        opts.ReadyTimeout,
		HealthCheckInterval: opts.HealthCheckInterval,
	})
	if err != nil {
		return nil, err
	}

	ci := &CodeInterpreter{Sandbox: sb}

	// Strict readiness: execd serving /ping (checked by WaitUntilReady above) is
	// not enough — execd starts serving before the entrypoint launches Jupyter.
	// Poll until the interpreter runtime is actually serving.
	if !opts.SkipHealthCheck {
		readyTimeout := opts.ReadyTimeout
		if readyTimeout == 0 {
			readyTimeout = time.Duration(DefaultReadyTimeoutSeconds) * time.Second
		}
		interval := opts.HealthCheckInterval
		if interval == 0 {
			interval = DefaultHealthCheckPollingInterval
		}
		if err := ci.waitRuntimeReady(ctx, readyTimeout, interval); err != nil {
			// The caller has no handle to the created sandbox; clean it up
			// best-effort, mirroring CreateSandbox's readiness-failure path.
			_ = sb.Kill(context.Background())
			return nil, err
		}
	}

	return ci, nil
}

// IsHealthy reports whether the code interpreter is strictly healthy: the
// execd daemon answers /ping and the interpreter runtime (Jupyter kernel
// gateway) is serving inside the sandbox.
func (ci *CodeInterpreter) IsHealthy(ctx context.Context) bool {
	if !ci.Sandbox.IsHealthy(ctx) {
		return false
	}
	exec, err := ci.Sandbox.RunCommand(ctx, CodeInterpreterRuntimeCheckCommand, nil)
	return err == nil && exec != nil && exec.Error == nil
}

// waitRuntimeReady polls the runtime check until it passes or the timeout
// expires.
func (ci *CodeInterpreter) waitRuntimeReady(ctx context.Context, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		exec, err := ci.Sandbox.RunCommand(ctx, CodeInterpreterRuntimeCheckCommand, nil)
		switch {
		case err == nil && (exec == nil || exec.Error == nil):
			return nil
		case err != nil:
			lastErr = err
		default:
			lastErr = fmt.Errorf("code interpreter runtime (jupyter) is not serving")
		}

		// Clamp the sleep to the remaining budget so the final failed check
		// does not overshoot the timeout by a full polling interval.
		wait := interval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	return &SandboxReadyTimeoutError{
		SandboxID: ci.Sandbox.ID(),
		Elapsed:   timeout.String(),
		LastErr:   lastErr,
	}
}

// Execute runs code in the specified language and returns the structured result.
// If language is non-empty, it is sent as CodeContext.Language.
func (ci *CodeInterpreter) Execute(ctx context.Context, language, code string, handlers *ExecutionHandlers) (*Execution, error) {
	req := RunCodeRequest{
		Code: code,
	}
	if language != "" {
		req.Context = &CodeContext{Language: language}
	}
	return ci.ExecuteCode(ctx, req, handlers)
}

// ExecuteInContext runs code in an existing context (for state persistence).
func (ci *CodeInterpreter) ExecuteInContext(ctx context.Context, contextID, language, code string, handlers *ExecutionHandlers) (*Execution, error) {
	req := RunCodeRequest{
		Context: &CodeContext{
			ID:       contextID,
			Language: language,
		},
		Code: code,
	}
	return ci.ExecuteCode(ctx, req, handlers)
}
