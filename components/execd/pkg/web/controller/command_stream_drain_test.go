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

package controller

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestForegroundCommandStreamDrainsOutputWithoutTailDelay(t *testing.T) {
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	codeRunner = runtime.NewController("", "")
	log.Info("starting foreground command stream test")
	const drainTimeout = 300 * time.Millisecond
	flag.ApiGracefulShutdownTimeout = drainTimeout
	t.Cleanup(func() { codeRunner, flag.ApiGracefulShutdownTimeout = previousRunner, previousTimeout })

	router := gin.New()
	router.POST("/command", func(c *gin.Context) { NewCodeInterpretingController(c).RunCommand() })
	router.GET("/command/status/:id", func(c *gin.Context) { NewCodeInterpretingController(c).GetCommandStatus() })
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 10 * time.Second

	for _, tc := range []struct {
		name, command, stdout, stderr string
		exitCode                      int
	}{
		{name: "short", command: "printf short", stdout: "short"},
		{name: "long", command: "printf 'first\\n'; sleep 1; printf stdout-tail; printf stderr-tail >&2", stdout: "first\nstdout-tail", stderr: "stderr-tail"},
		{name: "failure", command: "printf failure-out; printf failure-err >&2; exit 7", stdout: "failure-out", stderr: "failure-err", exitCode: 7},
		{name: "concurrent", command: "printf concurrent-out; printf concurrent-err >&2", stdout: "concurrent-out", stderr: "concurrent-err"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(map[string]string{"command": tc.command})
			require.NoError(t, err)
			resp, err := client.Post(server.URL+"/command", "application/json", strings.NewReader(string(body)))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			reader := bufio.NewReader(resp.Body)
			var stdout, stderr strings.Builder
			var id string
			var terminal time.Time
			for {
				line, err := reader.ReadBytes('\n')
				if err == io.EOF {
					require.Empty(t, line)
					break
				}
				require.NoError(t, err)
				if len(line) == 1 {
					continue
				}
				var event model.ServerStreamEvent
				require.NoError(t, json.Unmarshal(line, &event))
				switch event.Type {
				case model.StreamEventTypeInit:
					id = event.Text
				case model.StreamEventTypeStdout:
					require.True(t, terminal.IsZero(), "stdout after terminal event")
					stdout.WriteString(event.Text)
				case model.StreamEventTypeStderr:
					require.True(t, terminal.IsZero(), "stderr after terminal event")
					stderr.WriteString(event.Text)
				case model.StreamEventTypeComplete:
					require.True(t, terminal.IsZero(), "duplicate terminal event")
					require.Zero(t, tc.exitCode)
					require.NotNil(t, event.ExitCode)
					require.Zero(t, *event.ExitCode)
					terminal = time.Now()
				case model.StreamEventTypeError:
					require.True(t, terminal.IsZero(), "duplicate terminal event")
					require.NotZero(t, tc.exitCode)
					require.NotNil(t, event.Error)
					require.NotNil(t, event.Error.ExitCode)
					require.Equal(t, tc.exitCode, *event.Error.ExitCode)
					terminal = time.Now()
				}
			}
			require.Equal(t, tc.stdout, stdout.String())
			require.Equal(t, tc.stderr, stderr.String())
			require.False(t, terminal.IsZero(), "missing terminal event")
			elapsed := time.Since(terminal)
			t.Logf("terminal event to clean EOF: %s", elapsed)
			require.Less(t, elapsed, drainTimeout)
			require.NotEmpty(t, id)

			statusResp, err := client.Get(server.URL + "/command/status/" + id)
			require.NoError(t, err)
			defer statusResp.Body.Close()
			require.Equal(t, http.StatusOK, statusResp.StatusCode)
			var status model.CommandStatusResponse
			require.NoError(t, json.NewDecoder(statusResp.Body).Decode(&status))
			require.False(t, status.Running)
			require.NotNil(t, status.ExitCode)
			require.Equal(t, tc.exitCode, *status.ExitCode)
			require.NotNil(t, status.FinishedAt)
		})
	}
}

func TestCommandRetainsDrainWithoutForegroundCompletion(t *testing.T) {
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	const drainTimeout = 25 * time.Millisecond
	flag.ApiGracefulShutdownTimeout = drainTimeout
	t.Cleanup(func() { codeRunner, flag.ApiGracefulShutdownTimeout = previousRunner, previousTimeout })

	t.Run("background startup", func(t *testing.T) {
		codeRunner = &fakeCodeRunner{execute: func(request *runtime.ExecuteCodeRequest) error {
			request.Hooks.OnExecuteComplete(time.Millisecond)
			return nil
		}}
		ctx, rec := newTestContext(http.MethodPost, "/command", []byte(`{"command":"printf started","background":true}`))
		start := time.Now()
		NewCodeInterpretingController(ctx).RunCommand()
		require.GreaterOrEqual(t, time.Since(start), drainTimeout)
		require.Contains(t, rec.Body.String(), `"type":"execution_complete"`)
	})

	t.Run("foreground without terminal signal", func(t *testing.T) {
		codeRunner = &fakeCodeRunner{}
		ctx, _ := newTestContext(http.MethodPost, "/command", []byte(`{"command":"printf pending"}`))
		start := time.Now()
		NewCodeInterpretingController(ctx).RunCommand()
		require.GreaterOrEqual(t, time.Since(start), 2*drainTimeout)
	})
}
