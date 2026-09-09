// Copyright 2026 Alibaba Group Holding Ltd.
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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectEndpointReadiness(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint("resume=", resume), func(t *testing.T) {
			var attempts atomic.Int32
			var url string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					w.WriteHeader(204)
					return
				}
				if r.URL.Path == "/ping" {
					w.WriteHeader(200)
					return
				}
				if attempts.Add(1) < 3 {
					w.WriteHeader(404)
					fmt.Fprint(w, `{"code":"KUBERNETES::POD_IP_NOT_AVAILABLE","message":"starting"}`)
					return
				}
				fmt.Fprintf(w, `{"endpoint":%q,"headers":{"X-Test":"new"}}`, url)
			}))
			defer srv.Close()
			url = srv.URL
			connect := ConnectSandbox
			if resume {
				connect = ResumeSandbox
			}
			sb, err := connect(context.Background(), ConnectionConfig{Domain: srv.URL}, "sb", ReadyOptions{Timeout: time.Second, PollingInterval: time.Millisecond})
			require.NoError(t, err)
			require.Equal(t, int32(3), attempts.Load())
			require.NoError(t, sb.Close())
		})
	}
}

func TestConnectEndpointFailures(t *testing.T) {
	for _, code := range []string{"SANDBOX_NOT_FOUND", "KUBERNETES::POD_IP_NOT_AVAILABLE"} {
		t.Run(code, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(404)
				fmt.Fprintf(w, `{"code":%q,"message":"starting"}`, code)
			}))
			defer srv.Close()
			_, err := ConnectSandbox(context.Background(), ConnectionConfig{Domain: srv.URL}, "sb", ReadyOptions{Timeout: 30 * time.Millisecond, PollingInterval: time.Millisecond})
			var apiErr *APIError
			require.True(t, errors.As(err, &apiErr))
			require.Equal(t, code, apiErr.Response.Code)
			var timeout *SandboxReadyTimeoutError
			require.Equal(t, code != "SANDBOX_NOT_FOUND", errors.As(err, &timeout))
			if code == "SANDBOX_NOT_FOUND" {
				require.Equal(t, int32(1), attempts.Load())
			}
		})
	}
}

func TestConnectEndpointCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	stopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		cancel()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	_, err := ConnectSandbox(ctx, ConnectionConfig{Domain: srv.URL}, "sb", ReadyOptions{Timeout: time.Second})
	require.True(t, errors.Is(err, context.Canceled))
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop the endpoint request")
	}
}

type readinessRoundTripper func(*http.Request) (*http.Response, error)

func (f readinessRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadinessRejectsLateCustomResults(t *testing.T) {
	for _, phase := range []string{"health", "transport"} {
		t.Run(phase, func(t *testing.T) {
			calls := 0
			finished := false
			block := func(ctx context.Context) {
				calls++
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
					t.Fatal("readiness context was not cancelled")
				}
				finished = true
			}
			client := &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
				if phase == "transport" {
					block(r.Context())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"endpoint":"localhost:44772","headers":{}}`)),
				}, nil
			})}
			_, err := ConnectSandbox(context.Background(), ConnectionConfig{Domain: "localhost:8080", HTTPClient: client}, "sb", ReadyOptions{
				Timeout: 50 * time.Millisecond,
				HealthCheck: func(ctx context.Context, _ *Sandbox) (bool, error) {
					block(ctx)
					return true, nil
				},
			})
			var timeout *SandboxReadyTimeoutError
			require.ErrorAs(t, err, &timeout)
			require.True(t, finished)
			require.Equal(t, 1, calls)
		})
	}
}

func TestConnectEndpointSharesHealthDeadline(t *testing.T) {
	var endpointDeadline time.Time
	client := &http.Client{Transport: readinessRoundTripper(func(r *http.Request) (*http.Response, error) {
		endpointDeadline, _ = r.Context().Deadline()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"endpoint":"localhost:44772","headers":{}}`)),
		}, nil
	})}
	checked := false
	sb, err := ConnectSandbox(context.Background(), ConnectionConfig{Domain: "localhost:8080", HTTPClient: client}, "sb", ReadyOptions{
		Timeout: time.Second,
		HealthCheck: func(ctx context.Context, _ *Sandbox) (bool, error) {
			checked = true
			healthDeadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.True(t, !endpointDeadline.IsZero())
			require.True(t, healthDeadline.Equal(endpointDeadline))
			return true, nil
		},
	})
	require.NoError(t, err)
	require.True(t, checked)
	require.NoError(t, sb.Close())
}
