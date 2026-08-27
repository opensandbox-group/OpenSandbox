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
	"strings"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/renewintent"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

type Proxy struct {
	sandboxProvider      sandbox.Provider
	mode                 Mode
	renewIntentPublisher renewintent.Publisher

	secure *signature.Verifier
}

func NewProxy(_ context.Context, sandboxProvider sandbox.Provider, mode Mode, renewIntentPublisher renewintent.Publisher, secure *signature.Verifier) *Proxy {
	return &Proxy{
		sandboxProvider:      sandboxProvider,
		mode:                 mode,
		renewIntentPublisher: renewIntentPublisher,
		secure:               secure,
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusCapturingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	proxyType := "http"
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
		if status == 0 {
			status = http.StatusBadRequest
		}
		http.Error(sw, fmt.Sprintf("OpenSandbox Ingress: %v", err), status)
		return
	}

	targetHost, err, code := p.resolveRealHost(host)
	if err != nil {
		http.Error(sw, fmt.Sprintf("OpenSandbox Ingress: %v", err), code)
		return
	}

	if p.renewIntentPublisher != nil {
		p.renewIntentPublisher.PublishIntent(host.ingressKey, host.port, host.requestURI)
	}

	// modify if requestURI is not empty
	if host.requestURI != "" {
		r.URL.Path = host.requestURI
	}

	r.Host = targetHost
	r.URL.Host = targetHost
	r.Header.Del(SandboxIngress)
	r.Header.Del(signature.OpenSandboxSecureAccessCanonical)

	Logger.With(
		slogger.Field{Key: "target", Value: targetHost},
		slogger.Field{Key: "client", Value: p.getClientIP(r)},
		slogger.Field{Key: "uri", Value: r.RequestURI},
		slogger.Field{Key: "method", Value: r.Method},
	).Infof("ingress requested")
	p.serve(sw, r)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	if p.isWebSocketRequest(r) {
		if r.URL == nil {
			http.Error(w, "invalid request URL", http.StatusBadRequest)
			return
		}

		if r.URL.Scheme == "" {
			if r.TLS != nil {
				r.URL.Scheme = "wss"
			} else {
				r.URL.Scheme = "ws"
			}
		}
		NewWebSocketProxy(r.URL).ServeHTTP(w, r)
	} else {
		if r.URL.Scheme == "" {
			if r.TLS != nil {
				r.URL.Scheme = "https"
			} else {
				r.URL.Scheme = "http"
			}
		}
		NewHTTPProxy().ServeHTTP(w, r)
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

func (p *Proxy) resolveRealHost(host *sandboxHost) (string, error, int) {
	endpoint := host.endpoint
	if endpoint == "" {
		// Fallback lookup (should rarely happen because host parsing now fills endpoint).
		info, err := p.sandboxProvider.GetEndpoint(host.ingressKey)
		if err != nil {
			// Map sandbox errors to HTTP status codes
			switch {
			case errors.Is(err, sandbox.ErrSandboxNotFound):
				return "", err, http.StatusNotFound
			case errors.Is(err, sandbox.ErrSandboxNotReady):
				return "", err, http.StatusServiceUnavailable
			default:
				return "", err, http.StatusBadGateway
			}
		}
		endpoint = info.Endpoint
	}

	// Construct target host with port
	targetHost := fmt.Sprintf("%s:%d", endpoint, host.port)
	return targetHost, nil, 0
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
	return nil, nil, fmt.Errorf("upstream ResponseWriter does not implement http.Hijacker")
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
