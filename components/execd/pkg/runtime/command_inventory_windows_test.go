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

//go:build windows

package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

// TestCommandInventoryWindows exercises the Windows cmd.exe implementation,
// rather than merely cross-compiling the shared inventory tests.
func TestCommandInventoryWindows(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, success := range []bool{true, false} {
			background, success := background, success
			name := "foreground"
			if background {
				name = "background"
			}
			if success {
				name += "/success"
			} else {
				name += "/nonzero"
			}
			t.Run(name, func(t *testing.T) {
				testCommandInventoryWindowsTerminalLifecycle(t, background, success)
			})
		}
	}
}

func testCommandInventoryWindowsTerminalLifecycle(t *testing.T, background, success bool) {
	t.Helper()
	c := NewController("", "")
	label := fmt.Sprintf("inventory-windows-%t-%t", background, success)
	exitCode := 17
	if success {
		exitCode = 0
	}

	var session string
	request := &ExecuteCodeRequest{
		// cmd-compatible syntax is intentional: Windows runtime invokes cmd /C.
		Code: fmt.Sprintf("echo %s & exit /b %d", label, exitCode),
		Cwd:  t.TempDir(),
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(id string) { session = id },
			OnExecuteStdout:   func(string) {},
			OnExecuteStderr:   func(string) {},
			OnExecuteError:    func(*execute.ErrorOutput) {},
			OnExecuteComplete: func(time.Duration) {},
		},
	}

	if background {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, c.runBackgroundCommand(ctx, cancel, request))
	} else {
		require.NoError(t, c.runCommand(context.Background(), request))
	}
	require.NotEmpty(t, session)

	status := awaitWindowsTerminalStatus(t, c, session)
	require.Equal(t, exitCode, *status.ExitCode)
	awaitWindowsTerminalInventory(t, c, session, background, exitCode)

	logs, _, logsErr := c.SeekBackgroundCommandOutput(session, 0)
	if background {
		require.NoError(t, logsErr)
		require.Contains(t, string(logs), label)
	} else {
		require.EqualError(t, logsErr, "command "+session+" is not running in background")
	}
	require.EqualError(t, c.Interrupt(session), "command session "+session+" is not running")
}

// TestCommandInventorySafePoint is run by the Windows CI matrix alongside the
// Windows-specific lifecycle test. It verifies that cmd command startup is
// registered before the background call returns.
func TestCommandInventorySafePoint(t *testing.T) {
	for _, background := range []bool{false, true} {
		name := "foreground"
		if background {
			name = "background"
		}
		t.Run(name, func(t *testing.T) {
			testCommandInventoryWindowsSafePoint(t, background)
		})
	}
}

func testCommandInventoryWindowsSafePoint(t *testing.T, background bool) {
	t.Helper()
	c := NewController("", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The inventory mutex is the approved registration barrier. Holding it
	// proves registration is pending without relying on command duration.
	c.inventoryMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.inventoryMu.Unlock()
		}
	}()

	initReturned := make(chan string, 1)
	callbackEntered := make(chan struct{}, 1)
	releaseCallback := make(chan struct{})
	runDone := make(chan error, 1)
	request := &ExecuteCodeRequest{
		Cwd: t.TempDir(),
		Hooks: ExecuteResultHook{
			OnExecuteInit:   func(id string) { initReturned <- id },
			OnExecuteStdout: func(string) {},
			OnExecuteStderr: func(string) {},
			OnExecuteError:  func(*execute.ErrorOutput) {},
			OnExecuteComplete: func(time.Duration) {
				callbackEntered <- struct{}{}
				<-releaseCallback
			},
		},
	}
	if background {
		// Keep the real cmd process alive until this test interrupts it.
		request.Code = "ping -n 20 127.0.0.1 >nul & exit /b 0"
		go func() { runDone <- c.runBackgroundCommand(ctx, cancel, request) }()
	} else {
		// The completion callback keeps the terminal inventory transition from
		// overtaking the post-registration running assertion.
		request.Code = "exit /b 0"
		go func() { runDone <- c.runCommand(ctx, request) }()
	}

	session := <-initReturned
	require.NotEmpty(t, session)
	waitForWindowsStoredKernel(t, c, session)
	_, registered := c.bySession[session]
	require.False(t, registered, "inventory registration must block at the safe point")
	assertWindowsCommandBlocked(t, runDone)

	c.inventoryMu.Unlock()
	locked = false
	if background {
		awaitWindowsSignal(t, callbackEntered, "background startup callback")
		requireWindowsRunningInventory(t, c, session)
		require.NoError(t, c.Interrupt(session))
		close(releaseCallback)
		require.NoError(t, <-runDone)
		awaitWindowsTerminalStatus(t, c, session)
		return
	}

	awaitWindowsSignal(t, callbackEntered, "foreground completion callback")
	requireWindowsRunningInventory(t, c, session)
	assertWindowsCommandBlocked(t, runDone)
	close(releaseCallback)
	require.NoError(t, <-runDone)
	awaitWindowsTerminalStatus(t, c, session)
}

// TestCommandInventoryStartFailure verifies that failed cmd startup does not
// create an inventory summary on Windows.
func TestCommandInventoryStartFailure(t *testing.T) {
	for _, background := range []bool{false, true} {
		name := "foreground"
		if background {
			name = "background"
		}
		t.Run(name, func(t *testing.T) {
			c := NewController("", "")
			var session string
			req := &ExecuteCodeRequest{
				Code: "exit /b 0",
				Cwd:  filepath.Join(t.TempDir(), "missing"),
				Hooks: ExecuteResultHook{
					OnExecuteInit:     func(id string) { session = id },
					OnExecuteError:    func(*execute.ErrorOutput) {},
					OnExecuteComplete: func(time.Duration) {},
				},
			}
			if background {
				require.Error(t, c.runBackgroundCommand(context.Background(), func() {}, req))
			} else {
				require.NoError(t, c.runCommand(context.Background(), req))
			}
			require.NotEmpty(t, session)
			listed, err := c.ListCommands(ListCommandsRequest{Limit: 100})
			require.NoError(t, err)
			require.NotContains(t, commandInventorySessions(listed.Commands), session)
			_, err = c.GetCommandStatus(session)
			require.EqualError(t, err, "command not found: "+session)
		})
	}
}

func awaitWindowsTerminalStatus(t *testing.T, c *Controller, session string) *CommandStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := c.GetCommandStatus(session)
		if err == nil && !status.Running {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Windows command session %s did not reach terminal status", session)
	return nil
}

func awaitWindowsTerminalInventory(t *testing.T, c *Controller, session string, background bool, exitCode int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listed, err := c.ListCommands(ListCommandsRequest{Limit: 100})
		require.NoError(t, err)
		for _, summary := range listed.Commands {
			if summary.Session != session {
				continue
			}
			if !summary.Running && summary.Background == background && summary.ExitCode != nil && *summary.ExitCode == exitCode {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf(
		"Windows command session %s did not reach expected terminal inventory state (background=%t exit_code=%d)",
		session,
		background,
		exitCode,
	)
}

func waitForWindowsStoredKernel(t *testing.T, c *Controller, session string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.getCommandKernel(session) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Windows command session %s was not stored after init callback", session)
}

func requireWindowsRunningInventory(t *testing.T, c *Controller, session string) {
	t.Helper()
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	entry := c.bySession[session]
	require.NotNil(t, entry)
	require.True(t, entry.running)
	require.Equal(t, 1, c.runningByStartedAt.Len())
}

func assertWindowsCommandBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Windows command returned before inventory registration completed: %v", err)
	default:
	}
}

func awaitWindowsSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", name)
	}
}

func commandInventorySessions(commands []CommandSummary) []string {
	sessions := make([]string, 0, len(commands))
	for _, command := range commands {
		sessions = append(sessions, command.Session)
	}
	return sessions
}
