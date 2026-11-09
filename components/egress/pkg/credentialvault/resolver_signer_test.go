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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

func testEd25519PEM(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	var buf []byte
	buf = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return string(buf), priv
}

func writeSigningKeyDir(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, constants.CredentialResolverSigningKeyFile), []byte(contents), 0o600))
	return dir
}

func envMap(t *testing.T, values map[string]string) func(string) string {
	t.Helper()
	return func(name string) string { return values[name] }
}

func TestLoadResolverCredentialSignerMatrix(t *testing.T) {
	validPEM, _ := testEd25519PEM(t)
	ecPEM, _ := generateECPKCS8PEM(t)

	t.Run("unconfigured", func(t *testing.T) {
		signer, err := loadResolverCredentialSigner(envMap(t, map[string]string{}), os.ReadFile)
		require.NoError(t, err)
		require.Nil(t, signer, "no channel configured must yield a nil signer, not an error")
	})

	t.Run("dir channel valid", func(t *testing.T) {
		dir := writeSigningKeyDir(t, validPEM)
		signer, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
			constants.EnvCredentialResolverSigningKid:    "pool-a:v2",
		}), os.ReadFile)
		require.NoError(t, err)
		require.NotNil(t, signer)
		require.Equal(t, "pool-a:v2", signer.Kid())
	})

	t.Run("dir channel missing file", func(t *testing.T) {
		dir := t.TempDir() // exists, no signing.key inside
		_, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
			constants.EnvCredentialResolverSigningKid:    "pool-a:v2",
		}), os.ReadFile)
		require.ErrorContains(t, err, "signing.key")
	})

	t.Run("dir channel garbage pem", func(t *testing.T) {
		dir := writeSigningKeyDir(t, "not a pem")
		_, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
			constants.EnvCredentialResolverSigningKid:    "pool-a:v2",
		}), os.ReadFile)
		require.ErrorContains(t, err, "not a PEM block")
	})

	t.Run("dir channel wrong algorithm", func(t *testing.T) {
		dir := writeSigningKeyDir(t, ecPEM)
		_, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
			constants.EnvCredentialResolverSigningKid:    "pool-a:v2",
		}), os.ReadFile)
		require.ErrorContains(t, err, "want Ed25519")
	})

	t.Run("dir channel kid missing", func(t *testing.T) {
		dir := writeSigningKeyDir(t, validPEM)
		_, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
		}), os.ReadFile)
		require.ErrorContains(t, err, constants.EnvCredentialResolverSigningKid)
	})

	t.Run("dir channel kid off grammar", func(t *testing.T) {
		dir := writeSigningKeyDir(t, validPEM)
		_, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
			constants.EnvCredentialResolverSigningKid:    "../escape",
		}), os.ReadFile)
		require.ErrorContains(t, err, "platform identifier grammar")
	})

	t.Run("sidecar env channel valid", func(t *testing.T) {
		signer, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKey: validPEM,
			constants.EnvSandboxID:                    "sbx-7f3a2c",
		}), os.ReadFile)
		require.NoError(t, err)
		require.NotNil(t, signer)
		require.Equal(t, "sbx-7f3a2c", signer.Kid())
	})

	t.Run("sidecar env channel without sandbox id", func(t *testing.T) {
		_, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKey: validPEM,
		}), os.ReadFile)
		require.ErrorContains(t, err, constants.EnvSandboxID)
	})

	t.Run("dir channel takes precedence over env", func(t *testing.T) {
		dirPEM, _ := testEd25519PEM(t)
		dir := writeSigningKeyDir(t, dirPEM)
		otherPEM, _ := testEd25519PEM(t)
		signer, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKeyDir: dir,
			constants.EnvCredentialResolverSigningKid:    "pool-a:v2",
			constants.EnvCredentialResolverSigningKey:    otherPEM,
			constants.EnvSandboxID:                       "sbx-7f3a2c",
		}), os.ReadFile)
		require.NoError(t, err)
		require.Equal(t, "pool-a:v2", signer.Kid())
	})
}

func generateECPKCS8PEM(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return encodePKCS8PEM(t, key), key
}

func encodePKCS8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestResolverCredentialSignerMint(t *testing.T) {
	pemText, priv := testEd25519PEM(t)
	signer, err := loadResolverCredentialSigner(envMap(t, map[string]string{
		constants.EnvCredentialResolverSigningKey: pemText,
		constants.EnvSandboxID:                    "sbx-7f3a2c",
	}), os.ReadFile)
	require.NoError(t, err)

	jwt, err := signer.Mint("sbx-7f3a2c", "credential-resolver@resolver.internal", time.Unix(1790000000, 0))
	require.NoError(t, err)
	parts := strings.Split(jwt, ".")
	require.Len(t, parts, 3)

	// Round-trip: verify with the underlying public key.
	header := mustB64Decode(t, parts[0])
	claims := mustB64Decode(t, parts[1])
	sig := mustB64DecodeRaw(t, parts[2])
	require.True(t, ed25519.Verify(priv.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig))
	require.Contains(t, header, `"alg":"EdDSA"`)
	require.Contains(t, header, `"kid":"sbx-7f3a2c"`)
	require.Contains(t, claims, `"iss":"opensandbox"`)
	require.Contains(t, claims, `"aud":"credential-resolver@resolver.internal"`)
	require.Contains(t, claims, `"sub":"sbx-7f3a2c"`)
}

func mustB64Decode(t *testing.T, segment string) string {
	t.Helper()
	return string(mustB64DecodeRaw(t, segment))
}

func mustB64DecodeRaw(t *testing.T, segment string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	require.NoError(t, err)
	return raw
}

func testHTTPSExchangeVaultRequest() CreateRequest {
	request := testExchangeVaultRequest()
	request.Credentials[0].Source = mustMarshal(map[string]string{
		"type":     "exchange",
		"endpoint": "https://resolver.internal:8443",
	})
	return request
}

// TestExchangeHTTPSCredentialsRequireSigner is the W1.2 acceptance matrix:
// https:// exchange vaults are rejected at write time without a signer,
// unix:// exchange vaults and inline-only vaults keep serving, and a valid
// signer reopens the https path (transport-aware gate, fail closed).
func TestExchangeHTTPSCredentialsRequireSigner(t *testing.T) {
	t.Run("https without signer rejected", func(t *testing.T) {
		store := testResolverReadyStore()
		_, err := store.Create(testHTTPSExchangeVaultRequest(), testExchangePolicy(t))
		require.ErrorContains(t, err, "caller-authentication")
	})

	t.Run("unix without signer still accepted", func(t *testing.T) {
		store := testResolverReadyStore()
		_, err := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
		require.NoError(t, err)
	})

	t.Run("inline only without signer accepted", func(t *testing.T) {
		store := testResolverReadyStore()
		_, err := store.Create(testCredentialVaultRequest(), testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`))
		require.NoError(t, err)
	})

	t.Run("https with signer accepted", func(t *testing.T) {
		pemText, _ := testEd25519PEM(t)
		signer, err := loadResolverCredentialSigner(envMap(t, map[string]string{
			constants.EnvCredentialResolverSigningKey: pemText,
			constants.EnvSandboxID:                    "sbx-7f3a2c",
		}), os.ReadFile)
		require.NoError(t, err)

		store := testResolverReadyStore()
		store.SetResolverAuthConfig(ResolverAuthConfig{Signer: signer})
		_, err = store.Create(testHTTPSExchangeVaultRequest(), testExchangePolicy(t))
		require.NoError(t, err)
	})

	t.Run("https with unloadable signer rejected with diagnostic", func(t *testing.T) {
		store := testResolverReadyStore()
		store.SetResolverAuthConfig(ResolverAuthConfig{Problem: errors.New("boom")})
		_, err := store.Create(testHTTPSExchangeVaultRequest(), testExchangePolicy(t))
		require.ErrorContains(t, err, "unloadable")
		require.ErrorContains(t, err, "boom")
	})
}
