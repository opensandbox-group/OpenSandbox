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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func TestListCommands(t *testing.T) {
	t.Run("maps query and response", func(t *testing.T) {
		previousRunner := codeRunner
		startedAt := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
		finishedAt := startedAt.Add(time.Minute)
		exitCode := 17
		cursor := "next-cursor"
		codeRunner = &fakeCodeRunner{
			listCommands: func(request runtime.ListCommandsRequest) (runtime.ListCommandsResponse, error) {
				require.NotNil(t, request.Running)
				require.False(t, *request.Running)
				require.Equal(t, 25, request.Limit)
				require.Equal(t, "opaque-cursor", request.Cursor)
				return runtime.ListCommandsResponse{
					Commands: []runtime.CommandSummary{
						{Session: "running", Running: true, Background: true, StartedAt: startedAt},
						{Session: "terminal", Background: false, StartedAt: startedAt, FinishedAt: &finishedAt, ExitCode: &exitCode, Error: "failed"},
					},
					Pagination: runtime.CommandPagination{Limit: 25, NextCursor: &cursor},
				}, nil
			},
		}
		t.Cleanup(func() { codeRunner = previousRunner })

		ctx, w := newTestContext(http.MethodGet, "/command?running=false&limit=25&cursor=opaque-cursor", nil)
		NewCodeInterpretingController(ctx).ListCommands()

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		commands := response["commands"].([]any)
		running := commands[0].(map[string]any)
		require.NotContains(t, running, "finished_at")
		require.NotContains(t, running, "exit_code")
		terminal := commands[1].(map[string]any)
		require.Equal(t, "terminal", terminal["session"])
		require.Equal(t, float64(exitCode), terminal["exit_code"])
		require.Contains(t, terminal, "finished_at")
		pagination := response["pagination"].(map[string]any)
		require.Equal(t, "next-cursor", pagination["nextCursor"])
	})

	t.Run("maps running filters and default limit", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			path    string
			running *bool
		}{
			{name: "omitted selects all", path: "/command"},
			{name: "true selects running", path: "/command?running=true", running: boolPointer(true)},
		} {
			t.Run(test.name, func(t *testing.T) {
				previousRunner := codeRunner
				codeRunner = &fakeCodeRunner{
					listCommands: func(request runtime.ListCommandsRequest) (runtime.ListCommandsResponse, error) {
						require.Equal(t, test.running, request.Running)
						require.Equal(t, 50, request.Limit)
						require.Empty(t, request.Cursor)
						return runtime.ListCommandsResponse{Commands: []runtime.CommandSummary{}, Pagination: runtime.CommandPagination{Limit: 50}}, nil
					},
				}
				t.Cleanup(func() { codeRunner = previousRunner })

				ctx, w := newTestContext(http.MethodGet, test.path, nil)
				NewCodeInterpretingController(ctx).ListCommands()

				require.Equal(t, http.StatusOK, w.Code)
			})
		}
	})

	t.Run("terminal null fields remain explicit", func(t *testing.T) {
		previousRunner := codeRunner
		codeRunner = &fakeCodeRunner{
			listCommands: func(runtime.ListCommandsRequest) (runtime.ListCommandsResponse, error) {
				return runtime.ListCommandsResponse{
					Commands:   []runtime.CommandSummary{{Session: "terminal", Running: false}},
					Pagination: runtime.CommandPagination{Limit: 50},
				}, nil
			},
		}
		t.Cleanup(func() { codeRunner = previousRunner })

		ctx, w := newTestContext(http.MethodGet, "/command", nil)
		NewCodeInterpretingController(ctx).ListCommands()

		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		terminal := response["commands"].([]any)[0].(map[string]any)
		require.Contains(t, terminal, "finished_at")
		require.Nil(t, terminal["finished_at"])
		require.Contains(t, terminal, "exit_code")
		require.Nil(t, terminal["exit_code"])
	})
}

func boolPointer(value bool) *bool {
	return &value
}

func TestCommandInventoryHTTP(t *testing.T) {
	t.Run("invalid limit does not cache response", func(t *testing.T) {
		ctx, w := newTestContext(http.MethodGet, "/command?limit=0", nil)
		NewCodeInterpretingController(ctx).ListCommands()

		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	})

	t.Run("invalid query", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			path    string
			message string
		}{
			{name: "unknown", path: "/command?unknown=value", message: "invalid query parameter"},
			{name: "duplicate limit", path: "/command?limit=1&limit=2", message: "invalid query parameter"},
			{name: "duplicate running", path: "/command?running=true&running=false", message: "invalid query parameter"},
			{name: "duplicate cursor", path: "/command?cursor=one&cursor=two", message: "invalid query parameter"},
			{name: "empty running", path: "/command?running=", message: "invalid query parameter"},
			{name: "empty limit", path: "/command?limit=", message: "invalid query parameter"},
			{name: "percent decoded blank running", path: "/command?running=%20", message: "invalid query parameter"},
			{name: "invalid running", path: "/command?running=TRUE", message: "invalid query parameter"},
			{name: "zero limit", path: "/command?limit=0", message: "invalid query parameter"},
			{name: "large limit", path: "/command?limit=101", message: "invalid query parameter"},
			{name: "leading empty component", path: "/command?&running=true", message: "invalid query parameter"},
			{name: "trailing empty component", path: "/command?running=true&", message: "invalid query parameter"},
			{name: "empty components", path: "/command?&&", message: "invalid query parameter"},
			{name: "interior empty component", path: "/command?running=true&&limit=25", message: "invalid query parameter"},
			{name: "malformed percent escape", path: "/command?running=%zz", message: "invalid query parameter"},
			{name: "empty cursor", path: "/command?cursor=", message: "invalid cursor"},
			{name: "cursor without equals", path: "/command?cursor", message: "invalid cursor"},
			{name: "empty cursor takes precedence", path: "/command?cursor=&running=", message: "invalid cursor"},
			{name: "empty cursor takes precedence when later", path: "/command?running=&cursor=", message: "invalid cursor"},
			{name: "empty cursor takes precedence over empty component", path: "/command?&&cursor=", message: "invalid cursor"},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx, w := newTestContext(http.MethodGet, test.path, nil)
				NewCodeInterpretingController(ctx).ListCommands()

				require.Equal(t, http.StatusBadRequest, w.Code)
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				var response model.ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.Equal(t, model.ErrorCodeInvalidQuery, response.Code)
				require.Equal(t, test.message, response.Message)
			})
		}
	})

	t.Run("invalid runtime cursor is opaque", func(t *testing.T) {
		previousRunner := codeRunner
		codeRunner = &fakeCodeRunner{
			listCommands: func(runtime.ListCommandsRequest) (runtime.ListCommandsResponse, error) {
				return runtime.ListCommandsResponse{}, fmt.Errorf("cursor token secret: %w", runtime.ErrInvalidCommandCursor)
			},
		}
		t.Cleanup(func() { codeRunner = previousRunner })

		ctx, w := newTestContext(http.MethodGet, "/command?cursor=opaque", nil)
		NewCodeInterpretingController(ctx).ListCommands()

		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var response model.ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, model.ErrorCodeInvalidQuery, response.Code)
		require.Equal(t, "invalid cursor", response.Message)
	})

	t.Run("runtime errors remain runtime errors", func(t *testing.T) {
		previousRunner := codeRunner
		codeRunner = &fakeCodeRunner{
			listCommands: func(runtime.ListCommandsRequest) (runtime.ListCommandsResponse, error) {
				return runtime.ListCommandsResponse{}, errors.New("command inventory invariant: corrupted index")
			},
		}
		t.Cleanup(func() { codeRunner = previousRunner })

		ctx, w := newTestContext(http.MethodGet, "/command", nil)
		NewCodeInterpretingController(ctx).ListCommands()

		require.Equal(t, http.StatusInternalServerError, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var response model.ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, model.ErrorCodeRuntimeError, response.Code)
	})
}
