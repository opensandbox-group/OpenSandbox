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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/connectivity"
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
	transport := newHTTPTransport(connectivity.ObserverFunc(func(observation connectivity.Observation) {
		observations <- observation
	}), nil)
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
	for _, pool := range []*httpConnectionPoolConfig{nil, {maxIdleConns: 1000}} {
		if transport := newHTTPTransport(observer, pool); transport != nil {
			t.Fatalf("transport = %T, want nil fallback for non-standard default transport", transport)
		}
	}
	if client := newObservedWebSocketHTTPClient(observer); client != nil {
		t.Fatalf("WebSocket client = %v, want nil fallback for non-standard default transport", client)
	}
}

func TestHTTPConnectionPoolLeavesOtherTransportsUnchanged(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport)
	previous := base.Clone()
	observer := connectivity.ObserverFunc(func(connectivity.Observation) {})
	proxy := NewProxy(context.Background(), nil, ModeHeader, nil, nil, nil,
		WithConnectObserver(observer), WithHTTPConnectionPool(1000, 8, time.Minute))
	transport := proxy.httpTransport.(*http.Transport)
	websocketTransport := proxy.websocketClient.Transport.(*http.Transport)
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(websocketTransport.CloseIdleConnections)

	if transport == base || transport == websocketTransport {
		t.Fatal("HTTP pool must own its transport independently of the default and WebSocket transports")
	}
	for name, other := range map[string]*http.Transport{"default": base, "websocket": websocketTransport} {
		if other.MaxIdleConns != previous.MaxIdleConns || other.MaxIdleConnsPerHost != previous.MaxIdleConnsPerHost || other.IdleConnTimeout != previous.IdleConnTimeout {
			t.Fatalf("%s transport inherited the HTTP pool settings", name)
		}
	}
}

func TestObservedHTTPTransportHandlesNilDefaultDialer(t *testing.T) {
	previousTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{}
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	observer := connectivity.ObserverFunc(func(connectivity.Observation) {})
	transport, ok := newHTTPTransport(observer, nil).(*http.Transport)
	if !ok {
		t.Fatalf("observed transport = %T, want *http.Transport", transport)
	}
	if transport.DialContext == nil {
		t.Fatal("observed transport has nil DialContext")
	}
	client := newObservedWebSocketHTTPClient(observer)
	if client == nil || client.Transport.(*http.Transport).DialContext == nil {
		t.Fatal("observed WebSocket transport has nil DialContext")
	}
}

func TestFastPathDialTimeoutEnablesNilTransportDialer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	transport := &http.Transport{}
	enableFastPathDialTimeout(transport)
	if transport.DialContext == nil {
		t.Fatal("FastPath transport left the default DialContext unset")
	}
	config := &fastPathDialConfig{timeout: time.Second}
	conn, err := transport.DialContext(withFastPathDialConfig(context.Background(), config), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("FastPath dial with the default net.Dialer failed: %v", err)
	}
	_ = conn.Close()
	select {
	case acceptedConn := <-accepted:
		_ = acceptedConn.Close()
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the default net.Dialer connection")
	}
}

func TestFastPathDialWrapperPreservesOrdinaryRoutes(t *testing.T) {
	ordinaryErr := errors.New("ordinary route dial failed")
	fastPathErr := errors.New("fast path dial failed")
	var fastPathNotified bool
	dial := wrapFastPathDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
		if fastPathDialConfigFromContext(ctx) == nil {
			return nil, ordinaryErr
		}
		return nil, fastPathErr
	})

	_, err := dial(context.Background(), "tcp", "ordinary")
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("ordinary route error = %v, want %v", err, ordinaryErr)
	}

	config := &fastPathDialConfig{
		timeout: time.Second,
		onError: func(err error) { fastPathNotified = errors.Is(err, fastPathErr) },
	}
	_, err = dial(withFastPathDialConfig(context.Background(), config), "tcp", "fast-path")
	if !errors.Is(err, fastPathErr) {
		t.Fatalf("FastPath error = %v, want %v", err, fastPathErr)
	}
	if !fastPathNotified {
		t.Fatal("FastPath dial failure did not notify its observer")
	}
}

func TestFastPathDialWrapperNotifiesInternalTimeout(t *testing.T) {
	notified := make(chan error, 1)
	dial := wrapFastPathDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	config := &fastPathDialConfig{
		timeout: 10 * time.Millisecond,
		onError: func(err error) { notified <- err },
	}

	_, err := dial(withFastPathDialConfig(context.Background(), config), "tcp", "blackhole")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial error = %v, want context deadline exceeded", err)
	}
	select {
	case notification := <-notified:
		if !errors.Is(notification, context.DeadlineExceeded) {
			t.Fatalf("timeout notification = %v, want context deadline exceeded", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for internal timeout notification")
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
