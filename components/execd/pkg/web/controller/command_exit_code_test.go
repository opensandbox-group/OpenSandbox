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
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/stretchr/testify/require"
)

func commandWireEvents(t *testing.T, body string) []map[string]json.RawMessage {
	t.Helper()
	var events []map[string]json.RawMessage
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		events = append(events, event)
	}
	return events
}

func TestRunCommandSuccessfulExitCode(t *testing.T) {
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	flag.ApiGracefulShutdownTimeout = 0
	t.Cleanup(func() { codeRunner = previousRunner; flag.ApiGracefulShutdownTimeout = previousTimeout })

	for _, tc := range []struct {
		name, body, terminal string
		hasExitCode          bool
	}{
		{name: "zero", body: `{"argv":["/bin/sh","-c","exit 0"]}`, terminal: "execution_complete", hasExitCode: true},
		{name: "nonzero", body: `{"argv":["/bin/sh","-c","exit 7"]}`, terminal: "error"},
		{name: "start_failure", body: `{"argv":["/nonexistent-execd-exit-code-test"]}`, terminal: "error"},
		{name: "background", body: `{"argv":["/bin/sh","-c","exit 0"],"background":true}`, terminal: "execution_complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codeRunner = runtime.NewController("", "")
			ctx, w := newTestContext(http.MethodPost, "/command", []byte(tc.body))
			NewCodeInterpretingController(ctx).RunCommand()
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			found := false
			for _, event := range commandWireEvents(t, w.Body.String()) {
				if string(event["type"]) == `"`+tc.terminal+`"` {
					found = true
					if tc.hasExitCode {
						require.Contains(t, event, "exit_code")
						require.JSONEq(t, "0", string(event["exit_code"]))
					} else {
						require.NotContains(t, event, "exit_code")
					}
					if tc.name == "nonzero" {
						var payload map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(event["error"], &payload))
						require.JSONEq(t, "7", string(payload["exit_code"]))
					}
					if tc.name == "start_failure" {
						var payload map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(event["error"], &payload))
						require.NotContains(t, payload, "exit_code")
					}
				} else {
					require.NotContains(t, event, "exit_code")
				}
			}
			require.True(t, found, w.Body.String())
		})
	}
}

func TestRunCommandTimeoutDoesNotReportSuccess(t *testing.T) {
	previousRunner, previousTimeout := codeRunner, flag.ApiGracefulShutdownTimeout
	codeRunner = runtime.NewController("", "")
	flag.ApiGracefulShutdownTimeout = 0
	t.Cleanup(func() { codeRunner = previousRunner; flag.ApiGracefulShutdownTimeout = previousTimeout })
	ctx, w := newTestContext(http.MethodPost, "/command", []byte(`{"argv":["/bin/sh","-c","sleep 30"],"timeout":100}`))
	NewCodeInterpretingController(ctx).RunCommand()
	found := false
	for _, event := range commandWireEvents(t, w.Body.String()) {
		require.NotEqual(t, `"execution_complete"`, string(event["type"]))
		require.NotContains(t, event, "exit_code")
		if string(event["type"]) == `"error"` {
			found = true
			var payload map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(event["error"], &payload))
			require.JSONEq(t, "-1", string(payload["exit_code"]))
		}
	}
	require.True(t, found, w.Body.String())
}

func TestRunCodeCompletionOmitsProcessExitCode(t *testing.T) {
	previousRunner := codeRunner
	codeRunner = &fakeCodeRunner{execute: func(request *runtime.ExecuteCodeRequest) error {
		request.Hooks.OnExecuteComplete(time.Millisecond)
		return nil
	}}
	t.Cleanup(func() { codeRunner = previousRunner })
	ctx, w := newTestContext(http.MethodPost, "/code", []byte(`{"code":"print(1)","language":"python"}`))
	NewCodeInterpretingController(ctx).RunCode()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	events := commandWireEvents(t, w.Body.String())
	require.Len(t, events, 1)
	require.JSONEq(t, `"execution_complete"`, string(events[0]["type"]))
	require.NotContains(t, events[0], "exit_code")
}
