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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryNilSafety(t *testing.T) {
	var r *Registry
	require.True(t, r.Empty())
	require.Nil(t, r.List())
	_, ok := r.Handler("any")
	require.False(t, ok)
	require.NotPanics(t, func() { r.Close() })
}

func TestRegistryEmptyConfig(t *testing.T) {
	cfg, err := ParseConfig("", 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()
	require.True(t, r.Empty())
	_, ok := r.Handler("any")
	require.False(t, ok)
}

func TestRegistryHTTPDispatch(t *testing.T) {
	upstream := echoUpstream(t)
	defer upstream.Close()

	raw := `[{"name":"alpha","transport":"http","url":"` + upstream.URL + `/mcp"}]`
	cfg, err := ParseConfig(raw, 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()

	require.False(t, r.Empty())
	_, ok := r.Handler("alpha")
	require.True(t, ok)
	_, ok = r.Handler("missing")
	require.False(t, ok)

	require.Equal(t, []ServerInfo{{Name: "alpha", Transport: "http", Source: "config"}}, r.List())
}

func TestRegistryListSorted(t *testing.T) {
	cfg, err := ParseConfig(`[
		{"name":"zeta","transport":"http","url":"http://127.0.0.1:1/mcp"},
		{"name":"alpha","transport":"stdio","command":"x"}
	]`, 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()

	list := r.List()
	require.Len(t, list, 2)
	require.Equal(t, "alpha", list[0].Name)
	require.Equal(t, "stdio", list[0].Transport)
	require.Equal(t, "zeta", list[1].Name)
	require.Equal(t, "http", list[1].Transport)
}

func TestRegistryCloseRejectsNewLookups(t *testing.T) {
	cfg, err := ParseConfig(`[{"name":"alpha","transport":"http","url":"http://127.0.0.1:1/mcp"}]`, 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)

	r.Close()
	_, ok := r.Handler("alpha")
	require.False(t, ok)
}

func TestRegistryBadStdioCommandFails(t *testing.T) {
	// An http upstream with an unparseable URL must fail registry build.
	cfg, err := ParseConfig(`[{"name":"bad","transport":"http","url":"http://[::1]:namedport/mcp"}]`, 0)
	if err == nil {
		_, err = NewRegistry(cfg)
	}
	require.Error(t, err)
}

func TestRegistryAddDynamicHTTP(t *testing.T) {
	upstream := echoUpstream(t)
	defer upstream.Close()

	cfg, err := ParseConfig("", 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()

	added := &ServerConfig{Name: "dyn", Transport: TransportHTTP, URL: upstream.URL + "/mcp"}
	require.NoError(t, r.Add(added))

	_, ok := r.Handler("dyn")
	require.True(t, ok)
	require.Equal(t, []ServerInfo{{Name: "dyn", Transport: "http", Source: "dynamic"}}, r.List())

	require.NoError(t, r.Remove("dyn"))
	_, ok = r.Handler("dyn")
	require.False(t, ok)
	require.Empty(t, r.List())
}

func TestRegistryAddRejectsDuplicatesAndInvalid(t *testing.T) {
	cfg, err := ParseConfig(`[{"name":"alpha","transport":"http","url":"http://127.0.0.1:1/mcp"}]`, 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()

	err = r.Add(&ServerConfig{Name: "alpha", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"})
	require.ErrorIs(t, err, ErrDuplicateServer)

	require.Error(t, r.Add(&ServerConfig{Name: "bad name!", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}))
	require.ErrorContains(t, r.Add(&ServerConfig{Name: "servers", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}), "reserved")
	require.ErrorContains(t, r.Add(&ServerConfig{Name: "no-url", Transport: TransportHTTP}), "url")
}

func TestRegistryRemoveRejectsStaticAndUnknown(t *testing.T) {
	cfg, err := ParseConfig(`[{"name":"alpha","transport":"http","url":"http://127.0.0.1:1/mcp"}]`, 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()

	require.ErrorIs(t, r.Remove("alpha"), ErrStaticServer)
	require.ErrorIs(t, r.Remove("missing"), ErrUnknownServer)

	require.NoError(t, r.Add(&ServerConfig{Name: "dyn", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}))
	require.NoError(t, r.Remove("dyn"))
}

func TestRegistryRemoveDynamicStdioClosesBridge(t *testing.T) {
	cfg, err := ParseConfig("", 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	defer r.Close()

	require.NoError(t, r.Add(&ServerConfig{Name: "echo", Transport: TransportStdio, Command: buildEchoServer(t)}))

	r.mu.Lock()
	bridge := r.stdio["echo"]
	r.mu.Unlock()
	require.NotNil(t, bridge)

	require.NoError(t, r.Remove("echo"))
	r.mu.Lock()
	_, exists := r.stdio["echo"]
	r.mu.Unlock()
	require.False(t, exists)
	// Removing again reports unknown.
	require.ErrorIs(t, r.Remove("echo"), ErrUnknownServer)
}

func TestRegistryClosedRejectsManagement(t *testing.T) {
	cfg, err := ParseConfig("", 0)
	require.NoError(t, err)
	r, err := NewRegistry(cfg)
	require.NoError(t, err)
	r.Close()

	require.ErrorIs(t, r.Add(&ServerConfig{Name: "dyn", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}), ErrRegistryClosed)
	require.ErrorIs(t, r.Remove("dyn"), ErrRegistryClosed)
}
