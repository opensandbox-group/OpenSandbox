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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

func TestCommandInventorySafePoint(t *testing.T) {
	t.Run("foreground registration blocks lifecycle continuation after init", func(t *testing.T) {
		c := NewController("", "")
		c.inventoryMu.Lock()

		initReturned := make(chan string, 1)
		runDone := make(chan error, 1)
		go func() {
			runDone <- c.runCommand(context.Background(), &ExecuteCodeRequest{
				Code: "exit 0", Cwd: t.TempDir(),
				Hooks: ExecuteResultHook{
					OnExecuteInit:     func(session string) { initReturned <- session },
					OnExecuteStdout:   func(string) {},
					OnExecuteStderr:   func(string) {},
					OnExecuteError:    func(*execute.ErrorOutput) {},
					OnExecuteComplete: func(time.Duration) {},
				},
			})
		}()

		session := <-initReturned
		kernel := waitForStoredKernel(t, c, session)
		assertProcessIsZombieWhenAvailable(t, kernel.pid)
		_, exists := c.bySession[session]
		require.False(t, exists)
		assertNotReturned(t, runDone)

		c.inventoryMu.Unlock()
		require.NoError(t, <-runDone)
		awaitTerminalInventoryState(t, c, session)
	})

	t.Run("background registration blocks return", func(t *testing.T) {
		c := NewController("", "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.inventoryMu.Lock()

		initReturned := make(chan string, 1)
		runDone := make(chan error, 1)
		go func() {
			runDone <- c.runBackgroundCommand(ctx, cancel, &ExecuteCodeRequest{
				Code: "exit 0", Cwd: t.TempDir(),
				Hooks: ExecuteResultHook{
					OnExecuteInit:     func(session string) { initReturned <- session },
					OnExecuteComplete: func(time.Duration) {},
				},
			})
		}()

		session := <-initReturned
		kernel := waitForStoredKernel(t, c, session)
		assertProcessIsZombieWhenAvailable(t, kernel.pid)
		_, exists := c.bySession[session]
		require.False(t, exists)
		assertNotReturned(t, runDone)

		c.inventoryMu.Unlock()
		require.NoError(t, <-runDone)
		awaitTerminalInventoryState(t, c, session)
	})
}

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
				Code: "exit 0", Cwd: filepath.Join(t.TempDir(), "missing"),
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
			assertInventoryAbsent(t, c, session)
		})
	}
}

func TestCommandInventoryForegroundLifecycle(t *testing.T) {
	t.Run("success transitions after complete callback returns", func(t *testing.T) {
		c := NewController("", "")
		callbackEntered := make(chan struct{})
		releaseCallback := make(chan struct{})
		var session string
		runDone := make(chan error, 1)
		go func() {
			runDone <- c.runCommand(context.Background(), &ExecuteCodeRequest{
				Code: "exit 0", Cwd: t.TempDir(),
				Hooks: ExecuteResultHook{
					OnExecuteInit:   func(id string) { session = id },
					OnExecuteStdout: func(string) {},
					OnExecuteStderr: func(string) {},
					OnExecuteError:  func(*execute.ErrorOutput) {},
					OnExecuteComplete: func(time.Duration) {
						close(callbackEntered)
						<-releaseCallback
					},
				},
			})
		}()

		awaitSignal(t, callbackEntered, "complete callback")
		status, err := c.GetCommandStatus(session)
		require.NoError(t, err)
		require.False(t, status.Running, "success mutation must precede the complete callback")
		requireRunningInventoryState(t, c, session)
		assertNotReturned(t, runDone)

		close(releaseCallback)
		require.NoError(t, <-runDone)
		requireTerminalInventoryState(t, c, session)
	})

	t.Run("error transitions after error callback returns", func(t *testing.T) {
		c := NewController("", "")
		callbackEntered := make(chan struct{})
		releaseCallback := make(chan struct{})
		var session string
		runDone := make(chan error, 1)
		go func() {
			runDone <- c.runCommand(context.Background(), &ExecuteCodeRequest{
				Code: "exit 17", Cwd: t.TempDir(),
				Hooks: ExecuteResultHook{
					OnExecuteInit:     func(id string) { session = id },
					OnExecuteStdout:   func(string) {},
					OnExecuteStderr:   func(string) {},
					OnExecuteComplete: func(time.Duration) {},
					OnExecuteError: func(*execute.ErrorOutput) {
						close(callbackEntered)
						<-releaseCallback
					},
				},
			})
		}()

		awaitSignal(t, callbackEntered, "error callback")
		status, err := c.GetCommandStatus(session)
		require.NoError(t, err)
		require.True(t, status.Running, "error callback must precede the legacy terminal mutation")
		requireRunningInventoryState(t, c, session)
		assertNotReturned(t, runDone)

		close(releaseCallback)
		require.NoError(t, <-runDone)
		status, err = c.GetCommandStatus(session)
		require.NoError(t, err)
		require.False(t, status.Running)
		require.Equal(t, 17, *status.ExitCode)
		requireTerminalInventoryState(t, c, session)
	})
}

func TestCommandInventoryBackgroundLifecycle(t *testing.T) {
	t.Run("waiter transitions while startup callback is blocked", func(t *testing.T) {
		c := NewController("", "")
		startupEntered := make(chan struct{})
		releaseStartup := make(chan struct{})
		sessionReady := make(chan string, 1)
		runDone := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			runDone <- c.runBackgroundCommand(ctx, cancel, &ExecuteCodeRequest{
				Code: "exit 0", Cwd: t.TempDir(),
				Hooks: ExecuteResultHook{
					OnExecuteInit: func(id string) { sessionReady <- id },
					OnExecuteComplete: func(time.Duration) {
						close(startupEntered)
						<-releaseStartup
					},
				},
			})
		}()

		session := <-sessionReady
		awaitSignal(t, startupEntered, "background startup callback")
		awaitCommandTerminalStatus(t, c, session)
		awaitTerminalInventoryState(t, c, session)
		assertNotReturned(t, runDone)

		close(releaseStartup)
		require.NoError(t, <-runDone)
	})

	t.Run("fast exits retain terminal summaries without stale running entries", func(t *testing.T) {
		// These index assertions verify the registration and terminal-transition invariant.
		const commands = 8
		c := NewController("", "")
		sessions := make([]string, 0, commands)
		for range commands {
			var session string
			ctx, cancel := context.WithCancel(context.Background())
			require.NoError(t, c.runBackgroundCommand(ctx, cancel, &ExecuteCodeRequest{
				Code: "exit 0", Cwd: t.TempDir(),
				Hooks: ExecuteResultHook{
					OnExecuteInit:     func(id string) { session = id },
					OnExecuteComplete: func(time.Duration) {},
				},
			}))
			cancel()
			require.NotEmpty(t, session)
			sessions = append(sessions, session)
		}

		for _, session := range sessions {
			awaitCommandTerminalStatus(t, c, session)
			awaitTerminalInventoryState(t, c, session)
		}

		c.inventoryMu.Lock()
		defer c.inventoryMu.Unlock()
		require.Zero(t, c.runningByStartedAt.Len())
		require.Equal(t, commands, c.terminalByStartedAt.Len())
		require.Equal(t, commands, c.terminalByFinishedAt.Len())
		for _, session := range sessions {
			entry := c.bySession[session]
			require.NotNil(t, entry, "terminal transition must not overtake registration")
			require.False(t, entry.running)
		}
	})
}

func TestCommandInventoryLegacyCompatibility(t *testing.T) {
	for _, test := range []struct {
		name   string
		config CommandInventoryConfig
	}{
		{
			name:   "zero recovery TTL",
			config: CommandInventoryConfig{RecoveryTTL: 0, MaxTerminal: 1000},
		},
		{
			name:   "zero terminal capacity",
			config: CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 0},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Run("foreground", func(t *testing.T) {
				c := NewController("", "", WithCommandInventory(test.config))
				var session string
				require.NoError(t, c.runCommand(context.Background(), &ExecuteCodeRequest{
					Code: "exit 0", Cwd: t.TempDir(),
					Hooks: ExecuteResultHook{
						OnExecuteInit:     func(id string) { session = id },
						OnExecuteStdout:   func(string) {},
						OnExecuteStderr:   func(string) {},
						OnExecuteError:    func(*execute.ErrorOutput) {},
						OnExecuteComplete: func(time.Duration) {},
					},
				}))
				awaitInventoryAbsent(t, c, session)

				status, err := c.GetCommandStatus(session)
				require.NoError(t, err)
				require.Equal(t, "exit 0", status.Content)
				_, _, err = c.SeekBackgroundCommandOutput(session, 0)
				require.EqualError(t, err, "command "+session+" is not running in background")
				err = c.Interrupt(session)
				require.EqualError(t, err, "command session "+session+" is not running")
			})

			t.Run("background", func(t *testing.T) {
				c := NewController("", "", WithCommandInventory(test.config))
				var session string
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				require.NoError(t, c.runBackgroundCommand(ctx, cancel, &ExecuteCodeRequest{
					Code: "printf retained-output", Cwd: t.TempDir(),
					Hooks: ExecuteResultHook{
						OnExecuteInit:     func(id string) { session = id },
						OnExecuteComplete: func(time.Duration) {},
					},
				}))
				awaitCommandTerminalStatus(t, c, session)
				awaitInventoryAbsent(t, c, session)

				status, err := c.GetCommandStatus(session)
				require.NoError(t, err)
				require.Equal(t, "printf retained-output", status.Content)
				output, _, err := c.SeekBackgroundCommandOutput(session, 0)
				require.NoError(t, err)
				require.Equal(t, "retained-output", string(output))
				err = c.Interrupt(session)
				require.EqualError(t, err, "command session "+session+" is not running")
			})
		})
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", name)
	}
}

func awaitCommandTerminalStatus(t *testing.T, c *Controller, session string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := c.GetCommandStatus(session)
		if err == nil && !status.Running {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("command session %s did not reach terminal status", session)
}

func requireRunningInventoryState(t *testing.T, c *Controller, session string) {
	t.Helper()
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	entry := c.bySession[session]
	require.NotNil(t, entry)
	require.True(t, entry.running)
	require.Equal(t, 1, c.runningByStartedAt.Len())
	require.Zero(t, c.terminalByStartedAt.Len())
	require.Zero(t, c.terminalByFinishedAt.Len())
}

func requireTerminalInventoryState(t *testing.T, c *Controller, session string) {
	t.Helper()
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	entry := c.bySession[session]
	require.NotNil(t, entry)
	require.False(t, entry.running)
	require.Zero(t, c.runningByStartedAt.Len())
	require.Equal(t, 1, c.terminalByStartedAt.Len())
	require.Equal(t, 1, c.terminalByFinishedAt.Len())
}

func awaitTerminalInventoryState(t *testing.T, c *Controller, session string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.inventoryMu.Lock()
		entry := c.bySession[session]
		terminal := entry != nil && !entry.running
		c.inventoryMu.Unlock()
		if terminal {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("command session %s did not reach terminal inventory state", session)
}

func awaitInventoryAbsent(t *testing.T, c *Controller, session string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.inventoryMu.Lock()
		_, exists := c.bySession[session]
		c.inventoryMu.Unlock()
		if !exists {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("command session %s was retained by inventory", session)
}

func waitForStoredKernel(t *testing.T, c *Controller, session string) *commandKernel {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if kernel := c.getCommandKernel(session); kernel != nil && kernel.pid > 0 {
			return kernel
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("command kernel was not stored after init callback")
	return nil
}

func assertProcessIsZombieWhenAvailable(t *testing.T, pid int) {
	t.Helper()
	if _, err := os.Stat("/proc"); err != nil {
		return
	}
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			t.Fatal("command was reaped before inventory registration completed")
		}
		if err == nil && processState(contents) == 'Z' {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("command pid %d did not reach zombie state", pid)
}

func processState(stat []byte) byte {
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 || len(stat) <= end+2 {
		return 0
	}
	return stat[end+2]
}

func assertInventoryAbsent(t *testing.T, c *Controller, session string) {
	t.Helper()
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	_, exists := c.bySession[session]
	require.False(t, exists)
}

func assertNotReturned(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("command returned before inventory registration completed: %v", err)
	default:
	}
}
