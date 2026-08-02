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

package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
)

const commandInventoryResponse = `{
	"commands": [
		{"session":"running","running":true,"background":true,"started_at":"2026-07-22T12:00:00Z"},
		{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","finished_at":"2026-07-22T12:01:00Z","exit_code":null}
	],
	"pagination": {"limit":25,"nextCursor":"next-cursor"}
}`

func TestExecdClientListCommands_Query(t *testing.T) {
	tests := []struct {
		name string
		req  ListCommandsRequest
		want url.Values
	}{
		{name: "default", want: url.Values{}},
		{name: "running omitted", req: ListCommandsRequest{Limit: 25}, want: url.Values{"limit": {"25"}}},
		{name: "running true", req: ListCommandsRequest{Running: boolPtr(true)}, want: url.Values{"running": {"true"}}},
		{name: "running false", req: ListCommandsRequest{Running: boolPtr(false)}, want: url.Values{"running": {"false"}}},
		{name: "empty cursor is omitted", req: ListCommandsRequest{Cursor: ""}, want: url.Values{}},
		{name: "whitespace cursor is omitted", req: ListCommandsRequest{Cursor: "  \t "}, want: url.Values{}},
		{
			name: "limit and opaque cursor are escaped",
			req:  ListCommandsRequest{Limit: 25, Cursor: "opaque +/= cursor"},
			want: url.Values{"limit": {"25"}, "cursor": {"opaque +/= cursor"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				if r.URL.Path != "/command" {
					t.Errorf("path = %q, want /command", r.URL.Path)
				}
				if got := r.URL.Query(); !urlValuesEqual(got, test.want) {
					t.Errorf("query = %q, want %q", got.Encode(), test.want.Encode())
				}
				jsonResponse(w, http.StatusOK, json.RawMessage(commandInventoryResponse))
			})

			got, err := client.ListCommands(context.Background(), test.req)
			if err != nil {
				t.Fatalf("ListCommands() error = %v", err)
			}
			if got.Pagination.Limit != 25 {
				t.Errorf("pagination limit = %d, want 25", got.Pagination.Limit)
			}
		})
	}
}

func TestListCommandsResponse_RawJSONBranchesAndRoundTrip(t *testing.T) {
	var response ListCommandsResponse
	if err := json.Unmarshal([]byte(commandInventoryResponse), &response); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if response.Pagination.NextCursor == nil || *response.Pagination.NextCursor != "next-cursor" {
		t.Fatalf("next cursor = %v, want next-cursor", response.Pagination.NextCursor)
	}
	if response.Commands[1].ExitCode != nil {
		t.Errorf("terminal exit code = %v, want nil for explicit null", response.Commands[1].ExitCode)
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var raw struct {
		Commands   []map[string]json.RawMessage `json:"commands"`
		Pagination map[string]json.RawMessage   `json:"pagination"`
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshal round trip = %v", err)
	}
	if _, ok := raw.Commands[0]["finished_at"]; ok {
		t.Error("running command unexpectedly includes finished_at")
	}
	if _, ok := raw.Commands[0]["exit_code"]; ok {
		t.Error("running command unexpectedly includes exit_code")
	}
	if got, ok := raw.Commands[1]["finished_at"]; !ok || string(got) != `"2026-07-22T12:01:00Z"` {
		t.Errorf("terminal finished_at = %s, want timestamp", got)
	}
	if got, ok := raw.Commands[1]["exit_code"]; !ok || string(got) != "null" {
		t.Errorf("terminal exit_code = %s, want explicit null", got)
	}

	var finalPage ListCommandsResponse
	if err := json.Unmarshal([]byte(`{"commands":[],"pagination":{"limit":25}}`), &finalPage); err != nil {
		t.Fatalf("unmarshal final page = %v", err)
	}
	if finalPage.Pagination.NextCursor != nil {
		t.Errorf("final page next cursor = %v, want nil", *finalPage.Pagination.NextCursor)
	}
}

func TestListCommandsResponse_RejectsInvalidBranches(t *testing.T) {
	tests := []string{
		`{"commands":[{"session":"running","running":true,"background":true,"started_at":"2026-07-22T12:00:00Z","finished_at":null}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","finished_at":null,"exit_code":null,"error":123}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","finished_at":null}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"running","running":null,"background":true,"started_at":"2026-07-22T12:00:00Z"}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":null,"running":true,"background":true,"started_at":"2026-07-22T12:00:00Z"}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"running","running":true,"background":null,"started_at":"2026-07-22T12:00:00Z"}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"running","running":true,"background":true,"started_at":null}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","finished_at":"2026-07-22T12:01:00Z","exit_code":null,"error":null}],"pagination":{"limit":25}}`,
		`{"commands":[{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","finished_at":null,"exit_code":null}],"pagination":{"limit":25}}`,
		`{"commands":[],"pagination":{"limit":25,"nextCursor":null}}`,
		`{"commands":[],"pagination":{"limit":25,"nextCursor":""}}`,
		`{"commands":[],"pagination":{"limit":25,"nextCursor":"   "}}`,
		`{"commands":[],"pagination":{"limit":0}}`,
		`{"commands":[],"pagination":{"limit":-1}}`,
		`{"commands":[],"pagination":{"limit":101}}`,
		`{"commands":[{"session":"running","running":true,"background":true,"started_at":"2026-07-22T12:00:00Z","unknown":true}],"pagination":{"limit":25}}`,
	}
	for _, body := range tests {
		var response ListCommandsResponse
		if err := json.Unmarshal([]byte(body), &response); err == nil {
			t.Errorf("Unmarshal(%s) unexpectedly succeeded", body)
		}
	}
}

func TestListCommandsResponse_AcceptsTopLevelAndPaginationExtensions(t *testing.T) {
	var response ListCommandsResponse
	err := json.Unmarshal([]byte(`{
		"commands": [],
		"pagination": {"limit":25,"futurePaginationField":{"enabled":true}},
		"futureTopLevelField": true
	}`), &response)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v, want top-level and pagination extensions accepted", err)
	}
	if response.Pagination.Limit != 25 {
		t.Errorf("pagination limit = %d, want 25", response.Pagination.Limit)
	}
}

func TestCommandSummary_MarshalRejectsTerminalWithoutFinishedAt(t *testing.T) {
	_, err := json.Marshal(CommandSummary{
		Session:    "terminal",
		Running:    false,
		Background: false,
		ExitCode:   nil,
	})
	if err == nil {
		t.Fatal("Marshal() succeeded, want error for terminal command without finished_at")
	}
}

func TestCommandSummary_RequiresNestedBranchFields(t *testing.T) {
	tests := []string{
		`{"running":true,"background":true,"started_at":"2026-07-22T12:00:00Z"}`,
		`{"session":"running","background":true,"started_at":"2026-07-22T12:00:00Z"}`,
		`{"session":"running","running":true,"started_at":"2026-07-22T12:00:00Z"}`,
		`{"session":"running","running":true,"background":true}`,
		`{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","finished_at":"2026-07-22T12:01:00Z"}`,
		`{"session":"terminal","running":false,"background":false,"started_at":"2026-07-22T12:00:00Z","exit_code":null}`,
	}
	for _, body := range tests {
		var summary CommandSummary
		if err := json.Unmarshal([]byte(body), &summary); err == nil {
			t.Errorf("Unmarshal(%s) unexpectedly succeeded", body)
		}
	}
}

func TestExecdClientListCommands_HTTPError(t *testing.T) {
	_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusBadRequest, ErrorResponse{Code: "INVALID_QUERY", Message: "invalid cursor"})
	})
	_, err := client.ListCommands(context.Background(), ListCommandsRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ListCommands() error = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
}

func TestSandboxListCommands(t *testing.T) {
	t.Run("forwards to non-nil execd", func(t *testing.T) {
		_, client := newExecdServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("running") != "false" {
				t.Errorf("running = %q, want false", r.URL.Query().Get("running"))
			}
			jsonResponse(w, http.StatusOK, json.RawMessage(commandInventoryResponse))
		})
		sandbox := &Sandbox{execd: client}
		response, err := sandbox.ListCommands(context.Background(), ListCommandsRequest{Running: boolPtr(false)})
		if err != nil {
			t.Fatalf("Sandbox.ListCommands() error = %v", err)
		}
		if len(response.Commands) != 2 {
			t.Errorf("commands = %d, want 2", len(response.Commands))
		}
	})

	t.Run("preserves nil execd legacy error", func(t *testing.T) {
		_, err := (&Sandbox{}).ListCommands(context.Background(), ListCommandsRequest{})
		if err == nil || err.Error() != "opensandbox: execd client not initialized" {
			t.Errorf("error = %v, want legacy execd initialization error", err)
		}
	})
}

func boolPtr(value bool) *bool { return &value }

func urlValuesEqual(got, want url.Values) bool {
	return got.Encode() == want.Encode()
}
