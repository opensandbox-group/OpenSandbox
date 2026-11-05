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

func TestRegistryStartsEmpty(t *testing.T) {
	r := NewRegistry(0)
	defer r.Close()
	require.True(t, r.Empty())
	_, ok := r.Handler("any")
	require.False(t, ok)
	require.Empty(t, r.List())
}

func TestRegistryAddAndDispatchHTTP(t *testing.T) {
	upstream := echoUpstream(t)
	defer upstream.Close()

	r := NewRegistry(0)
	defer r.Close()

	require.NoError(t, r.Add(&ServerConfig{Name: "alpha", Transport: TransportHTTP, URL: upstream.URL + "/mcp"}))

	require.False(t, r.Empty())
	_, ok := r.Handler("alpha")
	require.True(t, ok)
	_, ok = r.Handler("missing")
	require.False(t, ok)

	require.Equal(t, []ServerInfo{{Name: "alpha", Transport: "http"}}, r.List())
}

func TestRegistryListSorted(t *testing.T) {
	r := NewRegistry(0)
	defer r.Close()

	require.NoError(t, r.Add(&ServerConfig{Name: "zeta", Transport: TransportHTTP, URL: "http://127.0.0.1:1/mcp"}))
	require.NoError(t, r.Add(&ServerConfig{Name: "alpha", Transport: TransportStdio, Command: "x"}))

	list := r.List()
	require.Len(t, list, 2)
	require.Equal(t, "alpha", list[0].Name)
	require.Equal(t, "stdio", list[0].Transport)
	require.Equal(t, "zeta", list[1].Name)
	require.Equal(t, "http", list[1].Transport)
}

func TestRegistryAddRejectsDuplicatesAndInvalid(t *testing.T) {
	r := NewRegistry(0)
	defer r.Close()

	require.NoError(t, r.Add(&ServerConfig{Name: "alpha", Transport: TransportHTTP, URL: "http://127.0.0.1:1/mcp"}))

	err := r.Add(&ServerConfig{Name: "alpha", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"})
	require.ErrorIs(t, err, ErrDuplicateServer)

	require.Error(t, r.Add(&ServerConfig{Name: "bad name!", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}))
	require.ErrorContains(t, r.Add(&ServerConfig{Name: "servers", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}), "reserved")
	require.ErrorContains(t, r.Add(&ServerConfig{Name: "no-url", Transport: TransportHTTP}), "url")
	// A failed Add must not leave a half-registered entry behind.
	_, ok := r.Handler("no-url")
	require.False(t, ok)
}

func TestRegistryRemoveLifecycle(t *testing.T) {
	upstream := echoUpstream(t)
	defer upstream.Close()

	r := NewRegistry(0)
	defer r.Close()

	require.NoError(t, r.Add(&ServerConfig{Name: "alpha", Transport: TransportHTTP, URL: upstream.URL + "/mcp"}))

	require.NoError(t, r.Remove("alpha"))
	_, ok := r.Handler("alpha")
	require.False(t, ok)
	require.Empty(t, r.List())
	require.ErrorIs(t, r.Remove("alpha"), ErrUnknownServer)
	require.ErrorIs(t, r.Remove("missing"), ErrUnknownServer)
}

func TestRegistryRemoveStdioClosesBridge(t *testing.T) {
	r := NewRegistry(0)
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
	require.ErrorIs(t, r.Remove("echo"), ErrUnknownServer)
}

func TestRegistryCloseRejectsEverything(t *testing.T) {
	r := NewRegistry(0)
	require.NoError(t, r.Add(&ServerConfig{Name: "alpha", Transport: TransportHTTP, URL: "http://127.0.0.1:1/mcp"}))

	r.Close()
	_, ok := r.Handler("alpha")
	require.False(t, ok)
	require.ErrorIs(t, r.Add(&ServerConfig{Name: "dyn", Transport: TransportHTTP, URL: "http://127.0.0.1:2/mcp"}), ErrRegistryClosed)
	require.ErrorIs(t, r.Remove("alpha"), ErrRegistryClosed)
}

func TestRegistryBadUpstreamFailsAdd(t *testing.T) {
	// An http upstream with an unparseable URL must fail registration.
	r := NewRegistry(0)
	defer r.Close()
	require.Error(t, r.Add(&ServerConfig{Name: "bad", Transport: TransportHTTP, URL: "http://[::1]:namedport/mcp"}))
}
