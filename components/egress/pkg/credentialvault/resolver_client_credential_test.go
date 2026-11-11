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

package credentialvault

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startUnixEnvelopeServer serves the resolve envelope over a real unix
// socket and records whether a caller credential header arrived.
func startUnixEnvelopeServer(t *testing.T) (endpoint string, sawCredential *atomic.Bool) {
	t.Helper()
	sawCredential = &atomic.Bool{}
	// Short temp path: unix socket addresses are bound by sockaddr_un's
	// ~104-char limit on macOS and t.TempDir()'s path overflows it.
	dir, err := os.MkdirTemp("", "credtest-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", dir+"/resolver.sock")
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCredential.Store(r.Header.Get(EgressCredentialHeader) != "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"headers":{}}`))
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return "unix://" + listener.Addr().String(), sawCredential
}

// TestResolverDialUnixCarriesNoCredential: unix dials never attach the
// caller credential — socket permissions are their trust boundary.
func TestResolverDialUnixCarriesNoCredential(t *testing.T) {
	endpoint, sawCredential := startUnixEnvelopeServer(t)
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:       endpoint,
		Timeout:        2 * time.Second,
		MaxConcurrency: 1,
		// A provider is supplied on purpose: unix dials must not attach the
		// credential even when one is available.
		Credentials: credProvider(t),
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	require.False(t, sawCredential.Load(), "unix dials must not carry the caller credential header")
}

// TestResolverDialTCPWithoutProviderFailsClosed: a TCP dial without a
// provisioned signer is denied before reaching the wire — never an
// unauthenticated dial.
func TestResolverDialTCPWithoutProviderFailsClosed(t *testing.T) {
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:       "https://resolver.invalid:8443",
		Timeout:        time.Second,
		MaxConcurrency: 1,
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	resolverErr := requireKind(t, err, ResolverUnavailable)
	require.Equal(t, "egress_credential_not_provisioned", resolverErr.Reason)
}

type failingProvider struct{}

func (failingProvider) Credential(_, _ string) (string, error) {
	return "", fmt.Errorf("signer exploded")
}

// TestResolverDialMintFailureFailsClosed: a mint failure denies the single
// resolve; it never dials without a credential and never panics.
func TestResolverDialMintFailureFailsClosed(t *testing.T) {
	_, tlsConfig := startEnvelopeServer(t, http.StatusOK, `{"headers":{}}`, &capturedRequest{})
	// The envelope server binds TLS on 127.0.0.1:port; dialing it is fine —
	// the point is that Resolve never gets there.
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        "https://resolver.invalid:8443",
		Timeout:         time.Second,
		MaxConcurrency:  1,
		TLSClientConfig: tlsConfig,
		Credentials:     failingProvider{},
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	resolverErr := requireKind(t, err, ResolverUnavailable)
	require.Equal(t, "egress_credential_mint_failed", resolverErr.Reason)
}

// TestResolverDialCredentialAudienceIsOrigin: the minted aud is the resolver
// origin — scheme://authority with the path prefix excluded — matching the
// string the verifying resolver configures as expected_aud.
func TestResolverDialCredentialAudienceIsOrigin(t *testing.T) {
	var captured capturedRequest
	endpoint, tlsConfig := startEnvelopeServer(t, http.StatusOK, `{"headers":{}}`, &captured)
	provider, verifier := testResolverCredentialProvider(t)
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        endpoint + "/deep/prefix",
		Timeout:         time.Second,
		MaxConcurrency:  1,
		TLSClientConfig: tlsConfig,
		Credentials:     provider,
	})
	require.NoError(t, err)
	_, err = client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err)
	frame := captured.snapshot()
	require.NotEmpty(t, frame.cred)
	requireCredential(t, frame.cred, verifier, endpoint, "sbx_01HZYEXAMPLE")
}
