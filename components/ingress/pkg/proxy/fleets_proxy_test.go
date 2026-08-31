// Copyright 2026 Alibaba Group Holding Ltd.
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

	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const fleetsScopeVector = "f1.dGVuYW50LWE.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ"

func init() {
	Logger = slogger.MustNew(slogger.Config{Level: "error"})
}

type fleetsProxyProvider struct {
	info        *sandbox.EndpointInfo
	err         error
	target      sandbox.EndpointTarget
	invalidated sandbox.EndpointTarget
}

func (p *fleetsProxyProvider) Invalidate(target sandbox.EndpointTarget) {
	p.invalidated = target
}

func (p *fleetsProxyProvider) Start(context.Context) error { return nil }

func (*fleetsProxyProvider) RequiresAuthenticatedRouteScope() {}

func (p *fleetsProxyProvider) ResolveEndpoint(_ context.Context, target sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	p.target = target
	if p.err != nil {
		return nil, p.err
	}
	return p.info, nil
}

func TestFleetsProxyPreservesPathQueryAndApplicationAuthorization(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/sandboxes/uid/components/execd/api/health", r.URL.Path)
		require.Equal(t, "watch=true", r.URL.RawQuery)
		require.Equal(t, "Bearer application-token", r.Header.Get("Authorization"))
		require.Equal(t, "issued-credential", r.Header.Get(sandbox.FastSandboxCredential))
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{
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
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	request.Header.Set("Authorization", "Bearer application-token")
	request.Header.Set(sandbox.FastSandboxCredential, "caller-spoof")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "ok", response.Body.String())
	require.Equal(t, sandbox.EndpointTarget{RouteKind: sandbox.RouteKindFleets, Namespace: "tenant-a", SandboxID: "sandbox-123", Port: sandbox.ExecdPort}, provider.target)
}

func TestFleetsProxyStripsCallerCredentialWhenProviderOmitsIt(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get(sandbox.FastSandboxCredential))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	request.Header.Set(sandbox.FastSandboxCredential, "caller-spoof")
	response := httptest.NewRecorder()

	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestCompositeProviderServesLegacyAndFleetsRoutes(t *testing.T) {
	legacyBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Backend", "legacy")
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
	}))
	defer legacyBackend.Close()
	fleetsBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Backend", "fleets")
	}))
	defer fleetsBackend.Close()

	legacyAddress := legacyBackend.Listener.Addr().(*net.TCPAddr)
	legacy := staticEndpointProvider{byID: map[string]sandbox.EndpointInfo{
		"legacy-sandbox": {Endpoint: legacyAddress.IP.String()},
	}}
	fleets := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: fleetsBackend.URL}}
	provider := sandbox.NewCompositeProvider(legacy, fleets)
	ingress := httptest.NewServer(NewProxy(
		context.Background(),
		provider,
		ModeHeader,
		nil,
		nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	))
	defer ingress.Close()

	legacyRoute := fmt.Sprintf("legacy-sandbox-%d", legacyAddress.Port)
	for _, test := range []struct {
		route   string
		backend string
	}{{legacyRoute, "legacy"}, {fleetsScopeVector, "fleets"}, {legacyRoute, "legacy"}} {
		request, err := http.NewRequest(http.MethodGet, ingress.URL, nil)
		require.NoError(t, err)
		request.Header.Set(SandboxIngress, test.route)
		response, err := ingress.Client().Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, test.backend, response.Header.Get("X-Test-Backend"))
		require.NoError(t, response.Body.Close())
	}
	require.Equal(t, sandbox.RouteKindFleets, fleets.target.RouteKind)
}

func TestFleetsProxyPreservesEscapedSlashInHeaderPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/a/b", r.URL.Path)
		require.Equal(t, "/route/a%2Fb", r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL + "/route"}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/a%2Fb", nil)
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFleetsProxyPreservesRawQueryHash(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "a=1#b=2", r.URL.RawQuery)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/path", nil)
	request.URL.RawQuery = "a=1#b=2"
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFleetsProxyURIScopeRemovesInternalPrefix(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/user/path", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL + "/route"}}
	p := NewProxy(
		context.Background(), provider, ModeURI, nil, nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)
	request := httptest.NewRequest(http.MethodGet, "http://ingress/"+fleetsScopeVector+"/user/path", nil)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFleetsProxyURIScopePreservesEscapedSlash(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/a/b", r.URL.Path)
		require.Equal(t, "/route/a%2Fb", r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL + "/route"}}
	p := NewProxy(context.Background(), provider, ModeURI, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/"+fleetsScopeVector+"/a%2Fb", nil)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusNoContent, response.Code)
}

func TestFleetsProxyMapsPendingAndUnsupported(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "pending", err: fmt.Errorf("%w: assignment pending", sandbox.ErrSandboxNotReady), wantStatus: http.StatusServiceUnavailable},
		{name: "phase 1a egress", err: fmt.Errorf("%w: egress", sandbox.ErrTargetUnsupported), wantStatus: http.StatusNotImplemented},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &fleetsProxyProvider{err: test.err}
			p := NewProxy(
				context.Background(), provider, ModeHeader, nil, nil,
				&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
			)
			request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
			request.Header.Set(SandboxIngress, fleetsScopeVector)
			response := httptest.NewRecorder()
			p.ServeHTTP(response, request)
			require.Equal(t, test.wantStatus, response.Code)
			if test.wantStatus == http.StatusServiceUnavailable {
				require.Equal(t, "1", response.Header().Get("Retry-After"))
			}
		})
	}
}

func TestFleetsProxyRejectsTamperedScopeBeforeProviderLookup(t *testing.T) {
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}}
	p := NewProxy(
		context.Background(), provider, ModeHeader, nil, nil,
		&routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}},
	)
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, "f1.dGVuYW50LWI.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, sandbox.EndpointTarget{}, provider.target)
}

func TestFleetsProxyRejectsLegacyUnsignedRoute(t *testing.T) {
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, "sandbox-123-44772")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, sandbox.EndpointTarget{}, provider.target)
}

func TestFleetsProxyInvalidatesStaleRouteWithoutReplayingRequest(t *testing.T) {
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
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodPost, "http://ingress/mutate", strings.NewReader("body"))
	request.Header.Set(SandboxIngress, fleetsScopeVector)
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

func TestFleetsProxyIgnoresStaleMarkerOnSuccessfulResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sandbox.FastSandboxProxyError, "stale_route")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	response := httptest.NewRecorder()

	p.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "ok", response.Body.String())
	require.Equal(t, sandbox.EndpointTarget{}, provider.invalidated)
}

func TestFleetsProxyInvalidatesRouteOnUpstreamConnectionFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: upstream}}
	p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}})
	request := httptest.NewRequest(http.MethodPost, "http://ingress/mutate", strings.NewReader("body"))
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Equal(t, provider.target, provider.invalidated)
}

func TestFleetsProxyWebSocketUsesBasePathAndUpstreamCredential(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/route/ws", r.URL.Path)
		require.Equal(t, "issued-credential", r.Header.Get(sandbox.FastSandboxCredential))
		require.Equal(t, "Bearer application-token", r.Header.Get("Authorization"))
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		messageType, message, err := conn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(messageType, message))
	}))
	defer backend.Close()
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{
		UpstreamURL:     backend.URL + "/route",
		UpstreamHeaders: http.Header{sandbox.FastSandboxCredential: []string{"issued-credential"}},
	}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	headers := http.Header{SandboxIngress: []string{fleetsScopeVector}, "Authorization": []string{"Bearer application-token"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ingress.URL, "http")+"/ws", headers)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("hello")))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "hello", string(message))
}

func TestFleetsProxyWebSocketHandshakeInvalidatesStaleRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sandbox.FastSandboxProxyError, "credential_rejected")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer backend.Close()
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	headers := http.Header{SandboxIngress: []string{fleetsScopeVector}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ingress.URL, "http")+"/ws", headers)
	if conn != nil {
		defer conn.Close()
	}
	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.Equal(t, "1", response.Header.Get("Retry-After"))
	require.Equal(t, provider.target, provider.invalidated)
}

func TestFleetsProxyWebSocketConnectionFailureInvalidatesRoute(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: upstream}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	headers := http.Header{SandboxIngress: []string{fleetsScopeVector}}
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ingress.URL, "http")+"/ws", headers)
	if connection != nil {
		defer connection.Close()
	}
	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.Equal(t, provider.target, provider.invalidated)
}

func TestFleetsProxyStreamsSSE(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: ready\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer backend.Close()
	defer close(release)
	provider := &fleetsProxyProvider{info: &sandbox.EndpointInfo{UpstreamURL: backend.URL}}
	ingress := httptest.NewServer(NewProxy(context.Background(), provider, ModeHeader, nil, nil, &routescope.Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}))
	defer ingress.Close()

	request, err := http.NewRequest(http.MethodGet, ingress.URL+"/events", nil)
	require.NoError(t, err)
	request.Header.Set(SandboxIngress, fleetsScopeVector)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: ready\n", line)
}
