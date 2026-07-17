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
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	slogger "github.com/alibaba/opensandbox/internal/logger"
)

type HTTPProxy struct{}

func NewHTTPProxy() *HTTPProxy {
	return &HTTPProxy{}
}

func (hp *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.String()

	proxy, err := hp.newReverseProxy(targetURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	proxy.ServeHTTP(w, r)
}

func (hp *HTTPProxy) newReverseProxy(targetHost string) (*httputil.ReverseProxy, error) {
	url, err := url.Parse(targetHost)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(url)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = url.Scheme
		req.URL.Host = url.Host
		req.Host = url.Host
		req.Header.Del(SandboxIngress)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Add(ReverseProxyServerPowerBy, "OpenSandbox-ingress")
		return nil
	}
	// Custom error handler: log the upstream error and return 502 only
	// when the response headers have not yet been committed. If the
	// response is already streaming (e.g. SSE), writing an error body
	// would corrupt the stream, so we silently let the connection close.
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		Logger.With(
			slogger.Field{Key: "error", Value: fmt.Sprintf("%v", err)},
			slogger.Field{Key: "uri", Value: req.RequestURI},
			slogger.Field{Key: "method", Value: req.Method},
		).Errorf("ingress: reverse proxy upstream error")

		// Attempt to set 502; this is a no-op if headers are already sent.
		rw.WriteHeader(http.StatusBadGateway)
	}
	return proxy, nil
}
