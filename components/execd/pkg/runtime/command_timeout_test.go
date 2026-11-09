// Copyright 2026 The OpenSandbox Authors
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

//go:build !windows

package runtime

import (
	"context"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
)

func TestCommandTimeoutClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		timeout   time.Duration
		errorName string
		exitCode  int
	}{
		{"deadline", "printf before; sleep 30", 100 * time.Millisecond, "CommandTimeout", -1},
		{"nonzero_exit", "printf before; exit 7", 5 * time.Second, "CommandExecError", 7},
		{"signal_exit", "printf before; kill -KILL $$", 5 * time.Second, "CommandExecError", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewController("", "")
			var got *execute.ErrorOutput
			var output, session string
			var complete bool
			req := &ExecuteCodeRequest{Language: Command, Code: tc.code, Timeout: tc.timeout}
			req.SetDefaultHooks()
			req.Hooks.OnExecuteInit = func(id string) { session = id }
			req.Hooks.OnExecuteStdout = func(s string) { output += s }
			req.Hooks.OnExecuteError = func(e *execute.ErrorOutput) { got = e }
			req.Hooks.OnExecuteComplete = func(time.Duration) { complete = true }
			require.NoError(t, c.Execute(req))
			require.NotNil(t, got)
			require.Equal(t, tc.errorName, got.EName)
			require.Equal(t, strconv.Itoa(tc.exitCode), got.EValue)
			require.NotNil(t, got.ExitCode)
			require.Equal(t, tc.exitCode, *got.ExitCode)
			require.NotEmpty(t, got.Traceback)
			require.Equal(t, "before", output)
			require.False(t, complete)
			status, err := c.GetCommandStatus(session)
			require.NoError(t, err)
			require.False(t, status.Running)
			require.NoFileExists(t, c.stdoutFileName(session))
			require.NoFileExists(t, c.stderrFileName(session))
		})
	}
}

func TestCommandCancellationIsNotTimeout(t *testing.T) {
	for _, action := range []string{"cancel", "external_kill", "interrupt"} {
		t.Run(action, func(t *testing.T) {
			c := NewController("", "")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var got *execute.ErrorOutput
			actionResult := make(chan error, 1)
			req := &ExecuteCodeRequest{Code: "sleep 30"}
			req.SetDefaultHooks()
			req.Hooks.OnExecuteError = func(e *execute.ErrorOutput) { got = e }
			req.Hooks.OnExecuteInit = func(id string) {
				switch action {
				case "cancel":
					cancel()
					actionResult <- nil
				case "external_kill":
					actionResult <- syscall.Kill(-c.commandSnapshot(id).pid, syscall.SIGKILL)
				case "interrupt":
					go func() { actionResult <- c.Interrupt(id) }()
				}
			}
			require.NoError(t, c.runCommand(ctx, req))
			require.NoError(t, <-actionResult)
			require.NotNil(t, got)
			require.Equal(t, "CommandExecError", got.EName)
			require.Equal(t, "-1", got.EValue)
		})
	}
}

func TestCommandOutputDrainDoesNotChangeExitClassification(t *testing.T) {
	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var got *execute.ErrorOutput
	req := &ExecuteCodeRequest{Code: "printf before; exit 7"}
	req.SetDefaultHooks()
	req.Hooks.OnExecuteStdout = func(string) { <-ctx.Done() }
	req.Hooks.OnExecuteError = func(e *execute.ErrorOutput) { got = e }
	require.NoError(t, c.runCommand(ctx, req))
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.NotNil(t, got)
	require.Equal(t, "CommandExecError", got.EName)
	require.Equal(t, "7", got.EValue)
}
