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

package mcpproxy

import (
	"net/http"
	"sync"
	"time"
)

// sseKeepaliveComment is an SSE comment frame: ignored by every conforming
// event-source parser, yet resets clients' read-idle timers (the python SDK
// drops an SSE stream after sse_read_timeout seconds of silence).
const sseKeepaliveComment = ": keepalive\n\n"

// keepaliveWriter wraps an http.ResponseWriter and injects an SSE comment
// line when a text/event-stream response goes idle for the configured
// interval. Non-SSE responses pass through untouched. The zero interval
// disables injection entirely.
type keepaliveWriter struct {
	http.ResponseWriter

	interval time.Duration

	mu           sync.Mutex
	lastWrite    time.Time
	started      bool
	contentSSE   bool
	writeErr     error
	stopped      bool
	stop         chan struct{}
	stopOnce     sync.Once
	idleInjected int
}

func newKeepaliveWriter(w http.ResponseWriter, interval time.Duration) *keepaliveWriter {
	return &keepaliveWriter{
		ResponseWriter: w,
		interval:       interval,
		stop:           make(chan struct{}),
		lastWrite:      time.Now(),
	}
}

// WriteHeader delegates and, once the response is known to be SSE, starts
// the idle watch loop.
func (k *keepaliveWriter) WriteHeader(status int) {
	k.ResponseWriter.WriteHeader(status)
	if k.interval <= 0 {
		return
	}
	contentType := k.ResponseWriter.Header().Get("Content-Type")
	if mediaType(contentType) == "text/event-stream" {
		k.mu.Lock()
		k.contentSSE = true
		started := k.started
		k.started = true
		k.mu.Unlock()
		if !started {
			go k.loop()
		}
	}
}

// Write delegates, records freshness, and stops injecting after a hard error.
func (k *keepaliveWriter) Write(p []byte) (int, error) {
	n, err := k.ResponseWriter.Write(p)
	k.mu.Lock()
	k.lastWrite = time.Now()
	if err != nil {
		k.writeErr = err
	}
	k.mu.Unlock()
	return n, err
}

// Flush delegates so the reverse proxy's flushes reach the client.
func (k *keepaliveWriter) Flush() {
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// loop injects a comment frame whenever the SSE response has been silent for
// a full interval.
func (k *keepaliveWriter) loop() {
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			k.mu.Lock()
			idle := time.Since(k.lastWrite)
			stopped := k.stopped
			failed := k.writeErr != nil
			sse := k.contentSSE
			k.mu.Unlock()
			if stopped || failed || !sse || idle < k.interval {
				continue
			}
			if _, err := k.ResponseWriter.Write([]byte(sseKeepaliveComment)); err != nil {
				return
			}
			if f, ok := k.ResponseWriter.(http.Flusher); ok {
				f.Flush()
			}
			k.mu.Lock()
			k.idleInjected++
			k.lastWrite = time.Now()
			k.mu.Unlock()
		}
	}
}

// Stop ends the idle watch loop. Safe to call multiple times.
func (k *keepaliveWriter) Stop() {
	k.stopOnce.Do(func() { close(k.stop) })
	k.mu.Lock()
	k.stopped = true
	k.mu.Unlock()
}

// IdleInjections reports how many keepalive frames were injected (tests).
func (k *keepaliveWriter) IdleInjections() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.idleInjected
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
