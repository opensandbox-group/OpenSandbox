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
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const inflightWakeBudget = 40 * time.Millisecond

// newInflightWakeServer answers the first request with a marked wake timeout
// and hands every later request to next.
func newInflightWakeServer(t *testing.T, next http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			writeStreamWakeTimeout(w)
			return
		}
		next(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// holdHeaders never answers until the client gives up or the test ends.
func holdHeaders(release <-chan struct{}) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

// stallMidHeaders sends the start of a response header block over a hijacked
// connection and then stalls until the client gives up or the test ends.
func stallMidHeaders(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n")
		_ = buf.Flush()
		readDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, conn)
			close(readDone)
		}()
		select {
		case <-readDone:
		case <-release:
		}
	}
}

func inflightWakeConfig(deadline time.Duration) RetryConfig {
	return RetryConfig{WakeMaxAttempts: 3, WakeOverallDeadline: deadline}
}

func TestWakeOverallDeadline_BoundsInflightRetry(t *testing.T) {
	cases := map[string]func(ctx context.Context, url string) error{
		"lifecycle": func(ctx context.Context, url string) error {
			client := NewLifecycleClient(url, "", WithRetry(inflightWakeConfig(inflightWakeBudget)))
			_, err := client.GetSandbox(ctx, "sandbox-1")
			return err
		},
		"stream-bootstrap": func(ctx context.Context, url string) error {
			client := NewExecdClient(url, "", WithRetry(inflightWakeConfig(inflightWakeBudget)))
			return client.RunCommand(ctx, RunCommandRequest{Command: "true"}, func(StreamEvent) error { return nil })
		},
	}
	stalls := map[string]func(<-chan struct{}) http.HandlerFunc{
		"no-headers":      holdHeaders,
		"partial-headers": stallMidHeaders,
	}
	for name, call := range cases {
		for stallName, stall := range stalls {
			t.Run(name+"/"+stallName, func(t *testing.T) {
				release := make(chan struct{})
				srv, calls := newInflightWakeServer(t, stall(release))
				t.Cleanup(func() { close(release) })

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				started := time.Now()
				err := call(ctx, srv.URL)
				elapsed := time.Since(started)

				if got := calls.Load(); got != 2 {
					t.Fatalf("calls = %d, want one marked response and one blocked retry", got)
				}
				if elapsed > time.Second {
					t.Fatalf("40ms wake budget did not bound the in-flight retry: elapsed %v", elapsed)
				}
				var budgetErr *wakeBudgetExceededError
				if !errors.As(err, &budgetErr) {
					t.Fatalf("err = %v, want wake budget exceeded", err)
				}
				if budgetErr.Attempts != 1 {
					t.Fatalf("Attempts = %d, want 1", budgetErr.Attempts)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want it to match context.DeadlineExceeded", err)
				}
				// The retry may have reached the sandbox: it must not be reported
				// as a never-forwarded wake timeout.
				var wakeErr *SandboxWakeTimeoutError
				if errors.As(err, &wakeErr) {
					t.Fatalf("err = %v, must not be a SandboxWakeTimeoutError", err)
				}
			})
		}
	}
}

func TestWakeOverallDeadline_EarlierCallerDeadlineWins(t *testing.T) {
	release := make(chan struct{})
	srv, _ := newInflightWakeServer(t, holdHeaders(release))
	t.Cleanup(func() { close(release) })

	client := NewLifecycleClient(srv.URL, "", WithRetry(inflightWakeConfig(time.Minute)))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.GetSandbox(ctx, "sandbox-1")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller deadline", err)
	}
	var budgetErr *wakeBudgetExceededError
	if errors.As(err, &budgetErr) {
		t.Fatalf("err = %v, the caller deadline must take precedence", err)
	}
}

func TestWakeOverallDeadline_DoesNotBoundRequestWithoutMarkedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * inflightWakeBudget)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sandbox-1"}`))
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "", WithRetry(inflightWakeConfig(inflightWakeBudget)))
	if _, err := client.GetSandbox(context.Background(), "sandbox-1"); err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
}

func TestWakeOverallDeadline_EstablishedStreamOutlivesBudget(t *testing.T) {
	srv, calls := newInflightWakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(4 * inflightWakeBudget)
		_, _ = w.Write([]byte(`{"type":"stdout","text":"done"}` + "\n\n"))
	})

	client := NewExecdClient(srv.URL, "", WithRetry(inflightWakeConfig(inflightWakeBudget)))
	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "true"}, func(ev StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want the event sent after the wake budget", len(events))
	}
}

func TestWakeOverallDeadline_DownloadBodyOutlivesBudget(t *testing.T) {
	srv, _ := newInflightWakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("head-"))
		w.(http.Flusher).Flush()
		time.Sleep(4 * inflightWakeBudget)
		_, _ = w.Write([]byte("tail"))
	})

	client := NewExecdClient(srv.URL, "", WithRetry(inflightWakeConfig(inflightWakeBudget)))
	body, err := client.DownloadFile(context.Background(), "/tmp/f", "")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "head-tail" {
		t.Fatalf("body = %q, want %q", data, "head-tail")
	}
}

// After a bounded wake retry received its headers, a mid-body failure on the
// reused pooled connection triggers the fresh-connection replay on the same
// attempt context. That replay's response is valid and must be honored.
func TestWakeOverallDeadline_FreshConnReplayAfterHeadersSucceeds(t *testing.T) {
	var truncated atomic.Bool
	srv, calls := newInflightWakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if truncated.CompareAndSwap(false, true) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
			_ = buf.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sandbox-1"}`))
	})

	client := NewLifecycleClient(srv.URL, "", WithRetry(inflightWakeConfig(time.Minute)))
	sb, err := client.GetSandbox(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if sb.ID != "sandbox-1" {
		t.Fatalf("ID = %q, want sandbox-1", sb.ID)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want marked response, truncated body and fresh-connection replay", got)
	}
}

// A generic backoff during the wake phase can spend the rest of the budget;
// the next attempt must then not be sent at all.
func TestWakeOverallDeadline_SpentByBackoffSendsNoFurtherAttempt(t *testing.T) {
	srv, calls := newInflightWakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	cfg := inflightWakeConfig(inflightWakeBudget)
	cfg.MaxRetries = 1
	cfg.InitialBackoff = 4 * inflightWakeBudget
	client := NewLifecycleClient(srv.URL, "", WithRetry(cfg))
	_, err := client.GetSandbox(context.Background(), "sandbox-1")

	var wakeErr *SandboxWakeTimeoutError
	if !errors.As(err, &wakeErr) {
		t.Fatalf("err = %v, want SandboxWakeTimeoutError", err)
	}
	if wakeErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", wakeErr.Attempts)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.WakeExpired {
		t.Fatalf("err = %v, want it to wrap the marked wake response", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want no attempt after the budget was spent", got)
	}
}
