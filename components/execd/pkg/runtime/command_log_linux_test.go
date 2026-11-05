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

//go:build linux

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCommandLogCorrelationDeadlineAfterProcessExit(t *testing.T) {
	logPath := captureCommandLog(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	require.Nil(t, initReaper, "this regression exercises exec.Cmd's classic-mode context watcher")
	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	var observationErr error
	var exitedBeforeDeadline bool
	var executionError *execute.ErrorOutput
	request := &ExecuteCodeRequest{
		Argv: []string{executable, "-test.run=^TestCommandLogProcess$"},
		Envs: map[string]string{"EXECD_COMMAND_LOG_HELPER": "1", "EXECD_COMMAND_LOG_EXIT": "0"},
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(session string) {
				id = session
				kernel := c.commandSnapshot(session)
				if kernel == nil {
					observationErr = errors.New("command kernel missing before init")
					return
				}
				// Observe the real child's exit without reaping it. The runtime
				// still owns Wait and must collect this child's actual exit 0.
				var info unix.Siginfo
				for {
					observationErr = unix.Waitid(unix.P_PID, kernel.pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
					if !errors.Is(observationErr, unix.EINTR) {
						break
					}
				}
				exitedBeforeDeadline = observationErr == nil && ctx.Err() == nil
				// Hold init until the request expires before runtime Wait. Give
				// exec.Cmd's watcher time to deliver cancellation to the already
				// exited (unreaped) child and record its context error.
				<-ctx.Done()
				time.Sleep(100 * time.Millisecond)
			},
			OnExecuteStdout: func(string) {},
			OnExecuteStderr: func(string) {},
			OnExecuteError:  func(output *execute.ErrorOutput) { executionError = output },
		},
	}
	request.SetDefaultHooks()
	runErr := c.runCommand(ctx, request)
	require.NoError(t, runErr)
	require.NoError(t, observationErr)
	require.True(t, exitedBeforeDeadline)
	require.NotNil(t, executionError)
	require.Equal(t, context.DeadlineExceeded.Error(), executionError.EValue)
	require.Nil(t, executionError.ExitCode, "a context error is not an exit-status error")
	status, err := c.GetCommandStatus(id)
	require.NoError(t, err)
	require.Equal(t, 1, *status.ExitCode, "existing fallback bookkeeping must remain unchanged")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "command: command_id="+id+" mode=foreground run failed") {
			found = true
			require.Contains(t, line, "exit_code=0 elapsed_ms=", "log the observed process exit, not fallback status 1")
			require.Contains(t, line, context.DeadlineExceeded.Error())
		}
	}
	require.True(t, found)
}

func TestCommandLogCorrelationInitReaperObservedStatus(t *testing.T) {
	logPath := captureCommandLog(t)
	startReaperForTest(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, signaled := range []bool{false, true} {
		name, expectedCode := "nonzero", 7
		cmd := exec.Command(executable, "-test.run=^TestCommandLogProcess$")
		cmd.Env = append(os.Environ(), "EXECD_COMMAND_LOG_HELPER=1", "EXECD_COMMAND_LOG_EXIT=7")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if signaled {
			name, expectedCode = "signal", -1
			cmd.Env = append(cmd.Env, "EXECD_COMMAND_LOG_GATE="+filepath.Join(t.TempDir(), "never-released"))
		}
		startedAt := time.Now()
		process, startErr := launchManaged(cmd)
		require.NoError(t, startErr)
		var killErr error
		if signaled {
			killErr = cmd.Process.Kill()
		}
		waitErr := process.Wait()
		require.NoError(t, killErr)
		require.Error(t, waitErr)
		require.Nil(t, cmd.ProcessState, "the init reaper supplies status without Cmd.Wait")
		require.Equal(t, expectedCode, process.ExitCode())
		var observed exitCoder
		require.True(t, errors.As(waitErr, &observed))
		require.Equal(t, expectedCode, observed.ExitCode())
		logCommandRunFailure("init-reaper-"+name, "foreground", cmd, startedAt, waitErr)
		data, readErr := os.ReadFile(logPath)
		require.NoError(t, readErr)
		found := false
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "command: command_id=init-reaper-"+name+" mode=foreground run failed") {
				found = true
				require.Contains(t, line, "exit_code="+strconv.Itoa(expectedCode)+" elapsed_ms=")
				require.NotContains(t, line, "exit_code=unknown")
			}
		}
		require.True(t, found)
	}
}
