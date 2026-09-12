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

package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/stretchr/testify/assert"
)

// stubNoSecureProvider is a minimal sandbox.Provider for unit tests (no access-token / secure routing).
type stubNoSecureProvider struct{}

func (stubNoSecureProvider) GetEndpoint(string) (*sandbox.EndpointInfo, error) {
	return &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}, nil
}

func (p stubNoSecureProvider) ResolveEndpoint(_ context.Context, target sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	return p.GetEndpoint(target.SandboxID)
}

func (stubNoSecureProvider) Start(context.Context) error { return nil }

// Test_WatchPods is removed as we now use BatchSandbox Provider instead of direct Pod watching

func TestIsWebSocketRequest(t *testing.T) {
	proxy := &Proxy{}

	// The canonical HTTP/1.1 upgrade shape.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	// Some L7 proxies (Envoy, older HAProxy) merge the client's
	// "Connection: Upgrade" with their own "keep-alive" hop, producing a
	// token list. RFC 7230 §6.1 allows this and gorilla's strict equality
	// missed it; the new matcher must accept it.
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "keep-alive, Upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	// Case-insensitive token matching for both Upgrade and Connection.
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "WebSocket")
	req.Header.Set("Connection", "upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	// No upgrade headers → plain HTTP request.
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	assert.False(t, proxy.isWebSocketRequest(req))

	// GET with Upgrade but wrong Connection token → still HTTP.
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "keep-alive")
	assert.False(t, proxy.isWebSocketRequest(req))

	// POST with upgrade headers is not a WebSocket handshake.
	req = httptest.NewRequest(http.MethodPost, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	assert.False(t, proxy.isWebSocketRequest(req))
}

// TestIsWebSocketRequestRejectsH2ExtendedConnect documents the ingress's
// current inability to accept RFC 8441 HTTP/2 WebSocket upgrades. coder/
// websocket v1.8.15 (accept.go:184-201) requires Method==GET and Upgrade/
// Connection headers, so an h2 CONNECT + :protocol=websocket request cannot
// complete the WebSocket handshake even if we routed it as a WebSocket
// request. Operators must configure the L7 frontend to translate h2 into h1
// or downgrade to h1 (see docs/components/ingress.md).
func TestIsWebSocketRequestRejectsH2ExtendedConnect(t *testing.T) {
	proxy := &Proxy{}

	// h2 Extended CONNECT with :protocol=websocket → NOT a WebSocket
	// upgrade path for this ingress. We reject at isWebSocketRequest so the
	// request flows through the plain HTTP reverse proxy and either the L7
	// misconfiguration is surfaced as a normal HTTP response or the backend
	// rejects the request itself.
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.ProtoMajor = 2
	req.Header.Set(":protocol", "websocket")
	assert.False(t, proxy.isWebSocketRequest(req))

	// h2 CONNECT without :protocol (a plain RFC 7230 CONNECT tunnel) also
	// remains outside the WebSocket path.
	req = httptest.NewRequest(http.MethodConnect, "example.com:443", nil)
	req.ProtoMajor = 2
	assert.False(t, proxy.isWebSocketRequest(req))
}

func TestParseHostRoute(t *testing.T) {
	pr, err := parseHostRoute("sandbox-1234.example.com")
	assert.NoError(t, err)
	assert.Equal(t, "sandbox", pr.sandboxID)
	assert.Equal(t, 1234, pr.port)

	pr, err = parseHostRoute("https://alpha-beta-8080.sandbox.test")
	assert.NoError(t, err)
	assert.Equal(t, "alpha-beta", pr.sandboxID)
	assert.Equal(t, 8080, pr.port)

	_, err = parseHostRoute("invalidhost")
	assert.Error(t, err)

	_, err = parseHostRoute("-1234.example.com")
	assert.Error(t, err)
}

func TestGetSandboxHostRejectsZeroPortWithStableError(t *testing.T) {
	proxy := NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "http://myhost-0.example.com/", nil)
	_, status, err := proxy.getSandboxHostDefinition(request)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.EqualError(t, err, "invalid ingress route: missing sandbox ID or port")
}

func TestGetClientIP(t *testing.T) {
	proxy := &Proxy{}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	assert.Equal(t, "192.0.2.1", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(XRealIP, "203.0.113.5")
	assert.Equal(t, "203.0.113.5", proxy.getClientIP(req))

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set(XForwardedFor, "10.0.0.1, 198.51.100.2")
	assert.Equal(t, "10.0.0.1", proxy.getClientIP(req))
}

func findAvailablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	return port, nil
}
