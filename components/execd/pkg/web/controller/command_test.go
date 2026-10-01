// Copyright 2025 The OpenSandbox Authors
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

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

// runCommandWithRequestContext runs RunCommand for body on a request whose
// context the caller cancels, and returns a channel closed when it returns.
func runCommandWithRequestContext(t *testing.T, reqCtx context.Context, body string) <-chan struct{} {
	t.Helper()
	previousRunner := codeRunner
	codeRunner = runtime.NewController("", "")
	t.Cleanup(func() { codeRunner = previousRunner })

	ctx, _ := newTestContext(http.MethodPost, "/command", []byte(body))
	ctx.Request = ctx.Request.WithContext(reqCtx)
	ctrl := NewCodeInterpretingController(ctx)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		ctrl.RunCommand()
	}()
	return returned
}

func TestRunCommand_ClientDisconnectKillsForegroundCommand(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "done")

	reqCtx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	body := fmt.Sprintf(`{"command":"touch '%s'; sleep 2; touch '%s'"}`, started, marker)
	returned := runCommandWithRequestContext(t, reqCtx, body)

	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "command did not start")
	disconnect()

	select {
	case <-returned:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("RunCommand did not return after the client disconnected")
	}
	// Give a surviving command time to write the marker.
	time.Sleep(2500 * time.Millisecond)
	_, err := os.Stat(marker)
	require.True(t, os.IsNotExist(err), "command kept running after the client disconnected")
}

func TestRunCommand_BackgroundCommandOutlivesRequest(t *testing.T) {
	requireBash(t)
	marker := filepath.Join(t.TempDir(), "done")

	reqCtx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	body := fmt.Sprintf(`{"command":"sleep 1; touch '%s'","background":true}`, marker)
	returned := runCommandWithRequestContext(t, reqCtx, body)

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCommand did not return for a background command")
	}
	disconnect()

	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 5*time.Second, 50*time.Millisecond, "background command was killed with the request")
}
