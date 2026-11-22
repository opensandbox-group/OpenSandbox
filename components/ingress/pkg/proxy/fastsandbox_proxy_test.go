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

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/ingress/pkg/proxy/connectivity"
	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

const fsbScopeVector = "f1.dGVuYW50LWE.c2FuZGJveC0xMjM.44772.k.gtJzW337dCO-kStxh2GPfA"

func init() {
	Logger = slogger.MustNew(slogger.Config{Level: "error"})
}

type fsbProxyProvider struct {
	info        *sandbox.EndpointInfo
	err         error
	target      sandbox.EndpointTarget
	invalidated sandbox.EndpointTarget
}

func (p *fsbProxyProvider) Invalidate(target sandbox.EndpointTarget) {
	p.invalidated = target
}

func (p *fsbProxyProvider) Start(context.Context) error { return nil }

func (*fsbProxyProvider) RequiresAuthenticatedRouteScope() {}

func (p *fsbProxyProvider) ResolveEndpoint(_ context.Context, target sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	p.target = target
	if p.err != nil {
		return nil, p.err
	}
	return p.info, nil
}

func TestFastSandboxWildcardHostRouting(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/a%2Fb", r.URL.EscapedPath())
		require.Equal(t, "watch=true", r.URL.RawQuery)
		require.Equal(t, "Bearer application-token", r.Header.Get("Authorization"))
		require.Equal(t, "issued-credential", r.Header.Get(sandbox.FastSandboxCredential))
		require.Empty(t, r.Header.Get(SandboxIngress))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	for _, mixed := range []bool{false, true} {
		for _, host := range []string{"fsb-123-44772.example.com", "FSB-123-44772.EXAMPLE.COM:28888"} {
			t.Run(fmt.Sprintf("mixed=%v/host=%s", mixed, host), func(t *testing.T) {
				fsb := &fsbProxyProvider{info: &sandbox.EndpointInfo{
					UpstreamURL:     backend.URL + "/route",
					UpstreamHeaders: http.Header{sandbox.FastSandboxCredential: []string{"issued-credential"}},
				}}
				var provider sandbox.Provider = fsb
				if mixed {
					provider = sandbox.NewCompositeProvider(staticEndpointProvider{}, fsb)
				}
				p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil, WithFastSandboxNamespace("tenant-a"))
				request := httptest.NewRequest(http.MethodGet, "http://"+host+"/a%2Fb?watch=true", nil)
				request.Header.Set("Authorization", "Bearer application-token")
				request.Header.Set("X-Namespace", "tenant-b")
				request.Header.Set(sandbox.FastSandboxCredential, "caller-spoof")
				response := httptest.NewRecorder()
				p.ServeHTTP(response, request)
				require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
				require.Equal(t, sandbox.EndpointTarget{RouteKind: sandbox.RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "fsb-123", Port: 44772}, fsb.target)
			})
		}
	}
}

func TestFastSandboxWildcardRoutingViaRoutingHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get(SandboxIngress))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	fsb := &fsbProxyProvider{info: &sandbox.EndpointInfo{
		UpstreamURL: backend.URL,
	}}
	p := NewProxy(context.Background(), fsb, ModeHeader, nil, nil, nil, WithFastSandboxNamespace("tenant-a"))
	request := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/", nil)
	// A front proxy that owns the external wildcard domain derives the route
	// label from the Host and forwards it as the routing header.
	request.Header.Set(SandboxIngress, "fsb-123-44772.example.com")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	require.Equal(t, sandbox.EndpointTarget{RouteKind: sandbox.RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "fsb-123", Port: 44772}, fsb.target)
}

func TestFastSandboxWildcardRouteLabelIgnoresCase(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, test := range []struct {
			name, url, header, value string
		}{
			{name: "host", url: "http://FSB-123-44772.example.com/"},
			{name: "routing header", url: "http://ingress/", header: SandboxIngress, value: "FSB-123-44772.example.com"},
			{name: "legacy routing header", url: "http://ingress/", header: DeprecatedSandboxIngress, value: "Fsb-123-44772"},
		} {
			t.Run(fmt.Sprintf("mixed=%v/%s", mixed, test.name), func(t *testing.T) {
				fsb := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: "http://fastlet/route"}}
				var provider sandbox.Provider = fsb
				if mixed {
					provider = sandbox.NewCompositeProvider(staticEndpointProvider{}, fsb)
				}
				p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil, WithFastSandboxNamespace("tenant-a"))
				request := httptest.NewRequest(http.MethodGet, test.url, nil)
				if test.header != "" {
					request.Header.Set(test.header, test.value)
				}
				_, status, err := p.doGetSandboxHostDefinition(request)
				require.Zero(t, status)
				require.NoError(t, err)
				require.Equal(t, sandbox.EndpointTarget{RouteKind: sandbox.RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "fsb-123", Port: 44772}, fsb.target)
			})
		}
	}
}

func TestFastSandboxNamespaceBoundary(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, test := range []struct {
			name, namespace, url, header, value string
			mode                                Mode
			keys                                bool
			status                              int
		}{
			{name: "wildcard not enabled", url: "http://fsb-123-44772.example.com/", mode: ModeHeader, status: 401},
			{name: "unsigned header allowed", namespace: "tenant-a", url: "http://ingress/", header: SandboxIngress, value: "fsb-123-44772", mode: ModeHeader, status: 0},
			{name: "unsigned legacy header allowed", namespace: "tenant-a", url: "http://ingress/", header: DeprecatedSandboxIngress, value: "fsb-123-44772", mode: ModeHeader, status: 0},
			{name: "unsigned URI", namespace: "tenant-a", url: "http://ingress/fsb-123/44772/path", mode: ModeURI, status: 401},
			{name: "wrong namespace header", namespace: "tenant-b", url: "http://ingress/", header: SandboxIngress, value: fsbScopeVector, mode: ModeHeader, keys: true, status: 401},
			{name: "wrong namespace URI", namespace: "tenant-b", url: "http://ingress/" + fsbScopeVector + "/path", mode: ModeURI, keys: true, status: 401},
			{name: "signed header allowed", namespace: "tenant-a", url: "http://ingress/", header: SandboxIngress, value: fsbScopeVector, mode: ModeHeader, keys: true, status: 0},
			{name: "signed URI allowed", namespace: "tenant-a", url: "http://ingress/" + fsbScopeVector + "/path", mode: ModeURI, keys: true, status: 0},
			{name: "signed without keys", namespace: "tenant-a", url: "http://ingress/", header: SandboxIngress, value: fsbScopeVector, mode: ModeHeader, status: 401},
			{name: "tampered signature", namespace: "tenant-a", url: "http://fsb-123-44772.example.com/", header: SandboxIngress, value: fsbScopeVector + "x", mode: ModeHeader, keys: true, status: 401},
			{name: "invalid port", namespace: "tenant-a", url: "http://fsb-123-65536.example.com/", mode: ModeHeader, status: 400},
		} {
			t.Run(fmt.Sprintf("mixed=%v/%s", mixed, test.name), func(t *testing.T) {
				fsb := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: "http://fastlet/route"}}
				var provider sandbox.Provider = fsb
				if mixed {
					provider = sandbox.NewCompositeProvider(staticEndpointProvider{}, fsb)
				}
				var verifier *routescope.Verifier
				if test.keys {
					verifier = &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
				}
				p := NewProxy(context.Background(), provider, test.mode, nil, nil, verifier, WithFastSandboxNamespace(test.namespace))
				request := httptest.NewRequest(http.MethodGet, test.url, nil)
				if test.header != "" {
					request.Header.Set(test.header, test.value)
				}
				_, status, err := p.doGetSandboxHostDefinition(request)
				require.Equal(t, test.status, status)
				if test.status == 0 {
					require.NoError(t, err)
					require.Equal(t, "tenant-a", fsb.target.Namespace)
				} else {
					require.Error(t, err)
					require.Empty(t, fsb.target)
				}
			})
		}
	}
}

func TestFastSandboxProxyPreservesPathQueryAndApplicationAuthorization(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/sandboxes/uid/components/execd/api/health", r.URL.Path)
		require.Equal(t, "watch=true", r.URL.RawQuery)
		require.Equal(t, "Bearer application-token", r.Header.Get("Authorization"))
		require.Equal(t, "issued-credential", r.Header.Get(sandbox.FastSandboxCredential))
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{
		UpstreamURL: backend.URL + "/v2/sandboxes/uid/components/execd",
		UpstreamHeaders: http.Header{
			sandbox.FastSandboxCredential: []string{"issued-credential"},
		},
	}}
	p := NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)

	request := httptest.NewRequest(http.MethodGet, "http://ingress/api/health?watch=true", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	request.Header.Set("Authorization", "Bearer application-token")
	request.Header.Set(sandbox.FastSandboxCredential, "caller-spoof")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "ok", response.Body.String())
	require.Equal(t, sandbox.EndpointTarget{RouteKind: sandbox.RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "sandbox-123", Port: sandbox.ExecdPort}, provider.target)
}

func TestFastSandboxProxyStripsCallerCredentialWhenProviderOmitsIt(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get(sandbox.FastSandboxCredential))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	request.Header.Set(sandbox.FastSandboxCredential, "caller-spoof")
	response := httptest.NewRecorder()

	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestCompositeProviderServesLegacyAndFastSandboxRoutes(t *testing.T) {
	legacyBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Backend", "legacy")
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
	}))
	defer legacyBackend.Close()
	fsbBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Backend", "fsb")
	}))
	defer fsbBackend.Close()

	legacyAddress := legacyBackend.Listener.Addr().(*net.TCPAddr)
	legacy := staticEndpointProvider{byID: map[string]sandbox.EndpointInfo{
		"legacy-sandbox": {Endpoint: legacyAddress.IP.String()},
	}}
	fsb := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: fsbBackend.URL}}
	provider := sandbox.NewCompositeProvider(legacy, fsb)
	ingress := httptest.NewServer(NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
		WithFastSandboxNamespace("tenant-a"),
	))
	defer ingress.Close()

	legacyRoute := fmt.Sprintf("legacy-sandbox-%d", legacyAddress.Port)
	for _, test := range []struct {
		route   string
		backend string
	}{{legacyRoute, "legacy"}, {fsbScopeVector, "fsb"}, {legacyRoute, "legacy"}} {
		request, err := http.NewRequest(http.MethodGet, ingress.URL, nil)
		require.NoError(t, err)
		request.Header.Set(SandboxIngress, test.route)
		response, err := ingress.Client().Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, test.backend, response.Header.Get("X-Test-Backend"))
		require.NoError(t, response.Body.Close())
	}
	require.Equal(t, sandbox.RouteKindFastSandbox, fsb.target.RouteKind)
}

func TestFastSandboxProxyPreservesEscapedSlashInHeaderPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/a/b", r.URL.Path)
		require.Equal(t, "/route/a%2Fb", r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL + "/route"}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/a%2Fb", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFastSandboxProxyPreservesRawQueryHash(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "a=1#b=2", r.URL.RawQuery)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/path", nil)
	request.URL.RawQuery = "a=1#b=2"
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFastSandboxProxyURIScopeRemovesInternalPrefix(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/user/path", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL + "/route"}}
	p := NewProxy(
		context.Background(), provider, ModeURI, nil, nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)
	request := httptest.NewRequest(http.MethodGet, "http://ingress/"+fsbScopeVector+"/user/path", nil)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFastSandboxProxyURIScopePreservesEscapedSlash(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/a/b", r.URL.Path)
		require.Equal(t, "/route/a%2Fb", r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL + "/route"}}
	p := NewProxy(context.Background(), provider, ModeURI, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/"+fsbScopeVector+"/a%2Fb", nil)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFastSandboxProxyMapsPendingAndUnsupported(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "pending", err: fmt.Errorf("%w: assignment pending", sandbox.ErrSandboxNotReady), wantStatus: http.StatusServiceUnavailable},
		{name: "phase 1a egress", err: fmt.Errorf("%w: egress", sandbox.ErrTargetUnsupported), wantStatus: http.StatusNotImplemented},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &fsbProxyProvider{err: test.err}
			p := NewProxy(
				context.Background(), provider, ModeHeader, nil, nil,
				&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
			)
			request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
			request.Header.Set(SandboxIngress, fsbScopeVector)
			response := httptest.NewRecorder()
			p.ServeHTTP(response, request)
			require.Equal(t, test.wantStatus, response.Code)
			if test.wantStatus == http.StatusServiceUnavailable {
				require.Equal(t, "1", response.Header().Get("Retry-After"))
			}
		})
	}
}

func TestFastSandboxProxyRejectsTamperedScopeBeforeProviderLookup(t *testing.T) {
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}}
	p := NewProxy(
		context.Background(), provider, ModeHeader, nil, nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, "f1.dGVuYW50LWI.c2FuZGJveC0xMjM.44772.k.gtJzW337dCO-kStxh2GPfA")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, sandbox.EndpointTarget{}, provider.target)
}

func TestFastSandboxProxyRejectsLegacyUnsignedRoute(t *testing.T) {
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, "sandbox-123-44772")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, sandbox.EndpointTarget{}, provider.target)
}

func TestFastSandboxProxyInvalidatesStaleRouteWithoutReplayingRequest(t *testing.T) {
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("ETag", `"internal"`)
		w.Header().Set("Location", "http://fastlet.internal/error")
		w.Header().Set("Set-Cookie", "internal=value")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("internal fastlet detail"))
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodPost, "http://ingress/mutate", strings.NewReader("body"))
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "1", response.Header().Get("Retry-After"))
	require.Empty(t, response.Header().Get(sandbox.FastSandboxProxyError))
	require.Empty(t, response.Header().Get("Content-Encoding"))
	require.Empty(t, response.Header().Get("Content-Type"))
	require.Empty(t, response.Header().Get("ETag"))
	require.Empty(t, response.Header().Get("Location"))
	require.Empty(t, response.Header().Get("Set-Cookie"))
	require.Empty(t, response.Body.String())
	require.Equal(t, 1, requests)
	require.Equal(t, provider.target, provider.invalidated)
}

func TestFastSandboxProxyIgnoresStaleMarkerOnSuccessfulResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()

	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "ok", response.Body.String())
	require.Equal(t, sandbox.EndpointTarget{}, provider.invalidated)
}

func TestFastSandboxProxyInvalidatesRouteOnUpstreamConnectionFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: upstream}}
	observations := make(chan connectivity.Observation, 1)
	p := NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
		WithConnectObserver(connectivity.ObserverFunc(func(observation connectivity.Observation) {
			observations <- observation
		})),
	)
	request := httptest.NewRequest(http.MethodPost, "http://ingress/mutate", strings.NewReader("body"))
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Equal(t, provider.target, provider.invalidated)
	observation := <-observations
	require.Equal(t, connectivity.ResultRefused, observation.Result)
	require.Equal(t, "http", observation.Protocol)
}

func TestFastSandboxProxyWebSocketUsesBasePathAndUpstreamCredential(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/ws", r.URL.Path)
		require.Equal(t, "issued-credential", r.Header.Get(sandbox.FastSandboxCredential))
		require.Equal(t, "Bearer application-token", r.Header.Get("Authorization"))
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		require.NoError(t, err)
		defer func() { _ = conn.CloseNow() }()
		msgType, msg, readErr := conn.Read(context.Background())
		require.NoError(t, readErr)
		require.NoError(t, conn.Write(context.Background(), msgType, msg))
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{
		UpstreamURL:     backend.URL + "/route",
		UpstreamHeaders: http.Header{sandbox.FastSandboxCredential: []string{"issued-credential"}},
	}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	headers := http.Header{SandboxIngress: []string{fsbScopeVector}, "Authorization": []string{"Bearer application-token"}}
	conn, _, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(ingress.URL, "http")+"/ws",
		&websocket.DialOptions{HTTPHeader: headers},
	)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte("hello")))
	_, message, err := conn.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, "hello", string(message))
}

func TestFastSandboxWildcardWebSocket(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/ws", r.URL.Path)
		require.Equal(t, "issued-credential", r.Header.Get(sandbox.FastSandboxCredential))
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		require.NoError(t, err)
		defer conn.CloseNow()
		msgType, msg, err := conn.Read(r.Context())
		require.NoError(t, err)
		require.NoError(t, conn.Write(r.Context(), msgType, msg))
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{
		UpstreamURL:     backend.URL + "/route",
		UpstreamHeaders: http.Header{sandbox.FastSandboxCredential: []string{"issued-credential"}},
	}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil, WithFastSandboxNamespace("tenant-a")))
	defer ingress.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, ingress.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	conn, _, err := websocket.Dial(context.Background(), "ws://fsb-123-44772.example.com/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	defer conn.CloseNow()
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte("hello")))
	_, message, err := conn.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, "hello", string(message))
	require.Equal(t, "tenant-a", provider.target.Namespace)
}

func TestFastSandboxProxyWebSocketHandshakeInvalidatesStaleRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sandbox.FastSandboxProxyError, "credential_rejected")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer backend.Close()
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	headers := http.Header{SandboxIngress: []string{fsbScopeVector}}
	conn, response, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(ingress.URL, "http")+"/ws",
		&websocket.DialOptions{HTTPHeader: headers},
	)
	if conn != nil {
		defer func() { _ = conn.CloseNow() }()
	}
	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.Equal(t, "1", response.Header.Get("Retry-After"))
	require.Equal(t, provider.target, provider.invalidated)
}

func TestFastSandboxProxyWebSocketConnectionFailureInvalidatesRoute(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: upstream}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	headers := http.Header{SandboxIngress: []string{fsbScopeVector}}
	connection, response, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(ingress.URL, "http")+"/ws",
		&websocket.DialOptions{HTTPHeader: headers},
	)
	if connection != nil {
		defer func() { _ = connection.CloseNow() }()
	}
	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.Equal(t, provider.target, provider.invalidated)
}

func TestFastSandboxProxyStreamsSSE(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: ready\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer backend.Close()
	defer close(release)
	provider := &fsbProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	request, err := http.NewRequest(http.MethodGet, ingress.URL+"/events", nil)
	require.NoError(t, err)
	request.Header.Set(SandboxIngress, fsbScopeVector)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: ready\n", line)
}
