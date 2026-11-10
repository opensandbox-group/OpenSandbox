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
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// End-to-end: the egress resolver client with a real signer against the
// enforcing test resolver (resolver.py --require-credential):
//
//   - egress dial resolves through the verified credential;
//   - a dial without the credential header — what a sandbox process could
//     do — gets 401;
//   - a TCP client with no provisioned signer fails closed before the wire.

// stubSDKEnv returns the PYTHONPATH that makes resolver.py's lazy
// opensandbox_resolver_auth import resolve to the in-repo SDK source.
func stubSDKEnv(t *testing.T) []string {
	t.Helper()
	sdkSrc, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "sdks", "resolver-auth", "python", "src"))
	require.NoError(t, err)
	return []string{"PYTHONPATH=" + sdkSrc}
}

// stubSDKAvailable probes whether python3 can import the resolver-auth SDK;
// tests skip when the environment cannot run the enforcing stub.
func stubSDKAvailable(t *testing.T) bool {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		return false
	}
	probe := exec.Command(python, "-c", "import opensandbox_resolver_auth")
	probe.Env = append(os.Environ(), stubSDKEnv(t)...)
	return probe.Run() == nil
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

// startEnforcingStubResolver boots resolver.py with --require-credential and
// a static JWKS holding the given key; returns its origin.
func startEnforcingStubResolver(t *testing.T, kid string, pub ed25519.PublicKey) string {
	t.Helper()
	jwk := map[string]any{
		"kty": "OKP", "crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
		"kid": kid, "alg": "EdDSA", "use": "sig",
	}
	jwksFile := filepath.Join(t.TempDir(), "jwks.json")
	raw, err := json.Marshal(map[string]any{kid: jwk})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(jwksFile, raw, 0o600))

	port := freeTCPPort(t)
	origin := fmt.Sprintf("https://127.0.0.1:%d", port)
	stub := startStubResolver(t, stubSDKEnv(t),
		"--https", fmt.Sprintf("127.0.0.1:%d", port),
		"--tls-cert", stubResolverPath(t, filepath.Join("fixtures", "test-localhost-cert.pem")),
		"--tls-key", stubResolverPath(t, filepath.Join("fixtures", "test-localhost-key.pem")),
		"--require-credential",
		"--jwks-file", jwksFile,
		"--expected-aud", origin,
	)
	require.Equal(t, origin, stub.endpoint)
	return origin
}

func TestResolverCallerAuthEndToEnd(t *testing.T) {
	if !stubSDKAvailable(t) {
		t.Skip("python3 + opensandbox-resolver-auth are required for the caller-auth e2e")
	}

	const kid = "pool-e2e:v1"
	pub, priv, err := ed25519.GenerateKey(cryptorand.Reader)
	require.NoError(t, err)
	origin := startEnforcingStubResolver(t, kid, pub)

	provider := newResolverCredentialManager(&ResolverCredentialSigner{key: priv, kid: kid})
	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        origin,
		Timeout:         5 * time.Second,
		MaxConcurrency:  2,
		TLSClientConfig: stubResolverTLSConfig(t),
		Credentials:     provider,
	})
	require.NoError(t, err)

	t.Run("egress dial resolves through the enforced credential", func(t *testing.T) {
		decision, err := client.Resolve(context.Background(), resolverTestInput())
		require.NoError(t, err)
		require.Equal(t, "Bearer test-spi-signer", decision.Headers["authorization"])
	})

	t.Run("direct dial without a credential is 401 (R4)", func(t *testing.T) {
		document, err := marshalResolveInput(resolverTestInput())
		require.NoError(t, err)
		request, err := http.NewRequest(http.MethodPost, origin+"/resolve", bytes.NewReader(document))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		httpClient := &http.Client{
			Transport: &http.Transport{TLSClientConfig: stubResolverTLSConfig(t)},
			Timeout:   5 * time.Second,
		}
		response, err := httpClient.Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		var body struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		require.Equal(t, "credential_missing", body.Error)
	})

	t.Run("TCP client without a signer fails closed before the wire", func(t *testing.T) {
		bare, err := NewResolverClient(ResolverClientOptions{
			Endpoint:        origin,
			Timeout:         2 * time.Second,
			MaxConcurrency:  1,
			TLSClientConfig: stubResolverTLSConfig(t),
		})
		require.NoError(t, err)
		_, err = bare.Resolve(context.Background(), resolverTestInput())
		resolverErr := requireKind(t, err, ResolverUnavailable)
		require.Equal(t, "egress_credential_not_provisioned", resolverErr.Reason)
	})
}

// TestResolverCallerAuthSidecarKidEndToEnd: sidecar kids are real
// sandboxIds (mixed-case ULIDs) — exercises the widened grammar end to end.
func TestResolverCallerAuthSidecarKidEndToEnd(t *testing.T) {
	if !stubSDKAvailable(t) {
		t.Skip("python3 + opensandbox-resolver-auth are required for the caller-auth e2e")
	}

	const kid = "sbx_01HZYEXAMPLE"
	pub, priv, err := ed25519.GenerateKey(cryptorand.Reader)
	require.NoError(t, err)
	origin := startEnforcingStubResolver(t, kid, pub)

	client, err := NewResolverClient(ResolverClientOptions{
		Endpoint:        origin,
		Timeout:         5 * time.Second,
		MaxConcurrency:  1,
		TLSClientConfig: stubResolverTLSConfig(t),
		Credentials: newResolverCredentialManager(
			&ResolverCredentialSigner{key: priv, kid: kid},
		),
	})
	require.NoError(t, err)

	decision, err := client.Resolve(context.Background(), resolverTestInput())
	require.NoError(t, err, "mixed-case sidecar kids must verify end to end")
	require.Equal(t, "Bearer test-spi-signer", decision.Headers["authorization"])
}
