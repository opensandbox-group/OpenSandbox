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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRunBackgroundCommandClosesHTTPStream(t *testing.T) {
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	t.Cleanup(func() {
		codeRunner, flag.ApiGracefulShutdownTimeout = previousRunner, previousTimeout
	})
	flag.ApiGracefulShutdownTimeout = 2 * time.Second
	codeRunner = &fakeCodeRunner{execute: func(request *runtime.ExecuteCodeRequest) error {
		request.Hooks.OnExecuteInit("exec-test")
		request.Hooks.OnExecuteComplete(time.Millisecond)
		return nil
	}}
	router := gin.New()
	router.POST("/command", func(ctx *gin.Context) { NewCodeInterpretingController(ctx).RunCommand() })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second // EOF must precede the old fixed sleep.
	response, err := client.Post(server.URL+"/command", "application/json",
		strings.NewReader(`{"command":"echo test","background":true}`))
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(data), "execution_complete")
}

func TestRunCommandReturnsPromptlyWhenCanceledAfterExecute(t *testing.T) {
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	t.Cleanup(func() {
		codeRunner, flag.ApiGracefulShutdownTimeout = previousRunner, previousTimeout
	})
	flag.ApiGracefulShutdownTimeout = 2 * time.Second
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codeRunner = &fakeCodeRunner{execute: func(_ *runtime.ExecuteCodeRequest) error {
		cancel()
		return nil
	}}
	ctx, _ := newTestContext(http.MethodPost, "/command", []byte(`{"command":"echo test"}`))
	ctx.Request = ctx.Request.WithContext(requestCtx)
	start := time.Now()
	NewCodeInterpretingController(ctx).RunCommand()
	require.Less(t, time.Since(start), time.Second)
}

func TestRunCommandRealProcessDrainsOutputBeforeEOF(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	t.Cleanup(func() {
		codeRunner, flag.ApiGracefulShutdownTimeout = previousRunner, previousTimeout
	})
	codeRunner = runtime.NewController("", "")
	flag.ApiGracefulShutdownTimeout = 5 * time.Second
	router := gin.New()
	router.POST("/command", func(ctx *gin.Context) { NewCodeInterpretingController(ctx).RunCommand() })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 4 * time.Second

	for _, exitCode := range []int{0, 7} {
		t.Run(fmt.Sprintf("exit-%d", exitCode), func(t *testing.T) {
			// More than 2 MiB across both streams, followed by unterminated UTF-8
			// tails. This exercises the real process, log readers and HTTP framing.
			command := `i=0; while [ "$i" -lt 512 ]; do printf 'out-%04d-%02048d\n' "$i" 0; printf 'err-%04d-%02048d\n' "$i" 0 >&2; i=$((i+1)); done; printf 'stdout-tail-你好'; printf 'stderr-tail-世界' >&2; exit ` + fmt.Sprint(exitCode)
			body, err := json.Marshal(model.RunCommandRequest{Command: command, Cwd: t.TempDir()})
			require.NoError(t, err)
			response, err := client.Post(server.URL+"/command", "application/json", strings.NewReader(string(body)))
			require.NoError(t, err)
			defer response.Body.Close()
			// Delay reading so output can accumulate before the client drains it.
			time.Sleep(100 * time.Millisecond)
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			var stdout, stderr []string
			terminalCount := 0
			for _, frame := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
				var event model.ServerStreamEvent
				require.NoError(t, json.Unmarshal([]byte(frame), &event))
				switch event.Type {
				case model.StreamEventTypeStdout, model.StreamEventTypeStderr:
					require.Zero(t, terminalCount, "output must precede terminal event")
					if event.Type == model.StreamEventTypeStdout {
						stdout = append(stdout, event.Text)
					} else {
						stderr = append(stderr, event.Text)
					}
				case model.StreamEventTypeComplete:
					require.Zero(t, exitCode)
					terminalCount++
				case model.StreamEventTypeError:
					require.Equal(t, 7, exitCode)
					require.NotNil(t, event.Error)
					require.Equal(t, "7", event.Error.EValue)
					terminalCount++
				}
			}
			require.Equal(t, 1, terminalCount)
			require.Len(t, stdout, 513)
			require.Len(t, stderr, 513)
			for i := 0; i < 512; i++ {
				require.Equal(t, fmt.Sprintf("out-%04d-%02048d", i, 0), stdout[i])
				require.Equal(t, fmt.Sprintf("err-%04d-%02048d", i, 0), stderr[i])
			}
			require.Equal(t, "stdout-tail-你好", stdout[512])
			require.Equal(t, "stderr-tail-世界", stderr[512])
		})
	}
}

func TestBuildExecuteCommandRequestForwardsEnvs(t *testing.T) {
	ctrl := &CodeInterpretingController{}
	envs := map[string]string{"FOO": "bar", "BAZ": "qux"}
	req := model.RunCommandRequest{
		Command: "echo hi",
		Cwd:     "/tmp",
		Envs:    envs,
	}

	execReq := ctrl.buildExecuteCommandRequest(req)

	require.Equal(t, runtime.Command, execReq.Language)
	require.True(t, reflect.DeepEqual(execReq.Envs, envs), "expected envs to be forwarded")
	require.Equal(t, "/tmp", execReq.Cwd)
}

func TestBuildExecuteCommandRequestForwardsEnvsBackground(t *testing.T) {
	ctrl := &CodeInterpretingController{}
	envs := map[string]string{"FOO": "bar"}
	req := model.RunCommandRequest{
		Argv:       []string{"tool", "", "$HOME"},
		Background: true,
		Envs:       envs,
	}

	execReq := ctrl.buildExecuteCommandRequest(req)

	require.Equal(t, runtime.BackgroundCommand, execReq.Language)
	require.Equal(t, req.Argv, execReq.Argv)
	require.Empty(t, execReq.Code)
	require.True(t, reflect.DeepEqual(execReq.Envs, envs), "expected envs to be forwarded")
}

func setupCommandController(method, path string) (*CodeInterpretingController, *httptest.ResponseRecorder) {
	ctx, w := newTestContext(method, path, nil)
	ctrl := NewCodeInterpretingController(ctx)
	return ctrl, w
}

func TestGetCommandStatus_MissingID(t *testing.T) {
	ctrl, w := setupCommandController(http.MethodGet, "/command/status/")

	ctrl.GetCommandStatus()

	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeInvalidRequest, resp.Code)
	require.Equal(t, "missing command execution id", resp.Message)
}

func TestGetBackgroundCommandOutput_MissingID(t *testing.T) {
	ctrl, w := setupCommandController(http.MethodGet, "/command/logs/")

	ctrl.GetBackgroundCommandOutput()

	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeMissingQuery, resp.Code)
	require.Equal(t, "missing command execution id", resp.Message)
}
