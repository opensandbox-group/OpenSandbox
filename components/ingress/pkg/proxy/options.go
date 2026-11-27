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
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/activity"
	"github.com/alibaba/opensandbox/ingress/pkg/connectivity"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
)

type Option func(*proxyOptions)

type proxyOptions struct {
	connectObserver           connectivity.Observer
	httpConnectionPool        *httpConnectionPoolConfig
	webSocketMessageSizeLimit int64
	activity                  activity.Recorder
	waker                     *wake.Waker
	fastSandboxNamespace      string
}

type httpConnectionPoolConfig struct {
	maxIdleConns        int
	maxIdleConnsPerHost int
	idleConnTimeout     time.Duration
}

// Zero values follow net/http.Transport semantics; values must be non-negative.
func WithHTTPConnectionPool(maxIdleConns, maxIdleConnsPerHost int, idleConnTimeout time.Duration) Option {
	return func(options *proxyOptions) {
		options.httpConnectionPool = &httpConnectionPoolConfig{
			maxIdleConns:        maxIdleConns,
			maxIdleConnsPerHost: maxIdleConnsPerHost,
			idleConnTimeout:     idleConnTimeout,
		}
	}
}

func WithFastSandboxNamespace(namespace string) Option {
	return func(options *proxyOptions) {
		options.fastSandboxNamespace = namespace
	}
}

// WithConnectObserver observes HTTP and WebSocket TCP connection attempts.
func WithConnectObserver(observer connectivity.Observer) Option {
	return func(options *proxyOptions) {
		options.connectObserver = observer
	}
}

// Non-positive values cannot disable the message size limit.
func WithWebSocketMessageSizeLimit(limit int64) Option {
	return func(options *proxyOptions) {
		if limit > 0 {
			options.webSocketMessageSizeLimit = limit
		}
	}
}
