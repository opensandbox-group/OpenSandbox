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
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	slogger "github.com/alibaba/opensandbox/internal/logger"
)

type httpProxy struct {
	responseObservers []func(*http.Response)
	errorObserver     func(error)
	transport         http.RoundTripper
}

func (hp *httpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	targetURL := *r.URL

	proxy := hp.newReverseProxy(&targetURL)

	proxy.ServeHTTP(w, r)
}

func (hp *httpProxy) newReverseProxy(targetURL *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	if hp.transport != nil {
		proxy.Transport = hp.transport
	}
	proxy.Director = func(req *http.Request) {
		rewriteRequestURL(req, targetURL)
		req.Header.Del(SandboxIngress)
		req.Header.Del(AccessRenew)
		req.Header.Del(SkipActivity)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		for _, observe := range hp.responseObservers {
			if observe != nil {
				observe(response)
			}
		}
		response.Header.Add(reverseProxyServerPowerBy, "OpenSandbox-ingress")
		return nil
	}
	// Avoid writing an error body into an already-streaming response.
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		Logger.With(
			slogger.Field{Key: "error", Value: fmt.Sprintf("%v", err)},
			slogger.Field{Key: "uri", Value: req.RequestURI},
			slogger.Field{Key: "method", Value: req.Method},
		).Errorf("ingress: reverse proxy upstream error")
		if req.Context().Err() == nil && hp.errorObserver != nil {
			if config := fastPathDialConfigFromContext(req.Context()); config == nil || !config.alreadyNotified() {
				hp.errorObserver(err)
			}
		}

		// Attempt to set 502; this is a no-op if headers are already sent.
		rw.WriteHeader(http.StatusBadGateway)
	}
	return proxy
}
