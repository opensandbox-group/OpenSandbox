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

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/stretchr/testify/require"
)

// Run the real test executable as a portable child, with controlled exit and output.
func TestCommandLogProcess(t *testing.T) {
	if os.Getenv("EXECD_COMMAND_LOG_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "private-command-stdout-payload")
	fmt.Fprintln(os.Stderr, "private-command-stderr-payload")
	if ready := os.Getenv("EXECD_COMMAND_LOG_READY"); ready != "" {
		if err := os.WriteFile(ready, nil, 0600); err != nil {
			os.Exit(90)
		}
	}
	if gate := os.Getenv("EXECD_COMMAND_LOG_GATE"); gate != "" {
		deadline := time.Now().Add(8 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(91)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	code, err := strconv.Atoi(os.Getenv("EXECD_COMMAND_LOG_EXIT"))
	if err != nil {
		os.Exit(92)
	}
	os.Exit(code)
}

func captureCommandLog(t *testing.T) (string, bool) {
	t.Helper()
	const testEnv = "EXECD_COMMAND_LOG_CAPTURE_TEST"
	const pathEnv = "EXECD_COMMAND_LOG_CAPTURE_PATH"
	name, path := os.Getenv(testEnv), os.Getenv(pathEnv)
	if name != "" || path != "" {
		require.Equal(t, t.Name(), name, "capture subprocess must run only its designated test")
		require.NotEmpty(t, path)
		t.Setenv("EXECD_LOG_FILE", path)
		log.Init(6)
		return path, true
	}

	// The parent owns the directory; only the child owns the logger and its
	// open sink. Process exit closes every handle before Windows cleanup.
	path = filepath.Join(t.TempDir(), "execd.log")
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), testEnv+"="+t.Name(), pathEnv+"="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "capture subprocess failed:\n%s", output)
	// Remove the actual captured log, not a probe file: on Windows this fails
	// if any process still holds the capture sink open.
	require.NoError(t, os.Remove(path), "capture sink must be removable after child exit")
	return "", false
}

func TestCommandLogCorrelationConcurrent(t *testing.T) {
	logPath, child := captureCommandLog(t)
	if !child {
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, background := range []bool{false, true} {
		mode := "foreground"
		language := Command
		if background {
			mode, language = "background", BackgroundCommand
		}
		t.Run(mode, func(t *testing.T) {
			c := NewController("", "")
			dir := t.TempDir()
			gate := filepath.Join(dir, "release")
			type outcome struct {
				allocated, initialized string
				allocationMode         bool
				complete, acknowledged bool
				executionError         *execute.ErrorOutput
				err                    error
			}
			var results [2]outcome
			var wg sync.WaitGroup
			for i, exitCode := range []int{0, 7} {
				wg.Add(1)
				go func(i, exitCode int) {
					defer wg.Done()
					result := &results[i]
					req := &ExecuteCodeRequest{
						Language: language,
						Argv:     []string{executable, "-test.run=^TestCommandLogProcess$", "--", "--token=command-log-sensitive-token"},
						Timeout:  10 * time.Second,
						Envs: map[string]string{
							"EXECD_COMMAND_LOG_HELPER": "1",
							"EXECD_COMMAND_LOG_EXIT":   strconv.Itoa(exitCode),
							"EXECD_COMMAND_LOG_READY":  filepath.Join(dir, strconv.Itoa(i)),
							"EXECD_COMMAND_LOG_GATE":   gate,
							"CORRELATION_PRIVATE_ENV":  "private-command-environment-value",
						},
						Hooks: ExecuteResultHook{
							OnCommandAllocated: func(id string, bg bool) { result.allocated, result.allocationMode = id, bg },
							OnExecuteInit:      func(id string) { result.initialized = id },
							OnExecuteStdout:    func(string) {},
							OnExecuteStderr:    func(string) {},
							OnExecuteError:     func(e *execute.ErrorOutput) { result.executionError = e },
							OnExecuteComplete: func(time.Duration) {
								result.complete = true
								status, err := c.GetCommandStatus(result.allocated)
								result.acknowledged = err == nil && status.Running && status.ExitCode == nil
							},
						},
					}
					req.SetDefaultHooks()
					result.err = c.Execute(req)
				}(i, exitCode)
			}
			// Both real processes must be alive together before either may exit.
			ready := func() bool {
				_, first := os.Stat(filepath.Join(dir, "0"))
				_, second := os.Stat(filepath.Join(dir, "1"))
				return first == nil && second == nil
			}
			deadline := time.Now().Add(5 * time.Second)
			for !ready() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			bothReady := ready()
			if background {
				// The acknowledgement must arrive while both children are gated.
				wg.Wait()
			}
			releaseErr := os.WriteFile(gate, nil, 0600)
			wg.Wait()
			// Drain every background waiter before inspecting the captured log.
			for _, result := range results {
				if result.allocated != "" && result.err == nil {
					require.Eventually(t, func() bool {
						status, err := c.GetCommandStatus(result.allocated)
						return err == nil && !status.Running
					}, 12*time.Second, 10*time.Millisecond)
				}
			}
			require.NoError(t, releaseErr)
			require.True(t, bothReady, "success and failure processes must overlap")
			require.NotEqual(t, results[0].allocated, results[1].allocated)
			data, err := os.ReadFile(logPath)
			require.NoError(t, err)
			text := string(data)
			for i, result := range results {
				require.NoError(t, result.err)
				require.NotEmpty(t, result.allocated)
				require.Equal(t, result.allocated, result.initialized)
				require.Equal(t, background, result.allocationMode)
				prefix := "command: command_id=" + result.allocated + " mode=" + mode
				require.Contains(t, text, prefix+" received")
				require.Contains(t, text, prefix+" started pid=")
				if i == 0 {
					require.Contains(t, text, prefix+" exited pid=")
					require.NotContains(t, text, prefix+" run failed")
				} else {
					require.Contains(t, text, prefix+" run failed pid=")
					require.NotContains(t, text, prefix+" exited")
				}
				status, err := c.GetCommandStatus(result.allocated)
				require.NoError(t, err)
				require.Equal(t, i*7, *status.ExitCode)
				for _, line := range strings.Split(text, "\n") {
					if strings.Contains(line, prefix+" exited") || strings.Contains(line, prefix+" run failed") {
						require.Contains(t, line, fmt.Sprintf("exit_code=%d elapsed_ms=", i*7))
					}
				}
				if background {
					_, _, outputErr := c.SeekBackgroundCommandOutput(result.allocated, -1)
					require.ErrorContains(t, outputErr, result.allocated)
					require.True(t, result.complete)
					require.True(t, result.acknowledged, "background completion acknowledges startup, not exit")
					removeCommandOutputFiles(c.combinedOutputFileName(result.allocated))
				} else if i == 0 {
					require.True(t, result.complete)
					require.False(t, result.acknowledged)
				} else {
					require.NotNil(t, result.executionError)
					require.False(t, result.complete)
				}
			}
			require.NotContains(t, text, "command-log-sensitive-token")
			require.Contains(t, text, "--token=****")
			require.NotContains(t, text, "private-command-environment-value")
			require.NotContains(t, text, "private-command-stdout-payload")
			require.NotContains(t, text, "private-command-stderr-payload")
		})
	}
}

func TestCommandLogCorrelationBeforeLaunch(t *testing.T) {
	logPath, child := captureCommandLog(t)
	if !child {
		return
	}
	for _, background := range []bool{false, true} {
		for _, preparationFailure := range []bool{false, true} {
			mode, language := "foreground", Command
			if background {
				mode, language = "background", BackgroundCommand
			}
			var allocated, initialized string
			req := &ExecuteCodeRequest{
				Language: language,
				Argv:     []string{filepath.Join(t.TempDir(), "nonexistent-executable")},
				Timeout:  time.Second,
				Hooks: ExecuteResultHook{
					OnCommandAllocated: func(id string, _ bool) { allocated = id },
					OnExecuteInit:      func(id string) { initialized = id },
					OnExecuteStdout:    func(string) {},
					OnExecuteStderr:    func(string) {},
					OnExecuteError:     func(*execute.ErrorOutput) {},
				},
			}
			if preparationFailure {
				req.Cwd = "${EXECD_COMMAND_LOG_UNDEFINED_CWD}"
				t.Setenv("EXECD_COMMAND_LOG_UNDEFINED_CWD", "")
				require.NoError(t, os.Unsetenv("EXECD_COMMAND_LOG_UNDEFINED_CWD"))
			}
			req.SetDefaultHooks()
			err := NewController("", "").Execute(req)
			require.NotEmpty(t, allocated)
			if background || preparationFailure {
				require.Error(t, err)
			} else {
				require.NoError(t, err) // Foreground launch errors use the existing error hook.
			}
			if initialized != "" {
				require.Equal(t, allocated, initialized)
			}
			data, readErr := os.ReadFile(logPath)
			require.NoError(t, readErr)
			prefix := "command: command_id=" + allocated + " mode=" + mode
			require.Contains(t, string(data), prefix+" received")
			failure := " start failed elapsed_ms="
			if preparationFailure {
				failure = " preparation failed elapsed_ms="
			}
			require.Contains(t, string(data), prefix+failure)
			require.NotContains(t, string(data), prefix+" started")
			require.NotContains(t, string(data), prefix+" exited")
			if background {
				removeCommandOutputFiles(NewController("", "").combinedOutputFileName(allocated))
			}
		}
	}
}

func TestCommandInterruptLogCorrelation(t *testing.T) {
	logPath, child := captureCommandLog(t)
	if !child {
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	c := NewController("", "")
	var id string
	req := &ExecuteCodeRequest{
		Language: BackgroundCommand,
		Argv:     []string{executable, "-test.run=^TestCommandLogProcess$"},
		Timeout:  10 * time.Second,
		Envs: map[string]string{
			"EXECD_COMMAND_LOG_HELPER": "1",
			"EXECD_COMMAND_LOG_EXIT":   "0",
			"EXECD_COMMAND_LOG_GATE":   filepath.Join(t.TempDir(), "never-released"),
		},
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(session string) { id = session },
			OnExecuteComplete: func(time.Duration) {},
		},
	}
	req.SetDefaultHooks()
	runErr := c.Execute(req)
	var interruptErr error
	if runErr == nil {
		interruptErr = c.Interrupt(id)
		require.Eventually(t, func() bool {
			status, err := c.GetCommandStatus(id)
			return err == nil && !status.Running
		}, 12*time.Second, 10*time.Millisecond)
	}
	require.NoError(t, runErr)
	require.NoError(t, interruptErr)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "interrupt: command_id="+id+" terminating process")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "interrupt:") {
			require.Contains(t, line, "command_id="+id)
			require.Contains(t, line, "pid=")
		}
	}
	removeCommandOutputFiles(c.combinedOutputFileName(id))
}

func TestCommandLogCorrelationUnknownWaitStatus(t *testing.T) {
	logPath, child := captureCommandLog(t)
	if !child {
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, "-test.run=^TestCommandLogProcess$")
	cmd.Env = append(os.Environ(), "EXECD_COMMAND_LOG_HELPER=1", "EXECD_COMMAND_LOG_EXIT=0")
	startedAt := time.Now()
	require.NoError(t, cmd.Start())
	// A real independent waiter consumes the status. Cmd.Wait then reports a
	// genuine wait failure without an observed ProcessState of its own.
	state, err := cmd.Process.Wait()
	require.NoError(t, err)
	require.Zero(t, state.ExitCode())
	waitErr := cmd.Wait()
	require.Error(t, waitErr)
	require.Nil(t, cmd.ProcessState)
	logCommandRunFailure("independently-reaped-command", "foreground", cmd, startedAt, waitErr)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "command: command_id=independently-reaped-command mode=foreground run failed")
	require.Contains(t, string(data), "exit_code=unknown elapsed_ms=")
	require.NotContains(t, string(data), "exit_code=1 elapsed_ms=")
}
