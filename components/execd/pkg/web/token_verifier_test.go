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

package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestAccessTokenHashedVerifierFormat(t *testing.T) {
	prevBinding := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prevBinding) })

	rawToken := "derived-raw-token-abc123"
	sum := sha256.Sum256([]byte(rawToken))
	verifierHex := hex.EncodeToString(sum[:])
	envValue := "sha256:v1:nonce_xyz_123456:" + verifierHex

	r := NewRouter(envValue)

	// /ping always open
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Missing header -> 401
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// Replaying the leaked env verifier string directly -> 401
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(model.ApiAccessTokenHeader, envValue)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// Presenting the derived raw token -> allowed
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(model.ApiAccessTokenHeader, rawToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.NotEqual(t, http.StatusUnauthorized, w.Code)
}

func TestAccessTokenTwoSegmentSha256Format(t *testing.T) {
	prevBinding := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prevBinding) })

	rawToken := "two-segment-raw-token"
	sum := sha256.Sum256([]byte(rawToken))
	envValue := binding.AccessTokenHashPrefix + hex.EncodeToString(sum[:])

	r := NewRouter(envValue)

	// Replaying the sha256:<hex> env value directly -> 401
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(model.ApiAccessTokenHeader, envValue)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// Presenting the raw token -> allowed
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(model.ApiAccessTokenHeader, rawToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.NotEqual(t, http.StatusUnauthorized, w.Code)
}

func TestAccessTokenMalformedSha256FailsClosed(t *testing.T) {
	prevBinding := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prevBinding) })

	for _, bad := range []string{
		"sha256:not-hex",
		"sha256::nonce:deadbeef",
		"sha256:v1::deadbeef",
		"sha256:v1:nonce:short",
		"sha256:a:b:c:d",
	} {
		r := NewRouter(bad)
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set(model.ApiAccessTokenHeader, bad)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code, "expected 401 for %q", bad)
	}
}
