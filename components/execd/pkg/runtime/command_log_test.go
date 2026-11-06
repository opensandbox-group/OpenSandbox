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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
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

func captureCommandLog(t *testing.T, levels ...int) (string, bool) {
	t.Helper()
	const testEnv = "EXECD_COMMAND_LOG_CAPTURE_TEST"
	const pathEnv = "EXECD_COMMAND_LOG_CAPTURE_PATH"
	name, path := os.Getenv(testEnv), os.Getenv(pathEnv)
	if name != "" || path != "" {
		require.Equal(t, t.Name(), name, "capture subprocess must run only its designated test")
		require.NotEmpty(t, path)
		t.Setenv("EXECD_LOG_FILE", path)
		level := 7
		if len(levels) != 0 {
			level = levels[0]
		}
		log.Init(level)
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

func commandLogRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var records []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}
	return records
}

func commandLogEvents(records []map[string]any, id, message string) []map[string]any {
	var events []map[string]any
	for _, record := range records {
		if record["command_id"] == id && record["msg"] == message {
			events = append(events, record)
		}
	}
	return events
}

func TestCommandLogCorrelationConcurrent(t *testing.T) {
	path, child := captureCommandLog(t, 6)
	if !child {
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	c := NewController("", "")
	var wg sync.WaitGroup
	for _, background := range []bool{false, true} {
		for _, code := range []int{0, 7} {
			wg.Add(1)
			go func(background bool, code int) {
				defer wg.Done()
				language := Command
				if background {
					language = BackgroundCommand
				}
				var allocated, initialized string
				request := &ExecuteCodeRequest{
					Language: language,
					Argv:     []string{executable, "-test.run=^TestCommandLogProcess$"},
					Timeout:  5 * time.Second,
					Envs:     map[string]string{"EXECD_COMMAND_LOG_HELPER": "1", "EXECD_COMMAND_LOG_EXIT": strconv.Itoa(code)},
					Hooks: ExecuteResultHook{
						OnCommandAllocated: func(id string) { allocated = id },
						OnExecuteInit:      func(id string) { initialized = id },
						OnExecuteError:     func(*execute.ErrorOutput) {},
					},
				}
				request.SetDefaultHooks()
				require.NoError(t, c.Execute(request))
				require.NotEmpty(t, allocated)
				require.Equal(t, allocated, initialized)
				if background {
					require.Eventually(t, func() bool { snapshot := c.commandSnapshot(allocated); return snapshot != nil && !snapshot.running }, 5*time.Second, 10*time.Millisecond)
				}
			}(background, code)
		}
	}
	wg.Wait()
	received, failed := 0, 0
	for _, record := range commandLogRecords(t, path) {
		require.Len(t, record, 4)
		require.NotEmpty(t, record["command_id"])
		message := record["msg"].(string)
		switch {
		case strings.HasPrefix(message, "command: received "):
			received++
			require.Equal(t, "info", record["level"])
		case strings.HasPrefix(message, "command: run failed: "):
			failed++
			require.Equal(t, "error", record["level"])
		default:
			t.Fatalf("unexpected event: %v", record)
		}
		require.NotContains(t, message, "private-command-stdout-payload")
		require.NotContains(t, message, "private-command-stderr-payload")
	}
	require.Equal(t, 4, received)
	require.Equal(t, 2, failed)
}

func TestCommandFailureCompatibility(t *testing.T) {
	path, child := captureCommandLog(t, 6)
	if !child {
		return
	}
	for _, preparation := range []bool{false, true} {
		c := NewController("", "")
		var allocated, initialized string
		var failure *execute.ErrorOutput
		request := &ExecuteCodeRequest{Language: Command, Argv: []string{filepath.Join(t.TempDir(), "missing-executable")}, Timeout: time.Second, Hooks: ExecuteResultHook{
			OnCommandAllocated: func(id string) { allocated = id },
			OnExecuteInit:      func(id string) { initialized = id },
			OnExecuteError:     func(err *execute.ErrorOutput) { failure = err },
		}}
		if preparation {
			t.Setenv("EXECD_COMPAT_UNDEFINED_CWD", "")
			require.NoError(t, os.Unsetenv("EXECD_COMPAT_UNDEFINED_CWD"))
			request.Cwd = "${EXECD_COMPAT_UNDEFINED_CWD}"
		}
		request.SetDefaultHooks()
		err := c.Execute(request)
		require.NotEmpty(t, allocated)
		if preparation {
			require.Error(t, err)
			if goruntime.GOOS == "windows" {
				require.Equal(t, allocated, initialized)
			} else {
				require.Empty(t, initialized)
			}
		} else {
			require.NoError(t, err)
			require.Equal(t, allocated, initialized)
			require.NotNil(t, failure)
		}
	}
	starts := 0
	for _, record := range commandLogRecords(t, path) {
		require.Len(t, record, 4)
		msg := record["msg"].(string)
		if strings.HasPrefix(msg, "command: start failed: ") {
			starts++
			require.Equal(t, "error", record["level"])
		} else {
			require.True(t, strings.HasPrefix(msg, "command: received "))
			require.Equal(t, "info", record["level"])
		}
	}
	require.Equal(t, 1, starts)
}

func TestCommandOutputCleanupLogCompatibility(t *testing.T) {
	path, child := captureCommandLog(t, 6)
	if !child {
		return
	}
	directory := filepath.Join(t.TempDir(), "nonempty-output")
	require.NoError(t, os.Mkdir(directory, 0700))
	payload := filepath.Join(directory, "retained")
	require.NoError(t, os.WriteFile(payload, []byte("keep"), 0600))
	var expected []string
	if err := truncateCommandOutputFile(directory); err != nil {
		expected = append(expected, fmt.Sprintf("command output: truncate %s: %v", directory, err))
	}
	removeErr := os.Remove(directory)
	require.Error(t, removeErr, "a nonempty directory must survive cleanup")
	expected = append(expected, fmt.Sprintf("command output: remove %s: %v", directory, removeErr))

	removeCommandOutputFiles("", directory, directory)
	removeCommandOutputFilesWithID("owned-cleanup", "", directory, directory)
	c := NewController("", "")
	finished := time.Now().Add(-time.Hour)
	c.storeCommandKernel("finished-cleanup", &commandKernel{
		stdoutPath: directory, stderrPath: directory, finishedAt: &finished,
	})
	c.cleanupFinishedCommands(time.Now())
	require.Nil(t, c.getCommandKernel("finished-cleanup"))
	records := commandLogRecords(t, path)
	require.Len(t, records, 3*len(expected), "each cleanup must globally deduplicate paths")
	for group, id := range []string{"", "owned-cleanup", "finished-cleanup"} {
		for index, message := range expected {
			record := records[group*len(expected)+index]
			require.Equal(t, "warn", record["level"])
			require.Equal(t, message, record["msg"])
			if id == "" {
				require.Len(t, record, 3)
				require.NotContains(t, record, "command_id")
			} else {
				require.Len(t, record, 4)
				require.Equal(t, id, record["command_id"])
			}
		}
	}
	data, err := os.ReadFile(payload)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
}
