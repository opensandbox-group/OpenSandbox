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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// Resolver caller authentication (OSEP-0025 §Resolver Caller Authentication).
// The egress mints a self-contained Ed25519 JWT per (subject, resolver
// origin) and attaches it to every TCP resolve dial; resolvers verify it
// offline against the server-published JWKS. The wire contract is frozen in
// testdata/resolver_auth_vectors.json — regenerate nothing here without
// regenerating the vectors.

const (
	// EgressCredentialHeader is the single header carrying the compact JWT.
	EgressCredentialHeader = "OpenSandbox-Egress-Credential"
	// CredentialIssuer is the platform identifier matched by the resolver SDK.
	CredentialIssuer = "opensandbox"
	// CredentialTTLMax bounds exp - iat; the resolver-side leeway is 30s.
	CredentialTTLMax = time.Hour
	// CredentialLeeway mirrors the resolver-side verification leeway.
	CredentialLeeway = 30 * time.Second
	// credentialJTIBytes is the fresh nonce minted per credential.
	credentialJTIBytes = 16
)

// credentialKidGrammar is the platform identifier grammar: a sidecar sandboxId
// or a fast-sandbox "<pool>:v<version>" kid. The delimiter is path-safe, so a
// valid kid substitutes into the JWKS lookup path without escaping ambiguity;
// off-grammar kids are rejected locally with no fetch (egress side validates
// its own kid at mint time, resolver side at verification time).
const credentialKidGrammarSrc = `^[A-Za-z0-9][A-Za-z0-9_-]{0,62}(:v[0-9]{1,8})?$`

var credentialKidGrammar = regexp.MustCompile(credentialKidGrammarSrc)

// ValidCredentialKid reports whether kid matches the platform identifier
// grammar (the same pattern frozen in the vectors' contract block).
func ValidCredentialKid(kid string) bool {
	return credentialKidGrammar.MatchString(kid)
}

// CredentialProvider supplies the caller credential for resolver dials.
// Implementations cache per (subject, audience) and refresh ahead of expiry;
// Credential must be safe for concurrent use.
type CredentialProvider interface {
	// Credential returns the compact JWT proving "this resolve call
	// originated from the egress process" for the given subject and
	// resolver-origin audience.
	Credential(subject, audience string) (string, error)
}

// credentialHeader is the mint-side JWS header: exactly alg/typ/kid, in that
// field order (the canonical vectors pin the marshaled bytes).
type credentialHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// credentialClaims is the mint-side payload: six claims, no more. Field order
// matches the canonical vectors; unknown claims are a resolver-side concern
// (ignored there, never minted here).
type credentialClaims struct {
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// mintResolverCredential signs one credential. iat/exp are Unix seconds and
// must satisfy 0 < exp-iat <= 3600. The output is the three-segment compact
// JWS carried in EgressCredentialHeader (~250-300 bytes).
func mintResolverCredential(key ed25519.PrivateKey, kid, subject, audience string, iat, exp int64, jti string) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("resolver credential: signing key is not Ed25519")
	}
	if !ValidCredentialKid(kid) {
		return "", fmt.Errorf("resolver credential: kid %q does not match the platform identifier grammar", kid)
	}
	if subject == "" || audience == "" || jti == "" {
		return "", fmt.Errorf("resolver credential: subject, audience, and jti must be non-empty")
	}
	window := exp - iat
	if window <= 0 || window > int64(CredentialTTLMax.Seconds()) {
		return "", fmt.Errorf("resolver credential: exp-iat must be in (0, %d] seconds, got %d", int64(CredentialTTLMax.Seconds()), window)
	}
	header, err := json.Marshal(credentialHeader{Alg: "EdDSA", Typ: "JWT", Kid: kid})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(credentialClaims{
		Iss: CredentialIssuer,
		Aud: audience,
		Sub: subject,
		Iat: iat,
		Exp: exp,
		Jti: jti,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	signature := ed25519.Sign(key, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// newCredentialJTI returns a fresh 128-bit nonce, base64url-encoded (22
// chars). Uniqueness is per mint window only — it is not a replay defense.
func newCredentialJTI() (string, error) {
	buf := make([]byte, credentialJTIBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("resolver credential: jti entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// resolverCredentialEnvNames are the two provisioning channels; kept together
// so the transport-aware exchange gate (W1.2) and the loader share one source.
func resolverCredentialEnvNames() (sidecarEnv, dirEnv string) {
	return constants.EnvCredentialResolverSigningKey, constants.EnvCredentialResolverSigningKeyDir
}
