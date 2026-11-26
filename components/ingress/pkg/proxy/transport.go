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

package proxy

import (
	"net"
	"net/http"

	"github.com/alibaba/opensandbox/ingress/pkg/connectivity"
)

func newHTTPTransport(observer connectivity.Observer, pool *httpConnectionPoolConfig) http.RoundTripper {
	if observer == nil && pool == nil {
		return nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil
	}
	transport := base.Clone()
	if pool != nil {
		transport.MaxIdleConns = pool.maxIdleConns
		transport.MaxIdleConnsPerHost = pool.maxIdleConnsPerHost
		transport.IdleConnTimeout = pool.idleConnTimeout
	}
	observeTransport(transport, observer, "http")
	return transport
}

func newObservedWebSocketHTTPClient(observer connectivity.Observer) *http.Client {
	if observer == nil {
		return nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil
	}
	transport := base.Clone()
	observeTransport(transport, observer, "websocket")
	return &http.Client{Transport: transport}
}

func observeTransport(transport *http.Transport, observer connectivity.Observer, protocol string) {
	if observer == nil {
		return
	}
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	transport.DialContext = connectivity.WrapDialContext(dial, observer, protocol)
}
