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
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/connectivity"
)

const fastPathDialTimeout = 5 * time.Second

type fastPathDialConfigKey struct{}

type fastPathDialConfig struct {
	timeout  time.Duration
	onError  func(error)
	notified atomic.Bool
}

func withFastPathDialConfig(ctx context.Context, config *fastPathDialConfig) context.Context {
	return context.WithValue(ctx, fastPathDialConfigKey{}, config)
}

func fastPathDialConfigFromContext(ctx context.Context) *fastPathDialConfig {
	config, _ := ctx.Value(fastPathDialConfigKey{}).(*fastPathDialConfig)
	return config
}

func (config *fastPathDialConfig) notify(err error) {
	if config == nil || config.onError == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if config.notified.CompareAndSwap(false, true) {
		config.onError(err)
	}
}

func (config *fastPathDialConfig) alreadyNotified() bool {
	return config != nil && config.notified.Load()
}

func wrapFastPathDialContext(next func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		config := fastPathDialConfigFromContext(ctx)
		if config == nil || config.timeout <= 0 {
			return next(ctx, network, address)
		}
		dialCtx, cancel := context.WithTimeout(ctx, config.timeout)
		defer cancel()
		conn, err := next(dialCtx, network, address)
		config.notify(err)
		return conn, err
	}
}

func enableFastPathDialTimeout(transport *http.Transport) {
	if transport == nil {
		return
	}
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	transport.DialContext = wrapFastPathDialContext(dial)
}

func cloneFastPathTransport(base http.RoundTripper) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return base
	}
	clone := transport.Clone()
	enableFastPathDialTimeout(clone)
	return clone
}

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
