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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// The frozen wire contract lives in testdata/resolver_auth_vectors.json (the
// same file the Python resolver SDK consumes). These tests keep the mint-side
// code locked to it: contract constants must match, canonical mints must be
// byte-identical, and the kid grammar must classify every vector kid the same
// way the resolver-side verifier will.

type vectorsContract struct {
	HeaderName               string `json:"header_name"`
	Issuer                   string `json:"issuer"`
	KidGrammar               string `json:"kid_grammar"`
	TTLMaxSeconds            int    `json:"ttl_max_seconds"`
	LeewaySeconds            int    `json:"leeway_seconds"`
	JtiBytes                 int    `json:"jti_bytes"`
	EnvSidecarPrivateKey     string `json:"env_sidecar_private_key"`
	EnvFastsandboxKeyDir     string `json:"env_fastsandbox_key_dir"`
	EnvFastsandboxSigningKid string `json:"env_fastsandbox_signing_kid"`
	SigningKeyFile           string `json:"signing_key_file"`
}

type vectorsKey struct {
	ID            string `json:"id"`
	PrivateKeyPEM string `json:"private_key_pem"`
}

type vectorsCase struct {
	Name    string `json:"name"`
	Key     string `json:"key"`
	Header  string `json:"header"`
	Payload string `json:"payload"`
	JWT     string `json:"jwt"`
	Checks  []struct {
		Now     int64  `json:"now"`
		Verdict string `json:"verdict"`
		Error   string `json:"error"`
	} `json:"checks"`
}

type vectorsFile struct {
	Contract vectorsContract `json:"contract"`
	Keys     []vectorsKey    `json:"keys"`
	Cases    []vectorsCase   `json:"cases"`
}

func loadResolverAuthVectors(t *testing.T) vectorsFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/resolver_auth_vectors.json")
	require.NoError(t, err)
	var v vectorsFile
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

func vectorPrivateKey(t *testing.T, v vectorsFile, id string) ed25519.PrivateKey {
	t.Helper()
	for _, k := range v.Keys {
		if k.ID != id {
			continue
		}
		block, _ := pem.Decode([]byte(k.PrivateKeyPEM))
		require.NotNil(t, block, "bad PEM in vector key %s", id)
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		require.NoError(t, err)
		key, ok := parsed.(ed25519.PrivateKey)
		require.True(t, ok, "vector key %s is not Ed25519", id)
		return key
	}
	t.Fatalf("vector key %s not found", id)
	return nil
}

// TestResolverAuthContractSync pins every code-side constant to the vectors'
// contract block so the two languages cannot drift.
func TestResolverAuthContractSync(t *testing.T) {
	v := loadResolverAuthVectors(t)
	c := v.Contract
	require.Equal(t, c.HeaderName, EgressCredentialHeader)
	require.Equal(t, c.Issuer, CredentialIssuer)
	require.Equal(t, c.KidGrammar, credentialKidGrammarSrc)
	require.Equal(t, c.TTLMaxSeconds, int(CredentialTTLMax.Seconds()))
	require.Equal(t, c.LeewaySeconds, int(CredentialLeeway.Seconds()))
	require.Equal(t, c.JtiBytes, credentialJTIBytes)
	sidecarEnv, dirEnv := resolverCredentialEnvNames()
	require.Equal(t, c.EnvSidecarPrivateKey, sidecarEnv)
	require.Equal(t, c.EnvFastsandboxKeyDir, dirEnv)
	require.Equal(t, c.EnvFastsandboxSigningKid, constants.EnvCredentialResolverSigningKid)
	require.Equal(t, c.SigningKeyFile, constants.CredentialResolverSigningKeyFile)
}

// TestMintMatchesCanonicalVectors mints the two canonical cases and requires
// byte-for-byte equality with the vectors — the mint-side wire format is
// frozen, not merely compatible.
func TestMintMatchesCanonicalVectors(t *testing.T) {
	v := loadResolverAuthVectors(t)
	for _, name := range []string{"valid_sidecar", "valid_pool"} {
		var vc vectorsCase
		found := false
		for _, c := range v.Cases {
			if c.Name == name {
				vc, found = c, true
				break
			}
		}
		require.True(t, found, "canonical case %s missing from vectors", name)

		var header struct {
			Kid string `json:"kid"`
		}
		require.NoError(t, json.Unmarshal([]byte(vc.Header), &header))
		var claims credentialClaims
		require.NoError(t, json.Unmarshal([]byte(vc.Payload), &claims))

		key := vectorPrivateKey(t, v, vc.Key)
		minted, err := mintResolverCredential(key, header.Kid, claims.Sub, claims.Aud, claims.Iat, claims.Exp, claims.Jti)
		require.NoError(t, err)
		require.Equal(t, vc.JWT, minted, "case %s: minted JWT must match the vector byte for byte", name)
	}
}

// TestCredentialKidGrammarAgainstVectors runs every vector kid through
// ValidCredentialKid and requires agreement with the expected verdict: kids
// whose rejection reason is kid_grammar/kid_missing must fail locally, every
// other kid (including unknown-but-well-formed ones) must pass the grammar.
func TestCredentialKidGrammarAgainstVectors(t *testing.T) {
	v := loadResolverAuthVectors(t)
	checked := 0
	for _, c := range v.Cases {
		var header struct {
			Kid string `json:"kid"`
		}
		require.NoError(t, json.Unmarshal([]byte(c.Header), &header), "case %s", c.Name)
		require.NotEmpty(t, c.Checks, "case %s has no checks", c.Name)
		errCode := c.Checks[0].Error
		grammarMustFail := errCode == "kid_grammar" || errCode == "kid_missing"
		require.Equal(t, !grammarMustFail, ValidCredentialKid(header.Kid),
			"case %s: kid %q grammar classification disagrees with the vector", c.Name, header.Kid)
		checked++
	}
	require.Greater(t, checked, 20, "expected the full format matrix")
}

func TestNewCredentialJTI(t *testing.T) {
	seen := make(map[string]struct{}, 128)
	for i := 0; i < 128; i++ {
		jti, err := newCredentialJTI()
		require.NoError(t, err)
		require.Len(t, jti, 22, "128-bit nonce encodes to 22 base64url chars")
		_, dup := seen[jti]
		require.False(t, dup, "jti must be fresh per mint")
		seen[jti] = struct{}{}
	}
}

func TestMintResolverCredentialValidation(t *testing.T) {
	v := loadResolverAuthVectors(t)
	key := vectorPrivateKey(t, v, "sidecar-1")
	now := time.Now().Unix()

	_, err := mintResolverCredential(key, "sbx-7f3a2c", "sbx-7f3a2c", "credential-resolver@resolver.internal", now, now+int64(CredentialTTLMax.Seconds())+1, "kX9sT2vZ8qLm4NpR6wYb3A")
	require.ErrorContains(t, err, "exp-iat")
	_, err = mintResolverCredential(key, "sbx-7f3a2c", "sbx-7f3a2c", "credential-resolver@resolver.internal", now, now, "kX9sT2vZ8qLm4NpR6wYb3A")
	require.ErrorContains(t, err, "exp-iat")
	_, err = mintResolverCredential(key, "sbx-7f3a2c", "sbx-7f3a2c", "credential-resolver@resolver.internal", now, now-1, "kX9sT2vZ8qLm4NpR6wYb3A")
	require.ErrorContains(t, err, "exp-iat")
	_, err = mintResolverCredential(key, "../escape", "sbx-7f3a2c", "credential-resolver@resolver.internal", now, now+60, "kX9sT2vZ8qLm4NpR6wYb3A")
	require.ErrorContains(t, err, "platform identifier grammar")
	_, err = mintResolverCredential(key, "sbx-7f3a2c", "", "credential-resolver@resolver.internal", now, now+60, "kX9sT2vZ8qLm4NpR6wYb3A")
	require.ErrorContains(t, err, "non-empty")

	// The maximal window is legal.
	jti, err := newCredentialJTI()
	require.NoError(t, err)
	minted, err := mintResolverCredential(key, "sbx-7f3a2c", "sbx-7f3a2c", "credential-resolver@resolver.internal", now, now+int64(CredentialTTLMax.Seconds()), jti)
	require.NoError(t, err)
	require.NotEmpty(t, minted)
}
