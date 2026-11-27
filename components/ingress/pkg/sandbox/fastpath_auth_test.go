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

package sandbox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func fastPathTestAuth(t *testing.T) (FastPathAuthConfig, credentials.TransportCredentials) {
	t.Helper()
	serving := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(serving.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serving.Certificate().Raw}), 0600))
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/identities/serviceaccounts/gateway-caller/token" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer bootstrap" {
			t.Errorf("unexpected bootstrap request: %s %s authenticated=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer bootstrap")
			http.Error(w, "invalid bootstrap request", http.StatusForbidden)
			return
		}
		var request authenticationv1.TokenRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != fastPathAudience || request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != 3600 {
			http.Error(w, "invalid audience or lifetime", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&authenticationv1.TokenRequest{
			TypeMeta: metav1.TypeMeta{APIVersion: "authentication.k8s.io/v1", Kind: "TokenRequest"},
			Status:   authenticationv1.TokenRequestStatus{Token: "test-caller", ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour))},
		})
	}))
	t.Cleanup(api.Close)
	return FastPathAuthConfig{Kubernetes: &rest.Config{Host: api.URL, BearerToken: "bootstrap", ContentConfig: rest.ContentConfig{ContentType: "application/json"}, TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})}}, Namespace: "identities", ServiceAccount: "gateway-caller", CAFile: ca}, credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: serving.TLS.Certificates})
}

func TestFastPathCredentialSingleFlightAndExpiry(t *testing.T) {
	now := time.Now()
	var count atomic.Int32
	fail := false
	source := &fastPathTokenSource{now: func() time.Time { return now }, create: func(context.Context) (*authenticationv1.TokenRequest, error) {
		count.Add(1)
		if fail {
			return nil, fmt.Errorf("sensitive Kubernetes error")
		}
		return &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "caller", ExpirationTimestamp: metav1.NewTime(now.Add(time.Hour))}}, nil
	}}
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			md, err := source.GetRequestMetadata(t.Context())
			if err != nil || md["authorization"] != "Bearer caller" {
				t.Error("credential unavailable", err)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, count.Load())
	require.True(t, source.RequireTransportSecurity())
	now = source.refresh.Add(time.Second)
	fail = true
	_, err := source.GetRequestMetadata(t.Context())
	require.NoError(t, err)
	// Wait for refresh completion before advancing the test clock.
	require.Eventually(t, func() bool {
		source.mu.Lock()
		defer source.mu.Unlock()
		return count.Load() == 2 && source.flight == nil
	}, time.Second, 5*time.Millisecond)
	now = source.expires.Add(time.Second)
	_, err = source.GetRequestMetadata(t.Context())
	require.ErrorContains(t, err, "TokenRequest unavailable")
	require.NotContains(t, err.Error(), "sensitive")
	_, err = source.GetRequestMetadata(t.Context())
	require.Error(t, err)
	require.EqualValues(t, 3, count.Load())
}

func TestFastPathCredentialServesValidTokenWhileRefreshIsSlow(t *testing.T) {
	now := time.Now()
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var calls atomic.Int32
	source := &fastPathTokenSource{now: func() time.Time { return now }, create: func(context.Context) (*authenticationv1.TokenRequest, error) {
		if calls.Add(1) == 1 {
			return &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "first", ExpirationTimestamp: metav1.NewTime(now.Add(time.Hour))}}, nil
		}
		close(refreshStarted)
		<-releaseRefresh
		return &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "second", ExpirationTimestamp: metav1.NewTime(now.Add(2 * time.Hour))}}, nil
	}}
	md, err := source.GetRequestMetadata(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Bearer first", md["authorization"])

	now = source.refresh.Add(time.Second)
	shortCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	md, err = source.GetRequestMetadata(shortCtx)
	require.NoError(t, err)
	require.Equal(t, "Bearer first", md["authorization"])
	<-refreshStarted
	close(releaseRefresh)
	require.Eventually(t, func() bool {
		md, err := source.GetRequestMetadata(t.Context())
		return err == nil && md["authorization"] == "Bearer second"
	}, time.Second, 5*time.Millisecond)
}

func TestFastPathTokenSourceRejectsInsecureBootstrap(t *testing.T) {
	for _, config := range []*rest.Config{
		{Host: "http://kubernetes.test"},
		{Host: "https://kubernetes.test", TLSClientConfig: rest.TLSClientConfig{Insecure: true}},
	} {
		_, err := newFastPathTokenSource(FastPathAuthConfig{Kubernetes: config, Namespace: "identities", ServiceAccount: "gateway-caller"})
		require.ErrorContains(t, err, "verified Kubernetes API TLS")
	}
}

func TestFastPathTLSRejectsInvalidTrustAndMissingIdentity(t *testing.T) {
	_, err := newFastPathTokenSource(FastPathAuthConfig{})
	require.Error(t, err)
	t.Setenv("POD_NAMESPACE", "custom-system")
	require.Equal(t, "custom-system", resolveFastPathAuthNamespace(""))
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a CA"), 0600))
	_, err = newFastPathTLS(path, "")
	require.Error(t, err)
}
