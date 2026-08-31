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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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

// TestSanitizeRequestID verifies the drop-not-mutate policy for caller
// supplied request ids: oversized ids and ids outside the safe charset are
// dropped entirely (returning ""), so the propagated id always matches the
// id the ingress layer logged and stays sound in POSIX and cmd.exe shells.
func TestSanitizeRequestID(t *testing.T) {
	cases := []struct {
		name string
		rid  string
		want string
	}{
		{"empty passes through as empty", "", ""},
		{"uuid style accepted", "3f2b9c1e-8d4a-4e7f-9b0c-1a2b3c4d5e6f", "3f2b9c1e-8d4a-4e7f-9b0c-1a2b3c4d5e6f"},
		{"plain alnum accepted", "req-abc_123", "req-abc_123"},
		{"oversized dropped", strings.Repeat("a", maxRequestIDLen+1), ""},
		{"exactly at cap accepted", strings.Repeat("a", maxRequestIDLen), strings.Repeat("a", maxRequestIDLen)},
		{"shell metacharacters dropped", "'; touch /tmp/pwned; '", ""},
		{"cmd metacharacters dropped", "a&calc", ""},
		{"variable expansion dropped", "$HOME", ""},
		{"space dropped", "req abc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRequestID(tc.rid); got != tc.want {
				t.Fatalf("sanitizeRequestID(%q) = %q, want %q", tc.rid, got, tc.want)
			}
		})
	}
}
