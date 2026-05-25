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

//go:build !windows
// +build !windows

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
)

// TestRunCommand_CancelKillsChildren verifies that cancelling the context
// terminates not only the bash group leader but also its descendant
// processes. Regression test for
// https://github.com/alibaba/OpenSandbox/issues/922.
func TestRunCommand_CancelKillsChildren(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")

	c := NewController("", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	var once sync.Once

	req := &ExecuteCodeRequest{
		// Spawn a sleep child, record its pid, then wait so the bash
		// leader stays alive until the context is cancelled.
		Code:    `sleep 30 & echo $! > "` + pidFile + `"; echo READY; wait`,
		Cwd:     t.TempDir(),
		Timeout: 30 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(_ string) {},
			OnExecuteStdout: func(s string) {
				if strings.TrimSpace(s) == "READY" {
					once.Do(func() { close(started) })
				}
			},
			OnExecuteStderr:   func(_ string) {},
			OnExecuteError:    func(_ *execute.ErrorOutput) {},
			OnExecuteComplete: func(_ time.Duration) {},
		},
	}

	done := make(chan struct{})
	go func() {
		_ = c.runCommand(ctx, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("command did not emit READY in time")
	}

	pidBytes, err := os.ReadFile(pidFile)
	require.NoError(t, err, "expected child pid file")
	childPid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.Positive(t, childPid)

	require.NoError(t, syscall.Kill(childPid, 0), "child should be alive before cancel")

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runCommand did not return after cancel")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPid, 0); err != nil {
			require.True(t, errors.Is(err, syscall.ESRCH),
				"unexpected liveness probe error: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child pid %d still alive 2s after cancel — process leak", childPid)
}

// TestInterrupt_AfterFinished_ReturnsError verifies that an Interrupt
// arriving after the command has completed does not signal a recycled PID.
// Without this guard, group-wide kill would amplify the stale-PID hazard
// to every process in an unrelated process group.
func TestInterrupt_AfterFinished_ReturnsError(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var session string
	completeCh := make(chan struct{}, 1)
	req := &ExecuteCodeRequest{
		Code:    `echo done`,
		Cwd:     t.TempDir(),
		Timeout: 5 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(s string) { session = s },
			OnExecuteStdout:   func(_ string) {},
			OnExecuteStderr:   func(_ string) {},
			OnExecuteError:    func(_ *execute.ErrorOutput) {},
			OnExecuteComplete: func(_ time.Duration) { completeCh <- struct{}{} },
		},
	}
	require.NoError(t, c.runCommand(ctx, req))

	select {
	case <-completeCh:
	case <-time.After(3 * time.Second):
		t.Fatal("command did not complete in time")
	}
	require.NotEmpty(t, session)

	err := c.Interrupt(session)
	require.Error(t, err, "Interrupt on finished session must error")
	require.Contains(t, err.Error(), "not running")

	snap := c.commandSnapshot(session)
	require.NotNil(t, snap)
	require.False(t, snap.running, "running flag should be cleared")
	require.Equal(t, 0, snap.pid, "pid should be cleared to avoid stale-PID kill")
}

// TestKillPid_ZombieLeaderDoesNotFail verifies that killPid does not
// return an error when a group leader becomes a zombie before its parent
// has reaped it. kill(-pid, 0) keeps reporting the group as observable
// while the zombie lingers, but SIGKILL has already been delivered and
// the kernel will tear the group down once Wait() runs. Treating that
// state as a failure caused Interrupt to surface a 500 even though the
// kill succeeded.
func TestKillPid_ZombieLeaderDoesNotFail(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	cmd := exec.Command("bash", "-c", `sleep 30 & wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	// Deliberately omit a reaper goroutine so the leader stays as a
	// zombie after kill — that is the condition we want to exercise.
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	// Give bash a moment to spawn the sleep child so the group has more
	// than just the leader.
	time.Sleep(100 * time.Millisecond)

	c := &Controller{}
	require.NoError(t, c.killPid(cmd.Process.Pid),
		"slow post-SIGKILL teardown must not be reported as a hard failure")
}

// TestKillPid_TerminatesEntireProcessGroup verifies that killPid signals
// the whole process group, not just the leader. Regression test for
// https://github.com/alibaba/OpenSandbox/issues/922.
func TestKillPid_TerminatesEntireProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("bash", "-c",
		`sleep 30 & echo $! > "`+pidFile+`"; wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	// Reap the leader concurrently so it doesn't linger as a zombie that
	// keeps the process group "alive" from killPid's liveness probe
	// perspective. Mirrors how runCommand's cmd.Wait() reaps in production.
	waitDone := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone
	})

	var childPid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				childPid = pid
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Positive(t, childPid, "failed to capture child pid")
	require.NoError(t, syscall.Kill(childPid, 0), "child should be alive before kill")

	c := &Controller{}
	require.NoError(t, c.killPid(cmd.Process.Pid))

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPid, 0); err != nil {
			require.True(t, errors.Is(err, syscall.ESRCH),
				"unexpected liveness probe error: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child pid %d still alive 2s after killPid — process leak", childPid)
}
