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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wakeConfig returns a RetryConfig with fast waits suitable for tests.
func wakeConfig(maxAttempts int, deadline time.Duration) RetryConfig {
	return RetryConfig{
		MaxRetries:          0,
		WakeRetryEnabled:    nil, // enabled by default
		WakeMaxAttempts:     maxAttempts,
		WakeOverallDeadline: deadline,
	}
}

func TestWakeRetry_Marked503OnPostIsRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		if attempts.Add(1) == 1 {
			w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"code":"sandbox_wake_timeout","message":"restore in progress"}}`))
			return
		}
		jsonResponse(w, http.StatusCreated, SandboxInfo{ID: "sbx-1", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(wakeConfig(5, time.Minute)))
	// The marker proves the request was never forwarded, so even a POST is
	// replayed (request bodies are re-marshaled per attempt).
	got, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.NoError(t, err)
	require.Equal(t, "sbx-1", got.ID)
	require.Equal(t, 2, int(attempts.Load()))
}

func TestWakeRetry_PostExhaustionReturnsTypedError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
		w.Header().Set("Retry-After", "0")
		w.Header().Set("X-Request-Id", "req-42")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"sandbox_wake_timeout","message":"restore in progress"}}`))
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(wakeConfig(3, time.Minute)))
	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.Error(t, err)
	var wakeErr *SandboxWakeTimeoutError
	require.ErrorAs(t, err, &wakeErr)
	require.Equal(t, 3, wakeErr.Attempts)
	if wakeErr.Waited <= 0 {
		t.Fatalf("expected positive waited duration, got %v", wakeErr.Waited)
	}
	var apiErr *APIError
	require.ErrorAs(t, wakeErr.LastErr, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	require.Equal(t, "req-42", apiErr.RequestID)
	require.Equal(t, 3, int(attempts.Load()))
}

func TestWakeRetry_RetryAfterExceedingBudgetStopsEarly(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(wakeConfig(5, 10*time.Second)))
	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.Error(t, err)
	var wakeErr *SandboxWakeTimeoutError
	require.ErrorAs(t, err, &wakeErr)
	require.Equal(t, 1, wakeErr.Attempts)
	require.Equal(t, 1, int(attempts.Load()))
}

func TestWakeRetry_Bare503OnPostIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(wakeConfig(5, time.Minute)))
	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.Error(t, err)
	var wakeErr *SandboxWakeTimeoutError
	if errors.As(err, &wakeErr) {
		t.Fatalf("expected a non-wake error, got *SandboxWakeTimeoutError")
	}
	require.Equal(t, 1, int(attempts.Load()))
}

func TestWakeRetry_ForgedMarkerIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set(wakeMarkerHeader, "bogus")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(wakeConfig(5, time.Minute)))
	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.Error(t, err)
	require.Equal(t, 1, int(attempts.Load()))
}

func TestWakeRetry_DisabledReturnsMarked503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{MaxRetries: 0, WakeRetryEnabled: WakeRetryDisabled()}))
	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.Error(t, err)
	require.Equal(t, 1, int(attempts.Load()))
}

func TestWakeRetry_WorksWithoutGenericRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-1", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	// MaxRetries == 0: no generic status retries, wake contract still active.
	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:          0,
		WakeRetryEnabled:    nil, // enabled by default
		WakeMaxAttempts:     DefaultWakeMaxAttempts,
		WakeOverallDeadline: DefaultWakeOverallDeadline,
	}))
	got, err := client.GetSandbox(context.Background(), "sbx-1")
	require.NoError(t, err)
	require.Equal(t, "sbx-1", got.ID)
	require.Equal(t, 2, int(attempts.Load()))
}

func TestWakeRetry_DefaultRetryConfigEnablesWake(t *testing.T) {
	cfg := DefaultRetryConfig()
	require.True(t, cfg.wakeEnabled())
	if cfg.WakeRetryEnabled != nil {
		t.Fatalf("WakeRetryEnabled should default to nil, got %v", *cfg.WakeRetryEnabled)
	}
	require.Equal(t, DefaultWakeMaxAttempts, cfg.WakeMaxAttempts)
	require.Equal(t, DefaultWakeOverallDeadline, cfg.WakeOverallDeadline)
}

func TestWakeRetry_HandBuiltConfigKeepsWakeEnabledByDefault(t *testing.T) {
	// A config that only sets the generic knobs must keep the wake contract
	// (nil WakeRetryEnabled means enabled) — a zero-value bool would
	// otherwise silently disable it.
	require.True(t, (&RetryConfig{MaxRetries: 5}).wakeEnabled())
	if (&RetryConfig{MaxRetries: 5, WakeRetryEnabled: WakeRetryDisabled()}).wakeEnabled() {
		t.Fatalf("explicit WakeRetryDisabled must turn the contract off")
	}
}

func TestWakeRetry_ExplicitRetryAfterZeroMeansImmediate(t *testing.T) {
	// Retry-After: 0 must retry immediately, not park for the 1s default.
	var attempts atomic.Int32
	started := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonResponse(w, http.StatusCreated, SandboxInfo{ID: "sbx-1", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(wakeConfig(5, time.Minute)))
	got, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		TemplateID: "tpl_1",
		Timeout:    intPtr(60),
	})
	require.NoError(t, err)
	require.Equal(t, "sbx-1", got.ID)
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Retry-After: 0 should retry immediately, waited %v", elapsed)
	}
}

func TestWakeRetry_NegativeDeadlineRemovesTimeBound(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// A 2s wait would exceed any small positive budget but the
			// negative deadline removes the bound; the wait itself is real.
			w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-1", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:          0,
		WakeRetryEnabled:    nil, // enabled by default
		WakeMaxAttempts:     5,
		WakeOverallDeadline: -1,
	}))
	got, err := client.GetSandbox(context.Background(), "sbx-1")
	require.NoError(t, err)
	require.Equal(t, "sbx-1", got.ID)
}

func TestIdlePolicyValidation(t *testing.T) {
	cases := []struct {
		name    string
		policy  *IdlePolicy
		wantErr string
	}{
		{"nil ok", nil, ""},
		{"below range", &IdlePolicy{intPtr(29), OnIdlePause, nil, nil}, "IdleTimeoutSeconds"},
		{"above range", &IdlePolicy{intPtr(86401), OnIdlePause, nil, nil}, "IdleTimeoutSeconds"},
		{"onIdle without timeout", &IdlePolicy{nil, OnIdlePause, nil, nil}, "OnIdle"},
		{"timeout without onIdle", &IdlePolicy{intPtr(300), "", nil, nil}, "OnIdle"},
		{"wrong action", &IdlePolicy{intPtr(300), "terminate", nil, nil}, "OnIdle"},
		{"probe without timeout", &IdlePolicy{nil, "", boolPtr(false), &ActivityProbe{Port: 8080, Path: "/active"}}, "ActiveProbe"},
		{"probe port", &IdlePolicy{intPtr(300), OnIdlePause, nil, &ActivityProbe{Port: 0, Path: "/active"}}, "ActiveProbe.Port"},
		{"probe path relative", &IdlePolicy{intPtr(300), OnIdlePause, nil, &ActivityProbe{Port: 8080, Path: "active"}}, "ActiveProbe.Path"},
		{"probe path whitespace", &IdlePolicy{intPtr(300), OnIdlePause, nil, &ActivityProbe{Port: 8080, Path: "/a b"}}, "ActiveProbe.Path"},
		{"probe timeout", &IdlePolicy{intPtr(300), OnIdlePause, nil, &ActivityProbe{Port: 8080, Path: "/active", TimeoutSeconds: intPtr(11)}}, "TimeoutSeconds"},
		{"valid", &IdlePolicy{intPtr(300), OnIdlePause, boolPtr(true), &ActivityProbe{Port: 8080, Path: "/active"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCreateSandbox_ForwardsIdlePolicyWire(t *testing.T) {
	var received *SandboxLifecycle
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var request CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			received = request.Lifecycle
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:        "sbx-idle",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx-idle":
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:        "sbx-idle",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	_, err := CreateSandbox(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, SandboxCreateOptions{
		Image:           "python:3.12",
		SkipHealthCheck: true,
		Lifecycle: &SandboxLifecycle{
			IdlePolicy: &IdlePolicy{
				IdleTimeoutSeconds: intPtr(300),
				OnIdle:             OnIdlePause,
				WakeOnAccess:       boolPtr(true),
				ActiveProbe:        &ActivityProbe{Port: 8080, Path: "/active"},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, received)
	require.NotNil(t, received.IdlePolicy)
	require.Equal(t, 300, *received.IdlePolicy.IdleTimeoutSeconds)
	require.Equal(t, OnIdlePause, received.IdlePolicy.OnIdle)
	require.NotNil(t, received.IdlePolicy.WakeOnAccess)
	require.True(t, *received.IdlePolicy.WakeOnAccess)
	require.NotNil(t, received.IdlePolicy.ActiveProbe)
	require.Equal(t, "/active", received.IdlePolicy.ActiveProbe.Path)
}

func TestCreateSandbox_RejectsInvalidIdlePolicy(t *testing.T) {
	_, err := CreateSandbox(context.Background(), ConnectionConfig{
		Domain:         "http://127.0.0.1:1",
		DisableMetrics: true,
	}, SandboxCreateOptions{
		Image:           "python:3.12",
		SkipHealthCheck: true,
		Lifecycle:       &SandboxLifecycle{IdlePolicy: &IdlePolicy{intPtr(10), OnIdlePause, nil, nil}},
	})
	require.Error(t, err)
	var argErr *InvalidArgumentError
	require.ErrorAs(t, err, &argErr)
	require.Equal(t, "IdleTimeoutSeconds", argErr.Field)
}

func TestRenewalPolicyValidation(t *testing.T) {
	cases := []struct {
		name    string
		policy  *RenewalPolicy
		wantErr string
	}{
		{"nil ok", nil, ""},
		{"below range", &RenewalPolicy{ExtendSeconds: 299}, "ExtendSeconds"},
		{"above range", &RenewalPolicy{ExtendSeconds: 86401}, "ExtendSeconds"},
		{"min boundary", &RenewalPolicy{ExtendSeconds: 300}, ""},
		{"max boundary", &RenewalPolicy{ExtendSeconds: 86400}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCreateSandbox_ForwardsRenewalPolicyWire(t *testing.T) {
	var received *SandboxLifecycle
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var request CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			received = request.Lifecycle
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:        "sbx-renew",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx-renew":
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:        "sbx-renew",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	_, err := CreateSandbox(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, SandboxCreateOptions{
		Image:           "python:3.12",
		SkipHealthCheck: true,
		Lifecycle: &SandboxLifecycle{
			RenewalPolicy: &RenewalPolicy{ExtendSeconds: 3600},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, received)
	require.NotNil(t, received.RenewalPolicy)
	require.Equal(t, 3600, received.RenewalPolicy.ExtendSeconds)
}

func TestCreateSandbox_RejectsInvalidRenewalPolicy(t *testing.T) {
	_, err := CreateSandbox(context.Background(), ConnectionConfig{
		Domain:         "http://127.0.0.1:1",
		DisableMetrics: true,
	}, SandboxCreateOptions{
		Image:           "python:3.12",
		SkipHealthCheck: true,
		Lifecycle:       &SandboxLifecycle{RenewalPolicy: &RenewalPolicy{ExtendSeconds: 299}},
	})
	require.Error(t, err)
	var argErr *InvalidArgumentError
	require.ErrorAs(t, err, &argErr)
	require.Equal(t, "ExtendSeconds", argErr.Field)
}

func TestCreateSandboxFromTemplate_ForwardsIdlePolicy(t *testing.T) {
	var received *CreateSandboxRequest
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var request CreateSandboxRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			received = &request
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:        "sbx-tpl",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx-tpl":
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:        "sbx-tpl",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/"):
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	_, err := CreateSandboxFromTemplate(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, "tpl_123", SandboxFromTemplateOptions{
		TimeoutSeconds:  600,
		SkipHealthCheck: true,
		IdlePolicy: &IdlePolicy{
			IdleTimeoutSeconds: intPtr(300),
			OnIdle:             OnIdlePause,
			WakeOnAccess:       boolPtr(true),
		},
	})
	require.NoError(t, err)
	require.NotNil(t, received)
	require.NotNil(t, received.Lifecycle)
	require.NotNil(t, received.Lifecycle.IdlePolicy)
	require.Equal(t, 300, *received.Lifecycle.IdlePolicy.IdleTimeoutSeconds)
	// Template mode must not forward workload-shaping lifecycle fields.
	if received.Lifecycle.PreStart != nil || received.Lifecycle.Periodic != nil {
		t.Fatalf("template mode must not forward workload-shaping lifecycle fields")
	}
}

func TestCreateSandboxFromTemplate_RejectsInvalidIdlePolicy(t *testing.T) {
	_, err := CreateSandboxFromTemplate(context.Background(), ConnectionConfig{
		Domain:         "http://127.0.0.1:1",
		DisableMetrics: true,
	}, "tpl_123", SandboxFromTemplateOptions{
		TimeoutSeconds: 600,
		IdlePolicy:     &IdlePolicy{nil, OnIdlePause, nil, nil},
	})
	require.Error(t, err)
	var argErr *InvalidArgumentError
	require.ErrorAs(t, err, &argErr)
	require.Equal(t, "OnIdle", argErr.Field)
}

func intPtr(v int) *int { return &v }

func boolPtr(v bool) *bool { return &v }

func TestWakeRetry_StreamConnectDoesNotParkOnWake(t *testing.T) {
	// The other SDKs leave streaming transports unwrapped: a marked 503 while
	// establishing a stream must fail fast instead of parking the connect for
	// up to the wake budget. The POST stream is not replayed by the generic
	// policy either, so the caller receives the marked error.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set(wakeMarkerHeader, wakeMarkerExpired)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: stdout\ndata: hello\n\n"))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(RetryConfig{
		MaxRetries:          1,
		InitialBackoff:      10 * time.Millisecond,
		WakeRetryEnabled:    nil, // enabled by default
		WakeMaxAttempts:     DefaultWakeMaxAttempts,
		WakeOverallDeadline: DefaultWakeOverallDeadline,
	}))

	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.Error(t, err, "a marked wake 503 on a POST stream must fail fast")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.True(t, apiErr.WakeExpired)
	require.Equal(t, 1, int(attempts.Load()))
	require.Len(t, events, 0)
}
