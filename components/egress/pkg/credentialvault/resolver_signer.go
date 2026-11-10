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
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// Resolver caller-authentication signer loading. Two operator channels —
// fast-sandbox Secret volume (signing key dir + kid env; dir wins) or
// sidecar env (PEM, kid = sandbox id). Missing and unloadable alike yield a
// nil signer; the exchange gate then fails closed for https:// endpoints
// only.

// ResolverCredentialSigner is the loaded signing identity of this egress.
type ResolverCredentialSigner struct {
	key ed25519.PrivateKey
	kid string
}

// Kid returns the signer's platform identifier (JWKS lookup key).
func (s *ResolverCredentialSigner) Kid() string { return s.kid }

// Mint signs one credential for (subject, audience) at the full 1h TTL with
// a fresh jti. Stateless; caching lives in resolverCredentialManager.
func (s *ResolverCredentialSigner) Mint(subject, audience string, now time.Time) (string, error) {
	jti, err := newCredentialJTI()
	if err != nil {
		return "", err
	}
	return mintResolverCredential(s.key, s.kid, subject, audience, now.Unix(), now.Add(CredentialTTLMax).Unix(), jti)
}

// LoadResolverCredentialSigner loads the signer from the process
// environment. See the channel table above for semantics.
func LoadResolverCredentialSigner() (*ResolverCredentialSigner, error) {
	return loadResolverCredentialSigner(os.Getenv, os.ReadFile)
}

func loadResolverCredentialSigner(getenv func(string) string, readFile func(string) ([]byte, error)) (*ResolverCredentialSigner, error) {
	if dir := strings.TrimSpace(getenv(constants.EnvCredentialResolverSigningKeyDir)); dir != "" {
		path := filepath.Join(dir, constants.CredentialResolverSigningKeyFile)
		data, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("resolver caller-auth signing key: read %s: %w", path, err)
		}
		kid := strings.TrimSpace(getenv(constants.EnvCredentialResolverSigningKid))
		if kid == "" {
			return nil, fmt.Errorf("resolver caller-auth signing key: %s is required with %s", constants.EnvCredentialResolverSigningKid, constants.EnvCredentialResolverSigningKeyDir)
		}
		if !ValidCredentialKid(kid) {
			return nil, fmt.Errorf("resolver caller-auth signing key: kid %q does not match the platform identifier grammar", kid)
		}
		return signerFromPEM(string(data), kid, path)
	}
	if raw := strings.TrimSpace(getenv(constants.EnvCredentialResolverSigningKey)); raw != "" {
		kid := strings.TrimSpace(getenv(constants.EnvSandboxID))
		if kid == "" {
			return nil, fmt.Errorf("resolver caller-auth signing key: %s is required with the sidecar key channel", constants.EnvSandboxID)
		}
		if !ValidCredentialKid(kid) {
			return nil, fmt.Errorf("resolver caller-auth signing key: sandbox id %q does not match the platform identifier grammar", kid)
		}
		return signerFromPEM(raw, kid, constants.EnvCredentialResolverSigningKey)
	}
	return nil, nil
}

func signerFromPEM(pemText, kid, source string) (*ResolverCredentialSigner, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("resolver caller-auth signing key (%s): not a PEM block", source)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("resolver caller-auth signing key (%s): parse PKCS8: %w", source, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("resolver caller-auth signing key (%s): algorithm is %T, want Ed25519", source, parsed)
	}
	return &ResolverCredentialSigner{key: key, kid: kid}, nil
}
