// Copyright 2026 Alibaba Group Holding Ltd.
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

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func getExecutionStatusHTTP(t *testing.T, base, path string, target any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	require.NoError(t, err)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", "test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(target))
}

func TestOperationPTYLostLaunchErrorStatus(t *testing.T) {
	ctrl := controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	for _, mode := range []string{"pty", "pipe"} {
		t.Run(mode, func(t *testing.T) {
			cwd := t.TempDir()
			marker := filepath.Join(cwd, "unexpected-start")
			key := operationIdentity(t, server.URL, "lost-launch-"+mode)
			body := map[string]any{"operation_id": key, "cwd": cwd, "command": "echo started > '" + marker + "'"}
			status, data := postOperation(t, server.URL, "/pty/operations", body)
			op := decodeOperation(t, status, data)
			t.Cleanup(func() { _ = ctrl.DeletePTYSession(op.ID) })
			require.Eventually(t, func() bool {
				status, data := getOperationHTTP(t, server.URL, "/execution/operation?kind=pty", key, "test-token")
				return decodeOperation(t, status, data).State == "created"
			}, 5*time.Second, time.Millisecond)
			var raw map[string]any
			getExecutionStatusHTTP(t, server.URL, "/pty/"+op.ID, &raw)
			require.Equal(t, false, raw["launch_attempted"])
			require.Equal(t, false, raw["launch_failed"])
			require.NoError(t, os.Remove(cwd))
			pty := "1"
			if mode == "pipe" {
				pty = "0"
			}
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/pty/" + op.ID + "/ws?pty=" + pty
			conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"X-EXECD-ACCESS-TOKEN": []string{"test-token"}})
			require.NoError(t, err)
			// Lose the WebSocket error frame; recovery must rely on GET status.
			require.NoError(t, conn.Close())
			var state model.PTYSessionStatusResponse
			require.Eventually(t, func() bool {
				getExecutionStatusHTTP(t, server.URL, "/pty/"+op.ID, &state)
				return state.LaunchFailed
			}, 5*time.Second, time.Millisecond)
			require.True(t, state.LaunchAttempted)
			require.False(t, state.Running)
			require.NoError(t, os.Mkdir(cwd, 0700))
			conn, _, err = websocket.DefaultDialer.Dial(url+"&takeover=1", http.Header{"X-EXECD-ACCESS-TOKEN": []string{"test-token"}})
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			var frame model.ServerFrame
			require.NoError(t, conn.ReadJSON(&frame))
			require.Equal(t, model.WSErrCodeStartFailed, frame.Code)
			status, data = postOperation(t, server.URL, "/pty/operations", body)
			recovered := decodeOperation(t, status, data)
			require.Equal(t, op.ID, recovered.ID)
			require.Equal(t, "created", recovered.State)
			getExecutionStatusHTTP(t, server.URL, "/pty/"+op.ID, &state)
			require.True(t, state.LaunchFailed)
			_, err = os.Stat(marker)
			require.True(t, os.IsNotExist(err), "recovery and reconnect must not launch after failure")
		})
	}
}

func TestOperationCommandDiagnosticsHTTP(t *testing.T) {
	controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	for _, background := range []bool{false, true} {
		for _, startupFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("background=%v/startupFailure=%v", background, startupFailure), func(t *testing.T) {
				key := operationIdentity(t, server.URL, fmt.Sprintf("diagnostic-%v-%v", background, startupFailure))
				body := map[string]any{"operation_id": key, "command": "echo diagnostic-stderr >&2; exit 7", "background": background}
				missing := filepath.Join(t.TempDir(), "missing-executable")
				wantState, wantError, wantContent, wantExit := "created", "exit status 7", body["command"].(string), 7
				if startupFailure {
					delete(body, "command")
					body["argv"] = []string{missing}
					content, err := json.Marshal(body["argv"])
					require.NoError(t, err)
					wantState, wantError, wantContent, wantExit = "failed", missing, string(content), 255
				}
				status, data := loseOperationResponse(t, server.URL, "/command/operations", body, nil)
				op := decodeOperation(t, status, data)
				require.Eventually(t, func() bool {
					status, data := getOperationHTTP(t, server.URL, "/execution/operation?kind=command", key, "test-token")
					return decodeOperation(t, status, data).State == wantState
				}, 5*time.Second, time.Millisecond)
				var state model.CommandStatusResponse
				require.Eventually(t, func() bool {
					getExecutionStatusHTTP(t, server.URL, "/command/status/"+op.ID, &state)
					return state.ExitCode != nil
				}, 5*time.Second, time.Millisecond)
				require.False(t, state.Running)
				require.Equal(t, wantExit, *state.ExitCode)
				require.Contains(t, state.Error, wantError)
				require.Equal(t, wantContent, state.Content)
				require.NotNil(t, state.FinishedAt)
				if startupFailure {
					require.NoError(t, os.WriteFile(missing, []byte("#!/bin/sh\nexit 0\n"), 0700))
				}
				status, data = postOperation(t, server.URL, "/command/operations", body)
				recovered := decodeOperation(t, status, data)
				require.Equal(t, op.ID, recovered.ID)
				require.Equal(t, wantState, recovered.State)
				var after model.CommandStatusResponse
				getExecutionStatusHTTP(t, server.URL, "/command/status/"+op.ID, &after)
				require.Equal(t, state, after, "retry must retain the original diagnostics")
				resp, err := http.Get(server.URL + "/command/status/" + op.ID)
				require.NoError(t, err)
				resp.Body.Close()
				require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			})
		}
	}
}

func TestOperationNativeArgvRecoveryHTTP(t *testing.T) {
	controller.InitCodeRunner()
	server := httptest.NewServer(NewRouter("test-token"))
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "starts")
	key := operationIdentity(t, server.URL, "native-argv-recovery")
	argv := []string{"/bin/sh", "-c", "echo $$ >> \"$1\"", "marker", marker}
	body := map[string]any{"operation_id": key, "argv": argv}
	status, data := loseOperationResponse(t, server.URL, "/command/operations", body, nil)
	op := decodeOperation(t, status, data)
	require.Eventually(t, func() bool {
		status, data := getOperationHTTP(t, server.URL, "/execution/operation?kind=command", key, "test-token")
		return decodeOperation(t, status, data).State == "created"
	}, 5*time.Second, time.Millisecond)
	var state model.CommandStatusResponse
	require.Eventually(t, func() bool {
		getExecutionStatusHTTP(t, server.URL, "/command/status/"+op.ID, &state)
		return state.ExitCode != nil
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, 0, *state.ExitCode)
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, op.ID, decodeOperation(t, status, data).ID)
	argv[3], argv[4] = argv[4], argv[3]
	status, data = postOperation(t, server.URL, "/command/operations", body)
	require.Equal(t, http.StatusConflict, status, string(data))
	require.Contains(t, string(data), "operation_conflict")
	starts, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(starts)), 1)
}
