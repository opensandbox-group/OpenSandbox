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

// This file contains DIAGNOSTIC tests for the "stale keep-alive connection"
// hypothesis behind intermittent GetEndpoint timeouts
// ("context deadline exceeded (Client.Timeout exceeded while awaiting headers)").
//
// They do not change any SDK behavior. They:
//  1. Prove the SDK's default transport reuses idle pooled connections.
//  2. Reproduce the exact "Client.Timeout ... while awaiting headers" error on a
//     connection that goes black-hole (dead but no FIN) after being idle.
//  3. Show that forcing a fresh connection (DisableKeepAlives, or an
//     IdleConnTimeout shorter than the idle gap) avoids the hang.
//  4. Provide an env-gated probe to run against the REAL lifecycle server.

package opensandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"
)

// traceTransport wraps a RoundTripper and logs per-request connection-reuse and
// timing info via httptrace, so we can see Reused / WasIdle / IdleTime and where
// a request stalls.
type traceTransport struct {
	base http.RoundTripper
	t    *testing.T
	mu   sync.Mutex
	n    int

	// lastReused records whether the most recent request reused a pooled
	// connection (from httptrace GotConn), so tests can assert reuse behavior.
	lastReused bool
}

func (tt *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tt.mu.Lock()
	tt.n++
	id := tt.n
	tt.mu.Unlock()

	start := time.Now()
	trace := &httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			tt.t.Logf("[req %d] +%-8v ConnectStart NEW dial %s %s", id, time.Since(start).Round(time.Millisecond), network, addr)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			tt.mu.Lock()
			tt.lastReused = info.Reused
			tt.mu.Unlock()
			tt.t.Logf("[req %d] +%-8v GotConn reused=%v wasIdle=%v idleTime=%v",
				id, time.Since(start).Round(time.Millisecond), info.Reused, info.WasIdle, info.IdleTime.Round(time.Millisecond))
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			tt.t.Logf("[req %d] +%-8v WroteRequest err=%v", id, time.Since(start).Round(time.Millisecond), info.Err)
		},
		GotFirstResponseByte: func() {
			tt.t.Logf("[req %d] +%-8v GotFirstResponseByte", id, time.Since(start).Round(time.Millisecond))
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			proto := cs.NegotiatedProtocol
			if proto == "" {
				proto = "(none/http1)"
			}
			tt.t.Logf("[req %d] +%-8v TLSHandshakeDone alpn=%q err=%v", id, time.Since(start).Round(time.Millisecond), proto, err)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := tt.base.RoundTrip(req)
	tt.t.Logf("[req %d] +%-8v DONE err=%v", id, time.Since(start).Round(time.Millisecond), err)
	return resp, err
}

// TestDiag_ConnReuseAfterIdle confirms the necessary precondition for the bug:
// the SDK's default transport keeps an idle connection in the pool and REUSES it
// on the next request (Reused=true, WasIdle=true) rather than dialing fresh.
func TestDiag_ConnReuseAfterIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	tt := &traceTransport{base: DefaultTransport(), t: t}
	c := NewClient(srv.URL, "k", "OPEN-SANDBOX-API-KEY",
		WithHTTPClient(&http.Client{Transport: tt}))

	for i := 0; i < 3; i++ {
		if err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil); err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		if i == 0 {
			require.True(t, !tt.lastReused, "request 1 should dial a fresh connection")
		} else {
			// After an idle gap well under IdleConnTimeout, the pooled connection
			// must be reused rather than re-dialed.
			require.True(t, tt.lastReused, "request %d should REUSE the pooled idle connection", i+1)
		}
		time.Sleep(200 * time.Millisecond) // idle gap; well under IdleConnTimeout (30s)
	}
}

// blackHoleAfterReuseServer models a load balancer that silently drops a
// connection after it goes idle: the FIRST request on any TCP connection is
// served normally (keep-alive), but a SECOND request on the SAME connection is
// black-holed (request read, no response, no FIN) — while a brand-new
// connection is always served fine.
//
// Returns the listener address and a cleanup func.
func blackHoleAfterReuseServer(t *testing.T) (addr string, cleanup func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup

	// Track live connections so cleanup can force-close them; otherwise a
	// goroutine blocked in ReadRequest (waiting for a request that never comes)
	// would delay cleanup's wg.Wait until its read deadline expired.
	var connMu sync.Mutex
	var conns []net.Conn

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connMu.Lock()
			conns = append(conns, conn)
			connMu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				br := bufio.NewReader(c)
				reqOnConn := 0
				for {
					_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					reqOnConn++
					if reqOnConn == 1 {
						// Drain the request body so the connection can be cleanly
						// reused for the next request (matters for POST/PUT bodies).
						_, _ = io.Copy(io.Discard, req.Body)
						_ = req.Body.Close()
						// First request on this connection: serve OK, keep alive.
						_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\n{}"))
						continue
					}
					// Reused connection: black-hole. Read the request but never
					// respond and never send FIN — the client cannot tell the
					// connection is dead and will hang until Client.Timeout.
					select {
					case <-done:
					case <-time.After(30 * time.Second):
					}
					return
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
		connMu.Lock()
		for _, c := range conns {
			_ = c.Close() // unblock any goroutine parked in ReadRequest
		}
		connMu.Unlock()
		wg.Wait()
	}
}

// TestDiag_ReusedConnBlackHoleReproducesClientTimeout demonstrates the raw
// transport-level root cause behind the screenshot error: issuing requests
// directly on the SDK's default (keep-alive) transport, the second request
// reuses the idle connection that the "LB" has black-holed, hangs until
// http.Client.Timeout, and fails with the exact
// "Client.Timeout exceeded while awaiting headers" text.
//
// This uses the raw *http.Client (NOT Client.doRequest) so it keeps documenting
// the underlying transport behavior even after the SDK's doRequest recovery is
// in place; the SDK-level recovery is covered by staleconn_fix_test.go.
func TestDiag_ReusedConnBlackHoleReproducesClientTimeout(t *testing.T) {
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()
	baseURL := "http://" + addr

	tt := &traceTransport{base: DefaultTransport(), t: t}
	httpClient := &http.Client{Transport: tt, Timeout: 800 * time.Millisecond}

	// Request 1: fresh dial, served OK. Connection goes back to the pool.
	resp1, err := httpClient.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("request 1 should succeed, got: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp1.Body)
	_ = resp1.Body.Close()
	time.Sleep(100 * time.Millisecond) // connection now idle in the pool

	// Request 2: reuses the idle (now black-holed) connection -> hangs -> timeout.
	_, err = httpClient.Get(baseURL + "/")
	require.Error(t, err)
	t.Logf("req2 error: %v", err)
	assert.Contains(t, err.Error(), "Client.Timeout exceeded while awaiting headers")
	t.Log("=> Raw transport: reused idle connection + black-hole => Client.Timeout while awaiting headers.")
}

// TestDiag_FixDisableKeepAlives shows that using a fresh connection per request
// (DisableKeepAlives) avoids the hang: every request is the FIRST on its
// connection, so the black-hole server always serves it.
func TestDiag_FixDisableKeepAlives(t *testing.T) {
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()
	baseURL := "http://" + addr

	tr := DefaultTransport()
	tr.DisableKeepAlives = true
	tt := &traceTransport{base: tr, t: t}
	c := NewClient(baseURL, "k", "OPEN-SANDBOX-API-KEY",
		WithHTTPClient(&http.Client{Transport: tt, Timeout: 800 * time.Millisecond}))

	for i := 0; i < 3; i++ {
		if err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil); err != nil {
			t.Fatalf("request %d should succeed with DisableKeepAlives, got: %v", i+1, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("=> DisableKeepAlives: every request dials fresh (no reuse) => no hang.")
}

// TestDiag_FixShortIdleConnTimeout shows that an IdleConnTimeout shorter than
// the idle gap makes the SDK evict the pooled connection before it is reused, so
// the next request dials fresh and the black-hole is avoided.
func TestDiag_FixShortIdleConnTimeout(t *testing.T) {
	addr, cleanup := blackHoleAfterReuseServer(t)
	defer cleanup()
	baseURL := "http://" + addr

	tr := DefaultTransport()
	tr.IdleConnTimeout = 50 * time.Millisecond // << idle gap below
	tt := &traceTransport{base: tr, t: t}
	c := NewClient(baseURL, "k", "OPEN-SANDBOX-API-KEY",
		WithHTTPClient(&http.Client{Transport: tt, Timeout: 800 * time.Millisecond}))

	if err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil); err != nil {
		t.Fatalf("request 1 should succeed, got: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // idle > IdleConnTimeout => connection evicted

	err := c.doRequest(context.Background(), http.MethodGet, "/", nil, nil)
	require.NoError(t, err)
	t.Log("=> Short IdleConnTimeout: idle connection evicted before reuse => req2 dials fresh => OK.")
}

// TestDiag_SDKTransportUsesHTTP1 proves that the SDK's default transport shape
// (custom DialContext + TLSClientConfig, no ForceAttemptHTTP2) negotiates
// HTTP/1.1 even against an HTTP/2-capable TLS server, and that HTTP/2 is only
// used when ForceAttemptHTTP2 is explicitly enabled.
func TestDiag_SDKTransportUsesHTTP1(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	srv.EnableHTTP2 = true // make the test server actually offer HTTP/2 via ALPN
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	// SDK-shaped transport: exactly what DefaultTransport() builds. We only add
	// RootCAs so TLS verifies, and clear VerifyConnection to isolate the protocol
	// question from the NIST cert policy. Crucially: ForceAttemptHTTP2 is NOT set.
	tr := DefaultTransport()
	tr.TLSClientConfig.RootCAs = pool
	tr.TLSClientConfig.VerifyConnection = nil

	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("SDK-shaped transport negotiated: %s", resp.Proto)
	require.Equal(t, "HTTP/1.1", resp.Proto)

	// Contrast: the SAME server negotiates HTTP/2 once we opt in.
	tr2 := DefaultTransport()
	tr2.TLSClientConfig.RootCAs = pool
	tr2.TLSClientConfig.VerifyConnection = nil
	tr2.ForceAttemptHTTP2 = true

	resp2, err := (&http.Client{Transport: tr2}).Get(srv.URL)
	require.NoError(t, err)
	defer resp2.Body.Close()
	t.Logf("with ForceAttemptHTTP2=true negotiated: %s", resp2.Proto)
	require.Equal(t, "HTTP/2.0", resp2.Proto)

	t.Log("=> SDK currently speaks HTTP/1.1 (no ForceAttemptHTTP2); HTTP/2 requires opting in.")
}
