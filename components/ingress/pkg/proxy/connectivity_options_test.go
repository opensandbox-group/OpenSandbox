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
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/proxy/connectivity"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestObservedHTTPTransportRecordsTCPConnect(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	observations := make(chan connectivity.Observation, 2)
	transport := newObservedHTTPTransport(connectivity.ObserverFunc(func(observation connectivity.Observation) {
		observations <- observation
	}))
	t.Cleanup(transport.(*http.Transport).CloseIdleConnections)
	client := &http.Client{Transport: transport}
	for range 2 {
		response, err := client.Get(backend.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}

	assertSuccessfulObservation(t, observations, "http")
	if len(observations) != 0 {
		t.Fatalf("keep-alive request opened %d additional TCP connections", len(observations))
	}
}

func TestObservedWebSocketHTTPClientRecordsTCPConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()

	observations := make(chan connectivity.Observation, 1)
	client := newObservedWebSocketHTTPClient(connectivity.ObserverFunc(func(observation connectivity.Observation) {
		observations <- observation
	}))
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", client.Transport)
	}
	conn, err := transport.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-accepted

	assertSuccessfulObservation(t, observations, "websocket")
}

func TestProxyConfiguresBothObservedDialers(t *testing.T) {
	observer := connectivity.ObserverFunc(func(connectivity.Observation) {})
	proxy := NewProxy(context.Background(), nil, ModeHeader, nil, nil, nil, WithConnectObserver(observer))

	if proxy.httpTransport == nil || proxy.websocketClient == nil {
		t.Fatalf("observed transports were not configured: %+v", proxy)
	}
}

func TestObservedHTTPTransportHandlesReplacedDefault(t *testing.T) {
	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	observer := connectivity.ObserverFunc(func(connectivity.Observation) {})
	if transport := newObservedHTTPTransport(observer); transport != nil {
		t.Fatalf("transport = %T, want nil fallback for non-standard default transport", transport)
	}
}

func TestObservedHTTPTransportHandlesNilDefaultDialer(t *testing.T) {
	previousTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{}
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	observer := connectivity.ObserverFunc(func(connectivity.Observation) {})
	transport, ok := newObservedHTTPTransport(observer).(*http.Transport)
	if !ok {
		t.Fatalf("observed transport = %T, want *http.Transport", transport)
	}
	if transport.DialContext == nil {
		t.Fatal("observed transport has nil DialContext")
	}
}

func assertSuccessfulObservation(t *testing.T, observations <-chan connectivity.Observation, protocol string) {
	t.Helper()
	select {
	case observation := <-observations:
		if observation.Protocol != protocol || observation.Result != connectivity.ResultSuccess {
			t.Fatalf("unexpected observation: %+v", observation)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s observation", protocol)
	}
}
