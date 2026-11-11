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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func writeStreamWakeTimeout(w http.ResponseWriter) {
	w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"code":"sandbox_wake_timeout","message":"restore in progress"}`))
}

// A bare 503 carries no proof that the request was not forwarded, so the stream
// bootstrap must stay unwrapped by any retry (OSEP-0017). This pins the boundary
// the OSEP-0024 wake retry must not cross.
func TestStreamWakeRetry_Bare503OnCommandStreamIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","message":"try again"}`))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(RetryConfig{
		MaxRetries:          2,
		WakeMaxAttempts:     5,
		WakeOverallDeadline: time.Minute,
	}))

	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(StreamEvent) error {
		return nil
	})
	require.Error(t, err, "a bare 503 on a POST stream must not be replayed")
	require.Equal(t, 1, int(attempts.Load()), "stream bootstrap attempts")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode, "status code")
}

// Exhausting the wake budget on a stream must surface the same typed error as
// the non-streaming paths so callers can tell "never forwarded" from a command
// failure.
func TestStreamWakeRetry_BudgetExhaustionReturnsTypedError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		writeStreamWakeTimeout(w)
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(wakeConfig(3, time.Minute)))

	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(StreamEvent) error {
		return nil
	})
	require.Error(t, err, "exhausted wake retry on a stream")
	var wakeErr *SandboxWakeTimeoutError
	require.ErrorAs(t, err, &wakeErr)
	require.Equal(t, 3, wakeErr.Attempts, "wake attempts")
	require.Equal(t, 3, int(attempts.Load()), "stream bootstrap attempts")
}

// A failure after the stream was established must never re-dispatch it: only the
// connect attempt is wrapped. The handler aborts after the first event, which is
// the clearest point at which a replay would execute the command twice.
func TestStreamWakeRetry_NotRetriedAfterFirstEvent(t *testing.T) {
	var attempts atomic.Int32
	stop := errors.New("consumer stopped")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"stdout\",\"text\":\"first\",\"timestamp\":1}\n\n" +
			"{\"type\":\"stdout\",\"text\":\"second\",\"timestamp\":2}\n\n"))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(wakeConfig(5, time.Minute)))

	seen := 0
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(StreamEvent) error {
		seen++
		return stop
	})
	require.ErrorIs(t, err, stop, "handler error must surface unchanged")
	require.Equal(t, 1, seen, "delivered events")
	require.Equal(t, 1, int(attempts.Load()), "an established stream is never re-dispatched")
}

// The wake retry stays opt-out: with it disabled a marked 503 reaches the caller
// immediately, so callers that relied on the previous fail-fast behavior keep it.
func TestStreamWakeRetry_DisabledReturnsMarked503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		writeStreamWakeTimeout(w)
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(RetryConfig{
		MaxRetries:          0,
		WakeRetryEnabled:    WakeRetryDisabled(),
		WakeMaxAttempts:     5,
		WakeOverallDeadline: time.Minute,
	}))

	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(StreamEvent) error {
		return nil
	})
	require.Error(t, err, "opted-out wake retry on a stream")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.True(t, apiErr.WakeExpired, "the marked error must reach the caller")
	require.Equal(t, 1, int(attempts.Load()), "stream bootstrap attempts")
}

// A marker received after a redirect chain is not proof about the original
// request: an earlier hop already received it and may have processed it. The
// bootstrap must not be replayed, so the original request is sent exactly once.
func TestStreamWakeRetry_NotRetriedAfterRedirect(t *testing.T) {
	var bootstraps, wakeAnswers atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		bootstraps.Add(1)
		http.Redirect(w, r, "/wake", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		wakeAnswers.Add(1)
		writeStreamWakeTimeout(w)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(wakeConfig(5, time.Minute)))

	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(StreamEvent) error {
		return nil
	})
	require.Error(t, err, "a marked 503 behind a redirect must not be replayed")
	require.Equal(t, 1, int(bootstraps.Load()), "original requests")
	require.Equal(t, 1, int(wakeAnswers.Load()), "redirected requests")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode, "status code")
	require.True(t, !apiErr.WakeExpired, "a redirected answer must not claim wake provenance")
}
