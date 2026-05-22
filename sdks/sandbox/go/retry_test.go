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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsTransient(t *testing.T) {
	tests := []struct {
		status    int
		transient bool
	}{
		{http.StatusTooManyRequests, true},      // 429
		{http.StatusBadGateway, true},           // 502
		{http.StatusServiceUnavailable, true},   // 503
		{http.StatusGatewayTimeout, true},       // 504
		{http.StatusBadRequest, false},          // 400
		{http.StatusUnauthorized, false},        // 401
		{http.StatusForbidden, false},           // 403
		{http.StatusNotFound, false},            // 404
		{http.StatusConflict, false},            // 409
		{http.StatusUnprocessableEntity, false}, // 422
		{http.StatusInternalServerError, false}, // 500
	}

	for _, tt := range tests {
		apiErr := &APIError{StatusCode: tt.status}
		if got := apiErr.IsTransient(); got != tt.transient {
			assert.Fail(t, fmt.Sprintf("status %d: IsTransient() = %v, want %v", tt.status, got, tt.transient))
		}
	}
}

func TestRetry_TransientThenSuccess(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"code":"UNAVAILABLE","message":"try again"}`))
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-ok", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}))

	got, err := client.GetSandbox(context.Background(), "sbx-ok")
	require.NoErrorf(t, err, "expected success after retries, got")
	if got.ID != "sbx-ok" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, "sbx-ok"))
	}
	if attempts.Load() != 3 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 3", attempts.Load()))
	}
}

func TestRetry_PermanentError(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		jsonResponse(w, http.StatusNotFound, ErrorResponse{
			Code:    "NOT_FOUND",
			Message: "sandbox not found",
		})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(DefaultRetryConfig()))

	_, err := client.GetSandbox(context.Background(), "sbx-missing")
	if err == nil {
		require.FailNow(t, "expected error, got nil")
	}
	if attempts.Load() != 1 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 1 (no retry on 404)", attempts.Load()))
	}
}

func TestRetry_Exhausted(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"UNAVAILABLE","message":"overloaded"}`))
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		Multiplier:     2.0,
	}))

	_, err := client.GetSandbox(context.Background(), "sbx-fail")
	require.Error(t, err)
	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		assert.Fail(t, fmt.Sprintf("StatusCode = %d, want 503", apiErr.StatusCode))
	}
	// 1 initial + 2 retries = 3
	if attempts.Load() != 3 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 3", attempts.Load()))
	}
}

func TestRetry_ContextCancelled(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"UNAVAILABLE","message":"down"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:     10,
		InitialBackoff: 30 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		Multiplier:     2.0,
	}))

	_, err := client.GetSandbox(ctx, "sbx-slow")
	if err == nil {
		require.FailNow(t, "expected error from context cancellation")
	}
	// Should have attempted at least once but not all 10 retries.
	if attempts.Load() < 1 {
		assert.Fail(t, "expected at least 1 attempt")
	}
	if attempts.Load() > 5 {
		assert.Fail(t, fmt.Sprintf("too many attempts (%d) — context should have cancelled", attempts.Load()))
	}
}

func TestRetry_Disabled(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"UNAVAILABLE","message":"down"}`))
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key") // no WithRetry

	_, err := client.GetSandbox(context.Background(), "sbx-noretry")
	if err == nil {
		require.FailNow(t, "expected error")
	}
	if attempts.Load() != 1 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 1 (retry disabled)", attempts.Load()))
	}
}

func TestRetry_RetryAfterHeader(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":"RATE_LIMITED","message":"slow down"}`))
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-rate", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     5 * time.Second,
		Multiplier:     2.0,
	}))

	start := time.Now()
	got, err := client.GetSandbox(context.Background(), "sbx-rate")
	elapsed := time.Since(start)

	require.NoErrorf(t, err, "expected success, got")
	if got.ID != "sbx-rate" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, "sbx-rate"))
	}
	// Retry-After: 1 means 1 second. The delay should be at least ~1s.
	if elapsed < 900*time.Millisecond {
		assert.Fail(t, fmt.Sprintf("elapsed = %v, expected >= ~1s from Retry-After header", elapsed))
	}
}

func TestRetry_RateLimit429(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":"RATE_LIMITED","message":"too fast"}`))
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-429", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}))

	got, err := client.GetSandbox(context.Background(), "sbx-429")
	require.NoErrorf(t, err, "expected success after 429 retry, got")
	if got.ID != "sbx-429" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, "sbx-429"))
	}
	if attempts.Load() != 2 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 2", attempts.Load()))
	}
}

func TestRetry_StreamingConnection(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"code":"UNAVAILABLE","message":"try again"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: stdout\ndata: hello\n\n"))
	}))
	defer srv.Close()

	client := NewExecdClient(srv.URL, "tok", WithRetry(RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		Multiplier:     2.0,
	}))

	var events []StreamEvent
	err := client.RunCommand(context.Background(), RunCommandRequest{Command: "echo hello"}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	require.NoErrorf(t, err, "expected success after stream retry, got")
	if len(events) != 1 || events[0].Data != "hello" {
		assert.Fail(t, fmt.Sprintf("events = %+v, want [{Event:stdout Data:hello}]", events))
	}
	if attempts.Load() != 2 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 2", attempts.Load()))
	}
}

func TestBackoff(t *testing.T) {
	cfg := RetryConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     10 * time.Second,
		Multiplier:     2.0,
		Jitter:         0, // no jitter for deterministic test
	}

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{10, 10 * time.Second}, // capped at MaxBackoff
	}

	for _, tt := range tests {
		got := cfg.backoff(tt.attempt)
		if got != tt.expected {
			assert.Fail(t, fmt.Sprintf("backoff(%d) = %v, want %v", tt.attempt, got, tt.expected))
		}
	}
}

func TestBackoff_WithJitter(t *testing.T) {
	cfg := RetryConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     10 * time.Second,
		Multiplier:     2.0,
		Jitter:         0.5,
	}

	// With 50% jitter, attempt 0 should be in [50ms, 150ms].
	for i := 0; i < 20; i++ {
		got := cfg.backoff(0)
		if got < 50*time.Millisecond || got > 150*time.Millisecond {
			assert.Fail(t, fmt.Sprintf("backoff(0) with 50%% jitter = %v, expected [50ms, 150ms]", got))
		}
	}
}

func TestDefaultTransport(t *testing.T) {
	tr := DefaultTransport()
	if tr.MaxIdleConns != 100 {
		assert.Fail(t, fmt.Sprintf("MaxIdleConns = %d, want 100", tr.MaxIdleConns))
	}
	if tr.MaxIdleConnsPerHost != 10 {
		assert.Fail(t, fmt.Sprintf("MaxIdleConnsPerHost = %d, want 10", tr.MaxIdleConnsPerHost))
	}
	if tr.IdleConnTimeout != 90*time.Second {
		assert.Fail(t, fmt.Sprintf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout))
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		assert.Fail(t, fmt.Sprintf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout))
	}
	if tr.TLSClientConfig == nil {
		assert.Fail(t, "TLSClientConfig is nil, want non-nil")
		return
	}
	if tr.TLSClientConfig.VerifyConnection == nil {
		assert.Fail(t, "VerifyConnection is nil, want NIST keylength verifier by default")
	}
}

func TestTransportConfig_NewTransport(t *testing.T) {
	cfg := TransportConfig{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		DialTimeout:         15 * time.Second,
		KeepAlive:           15 * time.Second,
	}
	tr := cfg.NewTransport()
	if tr.MaxIdleConns != 50 {
		assert.Fail(t, fmt.Sprintf("MaxIdleConns = %d, want 50", tr.MaxIdleConns))
	}
	if tr.MaxIdleConnsPerHost != 5 {
		assert.Fail(t, fmt.Sprintf("MaxIdleConnsPerHost = %d, want 5", tr.MaxIdleConnsPerHost))
	}
	if tr.TLSClientConfig == nil {
		assert.Fail(t, "TLSClientConfig is nil, want non-nil")
		return
	}
	if tr.TLSClientConfig.VerifyConnection == nil {
		assert.Fail(t, "VerifyConnection is nil, want NIST verifier when weak certs are disabled")
	}
}

func TestTransportConfig_NewTransport_AllowsWeakServerCertsWhenConfigured(t *testing.T) {
	cfg := TransportConfig{
		MaxIdleConns:                  50,
		MaxIdleConnsPerHost:           5,
		IdleConnTimeout:               60 * time.Second,
		TLSHandshakeTimeout:           5 * time.Second,
		DialTimeout:                   15 * time.Second,
		KeepAlive:                     15 * time.Second,
		AllowWeakServerCertKeyLengths: true,
	}
	tr := cfg.NewTransport()
	if tr.TLSClientConfig == nil {
		assert.Fail(t, "TLSClientConfig is nil, want non-nil")
		return
	}
	if tr.TLSClientConfig.VerifyConnection != nil {
		assert.Fail(t, "VerifyConnection is set, want nil when weak certs are explicitly allowed")
	}
}

func TestEnsureCertMeetsNISTMinimums_RSA1024Rejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.SHA256WithRSA,
		SerialNumber:          big.NewInt(1),
		BasicConstraintsValid: true,
	}
	require.Error(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnsureCertMeetsNISTMinimums_EC224Accepted(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
		SerialNumber:          big.NewInt(2),
		BasicConstraintsValid: true,
	}
	require.NoError(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnsureCertMeetsNISTMinimums_SHA1Rejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.SHA1WithRSA,
		SerialNumber:          big.NewInt(3),
		BasicConstraintsValid: true,
	}
	require.Error(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnsureCertMeetsNISTMinimums_UnknownSignatureAlgorithmRejected(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cert := &x509.Certificate{
		PublicKey:             &key.PublicKey,
		SignatureAlgorithm:    x509.UnknownSignatureAlgorithm,
		SerialNumber:          big.NewInt(4),
		BasicConstraintsValid: true,
	}
	require.Error(t, ensureCertMeetsNISTMinimums(cert))
}

func TestEnforceNISTPeerCertificateMinimums_RejectsWeakTrustAnchorKey(t *testing.T) {
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rootKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	leaf := &x509.Certificate{
		PublicKey:             &leafKey.PublicKey,
		SignatureAlgorithm:    x509.SHA256WithRSA,
		SerialNumber:          big.NewInt(5),
		BasicConstraintsValid: true,
	}
	root := &x509.Certificate{
		PublicKey:             &rootKey.PublicKey,
		SignatureAlgorithm:    x509.SHA1WithRSA,
		SerialNumber:          big.NewInt(6),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	err = enforceNISTPeerCertificateMinimums(tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{leaf, root}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate[1]")
}

func TestConnectionConfig_RetryAndTransport(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"code":"BAD_GATEWAY","message":"retry"}`))
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-cfg", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	retry := DefaultRetryConfig()
	retry.InitialBackoff = 10 * time.Millisecond
	transport := DefaultTransportConfig()

	config := ConnectionConfig{
		Domain:    srv.Listener.Addr().String(),
		Protocol:  "http",
		APIKey:    "test-key",
		Retry:     &retry,
		Transport: &transport,
	}

	lc := config.lifecycleClient()
	got, err := lc.GetSandbox(context.Background(), "sbx-cfg")
	require.NoErrorf(t, err, "expected success with ConnectionConfig retry, got")
	if got.ID != "sbx-cfg" {
		assert.Fail(t, fmt.Sprintf("ID = %q, want %q", got.ID, "sbx-cfg"))
	}
	if attempts.Load() != 2 {
		assert.Fail(t, fmt.Sprintf("attempts = %d, want 2", attempts.Load()))
	}
}

func TestAPIError_ErrorWithRequestID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-abc-123")
		jsonResponse(w, http.StatusNotFound, ErrorResponse{
			Code:    "NOT_FOUND",
			Message: "sandbox not found",
		})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key")
	_, err := client.GetSandbox(context.Background(), "sbx-missing")
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	if apiErr.RequestID != "req-abc-123" {
		assert.Fail(t, fmt.Sprintf("RequestID = %q, want %q", apiErr.RequestID, "req-abc-123"))
	}

	errMsg := apiErr.Error()
	if got, want := errMsg, "NOT_FOUND: sandbox not found (request_id: req-abc-123)"; got != want {
		assert.Fail(t, fmt.Sprintf("Error() = %q, want %q", got, want))
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected time.Duration
	}{
		{"seconds", "5", 5 * time.Second},
		{"zero", "0", 0},
		{"empty", "", 0},
		{"negative", "-1", 0},
		{"garbage", "not-a-number", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tt.header != "" {
				resp.Header.Set("Retry-After", tt.header)
			}
			got := parseRetryAfter(resp)
			if got != tt.expected {
				assert.Fail(t, fmt.Sprintf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.expected))
			}
		})
	}
}

func TestParseRetryAfter_NilResponse(t *testing.T) {
	got := parseRetryAfter(nil)
	if got != 0 {
		assert.Fail(t, fmt.Sprintf("parseRetryAfter(nil) = %v, want 0", got))
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"api 503", &APIError{StatusCode: 503}, true},
		{"api 429", &APIError{StatusCode: 429}, true},
		{"api 404", &APIError{StatusCode: 404}, false},
		{"api 400", &APIError{StatusCode: 400}, false},
		{"api 502", &APIError{StatusCode: 502}, true},
		{"api 504", &APIError{StatusCode: 504}, true},
		{"api 500", &APIError{StatusCode: 500}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientError(tt.err, nil); got != tt.transient {
				assert.Fail(t, fmt.Sprintf("isTransientError(%v) = %v, want %v", tt.err, got, tt.transient))
			}
		})
	}
}

func TestRetry_CustomRetryableStatusCodes(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":"INTERNAL","message":"temporary 500"}`))
			return
		}
		jsonResponse(w, http.StatusOK, SandboxInfo{ID: "sbx-500-retried", CreatedAt: time.Now()})
	}))
	defer srv.Close()

	client := NewLifecycleClient(srv.URL, "key", WithRetry(RetryConfig{
		MaxRetries:           2,
		InitialBackoff:       5 * time.Millisecond,
		MaxBackoff:           20 * time.Millisecond,
		Multiplier:           2.0,
		RetryableStatusCodes: []int{http.StatusInternalServerError},
	}))

	got, err := client.GetSandbox(context.Background(), "sbx-500-retried")
	require.NoErrorf(t, err, "expected success with custom retryable status codes")
	require.Equal(t, "sbx-500-retried", got.ID)
	require.Equal(t, int32(2), attempts.Load())
}
