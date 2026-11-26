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
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
)

// Match net/http's unread-body threshold for connection reuse.
const maxUnreadRequestBody = 256 << 10

type requestBody struct {
	io.ReadCloser
	size  int64
	read  atomic.Int64
	ended atomic.Bool
}

func (b *requestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	if err == io.EOF {
		b.ended.Store(true)
	}
	return n, err
}

// Unknown-length bodies cannot be drained ahead of the upstream transport.
func (b *requestBody) closesConnection() bool {
	if b.size < 0 {
		return !b.ended.Load()
	}
	return b.size-b.read.Load() >= maxUnreadRequestBody
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	outcome     string
	written     bool
	requestBody *requestBody
}

func (w *statusCapturingResponseWriter) setOutcome(outcome string) {
	w.outcome = outcome
}

func (w *statusCapturingResponseWriter) commit(code int) {
	w.statusCode = code
	w.written = true
	if code != http.StatusSwitchingProtocols && w.requestBody != nil && w.requestBody.closesConnection() {
		w.Header().Set("Connection", "close")
	}
}

func (w *statusCapturingResponseWriter) WriteHeader(code int) {
	// Informational responses other than 101 do not commit the final status.
	informational := code >= 100 && code < 200 && code != http.StatusSwitchingProtocols
	if !w.written && !informational {
		w.commit(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.commit(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusCapturingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusCapturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		conn, buf, err := hj.Hijack()
		if err == nil && !w.written {
			w.statusCode = http.StatusSwitchingProtocols
			w.written = true
		}
		return conn, buf, err
	}
	return nil, nil, errors.New("upstream ResponseWriter does not implement http.Hijacker")
}

func (w *statusCapturingResponseWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.written {
			w.commit(http.StatusOK)
		}
		fl.Flush()
	}
}
