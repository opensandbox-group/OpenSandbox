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

// Command gen deterministically rebuilds resolver_auth_vectors.json, the
// shared cross-language test-vector file for the OSEP-0025 Resolver Caller
// Authentication contract (egress mint side in Go, resolver verification
// side in Python).
//
// Determinism: key material is derived from fixed seeds (SHA-256 of a fixed
// label), header/payload JSON is built as ordered literal strings, and all
// timestamps are fixed epochs — rerunning the generator must reproduce the
// committed file byte for byte.
//
// Run from the credentialvault package root:
//
//	go run ./testdata/gen/main.go
//
// The canonical cases valid_sidecar and valid_pool double as mint-side
// fixtures: the egress mint implementation must produce their jwt strings
// byte for byte.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Fixed clock (seconds), mirroring the OSEP-0025 credential example.
const (
	iat      = 1790000000
	exp      = 1790003600
	nowValid = 1790000600
	leeway   = 30
)

const (
	defaultIssuer = "opensandbox"
	defaultAud    = "credential-resolver@resolver.internal"
	defaultSub    = "sbx-7f3a2c"
	jtiPrimary    = "kX9sT2vZ8qLm4NpR6wYb3A"
	jtiSecondary  = "mZ8tY3wW9qLn5MqR7xXc2B"
)

type contract struct {
	HeaderName               string            `json:"header_name"`
	Alg                      string            `json:"alg"`
	Typ                      string            `json:"typ,omitempty"`
	Issuer                   string            `json:"issuer"`
	DefaultAudience          string            `json:"default_expected_audience"`
	KidGrammar               string            `json:"kid_grammar"`
	KidNotes                 string            `json:"kid_notes"`
	TTLMaxSeconds            int               `json:"ttl_max_seconds"`
	LeewaySeconds            int               `json:"leeway_seconds"`
	JtiBytes                 int               `json:"jti_bytes"`
	EnvSidecarPrivateKey     string            `json:"env_sidecar_private_key"`
	EnvFastsandboxKeyDir     string            `json:"env_fastsandbox_key_dir"`
	EnvFastsandboxSigningKid string            `json:"env_fastsandbox_signing_kid"`
	AudienceDerivation       string            `json:"audience_derivation"`
	SigningKeyFile           string            `json:"signing_key_file"`
	KeyFormat                string            `json:"key_format"`
	JWKSPathTemplate         string            `json:"jwks_path_template"`
	RejectedHeaderParams     []string          `json:"rejected_header_params"`
	Errors                   map[string]string `json:"errors"`
	AllowedHeaderParams      []string          `json:"allowed_header_params"`
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type keyVector struct {
	ID            string `json:"id"`
	Profile       string `json:"profile"`
	Kid           string `json:"kid"`
	PrivateKeyPEM string `json:"private_key_pem"`
	PublicJWK     jwk    `json:"public_jwk"`
}

type check struct {
	Now     int64  `json:"now"`
	Verdict string `json:"verdict"`
	Error   string `json:"error,omitempty"`
}

type caseVector struct {
	Name             string  `json:"name"`
	Key              string  `json:"key,omitempty"`
	HeaderJSON       string  `json:"header"`
	PayloadJSON      string  `json:"payload,omitempty"`
	JWT              string  `json:"jwt"`
	DocumentSubject  string  `json:"document_subject,omitempty"`
	ExpectedAudience string  `json:"expected_audience,omitempty"`
	Checks           []check `json:"checks"`
	Note             string  `json:"note,omitempty"`
}

type vectorFile struct {
	Schema      string           `json:"schema"`
	GeneratedBy string           `json:"generated_by"`
	Contract    contract         `json:"contract"`
	Clock       map[string]int64 `json:"clock"`
	Keys        []keyVector      `json:"keys"`
	Cases       []caseVector     `json:"cases"`
}

// deriveKey deterministically derives an Ed25519 keypair from a fixed label.
func deriveKey(label string) (ed25519.PrivateKey, error) {
	seed := sha256.Sum256([]byte("opensandbox-resolver-auth-vectors/" + label))
	return ed25519.NewKeyFromSeed(seed[:]), nil
}

func privateKeyPEM(priv ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, block); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// signJWT signs the exact header/payload literal strings with the key and
// returns the compact JWS. Header and payload bytes are used verbatim so the
// mint side must reproduce the same field order to match byte for byte.
func signJWT(priv ed25519.PrivateKey, headerJSON, payloadJSON string) string {
	signingInput := b64([]byte(headerJSON)) + "." + b64([]byte(payloadJSON))
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + b64(sig)
}

func accept(now int64) check { return check{Now: now, Verdict: "accept"} }
func reject(now int64, errCode string) check {
	return check{Now: now, Verdict: "reject", Error: errCode}
}

func payloadWith(iss, aud, sub string, i, e int64, jtiValue, extra string) string {
	base := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":%q,"iat":%d,"exp":%d,"jti":%q`, iss, aud, sub, i, e, jtiValue)
	if extra == "" {
		return base + "}"
	}
	return base + "," + extra + "}"
}

func main() {
	out := flag.String("o", "testdata/resolver_auth_vectors.json", "output path for the vectors file")
	flag.Parse()

	sidecarPriv, err := deriveKey("sidecar-1")
	if err != nil {
		panic(err)
	}
	poolPriv, err := deriveKey("pool-1")
	if err != nil {
		panic(err)
	}
	sidecarPEM, err := privateKeyPEM(sidecarPriv)
	if err != nil {
		panic(err)
	}
	poolPEM, err := privateKeyPEM(poolPriv)
	if err != nil {
		panic(err)
	}

	keys := []keyVector{
		{
			ID: "sidecar-1", Profile: "sidecar", Kid: "sbx-7f3a2c",
			PrivateKeyPEM: sidecarPEM,
			PublicJWK: jwk{
				Kty: "OKP", Crv: "Ed25519", X: b64(sidecarPriv.Public().(ed25519.PublicKey)),
				Kid: "sbx-7f3a2c", Alg: "EdDSA", Use: "sig",
			},
		},
		{
			ID: "pool-1", Profile: "fast-sandbox", Kid: "pool-a:v2",
			PrivateKeyPEM: poolPEM,
			PublicJWK: jwk{
				Kty: "OKP", Crv: "Ed25519", X: b64(poolPriv.Public().(ed25519.PublicKey)),
				Kid: "pool-a:v2", Alg: "EdDSA", Use: "sig",
			},
		},
	}

	// Canonical header/payload literals. The mint-side structs (W1.1) must
	// marshal to exactly these bytes.
	hSidecar := `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c"}`
	hPool := `{"alg":"EdDSA","typ":"JWT","kid":"pool-a:v2"}`
	pDefault := payloadWith(defaultIssuer, defaultAud, defaultSub, iat, exp, jtiPrimary, "")

	cases := []caseVector{
		{
			Name: "valid_sidecar", Key: "sidecar-1", HeaderJSON: hSidecar, PayloadJSON: pDefault,
			JWT:    signJWT(sidecarPriv, hSidecar, pDefault),
			Checks: []check{accept(nowValid)},
			Note:   "mint-canonical (OSEP 0025 Credential Format example); W1.1 must reproduce this jwt byte for byte",
		},
		{
			Name: "valid_pool", Key: "pool-1", HeaderJSON: hPool, PayloadJSON: pDefault,
			JWT:    signJWT(poolPriv, hPool, pDefault),
			Checks: []check{accept(nowValid)},
			Note:   "mint-canonical for the fast-sandbox profile; kid grammar with the pool:version form",
		},
		{
			Name: "valid_typ_absent", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","kid":"sbx-7f3a2c"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","kid":"sbx-7f3a2c"}`, pDefault),
			Checks:      []check{accept(nowValid)},
			Note:        "typ is not required (OSEP: 'typ not required; if present it must be JWT')",
		},
		{
			Name: "valid_unknown_payload_claim", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: payloadWith(defaultIssuer, defaultAud, defaultSub, iat, exp, jtiPrimary, `"foo":"bar"`),
			JWT:         signJWT(sidecarPriv, hSidecar, payloadWith(defaultIssuer, defaultAud, defaultSub, iat, exp, jtiPrimary, `"foo":"bar"`)),
			Checks:      []check{accept(nowValid)},
			Note:        "unknown payload claims are ignored",
		},
		{
			Name: "valid_exp_leeway_boundary", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: pDefault, JWT: signJWT(sidecarPriv, hSidecar, pDefault),
			Checks: []check{accept(exp + leeway)},
			Note:   "now == exp + 30s is the last accepted instant (now <= exp + 30s)",
		},
		{
			Name: "valid_iat_leeway_boundary", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: payloadWith(defaultIssuer, defaultAud, defaultSub, 1790000030, exp, jtiPrimary, ""),
			JWT:         signJWT(sidecarPriv, hSidecar, payloadWith(defaultIssuer, defaultAud, defaultSub, 1790000030, exp, jtiPrimary, "")),
			Checks:      []check{accept(1790000000)},
			Note:        "iat at exactly now + 30s is still accepted ('iat more than the leeway in the future is rejected')",
		},
		{
			Name: "reject_expired_beyond_leeway", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: pDefault, JWT: signJWT(sidecarPriv, hSidecar, pDefault),
			Checks: []check{reject(exp+leeway+1, "expired")},
			Note:   "now > exp + 30s",
		},
		{
			Name: "reject_iat_future", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: payloadWith(defaultIssuer, defaultAud, defaultSub, 1790000180, exp, jtiPrimary, ""),
			JWT:         signJWT(sidecarPriv, hSidecar, payloadWith(defaultIssuer, defaultAud, defaultSub, 1790000180, exp, jtiPrimary, "")),
			Checks:      []check{reject(1790000000, "iat_future")},
			Note:        "iat 180s in the future at verification time exceeds the 30s leeway",
		},
		{
			Name: "reject_window_over_3600", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: payloadWith(defaultIssuer, defaultAud, defaultSub, iat, iat+3601, jtiPrimary, ""),
			JWT:         signJWT(sidecarPriv, hSidecar, payloadWith(defaultIssuer, defaultAud, defaultSub, iat, iat+3601, jtiPrimary, "")),
			Checks:      []check{reject(nowValid, "window")},
			Note:        "exp - iat must be <= 3600",
		},
		{
			Name: "reject_aud_mismatch", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: payloadWith(defaultIssuer, "other-resolver@elsewhere.internal", defaultSub, iat, exp, jtiPrimary, ""),
			JWT:         signJWT(sidecarPriv, hSidecar, payloadWith(defaultIssuer, "other-resolver@elsewhere.internal", defaultSub, iat, exp, jtiPrimary, "")),
			Checks:      []check{reject(nowValid, "audience")},
			Note:        "aud must equal the verifying resolver's own origin; cross-resolver replay dies here",
		},
		{
			Name: "reject_iss_mismatch", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON: payloadWith("not-opensandbox", defaultAud, defaultSub, iat, exp, jtiPrimary, ""),
			JWT:         signJWT(sidecarPriv, hSidecar, payloadWith("not-opensandbox", defaultAud, defaultSub, iat, exp, jtiPrimary, "")),
			Checks:      []check{reject(nowValid, "issuer")},
			Note:        "iss must equal the SDK's configured issuer",
		},
		{
			Name: "reject_sub_mismatch", Key: "sidecar-1", HeaderJSON: hSidecar,
			PayloadJSON:     payloadWith(defaultIssuer, defaultAud, "sbx-othe1", iat, exp, jtiPrimary, ""),
			JWT:             signJWT(sidecarPriv, hSidecar, payloadWith(defaultIssuer, defaultAud, "sbx-othe1", iat, exp, jtiPrimary, "")),
			DocumentSubject: defaultSub,
			Checks:          []check{reject(nowValid, "subject_mismatch")},
			Note:            "JWT sub must equal the resolve document's subject; the attested sub is the sole authoritative identity",
		},
		{
			Name: "reject_alg_none", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"none","typ":"JWT","kid":"sbx-7f3a2c"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"none","typ":"JWT","kid":"sbx-7f3a2c"}`, pDefault),
			Checks:      []check{reject(nowValid, "alg_pinned")},
			Note:        "alg is pinned to EdDSA; anything else is rejected with no algorithm negotiation",
		},
		{
			Name: "reject_alg_hs256", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"HS256","typ":"JWT","kid":"sbx-7f3a2c"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"HS256","typ":"JWT","kid":"sbx-7f3a2c"}`, pDefault),
			Checks:      []check{reject(nowValid, "alg_pinned")},
			Note:        "HMAC confusion: signature bytes are a valid Ed25519 signature but the alg is wrong",
		},
		{
			Name: "reject_typ_wrong", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"jose","kid":"sbx-7f3a2c"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"jose","kid":"sbx-7f3a2c"}`, pDefault),
			Checks:      []check{reject(nowValid, "typ_invalid")},
			Note:        "typ, when present, must be JWT",
		},
		{
			Name: "valid_kid_mixed_case", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx_01HZYEXAMPLE"}`,
			PayloadJSON: payloadWith(defaultIssuer, defaultAud, "sbx_01HZYEXAMPLE", iat, exp, jtiSecondary, ""),
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx_01HZYEXAMPLE"}`, payloadWith(defaultIssuer, defaultAud, "sbx_01HZYEXAMPLE", iat, exp, jtiSecondary, "")),
			Checks:      []check{accept(nowValid)},
			Note:        "sidecar kids are real sandboxIds (ULID-form, mixed case and underscores) — the grammar admits them",
		},
		{
			Name: "reject_kid_grammar_path", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"../escape"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"../escape"}`, pDefault),
			Checks:      []check{reject(nowValid, "kid_grammar")},
			Note:        "off-grammar kid rejected locally with no JWKS fetch; path traversal is the motivating case",
		},
		{
			Name: "reject_kid_grammar_space", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx bad"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx bad"}`, pDefault),
			Checks:      []check{reject(nowValid, "kid_grammar")},
			Note:        "no spaces in platform identifiers",
		},
		{
			Name: "reject_kid_grammar_slash", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx/1"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx/1"}`, pDefault),
			Checks:      []check{reject(nowValid, "kid_grammar")},
			Note:        "no slashes — kid substitutes into a URL path",
		},
		{
			Name: "reject_kid_grammar_colons", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"a:b:c"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"a:b:c"}`, pDefault),
			Checks:      []check{reject(nowValid, "kid_grammar")},
			Note:        "at most one :version suffix",
		},
		{
			Name: "reject_kid_grammar_version", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"pool-a:x9"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"pool-a:x9"}`, pDefault),
			Checks:      []check{reject(nowValid, "kid_grammar")},
			Note:        "the version suffix must be v followed by digits",
		},
		{
			Name: "reject_kid_missing", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT"}`, pDefault),
			Checks:      []check{reject(nowValid, "kid_missing")},
			Note:        "the header must carry alg and kid",
		},
		{
			Name: "reject_header_jku", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","jku":"https://attacker.example/keys"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","jku":"https://attacker.example/keys"}`, pDefault),
			Checks:      []check{reject(nowValid, "header_parameter")},
			Note:        "jku is rejected on sight; keys come from the configured endpoint and nowhere else",
		},
		{
			Name: "reject_header_x5u", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","x5u":"https://attacker.example/cert"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","x5u":"https://attacker.example/cert"}`, pDefault),
			Checks:      []check{reject(nowValid, "header_parameter")},
			Note:        "x5u is rejected on sight",
		},
		{
			Name: "reject_header_jwk", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","jwk":{"kty":"OKP","crv":"Ed25519","x":"AAAA"}}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","jwk":{"kty":"OKP","crv":"Ed25519","x":"AAAA"}}`, pDefault),
			Checks:      []check{reject(nowValid, "header_parameter")},
			Note:        "embedded jwk is rejected on sight (key-injection vector)",
		},
		{
			Name: "reject_header_unknown_param", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","foo":"bar"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx-7f3a2c","foo":"bar"}`, pDefault),
			Checks:      []check{reject(nowValid, "header_parameter")},
			Note:        "unknown header parameters are rejected; only alg/kid/typ are legal",
		},
		{
			Name: "reject_unknown_kid", Key: "sidecar-1",
			HeaderJSON:  `{"alg":"EdDSA","typ":"JWT","kid":"sbx-ffffaa"}`,
			PayloadJSON: pDefault,
			JWT:         signJWT(sidecarPriv, `{"alg":"EdDSA","typ":"JWT","kid":"sbx-ffffaa"}`, pDefault),
			Checks:      []check{reject(nowValid, "unknown_kid")},
			Note:        "kid is well-formed but absent: JWKS fetch misses (verify with a mock fetch); hits the negative cache",
		},
	}

	// Tampered-signature case: flip one bit of the valid_sidecar signature.
	validJWT := cases[0].JWT
	parts := stringsSplit(validJWT, ".")
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		panic(err)
	}
	sigBytes[0] ^= 1
	cases = append(cases, caseVector{
		Name: "reject_signature_tampered", Key: "sidecar-1", HeaderJSON: hSidecar, PayloadJSON: pDefault,
		JWT:    parts[0] + "." + parts[1] + "." + b64(sigBytes),
		Checks: []check{reject(nowValid, "signature")},
		Note:   "one signature byte flipped; everything else is the valid_sidecar token",
	})

	doc := vectorFile{
		Schema:      "opensandbox.resolver-auth.vectors/v1",
		GeneratedBy: "go run ./testdata/gen/main.go (components/egress/pkg/credentialvault)",
		Contract: contract{
			HeaderName:               "OpenSandbox-Egress-Credential",
			Alg:                      "EdDSA",
			Typ:                      "JWT",
			Issuer:                   defaultIssuer,
			DefaultAudience:          defaultAud,
			KidGrammar:               `^[A-Za-z0-9][A-Za-z0-9_-]{0,62}(:v[0-9]{1,8})?$`,
			KidNotes:                 "sidecar: the sandbox's sandboxId (mixed-case ULID-form ids are admitted); fast-sandbox: <pool>:v<version>; the optional :v suffix is digits-only, so a kid never contains '.', '/', or ' ' — path-safe for the JWKS lookup substitution",
			TTLMaxSeconds:            3600,
			LeewaySeconds:            leeway,
			JtiBytes:                 16,
			EnvSidecarPrivateKey:     "OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY",
			EnvFastsandboxKeyDir:     "OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY_DIR",
			EnvFastsandboxSigningKid: "OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KID",
			AudienceDerivation:       "web origin of the resolver endpoint, scheme://host[:port], path prefix excluded; the egress mints it as aud and the verifying resolver configures the identical string as expected_aud",
			SigningKeyFile:           "signing.key",
			KeyFormat:                "PKCS8 PEM (Ed25519); the env value and the signing.key file carry the same PEM form",
			JWKSPathTemplate:         "/jwks/{kid}",
			RejectedHeaderParams:     []string{"jku", "x5u", "jwk"},
			AllowedHeaderParams:      []string{"alg", "kid", "typ"},
			Errors: map[string]string{
				"alg_pinned":       "alg is not EdDSA",
				"typ_invalid":      "typ present but not JWT",
				"kid_grammar":      "kid does not match the platform identifier grammar (rejected locally, no fetch)",
				"kid_missing":      "header carries no kid",
				"header_parameter": "header parameter outside the alg/kid/typ whitelist (jku/x5u/jwk rejected on sight)",
				"signature":        "Ed25519 signature verification failed",
				"issuer":           "iss does not equal the configured issuer",
				"audience":         "aud does not equal the verifying resolver's origin",
				"subject_mismatch": "JWT sub does not equal the resolve document's subject",
				"expired":          "now > exp + leeway",
				"iat_future":       "iat is more than leeway seconds in the future",
				"window":           "exp - iat > 3600",
				"unknown_kid":      "kid well-formed but absent from JWKS",
			},
		},
		Clock: map[string]int64{
			"iat": iat, "exp": exp, "now_valid": nowValid,
			"exp_leeway_boundary": exp + leeway, "exp_rejected": exp + leeway + 1,
			"iat_leeway_boundary_at": 1790000000, "iat_leeway_boundary_iat": 1790000030,
		},
		Keys:  keys,
		Cases: cases,
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		// Fall back to the sibling path when invoked from inside gen/.
		fallback := "../resolver_auth_vectors.json"
		if err2 := os.WriteFile(fallback, data, 0o644); err2 != nil {
			panic(fmt.Errorf("write %s: %v; write %s: %v", *out, err, fallback, err2))
		}
		fmt.Println("wrote " + fallback)
		return
	}
	fmt.Println("wrote " + *out)
}

func stringsSplit(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
			i = start - 1
		}
	}
	out = append(out, s[start:])
	return out
}
