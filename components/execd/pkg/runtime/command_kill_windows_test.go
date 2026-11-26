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

//go:build windows
// +build windows

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

// childScript writes parent.cmd, which the command runs, and child.cmd, which
// parent.cmd starts in a second cmd.exe and waits for. The child marks when it
// starts and, about two seconds later, writes child.done unless it was killed
// together with the command.
func childScript(t *testing.T) (dir, started, done string) {
	t.Helper()
	dir = t.TempDir()
	parent := "@cmd /C \"%~dp0child.cmd\"\r\n"
	child := "@echo started> \"%~dp0child.started\"\r\n" +
		"@ping -n 3 127.0.0.1 >nul\r\n" +
		"@echo late> \"%~dp0child.done\"\r\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "parent.cmd"), []byte(parent), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "child.cmd"), []byte(child), 0o644))
	return dir, filepath.Join(dir, "child.started"), filepath.Join(dir, "child.done")
}

func requireChildKilled(t *testing.T, done string) {
	t.Helper()
	// Give a surviving child time to write the marker.
	time.Sleep(3 * time.Second)
	_, err := os.Stat(done)
	require.True(t, os.IsNotExist(err), "child process survived the command")
}

func TestKillProcessTree_WindowsSkipsReapedProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit 0")
	require.NoError(t, cmd.Run())

	// The PID may already belong to another process, so nothing is killed.
	err := killProcessTree("", cmd.Process)
	require.ErrorIs(t, err, os.ErrProcessDone)
}

func TestKillPid_WindowsUsesStartedProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/C", "exit 0")
	require.NoError(t, cmd.Run())

	// The command exited after Interrupt read its state. killPid must use the
	// reaped process instead of looking up a PID that may now be reused.
	c := NewController("", "")
	c.storeCommandKernel("session", &commandKernel{
		pid:     cmd.Process.Pid,
		process: cmd.Process,
		running: true,
	})
	require.NoError(t, c.killPid("session", cmd.Process.Pid))
}

func TestRunCommand_WindowsTimeoutKillsChildren(t *testing.T) {
	dir, started, done := childScript(t)

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var gotErr *execute.ErrorOutput
	req := &ExecuteCodeRequest{
		Code:    `.\parent.cmd`,
		Cwd:     dir,
		Timeout: time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteError: func(err *execute.ErrorOutput) { gotErr = err },
		},
	}
	req.SetDefaultHooks()

	require.NoError(t, c.runCommand(ctx, req))
	require.NotNil(t, gotErr)
	require.Equal(t, "CommandTimeout", gotErr.EName)
	require.FileExists(t, started, "child did not start before the timeout")
	requireChildKilled(t, done)
}

func TestRunBackgroundCommand_WindowsCancelKillsChildren(t *testing.T) {
	dir, started, done := childScript(t)

	c := NewController("", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var session string
	req := &ExecuteCodeRequest{
		Code: `.\parent.cmd`,
		Cwd:  dir,
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(s string) { session = s },
		},
	}
	req.SetDefaultHooks()

	require.NoError(t, c.runBackgroundCommand(ctx, cancel, req))
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "child did not start")
	cancel()
	require.Eventually(t, func() bool {
		status, err := c.GetCommandStatus(session)
		return err == nil && !status.Running
	}, 5*time.Second, 10*time.Millisecond)
	requireChildKilled(t, done)
}

func TestInterrupt_WindowsKillsChildren(t *testing.T) {
	dir, started, done := childScript(t)

	c := NewController("", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var session string
	req := &ExecuteCodeRequest{
		Code: `.\parent.cmd`,
		Cwd:  dir,
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(s string) { session = s },
		},
	}
	req.SetDefaultHooks()

	require.NoError(t, c.runBackgroundCommand(ctx, cancel, req))
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "child did not start")
	require.NoError(t, c.Interrupt(session))
	require.Eventually(t, func() bool {
		status, err := c.GetCommandStatus(session)
		return err == nil && !status.Running
	}, 5*time.Second, 10*time.Millisecond)
	requireChildKilled(t, done)
}
