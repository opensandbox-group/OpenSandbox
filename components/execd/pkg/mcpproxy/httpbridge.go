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
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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
}

func newHTTPBridge(cfg *ServerConfig, sseKeepalive time.Duration) (*httpBridge, error) {
	target, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcpproxy: parse upstream url: %w", err)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// No ResponseHeaderTimeout: MCP POST requests may legitimately stream
		// for a long time before (or without) final headers.
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Route to the configured upstream URL. The upstream path is
			// authoritative; the /mcp/{name} suffix is a routing prefix only.
			pr.SetURL(target)
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery

			pr.SetXForwarded()

			// The execd access token authenticates the sandbox API only;
			// forwarding it to an upstream would leak a credential.
			pr.Out.Header.Del(model.ApiAccessTokenHeader)

			// Configured headers carry upstream credentials or overrides and
			// win over client-supplied values.
			for k, v := range cfg.Headers {
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

	return &httpBridge{cfg: cfg, target: target, keepalive: sseKeepalive, proxy: proxy}, nil
}

// ServeHTTP forwards the request with idle-SSE keepalive injection.
func (b *httpBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kw := newKeepaliveWriter(w, b.keepalive)
	defer kw.Stop()
	b.proxy.ServeHTTP(kw, r)
}
