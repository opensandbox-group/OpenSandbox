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

// Regression tests for the intermittent GetEndpoint failure caused by reusing a
// keep-alive connection that a load balancer silently black-holes after it goes
// idle ("context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)").
//
// These tests drive the SDK's real GetEndpoint path (via ConnectionConfig with
// the endpoint cache disabled so every call hits the network) against the
// connection-level black-hole server from staleconn_diag_test.go. They MUST fail
// before the fix and pass after it.

package opensandbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The fixture serves only the first request on each connection; reuse stalls without a FIN.
func TestStaleConn_GetEndpointRecoversFromBlackHoledConn(t *testing.T) {
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()

	cfg := ConnectionConfig{
		Domain:                "http://" + addr,
		RequestTimeout:        300 * time.Millisecond, // detect a stalled reused conn quickly
		EndpointCacheDisabled: true,                   // force a network call every time
	}
	lc := cfg.lifecycleClient()
	useProxy := false

	// Call 1: fresh connection, served OK; connection returns to the idle pool.
	if _, err := lc.GetEndpoint(context.Background(), "sbx", DefaultExecdPort, &useProxy); err != nil {
		t.Fatalf("first GetEndpoint should succeed, got: %v", err)
	}

	// Idle gap well under IdleConnTimeout so the connection is reused (not evicted).
	time.Sleep(50 * time.Millisecond)

	// Call 2: reuses the black-holed connection. Must recover via a fresh-connection retry.
	_, err := lc.GetEndpoint(context.Background(), "sbx", DefaultExecdPort, &useProxy)
	require.NoError(t, err, "GetEndpoint must recover from a black-holed reused connection")
}

func TestStaleConn_IdleConnEvictedBeforeLBDrops(t *testing.T) {
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()

	trCfg := DefaultTransportConfig()
	trCfg.IdleConnTimeout = 40 * time.Millisecond // evict quickly
	cfg := ConnectionConfig{
		Domain:                "http://" + addr,
		RequestTimeout:        300 * time.Millisecond,
		EndpointCacheDisabled: true,
		Transport:             &trCfg,
	}
	lc := cfg.lifecycleClient()
	useProxy := false

	if _, err := lc.GetEndpoint(context.Background(), "sbx", DefaultExecdPort, &useProxy); err != nil {
		t.Fatalf("first GetEndpoint should succeed, got: %v", err)
	}

	// Idle longer than IdleConnTimeout: the pooled connection is evicted, so the
	// second call dials fresh (first-request-on-connection => served OK) and
	// never touches the black-holed connection.
	time.Sleep(150 * time.Millisecond)

	_, err := lc.GetEndpoint(context.Background(), "sbx", DefaultExecdPort, &useProxy)
	require.NoError(t, err, "GetEndpoint should dial a fresh connection after IdleConnTimeout eviction")
}

// Retrying a POST can execute the workload twice.
func TestStaleConn_NonIdempotentNotRetried(t *testing.T) {
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()

	c := NewClient("http://"+addr, "k", "OPEN-SANDBOX-API-KEY",
		WithTimeout(300*time.Millisecond))

	// Call 1 (POST): fresh connection, served OK; connection pooled.
	if err := c.doRequest(context.Background(), "POST", "/", map[string]string{"a": "b"}, nil); err != nil {
		t.Fatalf("first POST should succeed, got: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Call 2 (POST): reuses black-holed connection. POST must NOT be retried.
	err := c.doRequest(context.Background(), "POST", "/", map[string]string{"a": "b"}, nil)
	require.Error(t, err, "POST must not be transparently retried on a fresh connection")
	assert.Contains(t, err.Error(), "Client.Timeout exceeded while awaiting headers")
}

func TestStaleConn_FreshConnTimeoutNotRetried(t *testing.T) {
	var hits int32
	// Every request blocks past the client timeout (never sends headers), so a
	// first attempt on a fresh connection always times out.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "OPEN-SANDBOX-API-KEY", WithTimeout(200*time.Millisecond))

	// GET on a fresh connection: times out as a net.Error, but was NOT reused,
	// so it must fail after exactly one attempt.
	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.Error(t, err, "slow fresh-connection GET must fail (timeout)")
	var netErr net.Error
	require.True(t, errors.As(err, &netErr), "error should be a net timeout")
	require.Equal(t, int32(1), atomic.LoadInt32(&hits),
		"a fresh-connection timeout must NOT be retried (would double the timeout)")
}
