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

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/mcpproxy"
)

func testMCPRegistry(t *testing.T, upstreamURL string) *mcpproxy.Registry {
	t.Helper()
	registry := mcpproxy.NewRegistry(0)
	require.NoError(t, registry.Add(&mcpproxy.ServerConfig{Name: "alpha", Transport: mcpproxy.TransportHTTP, URL: upstreamURL + "/mcp"}))
	t.Cleanup(registry.Close)
	return registry
}

func doMCPRequest(r *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMCPRoutesIndexListsServers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	r := NewRouter("", testMCPRegistry(t, upstream.URL))
	w := doMCPRequest(r, http.MethodGet, "/mcp", "", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var servers []mcpproxy.ServerInfo
	require.NoError(t, json.NewDecoder(w.Body).Decode(&servers))
	require.Equal(t, []mcpproxy.ServerInfo{{Name: "alpha", Transport: "http"}}, servers)
}

func TestMCPRoutesIndexEmptyWhenUnconfigured(t *testing.T) {
	r := NewRouter("", nil)
	w := doMCPRequest(r, http.MethodGet, "/mcp", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[]`, w.Body.String())
}

func TestMCPRoutesForwardToBridge(t *testing.T) {
	var seenPath, seenMethod, seenSession, seenToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		seenSession = r.Header.Get("Mcp-Session-Id")
		seenToken = r.Header.Get("X-Execd-Access-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer upstream.Close()

	r := NewRouter("", testMCPRegistry(t, upstream.URL))
	w := doMCPRequest(r, http.MethodPost, "/mcp/alpha", `{"jsonrpc":"2.0","method":"initialize","id":1}`, map[string]string{
		"Content-Type":         "application/json",
		"Mcp-Session-Id":       "abc",
		"X-EXECD-ACCESS-TOKEN": "sandbox-token",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"jsonrpc":"2.0"`)

	require.Equal(t, "/mcp", seenPath)
	require.Equal(t, http.MethodPost, seenMethod)
	require.Equal(t, "abc", seenSession)
	// The sandbox token must not leak to the upstream.
	require.Empty(t, seenToken)
}

func TestMCPRoutesUnknownServerAndMethods(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	r := NewRouter("", testMCPRegistry(t, upstream.URL))

	w := doMCPRequest(r, http.MethodPost, "/mcp/unknown", "", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "unknown mcp server")

	// Only GET/POST/DELETE are bridged.
	w = doMCPRequest(r, http.MethodPatch, "/mcp/alpha", "", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestMCPRoutesRequireAccessToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := NewRouter("tok", testMCPRegistry(t, upstream.URL))

	w := doMCPRequest(r, http.MethodPost, "/mcp/alpha", "", nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w = doMCPRequest(r, http.MethodGet, "/mcp/alpha", "", map[string]string{"X-EXECD-ACCESS-TOKEN": "tok"})
	require.Equal(t, http.StatusOK, w.Code)
}

func TestMCPRoutesDynamicServerLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"dynamic":true}}`))
	}))
	defer upstream.Close()

	r := NewRouter("", testMCPRegistry(t, upstream.URL))

	// List shows the registered server.
	w := doMCPRequest(r, http.MethodGet, "/mcp/servers", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[{"name":"alpha","transport":"http"}]`, w.Body.String())

	// Register another upstream.
	body := `{"name":"dyn","transport":"http","url":"` + upstream.URL + `/mcp"}`
	w = doMCPRequest(r, http.MethodPost, "/mcp/servers", body, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusCreated, w.Code)
	require.JSONEq(t, `{"name":"dyn","transport":"http"}`, w.Body.String())

	// The new server is bridgeable immediately.
	w = doMCPRequest(r, http.MethodPost, "/mcp/dyn", `{"jsonrpc":"2.0","method":"initialize","id":1}`, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"dynamic":true`)

	// Both servers are listed.
	w = doMCPRequest(r, http.MethodGet, "/mcp/servers", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[{"name":"alpha","transport":"http"},{"name":"dyn","transport":"http"}]`, w.Body.String())

	// Remove it; dispatch answers 404 again.
	w = doMCPRequest(r, http.MethodDelete, "/mcp/servers/dyn", "", nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	w = doMCPRequest(r, http.MethodPost, "/mcp/dyn", "", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestMCPRoutesManageServerErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	r := NewRouter("", testMCPRegistry(t, upstream.URL))

	// Invalid JSON body.
	w := doMCPRequest(r, http.MethodPost, "/mcp/servers", `{`, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusBadRequest, w.Code)

	// Validation failures.
	w = doMCPRequest(r, http.MethodPost, "/mcp/servers", `{"name":"no-url","transport":"http"}`, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	w = doMCPRequest(r, http.MethodPost, "/mcp/servers", `{"name":"servers","transport":"http","url":"http://127.0.0.1:9/mcp"}`, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "reserved")

	// Duplicate of a registered server.
	w = doMCPRequest(r, http.MethodPost, "/mcp/servers", `{"name":"alpha","transport":"http","url":"`+upstream.URL+`/mcp"}`, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusConflict, w.Code)

	// Unknown removal.
	w = doMCPRequest(r, http.MethodDelete, "/mcp/servers/missing", "", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestMCPRoutesManageServersRequireAccessToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	r := NewRouter("tok", testMCPRegistry(t, upstream.URL))

	w := doMCPRequest(r, http.MethodPost, "/mcp/servers", `{"name":"dyn","transport":"http","url":"http://127.0.0.1:9/mcp"}`, nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w = doMCPRequest(r, http.MethodDelete, "/mcp/servers/alpha", "", nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w = doMCPRequest(r, http.MethodGet, "/mcp/servers", "", map[string]string{"X-EXECD-ACCESS-TOKEN": "tok"})
	require.Equal(t, http.StatusOK, w.Code)
}
