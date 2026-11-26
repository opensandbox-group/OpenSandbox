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

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"

	"github.com/alibaba/opensandbox/ingress/pkg/activity"
	"github.com/alibaba/opensandbox/ingress/pkg/renewintent"
	"github.com/alibaba/opensandbox/ingress/pkg/routescope"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
)

const httpScheme = "http"

// Mode selects how the target sandbox is discovered from an incoming request.
type Mode string

const (
	// ModeHeader resolves the target from the OpenSandbox-Ingress-To header or Host.
	ModeHeader Mode = "header"
	// ModeURI resolves the target from leading URI path segments.
	ModeURI Mode = "uri"
)

type Proxy struct {
	sandboxProvider      sandbox.Provider
	mode                 Mode
	renewIntentPublisher renewintent.Publisher
	activity             activity.Recorder
	waker                *wake.Waker

	secure               *signature.Verifier
	scope                *routescope.Verifier
	fastSandboxNamespace string

	httpTransport             http.RoundTripper
	websocketClient           *http.Client
	websocketMessageSizeLimit int64
}

func NewProxy(_ context.Context, sandboxProvider sandbox.Provider, mode Mode, renewIntentPublisher renewintent.Publisher, secure *signature.Verifier, scope *routescope.Verifier, opts ...Option) *Proxy {
	options := proxyOptions{webSocketMessageSizeLimit: defaultWebSocketMessageSizeLimit}
	for _, opt := range opts {
		opt(&options)
	}
	if options.activity == nil {
		options.activity = activity.Noop{}
	}

	return &Proxy{
		sandboxProvider:           sandboxProvider,
		mode:                      mode,
		renewIntentPublisher:      renewIntentPublisher,
		activity:                  options.activity,
		waker:                     options.waker,
		secure:                    secure,
		scope:                     scope,
		fastSandboxNamespace:      options.fastSandboxNamespace,
		httpTransport:             newHTTPTransport(options.connectObserver, options.httpConnectionPool),
		websocketClient:           newObservedWebSocketHTTPClient(options.connectObserver),
		websocketMessageSizeLimit: options.webSocketMessageSizeLimit,
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Prevent net/http from closing a request body still being read by the upstream transport.
	_ = http.NewResponseController(w).EnableFullDuplex()

	start := time.Now()
	sw := &statusCapturingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	// Full duplex disables net/http's unread-body close announcement; restore it in the writer.
	if r.Body != nil && r.ContentLength != 0 {
		body := &requestBody{ReadCloser: r.Body, size: r.ContentLength}
		r.Body = body
		sw.requestBody = body
	}
	proxyType := httpScheme
	if p.isWebSocketRequest(r) {
		proxyType = "websocket"
	}

	defer func() {
		if rcv := recover(); rcv != nil {
			// Let net/http abort broken upstream streams without injecting an error body.
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

			// Never append an error to an already-committed stream.
			if !sw.written {
				http.Error(sw, panicErr, http.StatusBadGateway)
			}
		}
		telemetry.RecordHTTPRequest(r.Method, sw.statusCode, proxyType, float64(time.Since(start))/float64(time.Millisecond))
	}()

	p.handle(sw, r)
}

func (p *Proxy) handle(sw *statusCapturingResponseWriter, r *http.Request) {
	host, status, err := p.getSandboxHostDefinition(r)
	if err != nil {
		var detailed interface{ InternalCause() error }
		if errors.As(err, &detailed) {
			Logger.With(slogger.Field{Key: "error", Value: detailed.InternalCause()}).Errorf("ingress: provider resolution failed")
		}
		if respondWakeTimeout(sw, err) {
			return
		}
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeIngressError(sw, err, status)
		return
	}

	// Skipping renewal does not suppress activity.
	if p.activity != nil && !IsSkipActivity(r.Header) {
		p.activity.Record(host.namespace, host.ingressKey)
	}

	targetURL, code, err := p.resolveRealHost(host, r.URL)
	if err != nil {
		writeIngressError(sw, err, code)
		return
	}

	if p.renewIntentPublisher != nil && !IsAccessRenewSkip(r.Header) {
		p.renewIntentPublisher.PublishIntent(host.namespace, host.ingressKey, host.port, host.requestURI)
	}

	// Keep the client-facing URL so a stale-route replay can restore and
	// re-resolve from the original request shape.
	originalURL := *r.URL
	originalHost := r.Host

	p.applyUpstreamRequest(r, host, targetURL)

	target := sandbox.EndpointTarget{RouteKind: host.routeKind, Namespace: host.namespace, SandboxID: host.ingressKey, Port: host.port}

	Logger.With(
		slogger.Field{Key: "target", Value: targetURL.String()},
		slogger.Field{Key: "client", Value: p.getClientIP(r)},
		slogger.Field{Key: "uri", Value: r.RequestURI},
		slogger.Field{Key: "method", Value: r.Method},
	).Infof("ingress requested")

	// A cached route may be paused between resolution and forwarding.
	var replay *wakeReplay
	if p.waker != nil && host.routeKind == sandbox.RouteKindFastSandbox && !p.isWebSocketRequest(r) {
		replay = newWakeReplay(p, sw, host, r, target)
	}
	if replay != nil {
		p.serve(replay, r, target, replay.observeStale()) //nolint:bodyclose // httputil.ReverseProxy owns response bodies.
	} else {
		p.serve(sw, r, target) //nolint:bodyclose // httputil.ReverseProxy owns response bodies.
	}

	if replay == nil || !replay.intercepted.Load() {
		return
	}
	if replay.wakeErr != nil {
		if status := httpStatusForWakeErr(replay.wakeErr); status != 0 {
			writeIngressError(sw, replay.wakeErr, status)
		}
		return
	}

	// Replay once on a fresh route; the client has received nothing and the body is buffered.
	*r.URL = originalURL
	r.Host = originalHost
	r.Body = io.NopCloser(bytes.NewReader(replay.copied))
	endpoint, err := p.sandboxProvider.ResolveEndpoint(r.Context(), target)
	if err != nil {
		writeIngressError(sw, err, providerErrHTTPStatus(err))
		return
	}
	host.info = endpoint
	targetURL, code, err = p.resolveRealHost(host, r.URL)
	if err != nil {
		writeIngressError(sw, err, code)
		return
	}
	p.applyUpstreamRequest(r, host, targetURL)
	p.serve(sw, r, target)
}

// Rewriting must be idempotent for stale-route replay.
func (p *Proxy) applyUpstreamRequest(r *http.Request, host *sandboxHost, targetURL *url.URL) {
	rewriteRequestURL(r, targetURL)
	r.Header.Del(SandboxIngress)
	r.Header.Del(DeprecatedSandboxIngress)
	r.Header.Del(AccessRenew)
	r.Header.Del(SkipActivity)
	r.Header.Del(signature.OpenSandboxSecureAccessCanonical)
	r.Header.Del(sandbox.FastSandboxCredential)
	// Replace client-supplied credentials before injecting provider-owned values.
	for name, values := range host.info.UpstreamHeaders {
		r.Header.Del(name)
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
}

//nolint:bodyclose // HTTP and WebSocket proxies own upstream response bodies.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, target sandbox.EndpointTarget, extraResponseObservers ...func(*http.Response)) {
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
		websocketProxy := newWebSocketProxy(r.URL, p.upstreamResponseObserver(target))
		websocketProxy.errorObserver = p.upstreamErrorObserver(target)
		websocketProxy.httpClient = p.websocketClient
		websocketProxy.messageSizeLimit = p.websocketMessageSizeLimit
		websocketProxy.ServeHTTP(w, r)
	} else {
		if r.URL.Scheme == "" {
			if r.TLS != nil {
				r.URL.Scheme = "https"
			} else {
				r.URL.Scheme = httpScheme
			}
		}
		observers := []func(*http.Response){stripWakeMarkerObserver()}
		observers = append(observers, extraResponseObservers...)
		observers = append(observers, p.upstreamResponseObserver(target))
		proxy := &httpProxy{
			responseObservers: observers,
			errorObserver:     p.upstreamErrorObserver(target),
			transport:         p.httpTransport,
		}
		proxy.ServeHTTP(w, r)
	}
}

func (p *Proxy) upstreamResponseObserver(target sandbox.EndpointTarget) func(*http.Response) {
	if target.RouteKind != sandbox.RouteKindFastSandbox {
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
	if target.RouteKind != sandbox.RouteKindFastSandbox {
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

// Accept RFC 6455 GET upgrades, not RFC 8441 Extended CONNECT.
func (p *Proxy) isWebSocketRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.EqualFold(r.Header.Get(hopByHopUpgrade), "websocket") {
		return false
	}
	return connectionHasToken(r, "upgrade")
}

// Connection headers carry comma-separated tokens (RFC 7230 §6.1).
func connectionHasToken(r *http.Request, token string) bool {
	for _, v := range r.Header.Values(hopByHopConnection) {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), token) {
				return true
			}
		}
	}
	return false
}

func (p *Proxy) getClientIP(r *http.Request) string {
	if xff := r.Header.Get(xForwardedFor); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	if realIP := r.Header.Get(xRealIP); realIP != "" {
		return realIP
	}
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	return clientIP
}
