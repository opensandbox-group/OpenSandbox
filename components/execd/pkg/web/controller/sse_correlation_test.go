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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func captureSSECorrelationLog(t *testing.T) string {
	t.Helper()
	t.Cleanup(func() { log.Init(6) })
	path := filepath.Join(t.TempDir(), "execd.log")
	t.Setenv("EXECD_LOG_FILE", path)
	log.Init(6)
	return path
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

func TestSSECommandCorrelationBeforeInit(t *testing.T) {
	logPath := captureSSECorrelationLog(t)
	ctx, recorder := newTestContext(http.MethodPost, "/command", nil)
	c := NewCodeInterpretingController(ctx)
	pingCtx, cancel := context.WithCancel(context.Background())
	hooks, stop := c.setServerEventsHandler(pingCtx)
	defer func() { cancel(); stop() }()

	hooks.OnCommandAllocated("early-command", true)
	require.False(t, ctx.Writer.Written())
	require.Empty(t, ctx.Writer.Header().Get("Content-Type"), "binding an ID must not commit headers")
	hooks.OnExecuteStdout("private-successful-stream-output")
	hooks.OnExecuteStatus("early-status")
	hooks.OnExecuteError(&execute.ErrorOutput{EName: "EarlyError", EValue: "early-error"})
	hooks.OnExecuteInit("early-command")
	hooks.OnExecuteComplete(time.Millisecond)
	cancel()
	stop()

	events := correlationEvents(t, recorder)
	require.GreaterOrEqual(t, len(events), 5)
	require.Equal(t, model.StreamEventTypeStdout, events[0].Type)
	require.Equal(t, "private-successful-stream-output", events[0].Text)
	require.Equal(t, model.StreamEventTypeStatus, events[1].Type)
	require.Equal(t, model.StreamEventTypeError, events[2].Type)
	require.Equal(t, model.StreamEventTypeInit, events[3].Type, "init must not be moved ahead of early output")
	require.Equal(t, "early-command", events[3].Text)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	text := string(data)
	for _, handler := range []string{"OnExecuteStatus", "OnExecuteError", "OnExecuteInit", "OnExecuteComplete"} {
		require.Contains(t, text, "sse: command_id=early-command "+handler+" write")
	}
	require.Contains(t, text, "acknowledgement=background_startup")
	require.NotContains(t, text, "private-successful-stream-output", "successful output writes must remain quiet")
}

func TestSSECommandCorrelationWriteFailureAndDisconnect(t *testing.T) {
	logPath := captureSSECorrelationLog(t)
	for _, failure := range []string{"write", "short_write", "disconnect"} {
		t.Run(failure, func(t *testing.T) {
			ctx, _ := newTestContext(http.MethodPost, "/command", nil)
			requestCtx, cancel := context.WithCancel(ctx.Request.Context())
			defer cancel()
			ctx.Request = ctx.Request.WithContext(requestCtx)
			if failure != "disconnect" {
				ctx.Writer = &correlationFailureWriter{ResponseWriter: ctx.Writer, short: failure == "short_write"}
			}
			c := NewCodeInterpretingController(ctx)
			hooks, stop := c.setServerEventsHandler(requestCtx)
			defer stop()
			id := "early-" + failure
			hooks.OnCommandAllocated(id, false)
			require.False(t, ctx.Writer.Written())
			if failure == "disconnect" {
				cancel()
			}
			// Unix tails can call this before OnExecuteInit. No init is needed
			// for either failure branch to know the runtime's allocated ID.
			hooks.OnExecuteStdout("early-output")
			data, err := os.ReadFile(logPath)
			require.NoError(t, err)
			prefix := "sse: command_id=" + id + " OnExecuteStdout "
			if failure == "disconnect" {
				require.Contains(t, string(data), prefix+"client disconnected")
				require.False(t, ctx.Writer.Written())
				require.Empty(t, ctx.Writer.Header().Get("Content-Type"))
			} else {
				require.Contains(t, string(data), prefix+"write")
				message := "correlation transport failure"
				if failure == "short_write" {
					message = "short write"
				}
				require.Contains(t, string(data), message)
			}
		})
	}
}

func TestSSENonCommandCorrelationIsSessionScoped(t *testing.T) {
	logPath := captureSSECorrelationLog(t)
	ctx, _ := newTestContext(http.MethodPost, "/code", nil)
	c := NewCodeInterpretingController(ctx)
	pingCtx, cancel := context.WithCancel(context.Background())
	hooks, stop := c.setServerEventsHandler(pingCtx)
	hooks.OnExecuteInit("jupyter-session")
	hooks.OnExecuteStatus("busy")
	hooks.OnExecuteComplete(time.Millisecond)
	cancel()
	stop()
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "sse: session_id=jupyter-session OnExecuteInit write")
	require.Contains(t, string(data), "sse: session_id=jupyter-session OnExecuteStatus write")
	require.Contains(t, string(data), "sse: session_id=jupyter-session OnExecuteComplete write")
	require.NotContains(t, string(data), "command_id=")
	require.NotContains(t, string(data), "acknowledgement=background_startup")
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

func TestRunCommandLogCorrelationConcurrent(t *testing.T) {
	logPath := captureSSECorrelationLog(t)
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	c := runtime.NewController("", "")
	codeRunner, flag.ApiGracefulShutdownTimeout = c, 0
	t.Cleanup(func() { codeRunner = previousRunner; flag.ApiGracefulShutdownTimeout = previousTimeout })
	configureSseLegacyCompat(t, false)
	executable, err := os.Executable()
	require.NoError(t, err)
	type commandCase struct {
		name       string
		background bool
		exitCode   int
		launchFail bool
	}
	cases := []commandCase{
		{name: "foreground-success"},
		{name: "foreground-failure", exitCode: 7},
		{name: "background-success", background: true},
		{name: "background-failure", background: true, exitCode: 9},
		{name: "foreground-launch-failure", launchFail: true},
		{name: "background-launch-failure", background: true, launchFail: true},
	}
	controllers := make([]*CodeInterpretingController, len(cases))
	recorders := make([]*httptest.ResponseRecorder, len(cases))
	for i, tc := range cases {
		argv := []string{executable, "-test.run=^TestSSELogProcess$"}
		if tc.launchFail {
			argv = []string{filepath.Join(t.TempDir(), "missing-command")}
		}
		body, err := json.Marshal(model.RunCommandRequest{
			Argv: argv, Background: tc.background, TimeoutMs: 10000,
			Envs: map[string]string{
				"EXECD_SSE_LOG_HELPER":    "1",
				"EXECD_SSE_LOG_EXIT":      strconv.Itoa(tc.exitCode),
				"PRIVATE_CORRELATION_ENV": "private-sse-environment-value",
			},
		})
		require.NoError(t, err)
		ctx, recorder := newTestContext(http.MethodPost, "/command", body)
		controllers[i], recorders[i] = NewCodeInterpretingController(ctx), recorder
	}
	var wg sync.WaitGroup
	for _, controller := range controllers {
		wg.Add(1)
		go func(c *CodeInterpretingController) {
			defer wg.Done()
			c.RunCommand()
		}(controller)
	}
	wg.Wait()

	// Extract IDs before assertions so all background waiters finish before
	// the capture logger can be restored, even when a later assertion fails.
	ids := make([]string, len(cases))
	for i, recorder := range recorders {
		for _, line := range strings.Split(recorder.Body.String(), "\n") {
			var event model.ServerStreamEvent
			if json.Unmarshal([]byte(line), &event) == nil && event.Type == model.StreamEventTypeInit {
				ids[i] = event.Text
			}
		}
	}
	for i, tc := range cases {
		if tc.background && !tc.launchFail && ids[i] != "" {
			require.Eventually(t, func() bool {
				status, err := c.GetCommandStatus(ids[i])
				return err == nil && !status.Running
			}, 12*time.Second, 10*time.Millisecond)
		}
	}
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	text := string(data)
	seen := make(map[string]bool)
	for i, tc := range cases {
		id := ids[i]
		require.NotEmpty(t, id, tc.name)
		require.False(t, seen[id], "requests must not reuse command IDs")
		seen[id] = true
		mode := "foreground"
		if tc.background {
			mode = "background"
		}
		prefix := "command: command_id=" + id + " mode=" + mode
		require.Contains(t, text, prefix+" received", tc.name)
		ssePrefix := "sse: command_id=" + id + " "
		require.Contains(t, text, ssePrefix+"OnExecuteInit write", tc.name)
		terminal := model.StreamEventTypeComplete
		if tc.launchFail || !tc.background && tc.exitCode != 0 {
			terminal = model.StreamEventTypeError
			require.Contains(t, text, ssePrefix+"OnExecuteError write", tc.name)
			require.NotContains(t, text, ssePrefix+"OnExecuteComplete write", tc.name)
		} else {
			require.Contains(t, text, ssePrefix+"OnExecuteComplete write", tc.name)
			require.NotContains(t, text, ssePrefix+"OnExecuteError write", tc.name)
		}
		found := false
		for _, event := range correlationEvents(t, recorders[i]) {
			if event.Type == terminal {
				found = true
				if tc.background || terminal == model.StreamEventTypeError {
					require.Nil(t, event.ExitCode)
				} else {
					require.NotNil(t, event.ExitCode)
					require.Zero(t, *event.ExitCode)
				}
			}
		}
		require.True(t, found, tc.name)
		if tc.launchFail {
			require.Contains(t, text, prefix+" start failed", tc.name)
			require.NotContains(t, text, prefix+" started", tc.name)
		} else if tc.exitCode == 0 {
			require.Contains(t, text, prefix+" exited", tc.name)
			require.NotContains(t, text, prefix+" run failed", tc.name)
		} else {
			require.Contains(t, text, prefix+" run failed", tc.name)
			require.NotContains(t, text, prefix+" exited", tc.name)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, ssePrefix+"OnExecuteComplete write") {
				require.Equal(t, tc.background, strings.Contains(line, "acknowledgement=background_startup"))
			}
		}
	}
	require.NotContains(t, text, "private-sse-stdout-payload")
	require.NotContains(t, text, "private-sse-stderr-payload")
	require.NotContains(t, text, "private-sse-environment-value")
}
