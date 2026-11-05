// Copyright 2025 The OpenSandbox Authors
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

package mcpproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// httpBridge proxies a Streamable HTTP MCP upstream as an opaque stream.
// It never inspects JSON-RPC payloads: request/response bodies, SSE streams,
// status codes, and MCP headers pass through unchanged, which keeps the
// bridge compatible with any protocol version both sides support.
type httpBridge struct {
	cfg       *ServerConfig
	target    *url.URL
	keepalive time.Duration
	proxy     *httputil.ReverseProxy
	transport *cancellableTransport
}

func newHTTPBridge(cfg *ServerConfig, sseKeepalive time.Duration) (*httpBridge, error) {
	target, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcpproxy: parse upstream url: %w", err)
	}

	transport := &cancellableTransport{
		inner: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			// No ResponseHeaderTimeout: MCP POST requests may legitimately stream
			// for a long time before (or without) final headers.
		},
		inflight: make(map[*http.Request]context.CancelFunc),
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Route to the configured upstream URL. The upstream path is
			// authoritative; the /mcp/{name} suffix is a routing prefix only.
			// SetURL also merges the upstream's query string with the
			// client's (?api_key=... upstream params survive), and RawPath
			// preserves intentional escaping in the configured path (%2F).
			pr.SetURL(target)
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawPath = target.RawPath

			pr.SetXForwarded()

			// The execd access token authenticates the sandbox API only;
			// forwarding it to an upstream would leak a credential.
			pr.Out.Header.Del(model.ApiAccessTokenHeader)

			// Configured headers carry upstream credentials or overrides and
			// win over client-supplied values. A configured Host is special:
			// the transport takes the Host header from Request.Host, not the
			// header map, so route it there.
			for k, v := range cfg.Headers {
				if http.CanonicalHeaderKey(k) == "Host" {
					pr.Out.Host = v
					continue
				}
				pr.Out.Header.Set(k, v)
			}
		},
		// Flush every write immediately: MCP POST responses and the GET
		// stream are frequently text/event-stream, and buffered chunks
		// stall clients and break their read-idle assumptions.
		FlushInterval: -1,
		Transport:     transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("mcpproxy: upstream %s: %v (request: %s %s)", cfg.Name, err, r.Method, r.URL.Path)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	return &httpBridge{cfg: cfg, target: target, keepalive: sseKeepalive, proxy: proxy, transport: transport}, nil
}

// ServeHTTP forwards the request with idle-SSE keepalive injection.
func (b *httpBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kw := newKeepaliveWriter(w, b.keepalive)
	defer kw.Stop()
	b.proxy.ServeHTTP(kw, r)
}

// Close aborts every in-flight upstream request (long-lived SSE streams
// included) and fails all later ones, so a removed or reset bridge stops
// serving the previous client immediately.
func (b *httpBridge) Close() {
	b.transport.Close()
}

// cancellableTransport wraps the bridge transport so Close can abort
// in-flight requests. Successful responses tie the derived context's cancel
// to resp.Body.Close, so cancellation only strikes requests still streaming
// when Close runs.
type cancellableTransport struct {
	inner    http.RoundTripper
	mu       sync.Mutex
	closed   bool
	inflight map[*http.Request]context.CancelFunc
}

func (t *cancellableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		cancel()
		return nil, errors.New("mcpproxy: bridge closed")
	}
	t.inflight[req] = cancel
	t.mu.Unlock()

	resp, err := t.inner.RoundTrip(req)

	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if err != nil {
		t.untrack(req)
		cancel()
		if closed {
			return nil, errors.New("mcpproxy: bridge closed")
		}
		return nil, err
	}
	// The request stays tracked until the response body closes, so Close can
	// abort streams that are still being copied to the client.
	resp.Body = cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel, remove: func() { t.untrack(req) }}
	return resp, nil
}

func (t *cancellableTransport) untrack(req *http.Request) {
	t.mu.Lock()
	delete(t.inflight, req)
	t.mu.Unlock()
}

// Close cancels every in-flight request, rejects later ones, and releases
// the wrapped transport's idle connections.
func (t *cancellableTransport) Close() {
	t.mu.Lock()
	t.closed = true
	cancels := make([]context.CancelFunc, 0, len(t.inflight))
	for _, cancel := range t.inflight {
		cancels = append(cancels, cancel)
	}
	t.inflight = make(map[*http.Request]context.CancelFunc)
	t.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	if idle, ok := t.inner.(interface{ CloseIdleConnections() }); ok {
		idle.CloseIdleConnections()
	}
}

// cancelOnCloseBody releases the request context (and untracks the request)
// when the proxy closes the response body.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	remove func()
}

func (b cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	b.remove()
	return err
}
