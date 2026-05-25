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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/alibaba/opensandbox/ingress/pkg/renewintent"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
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
	defer func() {
		if rcv := recover(); rcv != nil {
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
			http.Error(w, panicErr, http.StatusBadGateway)
		}
	}()

	host, status, err := p.getSandboxHostDefinition(r)
	if err != nil {
		if status == 0 {
			status = http.StatusBadRequest
		}
		http.Error(w, fmt.Sprintf("OpenSandbox Ingress: %v", err), status)
		return
	}

	targetHost, err, code := p.resolveRealHost(host)
	if err != nil {
		http.Error(w, fmt.Sprintf("OpenSandbox Ingress: %v", err), code)
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
	p.serve(w, r)
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
