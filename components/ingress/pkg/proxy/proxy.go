// Copyright 2025 Alibaba Group Holding Ltd.
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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"

	"github.com/alibaba/opensandbox/ingress/pkg/renewintent"
	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

const httpScheme = "http"

type Proxy struct {
	sandboxProvider      sandbox.Provider
	mode                 Mode
	renewIntentPublisher renewintent.Publisher

	secure *signature.Verifier
	scope  *routescope.Verifier
}

func NewProxy(_ context.Context, sandboxProvider sandbox.Provider, mode Mode, renewIntentPublisher renewintent.Publisher, secure *signature.Verifier, scope *routescope.Verifier) *Proxy {
	return &Proxy{
		sandboxProvider:      sandboxProvider,
		mode:                 mode,
		renewIntentPublisher: renewIntentPublisher,
		secure:               secure,
		scope:                scope,
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusCapturingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	proxyType := httpScheme
	if p.isWebSocketRequest(r) {
		proxyType = "websocket"
	}

	defer func() {
		if rcv := recover(); rcv != nil {
			// httputil.ReverseProxy panics with http.ErrAbortHandler when
			// the upstream connection drops while copying the response body
			// (e.g. SSE stream interrupted). Re-panic to let Go's net/http
			// handle it: it silently closes the connection without writing
			// anything. Catching it here and calling http.Error() would
			// leak the panic text ("net/http: abort Handler") into the
			// already-committed response stream.
			if err, ok := rcv.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(http.ErrAbortHandler)
			}

			panicErr := fmt.Sprintf("%v", rcv)
			if err, ok := rcv.(error); ok {
				panicErr = err.Error()
			}
			Logger.With(
				slogger.Field{Key: "error", Value: panicErr},
				slogger.Field{Key: "uri", Value: r.RequestURI},
				slogger.Field{Key: "host", Value: r.Host},
				slogger.Field{Key: "method", Value: r.Method},
			).Errorf("ingress: proxy causes panic")

			// Only write an error response if headers haven't been
			// committed yet. Writing to an already-committed stream
			// (e.g. an active SSE connection) would corrupt it.
			if !sw.written {
				http.Error(sw, panicErr, http.StatusBadGateway)
			}
		}
		telemetry.RecordHTTPRequest(r.Method, sw.statusCode, proxyType, float64(time.Since(start))/float64(time.Millisecond))
	}()

	host, status, err := p.getSandboxHostDefinition(r)
	if err != nil {
		var detailed interface{ InternalCause() error }
		if errors.As(err, &detailed) {
			Logger.With(slogger.Field{Key: "error", Value: detailed.InternalCause()}).Errorf("ingress: provider resolution failed")
		}
		if status == 0 {
			status = http.StatusBadRequest
		}
		if status == http.StatusServiceUnavailable {
			sw.Header().Set("Retry-After", "1")
		}
		http.Error(sw, fmt.Sprintf("OpenSandbox Ingress: %v", err), status)
		return
	}

	targetURL, code, err := p.resolveRealHost(host, r.URL)
	if err != nil {
		http.Error(sw, fmt.Sprintf("OpenSandbox Ingress: %v", err), code)
		return
	}

	if p.renewIntentPublisher != nil {
		p.renewIntentPublisher.PublishIntent(host.namespace, host.ingressKey, host.port, host.requestURI)
	}

	r.URL.Scheme = targetURL.Scheme
	r.URL.Host = targetURL.Host
	r.URL.Path = targetURL.Path
	r.URL.RawPath = targetURL.RawPath
	r.URL.RawQuery = targetURL.RawQuery
	r.Host = targetURL.Host
	r.Header.Del(SandboxIngress)
	r.Header.Del(DeprecatedSandboxIngress)
	r.Header.Del(signature.OpenSandboxSecureAccessCanonical)
	r.Header.Del(sandbox.FastSandboxCredential)
	// Host is carried by r.Host, not r.Header. The Phase 1a Fleets provider
	// accepts only the Fastlet route credential in UpstreamHeaders.
	for name, values := range host.info.UpstreamHeaders {
		r.Header.Del(name)
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}

	Logger.With(
		slogger.Field{Key: "target", Value: targetURL.String()},
		slogger.Field{Key: "client", Value: p.getClientIP(r)},
		slogger.Field{Key: "uri", Value: r.RequestURI},
		slogger.Field{Key: "method", Value: r.Method},
	).Infof("ingress requested")
	p.serve(sw, r, sandbox.EndpointTarget{RouteKind: host.routeKind, Namespace: host.namespace, SandboxID: host.ingressKey, Port: host.port})
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, target sandbox.EndpointTarget) {
	if p.isWebSocketRequest(r) {
		if r.URL == nil {
			http.Error(w, "invalid request URL", http.StatusBadRequest)
			return
		}

		switch r.URL.Scheme {
		case httpScheme:
			r.URL.Scheme = "ws"
		case "https":
			r.URL.Scheme = "wss"
		case "":
			if r.TLS != nil {
				r.URL.Scheme = "wss"
			} else {
				r.URL.Scheme = "ws"
			}
		}
		websocketProxy := NewWebSocketProxy(r.URL, p.upstreamResponseObserver(target)) //nolint:bodyclose // Failed handshake bodies are closed by copyResponse.
		websocketProxy.errorObserver = p.upstreamErrorObserver(target)
		websocketProxy.ServeHTTP(w, r)
	} else {
		if r.URL.Scheme == "" {
			if r.TLS != nil {
				r.URL.Scheme = "https"
			} else {
				r.URL.Scheme = httpScheme
			}
		}
		httpProxy := NewHTTPProxy(p.upstreamResponseObserver(target)) //nolint:bodyclose // httputil.ReverseProxy owns response bodies.
		httpProxy.errorObserver = p.upstreamErrorObserver(target)
		httpProxy.ServeHTTP(w, r)
	}
}

func (p *Proxy) upstreamResponseObserver(target sandbox.EndpointTarget) func(*http.Response) {
	if target.RouteKind != sandbox.RouteKindFleets {
		return nil
	}
	invalidator, ok := p.sandboxProvider.(sandbox.EndpointInvalidator)
	if !ok {
		return nil
	}
	return func(response *http.Response) {
		if response.StatusCode < http.StatusBadRequest || !sandbox.IsStaleFastPathResponse(response.Header) {
			return
		}
		invalidator.Invalidate(target)
		if response.Body != nil {
			_ = response.Body.Close()
		}
		response.Body = http.NoBody
		response.ContentLength = 0
		for name := range response.Header {
			delete(response.Header, name)
		}
		response.Trailer = nil
		response.StatusCode = http.StatusServiceUnavailable
		response.Status = fmt.Sprintf("%d %s", http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		response.Header.Set("Retry-After", "1")
	}
}

func (p *Proxy) upstreamErrorObserver(target sandbox.EndpointTarget) func(error) {
	if target.RouteKind != sandbox.RouteKindFleets {
		return nil
	}
	invalidator, ok := p.sandboxProvider.(sandbox.EndpointInvalidator)
	if !ok {
		return nil
	}
	return func(error) {
		invalidator.Invalidate(target)
	}
}

func (p *Proxy) isWebSocketRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.Header.Get("Upgrade") != "websocket" {
		return false
	}
	if r.Header.Get("Connection") != "Upgrade" {
		return false
	}
	return true
}

func (p *Proxy) resolveRealHost(host *sandboxHost, requestURL *url.URL) (*url.URL, int, error) {
	if host.info == nil {
		return nil, http.StatusBadGateway, errors.New("provider returned no endpoint information")
	}
	if host.info.UpstreamURL == "" {
		if host.endpoint == "" {
			return nil, http.StatusBadGateway, errors.New("provider returned an empty endpoint")
		}
		path := requestURL.Path
		rawPath := requestURL.EscapedPath()
		if host.requestURI != "" {
			path = host.requestURI
			rawPath = host.requestRawPath
		}
		return &url.URL{Host: fmt.Sprintf("%s:%d", host.endpoint, host.port), Path: path, RawPath: nonCanonicalRawPath(path, rawPath), RawQuery: requestURL.RawQuery}, 0, nil
	}
	upstream, err := url.Parse(host.info.UpstreamURL)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	upstreamPath := upstream.Path
	upstreamRawPath := upstream.EscapedPath()
	requestPath := requestURL.Path
	requestRawPath := requestURL.EscapedPath()
	if host.requestURI != "" {
		requestPath = host.requestURI
		requestRawPath = host.requestRawPath
	}
	upstream.Path = joinURLPath(upstreamPath, requestPath)
	joinedRawPath := joinURLPath(upstreamRawPath, requestRawPath)
	upstream.RawPath = nonCanonicalRawPath(upstream.Path, joinedRawPath)
	if upstream.RawQuery == "" {
		upstream.RawQuery = requestURL.RawQuery
	} else if requestURL.RawQuery != "" {
		upstream.RawQuery += "&" + requestURL.RawQuery
	}
	return upstream, 0, nil
}

func nonCanonicalRawPath(path, escapedPath string) string {
	if escapedPath == "" || escapedPath == (&url.URL{Path: path}).EscapedPath() {
		return ""
	}
	return escapedPath
}

func joinURLPath(base, suffix string) string {
	if base == "" {
		base = "/"
	}
	if suffix == "" || suffix == "/" {
		return strings.TrimRight(base, "/") + "/"
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (w *statusCapturingResponseWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusCapturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		conn, buf, err := hj.Hijack()
		if err == nil && !w.written {
			w.statusCode = http.StatusSwitchingProtocols
			w.written = true
		}
		return conn, buf, err
	}
	return nil, nil, errors.New("upstream ResponseWriter does not implement http.Hijacker")
}

func (w *statusCapturingResponseWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (p *Proxy) getClientIP(r *http.Request) string {
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if len(r.Header.Get(XForwardedFor)) != 0 {
		xff := r.Header.Get(XForwardedFor)
		s := strings.Index(xff, ", ")
		if s == -1 {
			s = len(r.Header.Get(XForwardedFor))
		}
		clientIP = xff[:s]
	} else if len(r.Header.Get(XRealIP)) != 0 {
		clientIP = r.Header.Get(XRealIP)
	}

	return clientIP
}
