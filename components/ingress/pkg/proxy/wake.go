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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/alibaba/opensandbox/ingress/pkg/activity"
	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
)

// wakeReplayBodyMax bounds request-body buffering for stale-route replay
// (OSEP-0024). Bodies up to this size are fully buffered so the request can
// be replayed once after a wake; larger bodies keep streaming and replay
// today's stale-route 503 instead.
const wakeReplayBodyMax = 1 << 20 // 1 MiB

// WithActivityRecorder installs the OSEP-0024 activity writer. Every routed
// request feeds the sandbox idle clock; writes are asynchronous and
// fire-and-forget, never adding latency to the proxy path.
func WithActivityRecorder(recorder activity.Recorder) Option {
	return func(options *proxyOptions) {
		options.activity = recorder
	}
}

// WithWaker installs the OSEP-0024 wake-on-access orchestrator. When set,
// resolution failures on fast-sandbox routes park on a paused sandbox until
// it resumes, and stale-route hits are retried once after a wake.
func WithWaker(waker *wake.Waker) Option {
	return func(options *proxyOptions) {
		options.waker = waker
	}
}

// httpStatusForWakeErr maps wake errors onto HTTP semantics. The 503 answers
// carry Retry-After: 1 via the shared error path in ServeHTTP.
func httpStatusForWakeErr(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, sandbox.ErrSandboxNotFound):
		return http.StatusNotFound
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 0 // client is gone; nothing to answer
	default:
		// Budget exhausted, parking lot full, transient FastPath failures.
		return http.StatusServiceUnavailable
	}
}

// wakeReplay implements OSEP-0024 stale-route re-entry for one request. It
// sits between httputil.ReverseProxy and the caller's ResponseWriter: when
// the response observer marks the response as a manufactured stale-route
// 503, the writer swallows it, parks the request on the waker, and lets the
// caller replay the request once on a fresh route. Nothing has been written
// to the client at interception time, so replay is legal.
type wakeReplay struct {
	http.ResponseWriter

	proxy  *Proxy
	host   *sandboxHost
	r      *http.Request
	target sandbox.EndpointTarget

	// copied is the fully buffered request body, or nil when the body
	// exceeded the replay cap and the request is not replayable.
	copied []byte

	stale       atomic.Bool // set by the response observer
	intercepted atomic.Bool // the manufactured 503 was swallowed for parking
	wakeErr     error
}

func newWakeReplay(proxy *Proxy, sw *statusCapturingResponseWriter, host *sandboxHost, r *http.Request, target sandbox.EndpointTarget) *wakeReplay {
	c := &wakeReplay{ResponseWriter: sw, proxy: proxy, host: host, r: r, target: target}
	c.copied = captureBodyForReplay(r)
	return c
}

// replayable reports whether the request can be replayed after a wake: the
// body was fully buffered within the cap. Idempotent methods with bodies too
// large to buffer are intentionally not replayed — a truncated replay would
// corrupt the upstream call.
func (c *wakeReplay) replayable() bool {
	return c.copied != nil
}

// captureBodyForReplay buffers the request body up to the replay cap. Over
// the cap the body keeps streaming through a passthrough reader and nil is
// returned (not replayable).
func captureBodyForReplay(r *http.Request) []byte {
	if r.Body == nil {
		return []byte{}
	}
	buf := make([]byte, wakeReplayBodyMax+1)
	n, err := io.ReadFull(r.Body, buf)
	if err != nil {
		// io.EOF or io.ErrUnexpectedEOF: the whole body fit in the buffer.
		body := buf[:n]
		r.Body = bodyWithClose{Reader: bytes.NewReader(body), closeFn: r.Body.Close}
		return body
	}
	// Larger than the cap: keep streaming the original body.
	r.Body = bodyWithClose{
		Reader:  io.MultiReader(bytes.NewReader(buf), r.Body),
		closeFn: r.Body.Close,
	}
	return nil
}

type bodyWithClose struct {
	io.Reader
	closeFn func() error
}

func (b bodyWithClose) Close() error {
	if b.closeFn != nil {
		return b.closeFn()
	}
	return nil
}

// observeStale marks the response as a manufactured stale-route rewrite. It
// wraps the regular upstream response observer.
func (c *wakeReplay) observeStale() func(*http.Response) {
	return func(response *http.Response) {
		if response.StatusCode >= http.StatusBadRequest && sandbox.IsStaleFastPathResponse(response.Header) {
			c.stale.Store(true)
		}
	}
}

func (c *wakeReplay) WriteHeader(code int) {
	if code == http.StatusServiceUnavailable &&
		c.replayable() && c.stale.Load() && c.intercepted.CompareAndSwap(false, true) {
		// Parking inside WriteHeader blocks the handler goroutine before any
		// byte reached the client: the park holds no upstream connection.
		c.wakeErr = c.proxy.waker.Wake(c.r.Context(), c.target)
		return
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *wakeReplay) Write(b []byte) (int, error) {
	if c.intercepted.Load() {
		return len(b), nil
	}
	return c.ResponseWriter.Write(b)
}
