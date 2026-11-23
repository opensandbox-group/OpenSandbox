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

package forwarder

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testDst = netip.MustParseAddrPort("203.0.113.10:443")

// connectGateway is a CONNECT proxy that records the request, answers with
// status, then (on 2xx) sends a server-first banner and echoes the tunnel.
type connectGateway struct {
	status      int
	connectResp string
	requests    chan *http.Request
}

func (g *connectGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.requests <- r
	if r.Method != http.MethodConnect {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if g.status < 200 || g.status > 299 {
		w.WriteHeader(g.status)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	resp := g.connectResp
	if resp == "" {
		resp = "HTTP/1.1 200 Connection Established\r\n\r\nbanner|"
	}
	_, _ = rw.WriteString(resp)
	_ = rw.Flush()
	_, _ = io.Copy(conn, rw)
}

func newGateway(t *testing.T, status int, useTLS bool) (*httptest.Server, *connectGateway) {
	t.Helper()
	gw := &connectGateway{status: status, requests: make(chan *http.Request, 4)}
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(gw)
	} else {
		srv = httptest.NewServer(gw)
	}
	t.Cleanup(srv.Close)
	return srv, gw
}

func newTestForwarder(t *testing.T, srv *httptest.Server, useTLS bool, id Identity) *Forwarder {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	cfg := Config{Scheme: "http", Host: host, Port: port, Authorization: "Bearer secret", Identity: id}
	if useTLS {
		cfg.Scheme = "https"
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		cfg.RootCAs = pool
	}
	f, err := New(cfg)
	require.NoError(t, err)
	f.originalDst = func(net.Conn) (netip.AddrPort, error) { return testDst, nil }
	return f
}

// tcpPair returns a connected (sandbox side, forwarder side) TCP pair.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	server := <-accepted
	require.NotNil(t, server)
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func TestNewValidatesConfig(t *testing.T) {
	base := Config{Scheme: "http", Host: "gw.example.com", Port: 3128, Authorization: "Bearer x", Identity: Identity{SandboxID: "sbx-1"}}
	_, err := New(base)
	require.NoError(t, err)

	cases := map[string]func(c *Config){
		"scheme":         func(c *Config) { c.Scheme = "socks5" },
		"port":           func(c *Config) { c.Port = 0 },
		"missing auth":   func(c *Config) { c.Authorization = " " },
		"auth CRLF":      func(c *Config) { c.Authorization = "Bearer x\r\nX-Evil: 1" },
		"missing id":     func(c *Config) { c.Identity.SandboxID = "" },
		"id whitespace":  func(c *Config) { c.Identity.SandboxID = "sbx 1" },
		"namespace CRLF": func(c *Config) { c.Identity.Namespace = "ns\r\nX-Evil: 1" },
		"tenant non-ascii": func(c *Config) {
			c.Identity.TenantName = "租户"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			_, err := New(c)
			require.Error(t, err)
		})
	}
}

func TestConnectRequestHeaders(t *testing.T) {
	f, err := New(Config{Scheme: "http", Host: "gw", Port: 1, Authorization: "Bearer t",
		Identity: Identity{SandboxID: "sbx-1"}})
	require.NoError(t, err)
	require.Equal(t,
		"CONNECT 203.0.113.10:443 HTTP/1.1\r\nHost: 203.0.113.10:443\r\n"+
			"Proxy-Authorization: Bearer t\r\nOpenSandbox-Sandbox-Id: sbx-1\r\n\r\n",
		string(f.connectRequest(testDst)))

	f.cfg.Identity = Identity{SandboxID: "sbx-1", Namespace: "team-a", TenantName: "acme"}
	require.Contains(t, string(f.connectRequest(testDst)),
		"OpenSandbox-Sandbox-Id: sbx-1\r\nOpenSandbox-Sandbox-Namespace: team-a\r\nOpenSandbox-Tenant-Name: acme\r\n\r\n")
}

func TestRelayThroughGateway(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run("tls="+strconv.FormatBool(useTLS), func(t *testing.T) {
			srv, gw := newGateway(t, http.StatusOK, useTLS)
			f := newTestForwarder(t, srv, useTLS, Identity{SandboxID: "sbx-1", Namespace: "team-a"})
			sandbox, accepted := tcpPair(t)
			go f.handle(accepted)

			req := <-gw.requests
			require.Equal(t, http.MethodConnect, req.Method)
			require.Equal(t, "203.0.113.10:443", req.Host)
			require.Equal(t, "Bearer secret", req.Header.Get("Proxy-Authorization"))
			require.Equal(t, "sbx-1", req.Header.Get(HeaderSandboxID))
			require.Equal(t, "team-a", req.Header.Get(HeaderSandboxNamespace))
			require.Empty(t, req.Header.Values(HeaderTenantName))

			_ = sandbox.SetDeadline(time.Now().Add(5 * time.Second))
			// Server-first bytes sent right after the 2xx must not be lost.
			banner := make([]byte, len("banner|"))
			_, err := io.ReadFull(sandbox, banner)
			require.NoError(t, err)
			require.Equal(t, "banner|", string(banner))

			payload := []byte("\x16\x03\x01opaque-tls-bytes")
			_, err = sandbox.Write(payload)
			require.NoError(t, err)
			echo := make([]byte, len(payload))
			_, err = io.ReadFull(sandbox, echo)
			require.NoError(t, err)
			require.Equal(t, payload, echo)
		})
	}
}

func TestRelayIgnoresConnectBodyFramingHeaders(t *testing.T) {
	cases := []struct {
		name        string
		connectResp string
		banner      string
	}{
		{
			name:        "content-length server-first",
			connectResp: "HTTP/1.1 200 Connection Established\r\nContent-Length: 5\r\n\r\nHELLO|PAYLOAD",
			banner:      "HELLO|PAYLOAD",
		},
		{
			name:        "content-length client-first",
			connectResp: "HTTP/1.1 200 Connection Established\r\nContent-Length: 5\r\n\r\n",
			banner:      "",
		},
		{
			name:        "chunked server-first",
			connectResp: "HTTP/1.1 200 Connection Established\r\nTransfer-Encoding: chunked\r\n\r\nHELLO|PAYLOAD",
			banner:      "HELLO|PAYLOAD",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gw := newGateway(t, http.StatusOK, false)
			gw.connectResp = tc.connectResp
			f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
			f.handshakeTimeout = 2 * time.Second
			sandbox, accepted := tcpPair(t)
			go f.handle(accepted)
			<-gw.requests

			_ = sandbox.SetDeadline(time.Now().Add(5 * time.Second))
			if tc.banner != "" {
				got := make([]byte, len(tc.banner))
				_, err := io.ReadFull(sandbox, got)
				require.NoError(t, err)
				require.Equal(t, tc.banner, string(got))
			}

			payload := []byte("\x16\x03\x01client-first-hello")
			_, err := sandbox.Write(payload)
			require.NoError(t, err)
			echo := make([]byte, len(payload))
			_, err = io.ReadFull(sandbox, echo)
			require.NoError(t, err)
			require.Equal(t, payload, echo)
		})
	}
}

func TestRelayUpstreamResetAbortsBothDirections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\npartial")
		_ = rw.Flush()
		// Wait until the sandbox has received the initial bytes, then RST.
		buf := make([]byte, 4)
		_, _ = io.ReadFull(rw, buf)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
	sandbox, accepted := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handle(accepted)
	}()

	_ = sandbox.SetDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len("partial"))
	_, err := io.ReadFull(sandbox, got)
	require.NoError(t, err)
	require.Equal(t, "partial", string(got))

	// Trigger the gateway RST while keeping the sandbox connection open.
	_, err = sandbox.Write([]byte("ping"))
	require.NoError(t, err)

	// relay must terminate without waiting for the sandbox to close.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay blocked after upstream reset while sandbox stayed open")
	}

	// Sandbox must observe a connection error (RST), not a clean EOF.
	_, err = sandbox.Read(make([]byte, 1))
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
}

func TestRelayPreservesHalfClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		req, err := io.ReadAll(rw)
		if err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("reply:"), req...))
	}))
	t.Cleanup(srv.Close)

	f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
	sandbox, accepted := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.handle(accepted)
	}()

	_ = sandbox.SetDeadline(time.Now().Add(5 * time.Second))
	_, err := sandbox.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, sandbox.(*net.TCPConn).CloseWrite())

	reply, err := io.ReadAll(sandbox)
	require.NoError(t, err)
	require.Equal(t, "reply:hello", string(reply))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not finish after clean half-close")
	}
}

func requireClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := c.Read(make([]byte, 1))
	require.Error(t, err)
	var ne net.Error
	require.False(t, errors.As(err, &ne) && ne.Timeout(), "connection was not closed: %v", err)
}

func TestGatewayRejectionClosesClient(t *testing.T) {
	srv, gw := newGateway(t, http.StatusForbidden, false)
	f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
	sandbox, accepted := tcpPair(t)
	go f.handle(accepted)
	<-gw.requests
	requireClosed(t, sandbox)
}

func TestGatewayDialFailureNeverDialsDirect(t *testing.T) {
	srv, _ := newGateway(t, http.StatusOK, false)
	f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
	var dials atomic.Int32
	f.dialGateway = func(context.Context) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("gateway down")
	}
	sandbox, accepted := tcpPair(t)
	go f.handle(accepted)
	requireClosed(t, sandbox)
	require.Equal(t, int32(1), dials.Load())
}

func TestNonRoutableDestinationsRefused(t *testing.T) {
	srv, _ := newGateway(t, http.StatusOK, false)
	for _, target := range []string{"127.0.0.1:18082", "169.254.169.254:80", "224.0.0.251:5353", "0.0.0.0:80"} {
		t.Run(target, func(t *testing.T) {
			f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
			f.originalDst = func(net.Conn) (netip.AddrPort, error) {
				return netip.MustParseAddrPort(target), nil
			}
			var dials atomic.Int32
			f.dialGateway = func(context.Context) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("unexpected")
			}
			sandbox, accepted := tcpPair(t)
			go f.handle(accepted)
			requireClosed(t, sandbox)
			require.Zero(t, dials.Load())
		})
	}
}

func TestProbeGateway(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		srv, gw := newGateway(t, http.StatusOK, useTLS)
		f := newTestForwarder(t, srv, useTLS, Identity{SandboxID: "sbx-1"})
		require.NoError(t, f.ProbeGateway(context.Background()))
		select {
		case <-gw.requests:
			t.Fatal("probe must not send CONNECT")
		default:
		}
	}

	// Untrusted TLS listener fails the probe like it would fail a tunnel.
	srv, _ := newGateway(t, http.StatusOK, true)
	f := newTestForwarder(t, srv, false, Identity{SandboxID: "sbx-1"})
	f.cfg.Scheme = "https"
	require.Error(t, f.ProbeGateway(context.Background()))

	srv.Close()
	require.Error(t, f.ProbeGateway(context.Background()))
}
