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
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type failingReadCloser struct{}

func (f failingReadCloser) Read(_ []byte) (int, error) { return 0, errors.New("boom") }
func (f failingReadCloser) Close() error               { return nil }

func TestHandleError_BodyReadFailure(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     make(http.Header),
		Body:       failingReadCloser{},
	}
	resp.Header.Set("X-Request-Id", "req-read-fail")

	err := handleError(resp)
	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	require.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	assert.Contains(t, apiErr.Response.Message, "failed to read error response body")
}

func TestRewriteEndpointURL_ReplacesAllMatches(t *testing.T) {
	cfg := ConnectionConfig{
		EndpointHostRewrite: map[string]string{
			"host.docker.internal": "localhost",
		},
	}

	in := "http://host.docker.internal/a/http://host.docker.internal/b"
	got := cfg.RewriteEndpointURL(in)
	want := "http://localhost/a/http://localhost/b"
	require.Equal(t, want, got)
}

func TestDefaultRetryConfig_HasRetryableStatusCodes(t *testing.T) {
	cfg := DefaultRetryConfig()
	require.NotEmpty(t, cfg.RetryableStatusCodes, "DefaultRetryConfig should include retryable status codes")
}

func TestOctalMode_DoesNotPanic(t *testing.T) {
	require.Equal(t, 755, OctalMode(0o755))
}

func TestHandleError_JSONBodyStillParsed(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"code":"RATE_LIMIT","message":"slow down"}`)),
	}

	err := handleError(resp)
	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	require.Equal(t, "RATE_LIMIT", apiErr.Response.Code)
}

func TestNewClient_DefaultTransportEnforcesNISTKeylengthPolicy(t *testing.T) {
	client := NewClient("https://example.com", "key", "OPEN-SANDBOX-API-KEY")
	require.NotNil(t, client.httpClient)
	require.NotNil(t, client.httpClient.Transport)

	tr, ok := client.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport, got %T", client.httpClient.Transport)
	require.NotNil(t, tr.TLSClientConfig)
	require.NotNil(t, tr.TLSClientConfig.VerifyConnection)
}

func TestNewClient_NilCustomHTTPClientFallsBackToDefaultSecureTransport(t *testing.T) {
	client := NewClient(
		"https://example.com",
		"key",
		"OPEN-SANDBOX-API-KEY",
		WithHTTPClient(nil),
	)
	require.NotNil(t, client.httpClient)
	require.NotNil(t, client.httpClient.Transport)

	tr, ok := client.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport, got %T", client.httpClient.Transport)
	require.NotNil(t, tr.TLSClientConfig)
	require.NotNil(t, tr.TLSClientConfig.VerifyConnection)
}
