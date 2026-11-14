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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func captureSSECorrelationLog(t *testing.T, levels ...int) (string, bool) {
	t.Helper()
	const testEnv = "EXECD_SSE_LOG_CAPTURE_TEST"
	const pathEnv = "EXECD_SSE_LOG_CAPTURE_PATH"
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

	// Keep the capture sink in a child process so its handles are closed
	// before the parent's TempDir cleanup, including on native Windows.
	path = filepath.Join(t.TempDir(), "execd.log")
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), testEnv+"="+t.Name(), pathEnv+"="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "capture subprocess failed:\n%s", output)
	// Exercise removal of the real sink after exit to catch Windows sharing
	// violations without relying on logger replacement or garbage collection.
	require.NoError(t, os.Remove(path), "capture sink must be removable after child exit")
	return "", false
}

func correlationLogRecords(t *testing.T, path string) []map[string]any {
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

func correlationLogEntries(records []map[string]any, idKey, id, handler, message string) []map[string]any {
	var entries []map[string]any
	for _, record := range records {
		if record[idKey] == id && (handler == "" || record["handler"] == handler) && record["msg"] == message {
			entries = append(entries, record)
		}
	}
	return entries
}

// Only the transport is faulted; real SSE serialization and logging still run.
type correlationFailureWriter struct {
	gin.ResponseWriter
	short bool
}

func (w *correlationFailureWriter) Write(payload []byte) (int, error) {
	if w.short {
		return len(payload) - 1, nil
	}
	return 0, errors.New("correlation transport failure")
}

func correlationEvents(t *testing.T, recorder *httptest.ResponseRecorder) []model.ServerStreamEvent {
	t.Helper()
	var events []model.ServerStreamEvent
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event model.ServerStreamEvent
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(line), &fields))
		require.NotContains(t, fields, "command_id", "correlation must not change the SSE schema")
		require.NotContains(t, fields, "session_id")
		events = append(events, event)
	}
	return events
}

func TestSSELogProcess(t *testing.T) {
	if os.Getenv("EXECD_SSE_LOG_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "private-sse-stdout-payload")
	fmt.Fprintln(os.Stderr, "private-sse-stderr-payload")
	time.Sleep(100 * time.Millisecond)
	code, err := strconv.Atoi(os.Getenv("EXECD_SSE_LOG_EXIT"))
	if err != nil {
		os.Exit(90)
	}
	os.Exit(code)
}

func TestSSECommandCompatibility(t *testing.T) {
	path, child := captureSSECorrelationLog(t, 6)
	if !child {
		return
	}
	for _, failure := range []string{"", "write", "short", "disconnect", "noncommand"} {
		ctx, _ := newTestContext(http.MethodPost, "/command", nil)
		requestCtx, cancel := context.WithCancel(ctx.Request.Context())
		ctx.Request = ctx.Request.WithContext(requestCtx)
		if failure == "write" || failure == "short" {
			ctx.Writer = &correlationFailureWriter{ResponseWriter: ctx.Writer, short: failure == "short"}
		}
		c := NewCodeInterpretingController(ctx)
		pingCtx, stopPing := context.WithCancel(context.Background())
		hooks, stop := c.setServerEventsHandler(pingCtx)
		id := "command-" + failure
		if failure != "noncommand" {
			hooks.OnCommandAllocated(id)
		}
		require.False(t, ctx.Writer.Written())
		require.Empty(t, ctx.Writer.Header().Get("Content-Type"))
		if failure == "disconnect" {
			cancel()
		}
		hooks.OnExecuteStdout("private-output")
		hooks.OnExecuteInit(id)
		hooks.OnExecuteComplete(0)
		stopPing()
		stop()
		cancel()
	}
	records := correlationLogRecords(t, path)
	require.NotEmpty(t, records)
	for _, record := range records {
		msg := record["msg"].(string)
		id, command := record["command_id"]
		if command {
			require.Len(t, record, 4)
		} else {
			require.Len(t, record, 3)
		}
		switch {
		case id == "command-disconnect":
			require.Equal(t, "error", record["level"])
			require.Contains(t, []string{"sse: OnExecuteStdout client disconnected", "sse: OnExecuteInit client disconnected", "sse: OnExecuteComplete client disconnected", "sse: Ping client disconnected"}, msg)
			require.NotContains(t, msg, "private-output")
		case id == "command-write" || id == "command-short":
			require.Equal(t, "error", record["level"])
			// The existing keepalive can race cleanup after init; failed ping
			// writes are diagnostics even though successful pings stay quiet.
			require.True(t, strings.HasPrefix(msg, "sse: OnExecute") || strings.HasPrefix(msg, "sse: Ping write "), msg)
			suffix := ": correlation transport failure"
			if id == "command-short" {
				suffix = ": short write"
			}
			require.True(t, strings.HasSuffix(msg, suffix), msg)
		default:
			require.Equal(t, "info", record["level"])
			expectedID := "command-"
			if !command {
				expectedID = "command-noncommand"
			}
			require.Contains(t, []string{"sse: OnExecuteInit write type=init text=" + expectedID, "sse: OnExecuteComplete write type=execution_complete"}, msg)
		}
	}
}

func TestSSEConcurrentRuntimeCompatibility(t *testing.T) {
	path, child := captureSSECorrelationLog(t, 6)
	if !child {
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	controller := runtime.NewController("", "")
	var wg sync.WaitGroup
	for _, background := range []bool{false, true} {
		for _, code := range []int{0, 7} {
			wg.Add(1)
			go func(background bool, code int) {
				defer wg.Done()
				ctx, recorder := newTestContext(http.MethodPost, "/command", nil)
				c := NewCodeInterpretingController(ctx)
				pingCtx, cancel := context.WithCancel(context.Background())
				hooks, stop := c.setServerEventsHandler(pingCtx)
				language := runtime.Command
				if background {
					language = runtime.BackgroundCommand
				}
				request := &runtime.ExecuteCodeRequest{Language: language, Argv: []string{executable, "-test.run=^TestSSELogProcess$"}, Timeout: 5 * time.Second, Envs: map[string]string{"EXECD_SSE_LOG_HELPER": "1", "EXECD_SSE_LOG_EXIT": strconv.Itoa(code)}, Hooks: hooks}
				request.SetDefaultHooks()
				require.NoError(t, controller.Execute(request))
				cancel()
				stop()
				var id string
				for _, event := range correlationEvents(t, recorder) {
					if event.Type == model.StreamEventTypeInit {
						id = event.Text
					}
				}
				require.NotEmpty(t, id)
				require.Equal(t, id, c.commandID)
				if background {
					require.Eventually(t, func() bool { status, err := controller.GetCommandStatus(id); return err == nil && !status.Running }, 5*time.Second, 10*time.Millisecond)
				}
			}(background, code)
		}
	}
	wg.Wait()
	for _, record := range correlationLogRecords(t, path) {
		require.Len(t, record, 4)
		require.NotEmpty(t, record["command_id"])
		message := record["msg"].(string)
		require.True(t, strings.HasPrefix(message, "command: ") || strings.HasPrefix(message, "sse: "), message)
		require.NotContains(t, message, "private-sse-stdout-payload")
		require.NotContains(t, message, "private-sse-stderr-payload")
	}
}
