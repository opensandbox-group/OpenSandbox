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

func (stubNoSecureProvider) Start(context.Context) error { return nil }

// Test_WatchPods is removed as we now use BatchSandbox Provider instead of direct Pod watching

func TestIsWebSocketRequest(t *testing.T) {
	proxy := &Proxy{}

	// Valid websocket request
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	assert.True(t, proxy.isWebSocketRequest(req))

	// Missing upgrade headers
	req = httptest.NewRequest(http.MethodGet, "/ws", nil)
	assert.False(t, proxy.isWebSocketRequest(req))

	// Wrong method
	req = httptest.NewRequest(http.MethodPost, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
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
