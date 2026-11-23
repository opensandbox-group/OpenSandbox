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

// Package forwarder implements the OSEP-0030 forward-mode relay: every
// redirected sandbox TCP connection is tunneled byte-for-byte to the
// configured gateway via HTTP CONNECT to its original IP:port. There is no
// local TLS interception and no direct-dial fallback: any tunnel failure
// resets the sandbox connection.
package forwarder

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// Identity headers sent on every CONNECT (closed set of platform-truth fields).
const (
	HeaderSandboxID        = "OpenSandbox-Sandbox-Id"
	HeaderSandboxNamespace = "OpenSandbox-Sandbox-Namespace"
	HeaderTenantName       = "OpenSandbox-Tenant-Name"
)

const (
	defaultDialTimeout      = 10 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
)

// Identity is the server-assigned sandbox identity asserted on the tunnel.
// SandboxID is required; Namespace and TenantName are sent only when set.
type Identity struct {
	SandboxID  string
	Namespace  string
	TenantName string
}

// Config describes the gateway hop.
type Config struct {
	Scheme        string // "http" or "https"
	Host          string // gateway hostname or literal IP
	Port          int
	Authorization string // complete Proxy-Authorization value; never logged
	RootCAs       *x509.CertPool
	Identity      Identity
	// Mark is set as SO_MARK on every gateway socket (Linux only).
	Mark int
}

// Forwarder relays accepted connections to the gateway.
type Forwarder struct {
	cfg              Config
	gatewayAddr      string
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
	// Seams for tests; production uses the Linux socket options.
	originalDst func(net.Conn) (netip.AddrPort, error)
	dialGateway func(ctx context.Context) (net.Conn, error)
}

// New validates cfg and returns a Forwarder.
func New(cfg Config) (*Forwarder, error) {
	if cfg.Scheme != "http" && cfg.Scheme != "https" {
		return nil, fmt.Errorf("unsupported gateway scheme %q", cfg.Scheme)
	}
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid gateway address")
	}
	if strings.TrimSpace(cfg.Authorization) == "" {
		return nil, fmt.Errorf("gateway authorization is required")
	}
	if strings.ContainsAny(cfg.Authorization, "\r\n\x00") {
		return nil, fmt.Errorf("gateway authorization contains control characters")
	}
	if cfg.Identity.SandboxID == "" {
		return nil, fmt.Errorf("sandbox id is required")
	}
	for name, v := range map[string]string{
		HeaderSandboxID:        cfg.Identity.SandboxID,
		HeaderSandboxNamespace: cfg.Identity.Namespace,
		HeaderTenantName:       cfg.Identity.TenantName,
	} {
		if err := ValidateIdentityValue(v); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	f := &Forwarder{
		cfg:              cfg,
		gatewayAddr:      net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		dialTimeout:      defaultDialTimeout,
		handshakeTimeout: defaultHandshakeTimeout,
		originalDst:      originalDst,
	}
	f.dialGateway = f.dialGatewayConn
	return f, nil
}

// ValidateIdentityValue accepts empty or printable ASCII without whitespace
// (0x21-0x7E), so identity can never inject or split CONNECT headers.
func ValidateIdentityValue(v string) error {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return errors.New("must be printable ASCII without whitespace or control characters")
		}
	}
	return nil
}

// Serve accepts connections from ln until it is closed.
func (f *Forwarder) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Transient failures (e.g. EMFILE) must not stop the relay for
			// the sandbox's lifetime; back off and keep accepting.
			log.Warnf("[forward] accept failed: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go f.handle(conn)
	}
}

func (f *Forwarder) handle(client net.Conn) {
	dst, err := f.originalDst(client)
	if err != nil {
		log.Warnf("[forward] original destination unavailable (remote=%s): %v", client.RemoteAddr(), err)
		reset(client)
		return
	}
	if !dst.IsValid() || dst.Addr().IsLoopback() || dst.Addr().IsUnspecified() ||
		dst.Addr().IsLinkLocalUnicast() || dst.Addr().IsLinkLocalMulticast() {
		// Loopback and link-local are never redirected; a direct dial to the
		// forwarder port must not be relayed as if it were sandbox egress.
		log.Warnf("[forward] refusing non-routable destination %s", dst)
		reset(client)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.dialTimeout+f.handshakeTimeout)
	upstream, br, err := f.openTunnel(ctx, dst)
	cancel()
	if err != nil {
		log.Warnf("[forward] tunnel to %s via gateway failed: %v", dst, err)
		reset(client)
		return
	}
	log.Debugf("[forward] tunnel established to %s", dst)
	relay(client, upstream, br)
}

// openTunnel dials the gateway and completes the CONNECT handshake. The
// returned reader must be used for upstream reads: it may already hold bytes
// the origin sent right after the 2xx response.
func (f *Forwarder) openTunnel(ctx context.Context, dst netip.AddrPort) (net.Conn, *bufio.Reader, error) {
	upstream, err := f.dialGateway(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("dial gateway: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = upstream.SetDeadline(deadline)
	}
	if _, err := upstream.Write(f.connectRequest(dst)); err != nil {
		_ = upstream.Close()
		return nil, nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = upstream.Close()
		return nil, nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = upstream.Close()
		return nil, nil, fmt.Errorf("gateway answered %s", resp.Status)
	}
	// RFC 9110 §9.3.6: ignore any Content-Length/Transfer-Encoding framing on a
	// 2xx CONNECT response. Do not call resp.Body.Close(), which would drain
	// tunnel bytes (or block on client-first protocols) from br.
	_ = upstream.SetDeadline(time.Time{})
	return upstream, br, nil
}

func (f *Forwarder) connectRequest(dst netip.AddrPort) []byte {
	target := dst.String()
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	fmt.Fprintf(&b, "Proxy-Authorization: %s\r\n", f.cfg.Authorization)
	fmt.Fprintf(&b, "%s: %s\r\n", HeaderSandboxID, f.cfg.Identity.SandboxID)
	if f.cfg.Identity.Namespace != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", HeaderSandboxNamespace, f.cfg.Identity.Namespace)
	}
	if f.cfg.Identity.TenantName != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", HeaderTenantName, f.cfg.Identity.TenantName)
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// ProbeGateway checks that the gateway is reachable over the same path as a
// tunnel (forwarder mark, DNS, nft scope, TLS trust) without sending CONNECT.
func (f *Forwarder) ProbeGateway(ctx context.Context) error {
	conn, err := f.dialGateway(ctx)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (f *Forwarder) dialGatewayConn(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: f.dialTimeout, Control: MarkControl(f.cfg.Mark)}
	conn, err := d.DialContext(ctx, "tcp", f.gatewayAddr)
	if err != nil {
		return nil, err
	}
	if f.cfg.Scheme != "https" {
		return conn, nil
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: f.cfg.Host,
		RootCAs:    f.cfg.RootCAs,
		MinVersion: tls.VersionTLS12,
	})
	hsCtx, cancel := context.WithTimeout(ctx, f.handshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gateway TLS handshake: %w", err)
	}
	return tlsConn, nil
}

type closeWriter interface {
	CloseWrite() error
}

// relay copies bytes in both directions, preserving half-closes on clean EOF
// and aborting both connections if either copy fails.
func relay(client, upstream net.Conn, upstreamReader io.Reader) {
	var (
		wg      sync.WaitGroup
		aborted atomic.Bool
	)
	abort := func() {
		if aborted.CompareAndSwap(false, true) {
			reset(client)
			reset(upstream)
		}
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(upstream, client); err != nil {
			abort()
			return
		}
		if cw, ok := upstream.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = upstream.Close()
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(client, upstreamReader); err != nil {
			abort()
			return
		}
		if cw, ok := client.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = client.Close()
		}
	}()
	wg.Wait()
	_ = client.Close()
	_ = upstream.Close()
}

// reset closes c with SO_LINGER 0 so the sandbox sees an RST, not a clean EOF.
func reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}
