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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestBridge(t *testing.T, upstreamURL string) *httpBridge {
	t.Helper()
	cfg := &ServerConfig{Name: "test", Transport: TransportHTTP, URL: upstreamURL}
	bridge, err := newHTTPBridge(cfg, 0)
	require.NoError(t, err)
	return bridge
}

// echoUpstream returns a server that answers with the request facts as JSON,
// emulating what the bridge actually sent.
func echoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"method": r.Method,
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
			"headers": map[string]string{
				"Authorization":        r.Header.Get("Authorization"),
				"Mcp-Session-Id":       r.Header.Get("Mcp-Session-Id"),
				"Mcp-Protocol-Version": r.Header.Get("Mcp-Protocol-Version"),
				"X-Execd-Access-Token": r.Header.Get("X-Execd-Access-Token"),
				"X-Upstream-Auth":      r.Header.Get("X-Upstream-Auth"),
				"Accept":               r.Header.Get("Accept"),
			},
			"body": string(body),
		})
	}))
}

func TestHTTPBridgePassthrough(t *testing.T) {
	upstream := echoUpstream(t)
	defer upstream.Close()
	bridge := newTestBridge(t, upstream.URL+"/mcp")

	server := httptest.NewServer(bridge)
	defer server.Close()

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{"jsonrpc":"2.0","method":"tools/list","id":1}`)
		}
		req, err := http.NewRequest(method, server.URL+"/mcp/test?foo=bar", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Session-Id", "sess-123")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		req.Header.Set("X-Execd-Access-Token", "sandbox-secret")
		req.Header.Set("Authorization", "Bearer upstream-cred")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, method)
		var got struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Query   string            `json:"query"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		require.NoError(t, resp.Body.Close())

		require.Equal(t, method, got.Method)
		// The upstream path is authoritative; query survives.
		require.Equal(t, "/mcp", got.Path)
		require.Equal(t, "foo=bar", got.Query)
		// MCP headers pass through untouched.
		require.Equal(t, "sess-123", got.Headers["Mcp-Session-Id"])
		require.Equal(t, "2025-06-18", got.Headers["Mcp-Protocol-Version"])
		require.Equal(t, "application/json, text/event-stream", got.Headers["Accept"])
		// Client Authorization is forwarded (credential passthrough mode).
		require.Equal(t, "Bearer upstream-cred", got.Headers["Authorization"])
		// The execd token never reaches the upstream.
		require.Empty(t, got.Headers["X-Execd-Access-Token"])
		if method == http.MethodPost {
			require.Contains(t, got.Body, "tools/list")
		}
	}
}

func TestHTTPBridgeConfiguredHeadersOverride(t *testing.T) {
	upstream := echoUpstream(t)
	defer upstream.Close()

	cfg := &ServerConfig{
		Name:      "test",
		Transport: TransportHTTP,
		URL:       upstream.URL + "/mcp",
		Headers:   map[string]string{"X-Upstream-Auth": "injected", "Authorization": "Bearer platform"},
	}
	bridge, err := newHTTPBridge(cfg, 0)
	require.NoError(t, err)

	server := httptest.NewServer(bridge)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp/test", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer client-attempt")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var got struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "injected", got.Headers["X-Upstream-Auth"])
	// Configured credentials win over client-supplied ones.
	require.Equal(t, "Bearer platform", got.Headers["Authorization"])
}

func TestHTTPBridgeSSEStreamsUnbuffered(t *testing.T) {
	firstArrived := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"first\":true}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(firstArrived)
		// Hold the stream open: the client must receive the first event
		// before this second frame is written.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			fmt.Fprint(w, "event: message\ndata: {\"second\":true}\n\n")
		}
	}))
	defer upstream.Close()

	bridge := newTestBridge(t, upstream.URL+"/mcp")
	server := httptest.NewServer(bridge)
	defer server.Close()

	ctx := server.Client() // default client has no timeout
	req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp/test", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := ctx.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// First event must arrive while the upstream stream is still open.
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "event: message\n", line)
	select {
	case <-firstArrived:
	default:
		t.Fatal("bridge buffered the SSE response; first event not forwarded")
	}
}

func TestHTTPBridgeKeepaliveInjectedOnIdleSSE(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"start\":true}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the stream open silently until the test is done.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)

	bridge, err := newHTTPBridge(&ServerConfig{
		Name:      "test",
		Transport: TransportHTTP,
		URL:       upstream.URL + "/mcp",
	}, 10*time.Millisecond)
	require.NoError(t, err)
	server := httptest.NewServer(bridge)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp/test", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read until several keepalive comment frames arrive.
	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	keepalives := 0
	for keepalives < 3 && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if line == ": keepalive\n" {
			keepalives++
		}
	}
	require.GreaterOrEqual(t, keepalives, 3, "expected periodic SSE keepalive frames")
}

func TestHTTPBridgeUpstreamErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			// Session-terminated semantics.
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer upstream.Close()

	bridge := newTestBridge(t, upstream.URL)
	server := httptest.NewServer(bridge)
	defer server.Close()

	// 404 (session terminated semantics) must reach the client unmodified.
	resp, err := server.Client().Post(server.URL+"/mcp/test", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// 405 semantics too.
	resp, err = server.Client().Get(server.URL + "/mcp/test")
	require.NoError(t, err)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}

func TestHTTPBridgeBadGatewayWhenUpstreamDown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close() // port now closed

	bridge := newTestBridge(t, url)
	server := httptest.NewServer(bridge)
	defer server.Close()

	resp, err := server.Client().Post(server.URL+"/mcp/test", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}
