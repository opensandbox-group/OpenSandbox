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
	"time"
)

// CodeInterpreterImage is the default container image for the code interpreter.
const CodeInterpreterImage = "opensandbox/code-interpreter:latest"

// CodeInterpreterEntrypoint is the default entrypoint for the code interpreter.
var CodeInterpreterEntrypoint = []string{"/opt/opensandbox/code-interpreter.sh"}

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

	// SkipHealthCheck skips the WaitUntilReady call.
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

	return &CodeInterpreter{Sandbox: sb}, nil
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
