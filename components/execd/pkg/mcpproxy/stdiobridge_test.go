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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// startBridgedEchoServer serves a stdio bridge over an httptest server and
// returns a connected MCP client session.
func startBridgedEchoServer(t *testing.T, marker string) (*testHTTPServer, *Registry, *mcp.ClientSession) {
	t.Helper()
	binary := buildEchoServer(t)

	registry := NewRegistry(0)
	require.NoError(t, registry.Add(&ServerConfig{Name: "echo", Transport: TransportStdio, Command: binary, Env: map[string]string{"MCPECHO_MARKER": marker}}))
	t.Cleanup(registry.Close)

	handler, ok := registry.Handler("echo")
	require.True(t, ok)
	server := newTestHTTPServer(handler)
	t.Cleanup(server.server.Close)

	return server, registry, connectEchoClient(t, server.baseURL+"/mcp/echo")
}

func connectEchoClient(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "mcpproxy-test", Version: "0.0.1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func echoBridgedSessions(t *testing.T, registry *Registry) *stdioBridge {
	t.Helper()
	handler, ok := registry.Handler("echo")
	require.True(t, ok)
	return handler.(*stdioBridge)
}

func TestStdioBridgeEndToEnd(t *testing.T) {
	_, _, session := startBridgedEchoServer(t, "marker-1")

	// initialize happens inside Connect; list tools through the bridge.
	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	require.True(t, names["echo"], "echo tool must be bridged")
	require.True(t, names["explode"], "explode tool must be bridged")

	// tools/call round trip through the subprocess.
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello bridge"},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.Equal(t, "hello bridge", text.Text)
}

func TestStdioBridgeEnvAndStructuredOutput(t *testing.T) {
	_, _, session := startBridgedEchoServer(t, "marker-env")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "x"},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, map[string]any{"echo": "x", "pid_env": "marker-env"}, result.StructuredContent)
}

func TestStdioBridgeToolErrorPropagates(t *testing.T) {
	_, _, session := startBridgedEchoServer(t, "marker-err")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "explode",
		Arguments: map[string]any{},
	})
	require.NoError(t, err, "tool errors arrive as IsError results, not transport errors")
	require.NotNil(t, result)
	require.True(t, result.IsError)
}

func TestStdioBridgeSessionIsolationAndDelete(t *testing.T) {
	server, registry, session1 := startBridgedEchoServer(t, "marker-iso")

	session2 := connectEchoClient(t, server.baseURL+"/mcp/echo")
	stdio := echoBridgedSessions(t, registry)

	// Two client sessions → two subprocesses.
	require.Eventually(t, func() bool {
		return len(stdio.snapshotSessions()) == 2
	}, 10*time.Second, 20*time.Millisecond)

	// Both sessions serve independently.
	r1, err := session1.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "one"}})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"echo": "one", "pid_env": "marker-iso"}, r1.StructuredContent)
	r2, err := session2.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "two"}})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"echo": "two", "pid_env": "marker-iso"}, r2.StructuredContent)

	// Closing session1 (client DELETE) must retire exactly one subprocess.
	require.NoError(t, session1.Close())
	require.Eventually(t, func() bool {
		return len(stdio.snapshotSessions()) == 1
	}, 10*time.Second, 20*time.Millisecond)

	// Session 2 keeps working.
	r2, err = session2.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "still-alive"}})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"echo": "still-alive", "pid_env": "marker-iso"}, r2.StructuredContent)
}

func TestChildEnvironFiltersExecdSecrets(t *testing.T) {
	t.Setenv("EXECD_ACCESS_TOKEN", "sandbox-secret")
	t.Setenv("JUPYTER_TOKEN", "jupyter-secret")
	t.Setenv("MCPECHO_MARKER", "plain-env-value")

	env := childEnviron(map[string]string{"FOO": "bar"})

	has := func(key string) (string, bool) {
		for _, kv := range env {
			if k, v, _ := strings.Cut(kv, "="); k == key {
				return v, true
			}
		}
		return "", false
	}
	for _, secret := range []string{"EXECD_ACCESS_TOKEN", "JUPYTER_TOKEN"} {
		if _, present := has(secret); present {
			t.Errorf("%s leaked into stdio child environment", secret)
		}
	}
	if v, ok := has("FOO"); !ok || v != "bar" {
		t.Error("cfg.Env overlay missing from child environment")
	}
	if _, ok := has("PATH"); !ok {
		t.Error("inherited environment lost")
	}
}

func TestStdioBridgeAdvertisesCompletionCapability(t *testing.T) {
	_, _, session := startBridgedEchoServer(t, "completions")
	res := session.InitializeResult()
	require.NotNil(t, res)
	require.NotNil(t, res.Capabilities)
	require.NotNil(t, res.Capabilities.Completions)
	require.NotNil(t, res.Capabilities.Tools)
}

func TestStdioBridgeChildExitClosesHTTPSession(t *testing.T) {
	_, _, session := startBridgedEchoServer(t, "die-marker")

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "die"})
	require.NoError(t, err)

	// The child exits shortly after; the bridge must close the HTTP session
	// so the next client call fails with a session-missing error instead of
	// talking to a dead bridge forever.
	require.Eventually(t, func() bool {
		_, err := session.ListTools(context.Background(), nil)
		return err != nil
	}, 15*time.Second, 100*time.Millisecond)
}

func TestStdioBridgeSpawnFailureSurfacesAsRPCError(t *testing.T) {
	registry := NewRegistry(0)
	require.NoError(t, registry.Add(&ServerConfig{Name: "broken", Transport: TransportStdio, Command: "/nonexistent/mcp-server-xyz"}))
	defer registry.Close()

	handler, ok := registry.Handler("broken")
	require.True(t, ok)
	server := newTestHTTPServer(handler)
	// Cleanup order (LIFO) matters: the client session must close before
	// the httptest server, or its SSE stream blocks server.Close.
	t.Cleanup(server.server.Close)

	session := connectEchoClient(t, server.baseURL+"/mcp/broken")

	_, err := session.ListTools(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not available")
}

func TestStdioBridgeCloseKillsSubprocesses(t *testing.T) {
	binary := buildEchoServer(t)
	registry := NewRegistry(0)
	require.NoError(t, registry.Add(&ServerConfig{Name: "echo", Transport: TransportStdio, Command: binary}))

	handler, ok := registry.Handler("echo")
	require.True(t, ok)
	stdio := handler.(*stdioBridge)
	server := newTestHTTPServer(handler)
	t.Cleanup(server.server.Close)

	session := connectEchoClient(t, server.baseURL+"/mcp/echo")

	require.Eventually(t, func() bool {
		sessions := stdio.snapshotSessions()
		return len(sessions) == 1 && sessions[0].upstream() != nil
	}, 10*time.Second, 20*time.Millisecond)

	registry.Close()
	require.Eventually(t, func() bool {
		return len(stdio.snapshotSessions()) == 0
	}, 10*time.Second, 20*time.Millisecond)

	// The client observes the session termination on its next call.
	_, err := session.ListTools(context.Background(), nil)
	require.Error(t, err)
}
