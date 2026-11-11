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
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

func resetDefaultProvider() {
	defaultCredentialProvider.mu.Lock()
	defer defaultCredentialProvider.mu.Unlock()
	defaultCredentialProvider.provider = nil
	defaultCredentialProvider.fingerprint = ""
	defaultCredentialProvider.lastAttempt = time.Time{}
}

func writeSignerDir(t *testing.T, dir string, kid string) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(cryptorand.Reader)
	require.NoError(t, err)
	pemText, err := pemForKey(t, key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, constants.CredentialResolverSigningKeyFile), []byte(pemText), 0o600))
	return key
}

func pemForKey(t *testing.T, key ed25519.PrivateKey) (string, error) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// TestLazyDefaultProviderRecoversAfterRepair: an endpoint client built
// while the signer is missing must start working once the key material is
// repaired — no process restart, no client-cache pinning of the nil
// provider.
func TestLazyDefaultProviderRecoversAfterRepair(t *testing.T) {
	resetDefaultProvider()
	t.Setenv(constants.EnvCredentialResolverSigningKeyDir, t.TempDir())
	t.Setenv(constants.EnvCredentialResolverSigningKid, "pool-lazy:v1")
	t.Cleanup(resetDefaultProvider)

	client, err := NewResolverClientFromEnv("https://resolver.invalid:8443")
	require.NoError(t, err)
	_, firstErr := client.Resolve(t.Context(), resolverTestInput())
	resolverErr := requireKind(t, firstErr, ResolverUnavailable)
	require.Equal(t, "egress_credential_not_provisioned", resolverErr.Reason)

	// Repair: write the key into the dir channel and let the backoff pass.
	writeSignerDir(t, os.Getenv(constants.EnvCredentialResolverSigningKeyDir), "pool-lazy:v1")
	defaultCredentialProvider.mu.Lock()
	defaultCredentialProvider.lastAttempt = time.Time{}
	defaultCredentialProvider.mu.Unlock()

	_, err = client.Resolve(t.Context(), resolverTestInput())
	require.Error(t, err)
	resolverErr = requireKind(t, err, ResolverUnavailable)
	require.NotEqual(t, "egress_credential_not_provisioned", resolverErr.Reason,
		"the repaired signer must now be reachable at dial time (the dial "+
			"itself fails against resolver.invalid — that is expected here)")
}

// TestDefaultProviderSwapsOnRotation: a rotated key (same channel, new
// bytes) replaces the memoized signer on the backoff cadence.
func TestDefaultProviderSwapsOnRotation(t *testing.T) {
	resetDefaultProvider()
	dir := t.TempDir()
	t.Setenv(constants.EnvCredentialResolverSigningKeyDir, dir)
	t.Setenv(constants.EnvCredentialResolverSigningKid, "pool-rot:v1")
	t.Cleanup(resetDefaultProvider)

	first := writeSignerDir(t, dir, "pool-rot:v1")
	p1 := DefaultResolverCredentialProvider()
	require.NotNil(t, p1)

	// Same fingerprint within the backoff window: same instance.
	require.Same(t, p1, DefaultResolverCredentialProvider())

	// Rotate: new key bytes, same kid. After the backoff, a new manager.
	second := writeSignerDir(t, dir, "pool-rot:v1")
	require.NotEqual(t, first, second)
	defaultCredentialProvider.mu.Lock()
	defaultCredentialProvider.lastAttempt = time.Time{}
	defaultCredentialProvider.mu.Unlock()
	p2 := DefaultResolverCredentialProvider()
	require.NotNil(t, p2)
	require.NotSame(t, p1, p2, "rotated key material must swap the memoized signer")
}
