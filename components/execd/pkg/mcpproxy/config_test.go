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

package mcpproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerConfigJSONRoundTrip(t *testing.T) {
	raw := `{"name":"http-upstream","transport":"http","url":"http://127.0.0.1:8000/mcp","headers":{"Authorization":"Bearer x"}}`
	var s ServerConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	require.Equal(t, "http-upstream", s.Name)
	require.Equal(t, TransportHTTP, s.Transport)
	require.Equal(t, "http://127.0.0.1:8000/mcp", s.URL)
	require.Equal(t, map[string]string{"Authorization": "Bearer x"}, s.Headers)
}

func TestServerConfigValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"missing name", ServerConfig{Transport: TransportHTTP, URL: "http://x/mcp"}, "invalid name"},
		{"bad name chars", ServerConfig{Name: "a/b", Transport: TransportHTTP, URL: "http://x/mcp"}, "invalid name"},
		{"name too long", ServerConfig{Name: strings.Repeat("a", 65), Transport: TransportHTTP, URL: "http://x/mcp"}, "invalid name"},
		{"unknown transport", ServerConfig{Name: "a", Transport: "grpc", URL: "http://x/mcp"}, "unknown transport"},
		{"http missing url", ServerConfig{Name: "a", Transport: TransportHTTP}, "must use http or https"},
		{"http bad scheme", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "ftp://x/mcp"}, "must use http or https"},
		{"http no host", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "http:///mcp"}, "missing a host"},
		{"http with command", ServerConfig{Name: "a", Transport: TransportHTTP, URL: "http://x/mcp", Command: "foo"}, "must not set command"},
		{"stdio missing command", ServerConfig{Name: "a", Transport: TransportStdio}, "requires command"},
		{"stdio with url", ServerConfig{Name: "a", Transport: TransportStdio, Command: "x", URL: "http://x/mcp"}, "must not set url"},
		{"reserved name", ServerConfig{Name: "servers", Transport: TransportHTTP, URL: "http://x/mcp"}, "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestServerConfigValidateNameBoundaries(t *testing.T) {
	valid := []string{"a", "Z", "0srv", "a.b", "a_b", "a-b", strings.Repeat("a", 64)}
	for _, name := range valid {
		require.NoError(t, (&ServerConfig{Name: name, Transport: TransportStdio, Command: "x"}).Validate(), name)
	}
	invalid := []string{"", "-a", ".a", "_a", "a b", "a/b", strings.Repeat("a", 65)}
	for _, name := range invalid {
		require.Error(t, (&ServerConfig{Name: name, Transport: TransportStdio, Command: "x"}).Validate(), name)
	}
	// Reserved by the /mcp/servers management routes.
	require.ErrorContains(t, (&ServerConfig{Name: "servers", Transport: TransportStdio, Command: "x"}).Validate(), "reserved")
}
