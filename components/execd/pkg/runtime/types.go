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

package runtime

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

// ExecuteResultHook groups execution callbacks.
type ExecuteResultHook struct {
	OnExecuteInit     func(context string)
	OnExecuteResult   func(result map[string]any, count int)
	OnExecuteStatus   func(status string)
	OnExecuteStdout   func(eid int64, stdout string) //nolint:predeclared
	OnExecuteStderr   func(eid int64, stderr string) //nolint:predeclared
	OnExecuteError    func(err *execute.ErrorOutput)
	OnExecuteComplete func(executionTime time.Duration)
}

// ExecuteCodeRequest represents a code execution request with context and hooks.
type ExecuteCodeRequest struct {
	Language Language          `json:"language"`
	Code     string            `json:"code"`
	Context  string            `json:"context"`
	Timeout  time.Duration     `json:"timeout"`
	Cwd      string            `json:"cwd"`
	Envs     map[string]string `json:"envs"`
	Uid      *uint32           `json:"uid,omitempty"`
	Gid      *uint32           `json:"gid,omitempty"`
	Hooks    ExecuteResultHook

	eventSeq atomic.Uint64
}

func (req *ExecuteCodeRequest) NextEventID() int64 {
	if req == nil {
		return 0
	}
	return int64(req.eventSeq.Add(1))
}

func (req *ExecuteCodeRequest) wrapStdoutPipeHook() func(string) {
	return func(text string) {
		if text == "" || req.Hooks.OnExecuteStdout == nil {
			return
		}
		eid := req.NextEventID()
		req.Hooks.OnExecuteStdout(eid, text)
	}
}

func (req *ExecuteCodeRequest) wrapStderrPipeHook() func(string) {
	return func(text string) {
		if text == "" || req.Hooks.OnExecuteStderr == nil {
			return
		}
		eid := req.NextEventID()
		req.Hooks.OnExecuteStderr(eid, text)
	}
}

// SetDefaultHooks installs stdout logging fallbacks for unset hooks.
func (req *ExecuteCodeRequest) SetDefaultHooks() {
	if req.Hooks.OnExecuteResult == nil {
		req.Hooks.OnExecuteResult = func(result map[string]any, count int) { fmt.Printf("OnExecuteResult: %d, %++v\n", count, result) }
	}
	if req.Hooks.OnExecuteStatus == nil {
		req.Hooks.OnExecuteStatus = func(status string) { fmt.Printf("OnExecuteStatus: %s\n", status) }
	}
	if req.Hooks.OnExecuteStdout == nil {
		req.Hooks.OnExecuteStdout = func(eid int64, stdout string) { fmt.Printf("OnExecuteStdout: eid=%d %s\n", eid, stdout) }
	}
	if req.Hooks.OnExecuteStderr == nil {
		req.Hooks.OnExecuteStderr = func(eid int64, stderr string) { fmt.Printf("OnExecuteStderr: eid=%d %s\n", eid, stderr) }
	}
	if req.Hooks.OnExecuteError == nil {
		req.Hooks.OnExecuteError = func(err *execute.ErrorOutput) { fmt.Printf("OnExecuteError: %++v\n", err) }
	}
	if req.Hooks.OnExecuteComplete == nil {
		req.Hooks.OnExecuteComplete = func(executionTime time.Duration) {
			fmt.Printf("OnExecuteComplete: %v\n", executionTime)
		}
	}
	if req.Hooks.OnExecuteInit == nil {
		req.Hooks.OnExecuteInit = func(session string) { fmt.Printf("OnExecuteInit: %s\n", session) }
	}
}

// CreateContextRequest represents a stateful session creation request.
type CreateContextRequest struct {
	Language Language `json:"language"`
	Cwd      string   `json:"cwd"`
}

type CodeContext struct {
	ID       string   `json:"id,omitempty"`
	Language Language `json:"language"`
}

// bashSessionConfig holds bash session configuration.
type bashSessionConfig struct {
	// StartupSource is a list of scripts sourced on startup.
	StartupSource []string
	// Session is the session identifier.
	Session string
	// StartupTimeout is the startup timeout.
	StartupTimeout time.Duration
	// Cwd is the working directory.
	Cwd string
}

// bashSession represents a bash session.
type bashSession struct {
	config  *bashSessionConfig
	mu      sync.Mutex
	started bool
	env     map[string]string
	cwd     string

	// currentProcessPid is the pid of the active run's process group leader (bash).
	// Set after cmd.Start(), cleared when run() returns. Used by close() to kill the process group.
	currentProcessPid int
}
