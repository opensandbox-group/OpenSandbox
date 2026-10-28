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

package mcpproxy

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseConfigEmpty(t *testing.T) {
	cfg, err := ParseConfig("", 30*time.Second)
	require.NoError(t, err)
	require.Empty(t, cfg.Servers)
	require.Equal(t, 30*time.Second, cfg.SSEKeepalive)
}

func TestParseConfigValid(t *testing.T) {
	raw := `[
		{"name":"http-upstream","transport":"http","url":"http://127.0.0.1:8000/mcp","headers":{"Authorization":"Bearer x"}},
		{"name":"stdio-upstream","transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-everything"],"cwd":"/workspace","env":{"FOO":"bar"}},
		{"name":"remote","transport":"http","url":"https://mcp.example.com/mcp"}
	]`

	cfg, err := ParseConfig(raw, 0)
	require.NoError(t, err)
	require.Len(t, cfg.Servers, 3)

	first := cfg.Servers[0]
	require.Equal(t, "http-upstream", first.Name)
	require.Equal(t, TransportHTTP, first.Transport)
	require.Equal(t, "http://127.0.0.1:8000/mcp", first.URL)
	require.Equal(t, map[string]string{"Authorization": "Bearer x"}, first.Headers)

	second := cfg.Servers[1]
	require.Equal(t, "stdio-upstream", second.Name)
	require.Equal(t, TransportStdio, second.Transport)
	require.Equal(t, "npx", second.Command)
	require.Equal(t, []string{"-y", "@modelcontextprotocol/server-everything"}, second.Args)
	require.Equal(t, "/workspace", second.CWD)
	require.Equal(t, map[string]string{"FOO": "bar"}, second.Env)

	third := cfg.Servers[2]
	require.Equal(t, "https://mcp.example.com/mcp", third.URL)
}

func TestParseConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"not json", `[{`, "parse servers JSON"},
		{"null entry", `[null]`, "is null"},
		{"missing name", `[{"transport":"http","url":"http://x/mcp"}]`, "invalid name"},
		{"bad name chars", `[{"name":"a/b","transport":"http","url":"http://x/mcp"}]`, "invalid name"},
		{"name too long", `[{"name":"` + strings.Repeat("a", 65) + `","transport":"http","url":"http://x/mcp"}]`, "invalid name"},
		{"unknown transport", `[{"name":"a","transport":"grpc","url":"http://x/mcp"}]`, "unknown transport"},
		{"http missing url", `[{"name":"a","transport":"http"}]`, "must use http or https"},
		{"http bad scheme", `[{"name":"a","transport":"http","url":"ftp://x/mcp"}]`, "must use http or https"},
		{"http no host", `[{"name":"a","transport":"http","url":"http:///mcp"}]`, "missing a host"},
		{"http with command", `[{"name":"a","transport":"http","url":"http://x/mcp","command":"foo"}]`, "must not set command"},
		{"stdio missing command", `[{"name":"a","transport":"stdio"}]`, "requires command"},
		{"stdio with url", `[{"name":"a","transport":"stdio","command":"x","url":"http://x/mcp"}]`, "must not set url"},
		{"duplicate names", `[{"name":"a","transport":"http","url":"http://x/mcp"},{"name":"a","transport":"http","url":"http://y/mcp"}]`, "duplicate server name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig(tc.raw, 0)
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
}
