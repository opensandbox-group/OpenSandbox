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

package mcpproxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestIsNotification(t *testing.T) {
	t.Run("with id", func(t *testing.T) {
		r := &Request{ID: 1, Method: "ping"}
		require.False(t, r.IsNotification())
	})
	t.Run("without id", func(t *testing.T) {
		r := &Request{Method: "notifications/initialized"}
		require.True(t, r.IsNotification())
	})
}

func TestNewErrorResponse(t *testing.T) {
	resp := newErrorResponse(42, CodeMethodNotFound, "not found")
	require.Equal(t, jsonrpcVersion, resp.JSONRPC)
	require.Equal(t, 42, resp.ID)
	require.NotNil(t, resp.Error)
	require.Equal(t, CodeMethodNotFound, resp.Error.Code)
	require.Equal(t, "not found", resp.Error.Message)
	require.Nil(t, resp.Result)
}

func TestNewResultResponse(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"ok": "true"})
	resp := newResultResponse("req-1", data)
	require.Equal(t, jsonrpcVersion, resp.JSONRPC)
	require.Equal(t, "req-1", resp.ID)
	require.Nil(t, resp.Error)
	require.JSONEq(t, `{"ok":"true"}`, string(resp.Result))
}

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		input any
		want  string
	}{
		{float64(42), "42"},
		{int64(7), "7"},
		{"abc", "abc"},
		{nil, "<nil>"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, normalizeID(tt.input))
	}
}

func TestRequestRoundTrip(t *testing.T) {
	orig := Request{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"echo"}`),
	}
	data, err := json.Marshal(orig)
	require.NoError(t, err)

	var decoded Request
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, orig.Method, decoded.Method)
	require.JSONEq(t, `{"name":"echo"}`, string(decoded.Params))
}
