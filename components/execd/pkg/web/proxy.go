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

package web

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

func ProxyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, "/proxy/") {
			c.Next()
			return
		}

		r := c.Request
		w := c.Writer

		rest := strings.TrimPrefix(r.URL.Path, "/proxy/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, "port is required", http.StatusBadRequest)
			c.Abort()
			return
		}

		port := parts[0]
		path := "/"
		if len(parts) == 2 && parts[1] != "" {
			path += parts[1]
		}

		target := &url.URL{
			Scheme: "http",
			Host:   "127.0.0.1:" + port,
			Path:   path,
		}

		isWebSocket := strings.ToLower(r.Header.Get("Upgrade")) == "websocket"

		proxy := httputil.NewSingleHostReverseProxy(target)
		// Flush SSE chunks promptly; a small interval avoids buffering breaks chunked streams.
		proxy.FlushInterval = 200 * time.Millisecond

		proxy.Director = func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "127.0.0.1:" + port
			req.URL.Path = path
			req.URL.RawQuery = r.URL.RawQuery
			req.URL.RawPath = ""
			req.RequestURI = ""

			req.Header.Set("X-Forwarded-For", getClientIP(r))
			req.Header.Set("X-Forwarded-Proto", "http")
			req.Header.Del("X-Forwarded-Host")

			if isWebSocket {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Sec-WebSocket-Version", "13")
				if key := r.Header.Get("Sec-WebSocket-Key"); key != "" {
					req.Header.Set("Sec-WebSocket-Key", key)
				}
			}
		}

		proxy.Transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   600 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     600 * time.Second,
		}

		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			log.Error("proxy: %v (request: %s %s)", err, req.Method, req.RequestURI)
			http.Error(rw, "Bad Gateway", http.StatusBadGateway)
		}

		log.Info("proxy: %s %s -> %s (websocket=%v)", r.Method, r.RequestURI, target.Host, isWebSocket)

		proxy.ServeHTTP(w, r)
		c.Abort()
	}
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
