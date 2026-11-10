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

package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCodeInterpreter_RuntimeDeadlineBoundsProbe(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_started_%t", flushHeaders), func(t *testing.T) {
			cancelled := make(chan struct{})
			stop := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if flushHeaders {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"init\",\"text\":\"probe\"}\n\n")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
					close(cancelled)
				case <-stop:
				}
			}))
			defer srv.Close()
			defer close(stop)
			ci := newTestCodeInterpreter(t, srv.URL)
			// A longer parent deadline is only a safety net for the unfixed code.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err := ci.waitRuntimeReady(ctx, 40*time.Millisecond, time.Millisecond)
			var readyErr *SandboxReadyTimeoutError
			if !errors.As(err, &readyErr) {
				t.Fatalf("got %T %v, want readiness timeout", err, err)
			}
			if ctx.Err() != nil {
				t.Fatalf("probe ignored readiness deadline and exhausted parent: %v", ctx.Err())
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("probe request was not cancelled")
			}
		})
	}
}

func TestCodeInterpreter_RuntimeDeadlinePreservesParentCancellation(t *testing.T) {
	started := make(chan struct{})
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer srv.Close()
	defer close(stop)
	ci := newTestCodeInterpreter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-started; cancel() }()
	err := ci.waitRuntimeReady(ctx, time.Second, time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want parent cancellation", err)
	}
}
