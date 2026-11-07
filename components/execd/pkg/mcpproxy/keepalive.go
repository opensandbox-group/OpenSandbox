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

package mcpproxy

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseKeepaliveComment is an SSE comment frame: ignored by every conforming
// event-source parser, yet resets clients' read-idle timers (the python SDK
// drops an SSE stream after sse_read_timeout seconds of silence).
const sseKeepaliveComment = ": keepalive\n\n"

// keepaliveWriter injects an SSE comment frame when a text/event-stream
// response goes idle for the configured interval. Injections happen only at
// event boundaries and are serialized with proxied writes. Non-SSE and
// content-encoded responses pass through untouched; zero disables.
type keepaliveWriter struct {
	http.ResponseWriter

	interval time.Duration

	// wmu serializes writes to the underlying ResponseWriter between the
	// reverse proxy and the keepalive loop.
	wmu sync.Mutex

	mu           sync.Mutex
	lastWrite    time.Time
	started      bool
	contentSSE   bool
	writeErr     error
	stopped      bool
	boundary     sseBoundaryTracker
	stop         chan struct{}
	stopOnce     sync.Once
	done         chan struct{}
	idleInjected int
}

func newKeepaliveWriter(w http.ResponseWriter, interval time.Duration) *keepaliveWriter {
	return &keepaliveWriter{
		ResponseWriter: w,
		interval:       interval,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		lastWrite:      time.Now(),
		boundary:       newSSEBoundaryTracker(),
	}
}

// WriteHeader delegates and, once the response is known to be SSE, starts
// the idle watch loop.
func (k *keepaliveWriter) WriteHeader(status int) {
	k.wmu.Lock()
	k.ResponseWriter.WriteHeader(status)
	k.wmu.Unlock()
	if k.interval <= 0 {
		return
	}
	contentType := k.ResponseWriter.Header().Get("Content-Type")
	if mediaType(contentType) != "text/event-stream" {
		return
	}
	// Appending plaintext to an encoded stream would corrupt it.
	if enc := k.ResponseWriter.Header().Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return
	}
	k.mu.Lock()
	k.contentSSE = true
	started := k.started
	k.started = true
	k.mu.Unlock()
	if !started {
		go k.loop()
	}
}

// Write delegates, records freshness, tracks the SSE event boundary, and
// stops injecting after a hard error.
func (k *keepaliveWriter) Write(p []byte) (int, error) {
	k.wmu.Lock()
	n, err := k.ResponseWriter.Write(p)
	k.wmu.Unlock()
	k.mu.Lock()
	k.boundary.observe(p)
	k.lastWrite = time.Now()
	if err != nil {
		k.writeErr = err
	}
	k.mu.Unlock()
	return n, err
}

// Flush delegates so the reverse proxy's flushes reach the client.
func (k *keepaliveWriter) Flush() {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// loop injects a comment when the response has been idle a full interval
// and sits at an event boundary. The write lock is held from the boundary
// check through the injection so a proxied write cannot land in between.
func (k *keepaliveWriter) loop() {
	defer close(k.done)
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			k.wmu.Lock()
			k.mu.Lock()
			idle := time.Since(k.lastWrite)
			stopped := k.stopped
			failed := k.writeErr != nil
			sse := k.contentSSE
			atBoundary := k.boundary.atBoundary()
			k.mu.Unlock()
			if stopped {
				k.wmu.Unlock()
				return
			}
			if failed || !sse || !atBoundary || idle < k.interval {
				k.wmu.Unlock()
				continue
			}
			_, err := k.ResponseWriter.Write([]byte(sseKeepaliveComment))
			if err == nil {
				if f, ok := k.ResponseWriter.(http.Flusher); ok {
					f.Flush()
				}
			}
			k.wmu.Unlock()
			if err != nil {
				return
			}
			k.mu.Lock()
			k.idleInjected++
			k.lastWrite = time.Now()
			k.mu.Unlock()
		}
	}
}

// Stop ends the loop and joins it, so no injection touches the
// ResponseWriter after ServeHTTP returns. Safe to call multiple times.
func (k *keepaliveWriter) Stop() {
	k.stopOnce.Do(func() {
		k.mu.Lock()
		started := k.started
		k.stopped = true
		k.mu.Unlock()
		close(k.stop)
		if started {
			<-k.done
		}
	})
}

// IdleInjections reports how many keepalive frames were injected (tests).
func (k *keepaliveWriter) IdleInjections() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.idleInjected
}

// sseBoundaryTracker reports whether the stream sits at an event boundary:
// at the start, or right after the blank line that terminates an event.
// Comments injected there can never split a partial event.
type sseBoundaryTracker struct {
	lineLen  int  // bytes seen in the current line, excluding terminators
	carryCR  bool // last byte was CR: swallow a following LF as the same terminator
	boundary bool
}

func newSSEBoundaryTracker() sseBoundaryTracker {
	return sseBoundaryTracker{boundary: true}
}

// observe folds one write chunk into the tracker state.
func (t *sseBoundaryTracker) observe(p []byte) {
	for _, b := range p {
		switch {
		case t.carryCR && b == '\n':
			t.carryCR = false // second byte of CRLF; the line already ended
		case b == '\n':
			t.carryCR = false
			t.endLine()
		case b == '\r':
			t.carryCR = true
			t.endLine()
		default:
			t.carryCR = false
			t.lineLen++
			t.boundary = false
		}
	}
}

// endLine closes the current line: an empty line completes an event.
func (t *sseBoundaryTracker) endLine() {
	if t.lineLen == 0 {
		t.boundary = true
	} else {
		t.lineLen = 0
		t.boundary = false
	}
}

// atBoundary reports whether the stream position is safe for injection.
// Callers must hold the enclosing keepaliveWriter's mu.
func (t *sseBoundaryTracker) atBoundary() bool {
	return t.boundary
}

// mediaType returns the MIME type without parameters.
func mediaType(contentType string) string {
	for i := range len(contentType) {
		if contentType[i] == ';' || contentType[i] == ' ' {
			return contentType[:i]
		}
	}
	return contentType
}
