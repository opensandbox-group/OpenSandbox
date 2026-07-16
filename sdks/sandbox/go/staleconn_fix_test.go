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

// TestStaleConn_GetEndpointRecoversFromBlackHoledConn is the deterministic,
// millisecond-scale regression test.
//
// Setup: the black-hole server serves the FIRST request on any connection and
// silently hangs on a SECOND request over the SAME connection (modeling an LB
// that dropped the idle connection without sending FIN). The idle gap between
// the two calls (50ms) is far below IdleConnTimeout, so the SDK keeps and
// REUSES the (now black-holed) connection for the second call.
//
//   - Before the fix: the second GetEndpoint reuses the dead connection, stalls,
//     and fails at RequestTimeout with "Client.Timeout exceeded while awaiting
//     headers".
//   - After the fix (B): doRequest detects the connection-level stall, purges
//     idle connections, and retries once on a fresh connection -> success.
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

// TestStaleConn_GetEndpointRecovers_RealTimescale exercises the same recovery at
// realistic (second-scale) timings. Skipped under `go test -short`.
func TestStaleConn_GetEndpointRecovers_RealTimescale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping second-scale stale-connection test in -short mode")
	}
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()

	cfg := ConnectionConfig{
		Domain:                "http://" + addr,
		RequestTimeout:        3 * time.Second,
		EndpointCacheDisabled: true,
	}
	lc := cfg.lifecycleClient()
	useProxy := false

	if _, err := lc.GetEndpoint(context.Background(), "sbx", DefaultExecdPort, &useProxy); err != nil {
		t.Fatalf("first GetEndpoint should succeed, got: %v", err)
	}

	// Real idle gap: the connection stays pooled and is reused for call 2.
	time.Sleep(2 * time.Second)

	_, err := lc.GetEndpoint(context.Background(), "sbx", DefaultExecdPort, &useProxy)
	require.NoError(t, err, "GetEndpoint must recover from a black-holed reused connection at second scale")
}

// TestStaleConn_IdleConnEvictedBeforeLBDrops verifies the "A" lever: when a
// connection stays idle longer than IdleConnTimeout, the SDK evicts it, so the
// next request dials a FRESH connection instead of reusing a possibly-dead one.
// Uses a short IdleConnTimeout via a custom transport so the test is fast and
// deterministic.
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

// TestStaleConn_NonIdempotentNotRetried verifies the fresh-connection retry is
// restricted to idempotent methods: a POST that hits a black-holed reused
// connection is NOT retried (to avoid the risk of double execution) and fails.
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

// TestStaleConn_FreshConnTimeoutNotRetried verifies the fresh-connection retry
// is gated on connection REUSE: a GET whose FIRST (brand-new) connection simply
// times out against a slow server must NOT be retried, otherwise a slow server
// would be hit twice and the effective timeout would roughly double. This is
// the guard requested in PR review: only reused pooled connections are retried.
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
