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
//
// The concrete *statusCapturingResponseWriter is embedded (not the
// http.ResponseWriter interface) so Flush and Hijack are promoted:
// ReverseProxy must keep flushing streaming responses (SSE) after every
// copied event, which the interface embedding silently broke.
type wakeReplay struct {
	*statusCapturingResponseWriter

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
	c := &wakeReplay{statusCapturingResponseWriter: sw, proxy: proxy, host: host, r: r, target: target}
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

// captureBodyForReplay buffers the request body up to the replay cap.
// Over the cap the body keeps streaming untouched and nil is returned (not
// replayable). Content-Length short-circuits skip touching the body for the
// common bodyless and obviously oversized requests.
func captureBodyForReplay(r *http.Request) []byte {
	if r.Body == nil || r.ContentLength == 0 {
		return []byte{}
	}
	if r.ContentLength > wakeReplayBodyMax {
		return nil
	}
	// An exact-size buffer when Content-Length is authoritative; one extra
	// byte only for chunked bodies, so a full read proves the body exceeded
	// the cap.
	knownLength := r.ContentLength > 0
	size := wakeReplayBodyMax + 1
	if knownLength {
		size = int(r.ContentLength)
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(r.Body, buf)
	if err != nil || knownLength {
		// Complete read: either the short-read EOF family, or a
		// Content-Length body fully consumed.
		body := buf[:n]
		if !knownLength {
			// Release the oversized scratch so a parked request retains only
			// the captured bytes.
			body = append([]byte(nil), buf[:n]...)
		}
		r.Body = bodyWithClose{Reader: bytes.NewReader(body), closeFn: r.Body.Close}
		return body
	}
	// Larger than the cap (unknown Content-Length): keep streaming the
	// original body.
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
		// ReverseProxy's copyHeader has already copied the manufactured 503's
		// headers into the underlying map; drop them so neither the replay's
		// real response nor a wake-error answer inherits the stray
		// Retry-After and duplicated power-by.
		clear(c.Header())
		c.wakeErr = c.proxy.waker.Wake(c.r.Context(), c.target)
		return
	}
	c.statusCapturingResponseWriter.WriteHeader(code)
}

func (c *wakeReplay) Write(b []byte) (int, error) {
	if c.intercepted.Load() {
		return len(b), nil
	}
	return c.statusCapturingResponseWriter.Write(b)
}

// Flush forwards to the capturing writer unless the stale 503 was
// intercepted: a flush after a swallowed WriteHeader would implicitly commit
// a 200 to the client while the request is parked.
func (c *wakeReplay) Flush() {
	if c.intercepted.Load() {
		return
	}
	c.statusCapturingResponseWriter.Flush()
}
