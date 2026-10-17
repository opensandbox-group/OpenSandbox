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

package opensandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capturedHeaders records the headers of each request a fake proxy receives.
type capturedHeaders struct {
	mu     sync.Mutex
	values []http.Header
}

func (c *capturedHeaders) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values, r.Header.Clone())
}

func (c *capturedHeaders) first(t *testing.T) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.values, "proxy received no request")
	return c.values[0]
}

// countHeaderFold counts entries whose name matches name under any HTTP casing.
func countHeaderFold(headers map[string]string, name string) int {
	count := 0
	for key := range headers {
		if strings.EqualFold(key, name) {
			count++
		}
	}
	return count
}

// serverProxyFixture publishes proxyURL from a fake lifecycle server so the
// SDK resolves every data-plane endpoint through it.
func serverProxyFixture(
	t *testing.T,
	proxyURL string,
	endpointHeaders map[string]string,
	useServerProxy bool,
) *Sandbox {
	t.Helper()
	lifecycle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/") {
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: proxyURL,
				Headers:  endpointHeaders,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(lifecycle.Close)

	config := ConnectionConfig{
		Domain:         lifecycle.URL,
		APIKey:         "tenant-key-1",
		UseServerProxy: useServerProxy,
	}
	return &Sandbox{
		id:        "sbx-proxy-auth",
		config:    &config,
		lifecycle: config.lifecycleClient(),
	}
}

// TestResolveExecdProxyModeAuthenticatesWithServerHeader pins the proxy-mode
// data-plane auth contract shared by every SDK: the request carries the tenant
// key in OPEN-SANDBOX-API-KEY (the only header the server's auth middleware
// reads), and never smuggles that key into the sandbox under the execd token
// header.
func TestResolveExecdProxyModeAuthenticatesWithServerHeader(t *testing.T) {
	var proxyHeaders capturedHeaders
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHeaders.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	sb := serverProxyFixture(t, proxy.URL, nil, true)

	require.NoErrorf(t, sb.resolveExecd(context.Background()), "resolveExecd")
	require.NoErrorf(t, sb.execd.Ping(context.Background()), "Ping")

	headers := proxyHeaders.first(t)
	require.Equal(t, "", headers.Get("X-EXECD-ACCESS-TOKEN"),
		"the tenant API key must never be sent into the sandbox as an execd token")
	require.Equal(t, "tenant-key-1", headers.Get("OPEN-SANDBOX-API-KEY"),
		"server-proxy requests must authenticate with OPEN-SANDBOX-API-KEY")
}

// TestResolveExecdProxyModeKeepsEndpointHeaders ensures the proxy auth fix
// still forwards the endpoint-resolved execd credential and routing headers
// returned by the lifecycle API.
func TestResolveExecdProxyModeKeepsEndpointHeaders(t *testing.T) {
	var proxyHeaders capturedHeaders
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHeaders.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	sb := serverProxyFixture(t, proxy.URL, map[string]string{
		"X-EXECD-ACCESS-TOKEN": "sandbox-execd-token",
		"X-Route-Hint":         "vip-pool",
	}, true)

	require.NoErrorf(t, sb.resolveExecd(context.Background()), "resolveExecd")
	require.NoErrorf(t, sb.execd.Ping(context.Background()), "Ping")

	headers := proxyHeaders.first(t)
	require.Equal(t, "tenant-key-1", headers.Get("OPEN-SANDBOX-API-KEY"))
	require.Equal(t, "sandbox-execd-token", headers.Get("X-EXECD-ACCESS-TOKEN"))
	require.Equal(t, "vip-pool", headers.Get("X-Route-Hint"))
}

// TestResolveExecdDirectModeCarriesNoTenantKey pins the security boundary in
// the opposite direction: direct requests never carry the tenant key, whether
// under the server header or the execd token header.
func TestResolveExecdDirectModeCarriesNoTenantKey(t *testing.T) {
	var directHeaders capturedHeaders
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHeaders.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer direct.Close()

	sb := serverProxyFixture(t, direct.URL, nil, false)

	require.NoErrorf(t, sb.resolveExecd(context.Background()), "resolveExecd")
	require.NoErrorf(t, sb.execd.Ping(context.Background()), "Ping")

	headers := directHeaders.first(t)
	require.Equal(t, "", headers.Get("OPEN-SANDBOX-API-KEY"))
	require.Equal(t, "", headers.Get("X-EXECD-ACCESS-TOKEN"))
}

// TestResolveEgressProxyModeAuthenticatesWithServerHeader covers the egress
// sidecar path, which had the same wrong-header fallback as execd.
func TestResolveEgressProxyModeAuthenticatesWithServerHeader(t *testing.T) {
	var proxyHeaders capturedHeaders
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHeaders.record(r)
		jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
	}))
	defer proxy.Close()

	sb := serverProxyFixture(t, proxy.URL, map[string]string{
		"OPENSANDBOX-EGRESS-AUTH": "egress-token",
	}, true)

	require.NoErrorf(t, sb.resolveEgress(context.Background()), "resolveEgress")
	_, err := sb.egress.GetPolicy(context.Background())
	require.NoErrorf(t, err, "GetPolicy")

	headers := proxyHeaders.first(t)
	require.Equal(t, "egress-token", headers.Get("OPENSANDBOX-EGRESS-AUTH"),
		"the endpoint-resolved egress credential must be preserved")
	require.Equal(t, "tenant-key-1", headers.Get("OPEN-SANDBOX-API-KEY"))
}

// TestResolveEgressProxyModeDoesNotSmuggleTenantKey covers the case where the
// lifecycle response has no egress credential: the SDK must not fill that
// header with the tenant API key.
func TestResolveEgressProxyModeDoesNotSmuggleTenantKey(t *testing.T) {
	var proxyHeaders capturedHeaders
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHeaders.record(r)
		jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
	}))
	defer proxy.Close()

	sb := serverProxyFixture(t, proxy.URL, nil, true)

	require.NoErrorf(t, sb.resolveEgress(context.Background()), "resolveEgress")
	_, err := sb.egress.GetPolicy(context.Background())
	require.NoErrorf(t, err, "GetPolicy")

	headers := proxyHeaders.first(t)
	require.Equal(t, "", headers.Get("OPENSANDBOX-EGRESS-AUTH"),
		"the tenant API key must never be sent to the egress sidecar as its auth token")
	require.Equal(t, "tenant-key-1", headers.Get("OPEN-SANDBOX-API-KEY"))
}

// TestResolveExecdProxyModeKeepsEndpointProxyKeyCasing covers an endpoint that
// already supplies the proxy auth header under a non-canonical casing: the
// endpoint value must win and the SDK must not add a second, conflicting entry.
func TestResolveExecdProxyModeKeepsEndpointProxyKeyCasing(t *testing.T) {
	var proxyHeaders capturedHeaders
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHeaders.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	sb := serverProxyFixture(t, proxy.URL, map[string]string{
		"open-sandbox-api-key": "endpoint-key",
	}, true)

	require.NoErrorf(t, sb.resolveExecd(context.Background()), "resolveExecd")
	require.NoErrorf(t, sb.execd.Ping(context.Background()), "Ping")

	require.Equal(t, 1, countHeaderFold(sb.execd.client.headers, "OPEN-SANDBOX-API-KEY"),
		"an endpoint-provided proxy key must not be duplicated by the fallback")
	require.Equal(t, "endpoint-key", proxyHeaders.first(t).Get("OPEN-SANDBOX-API-KEY"))
}

// TestResolveEgressProxyModeKeepsEndpointProxyKeyCasing is the egress twin of
// the execd casing test.
func TestResolveEgressProxyModeKeepsEndpointProxyKeyCasing(t *testing.T) {
	var proxyHeaders capturedHeaders
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHeaders.record(r)
		jsonResponse(w, http.StatusOK, PolicyStatusResponse{Status: "ok"})
	}))
	defer proxy.Close()

	sb := serverProxyFixture(t, proxy.URL, map[string]string{
		"open-sandbox-api-key": "endpoint-key",
	}, true)

	require.NoErrorf(t, sb.resolveEgress(context.Background()), "resolveEgress")
	_, err := sb.egress.GetPolicy(context.Background())
	require.NoErrorf(t, err, "GetPolicy")

	require.Equal(t, 1, countHeaderFold(sb.egress.headers, "OPEN-SANDBOX-API-KEY"),
		"an endpoint-provided proxy key must not be duplicated by the fallback")
	require.Equal(t, "endpoint-key", proxyHeaders.first(t).Get("OPEN-SANDBOX-API-KEY"))
}
