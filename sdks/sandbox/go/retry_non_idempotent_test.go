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
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetry_NonIdempotentStatusCodesNotRetriedByDefault(t *testing.T) {
	t.Run("POST CreateSession", func(t *testing.T) {
		srv, attempts := newRetryFailureServer(http.StatusServiceUnavailable)
		defer srv.Close()

		client := NewExecdClient(srv.URL, "token", WithRetry(RetryConfig{
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
		}))

		_, err := client.CreateSession(context.Background())
		requireStatusError(t, err, http.StatusServiceUnavailable)
		require.Equal(t, int32(1), attempts.Load())
	})

	t.Run("PATCH PatchSandboxMetadata", func(t *testing.T) {
		srv, attempts := newRetryFailureServer(http.StatusServiceUnavailable)
		defer srv.Close()

		client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
		}))

		_, err := client.PatchSandboxMetadata(context.Background(), "sbx-1", MetadataPatch{})
		requireStatusError(t, err, http.StatusServiceUnavailable)
		require.Equal(t, int32(1), attempts.Load())
	})
}

func TestRetry_NonIdempotentStatusOptIn(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","message":"try again"}`))
			return
		}
		jsonResponse(w, http.StatusOK, Session{ID: "session-1"})
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "token", WithRetry(RetryConfig{
		MaxRetries:                        1,
		InitialBackoff:                    time.Millisecond,
		RetryableStatusCodesNonIdempotent: []int{http.StatusServiceUnavailable},
	}))

	session, err := client.CreateSession(context.Background())
	require.NoError(t, err)
	require.Equal(t, "session-1", session.ID)
	require.Equal(t, int32(2), attempts.Load())
}

func TestRetry_NonIdempotentTransportFailures(t *testing.T) {
	tests := []struct {
		name         string
		op           string
		wantAttempts int32
		wantSuccess  bool
	}{
		{"pre-send dial retried", "dial", 2, true},
		{"post-send read not retried", "read", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			transport := retryRoundTripFunc(func(*http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 || !tt.wantSuccess {
					return nil, &net.OpError{Op: tt.op, Net: "tcp", Err: fmt.Errorf("transport failure")}
				}
				return retryJSONResponse(`{"session_id":"session-1"}`), nil
			})
			client := NewExecdClient("http://example.invalid", "token",
				WithHTTPClient(&http.Client{Transport: transport}),
				WithRetry(RetryConfig{MaxRetries: 1, InitialBackoff: time.Millisecond}),
			)

			session, err := client.CreateSession(context.Background())
			if tt.wantSuccess {
				require.NoError(t, err)
				require.Equal(t, "session-1", session.ID)
			} else {
				require.Error(t, err)
				var netErr net.Error
				require.ErrorAs(t, err, &netErr)
			}
			require.Equal(t, tt.wantAttempts, attempts.Load())
		})
	}
}

func TestRetry_NonIdempotentRedirectDialNotRetried(t *testing.T) {
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	target := "http://" + targetListener.Addr().String() + "/next"
	require.NoError(t, targetListener.Close())

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "token", WithRetry(RetryConfig{
		MaxRetries:     1,
		InitialBackoff: time.Millisecond,
	}))

	_, err = client.CreateSession(context.Background())
	require.Error(t, err)
	require.Equal(t, int32(1), attempts.Load(), "a redirected POST must not replay its first hop")
}

func TestRetry_NonIdempotentTLSHandshakeFailureRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	var accepts atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = conn.Close()
		}
	}()

	client := NewClient("https://"+ln.Addr().String(), "token", "OPEN-SANDBOX-API-KEY",
		WithHTTPClient(&http.Client{Transport: &http.Transport{TLSHandshakeTimeout: time.Second}}),
		WithRetry(RetryConfig{MaxRetries: 1, InitialBackoff: time.Millisecond}),
	)
	err = client.doRequest(context.Background(), http.MethodPost, "/", map[string]string{"a": "b"}, nil)
	require.Error(t, err)
	require.Equal(t, int32(2), accepts.Load(), "a failed TLS handshake is a pre-send failure")
}

func TestRetry_NonIdempotentProxyConnectFailureRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	var accepts atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	client := NewClient("https://sandbox.invalid", "token", "OPEN-SANDBOX-API-KEY",
		WithHTTPClient(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}),
		WithRetry(RetryConfig{MaxRetries: 1, InitialBackoff: time.Millisecond}),
	)
	err = client.doRequest(context.Background(), http.MethodPost, "/", map[string]string{"a": "b"}, nil)
	require.Error(t, err)
	require.Equal(t, int32(2), accepts.Load(), "a failed proxy CONNECT is a pre-send failure")
}

type retryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f retryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func retryJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newRetryFailureServer(status int) (*httptest.Server, *atomic.Int32) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","message":"try again"}`))
	}))
	return srv, &attempts
}

func requireStatusError(t *testing.T, err error, status int) {
	t.Helper()
	require.Error(t, err)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, status, apiErr.StatusCode)
}
