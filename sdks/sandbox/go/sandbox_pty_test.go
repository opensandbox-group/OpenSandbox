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

package opensandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSandbox_PtyLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/pty":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": "sess-123"})
		case r.Method == http.MethodGet && r.URL.Path == "/pty/sess-123":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_id":    "sess-123",
				"running":       true,
				"output_offset": 4096,
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/pty/sess-123":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sb := &Sandbox{id: "sbx-pty", execd: NewExecdClient(srv.URL, "tok")}
	ctx := context.Background()

	sess, err := sb.CreatePtySession(ctx, PtyCreateRequest{Cwd: "/tmp", Command: "bash"})
	require.NoError(t, err)
	require.Equal(t, "sess-123", sess.SessionID)

	status, err := sb.GetPtySession(ctx, "sess-123")
	require.NoError(t, err)
	require.Equal(t, "sess-123", status.SessionID)
	require.True(t, status.Running)
	require.Equal(t, int64(4096), status.OutputOffset)

	require.NoError(t, sb.DeletePtySession(ctx, "sess-123"))
}

func TestSandbox_Pty_ExecdNil(t *testing.T) {
	sb := &Sandbox{id: "no-execd"}
	_, err := sb.CreatePtySession(context.Background(), PtyCreateRequest{})
	require.Error(t, err)
}
