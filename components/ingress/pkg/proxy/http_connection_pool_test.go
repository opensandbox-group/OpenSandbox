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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/proxy/connectivity"
)

func TestHTTPConnectionPoolReusesConnectionsAcrossUpstreams(t *testing.T) {
	const upstreams, rounds = 101, 3
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprintf("observer=%t", observed), func(t *testing.T) {
			routes, connections := newPoolUpstreams(t, upstreams)
			for _, test := range []struct {
				name       string
				capacity   int
				wantOpened int64
			}{
				{name: "default capacity", capacity: 100, wantOpened: upstreams * rounds},
				{name: "expanded capacity", capacity: upstreams, wantOpened: upstreams},
				{name: "unlimited capacity", capacity: 0, wantOpened: upstreams},
			} {
				t.Run(test.name, func(t *testing.T) {
					opts := []Option{WithHTTPConnectionPool(test.capacity, 2, 90*time.Second)}
					if observed {
						opts = append(opts, WithConnectObserver(connectivity.ObserverFunc(func(connectivity.Observation) {})))
					}
					gateway := newPoolProxy(t, opts...)
					before := connections.Load()
					for range rounds {
						for _, route := range routes {
							requestPoolUpstream(t, gateway, route)
						}
					}
					if opened := connections.Load() - before; opened != test.wantOpened {
						t.Fatalf("opened %d upstream connections for %d requests, want %d", opened, upstreams*rounds, test.wantOpened)
					}
				})
			}
		})
	}
}

func TestHTTPConnectionPoolPerHostLimit(t *testing.T) {
	for _, test := range []struct {
		limit      int
		wantOpened int64
	}{
		{limit: 1, wantOpened: 5},
		{limit: 3, wantOpened: 3},
		{limit: 0, wantOpened: 4},
	} {
		t.Run(fmt.Sprintf("limit=%d", test.limit), func(t *testing.T) {
			var connections atomic.Int64
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ready")
			}))
			backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			backend.Start()
			t.Cleanup(backend.Close)
			gateway := newPoolProxy(t, WithHTTPConnectionPool(0, test.limit, 0))
			client := &http.Client{Transport: gateway.httpTransport, Timeout: 5 * time.Second}
			for range 2 {
				responses := make([]*http.Response, 0, 3)
				for range 3 {
					response, err := client.Get(backend.URL)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = response.Body.Close() })
					responses = append(responses, response)
				}
				// 先保留三条活跃响应，再依次读完归还，确保下一轮只复用池内保留的连接。
				for _, response := range responses {
					body, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if err != nil || string(body) != "ready" || response.StatusCode != http.StatusOK {
						t.Fatalf("response: status=%d body=%q error=%v", response.StatusCode, body, err)
					}
				}
			}
			if got := connections.Load(); got != test.wantOpened {
				t.Fatalf("opened %d upstream connections, want %d", got, test.wantOpened)
			}
		})
	}
}

func TestHTTPConnectionPoolIdleTimeout(t *testing.T) {
	for _, idleTimeout := range []time.Duration{20 * time.Millisecond, 0} {
		t.Run(idleTimeout.String(), func(t *testing.T) {
			closed := make(chan struct{}, 2)
			var connections atomic.Int64
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ready")
			}))
			backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				switch state {
				case http.StateNew:
					connections.Add(1)
				case http.StateClosed:
					closed <- struct{}{}
				}
			}
			backend.Start()
			t.Cleanup(backend.Close)
			gateway := newPoolProxy(t, WithHTTPConnectionPool(100, 2, idleTimeout))
			route := poolUpstreamRoute(backend.URL)
			requestPoolUpstream(t, gateway, route)
			wantConnections := int64(1)
			if idleTimeout > 0 {
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("idle upstream connection did not expire")
				}
				wantConnections = 2
			} else {
				select {
				case <-closed:
					t.Fatal("zero idle timeout closed the upstream connection")
				case <-time.After(80 * time.Millisecond):
				}
			}
			requestPoolUpstream(t, gateway, route)
			if got := connections.Load(); got != wantConnections {
				t.Fatalf("opened %d upstream connections, want %d", got, wantConnections)
			}
		})
	}
}

func TestHTTPConnectionPoolIdleTimeoutPreservesActiveSSE(t *testing.T) {
	const idleTimeout = 20 * time.Millisecond
	const firstEvent, finalEvent = "data: ready\n\n", "data: complete\n\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, firstEvent)
		w.(http.Flusher).Flush()
		timer := time.NewTimer(4 * idleTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			_, _ = io.WriteString(w, finalEvent)
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)
	gateway := newPoolProxy(t, WithHTTPConnectionPool(100, 2, idleTimeout))
	ingress := httptest.NewServer(gateway)
	t.Cleanup(ingress.Close)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get(ingress.URL + strings.TrimPrefix(poolUpstreamRoute(backend.URL), "http://ingress.test"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != firstEvent+finalEvent || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE response: body=%q content-type=%q error=%v", body, response.Header.Get("Content-Type"), err)
	}
}

func newPoolProxy(tb testing.TB, opts ...Option) *Proxy {
	tb.Helper()
	gateway := NewProxy(context.Background(), stubNoSecureProvider{}, ModeURI, nil, nil, nil, opts...)
	transport, ok := gateway.httpTransport.(*http.Transport)
	if !ok {
		tb.Fatalf("configured HTTP transport = %T, want *http.Transport", gateway.httpTransport)
	}
	tb.Cleanup(transport.CloseIdleConnections)
	return gateway
}

func newPoolUpstreams(tb testing.TB, count int) ([]string, *atomic.Int64) {
	tb.Helper()
	connections := new(atomic.Int64)
	routes := make([]string, 0, count)
	for range count {
		backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/status" || request.URL.Query().Get("probe") != "ok" {
				http.Error(w, "unexpected upstream request", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, "ready")
		}))
		backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
		backend.Start()
		tb.Cleanup(backend.Close)
		routes = append(routes, poolUpstreamRoute(backend.URL))
	}
	return routes, connections
}

func poolUpstreamRoute(upstreamURL string) string {
	return "http://ingress.test/pool/" + strings.TrimPrefix(upstreamURL, "http://127.0.0.1:") + "/status?probe=ok"
}

func requestPoolUpstream(tb testing.TB, gateway *Proxy, route string) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, route, nil))
	if response.Code != http.StatusOK || response.Body.String() != "ready" {
		tb.Fatalf("upstream response: status=%d body=%q", response.Code, response.Body.String())
	}
}
