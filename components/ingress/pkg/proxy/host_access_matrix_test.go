// Copyright 2026 Alibaba Group Holding Ltd.
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

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
	"github.com/stretchr/testify/assert"
)

// Test sandboxes: distinct ids so GetEndpoint can return with/without secure access.
const (
	testIDNoAccess = "nosec"
	testIDSecure   = "sec"
	testToken      = "annot-token"
)

type staticEndpointProvider struct {
	byID map[string]sandbox.EndpointInfo
}

func (p staticEndpointProvider) GetEndpoint(id string) (*sandbox.EndpointInfo, error) {
	if e, ok := p.byID[id]; ok {
		cp := e
		return &cp, nil
	}
	return &sandbox.EndpointInfo{Endpoint: "127.0.0.1"}, nil
}

func (staticEndpointProvider) Start(context.Context) error { return nil }

func newTestProvider() staticEndpointProvider {
	return staticEndpointProvider{byID: map[string]sandbox.EndpointInfo{
		testIDNoAccess: {Endpoint: "10.0.0.1", SecureAccessToken: ""},
		testIDSecure:   {Endpoint: "10.0.0.2", SecureAccessToken: testToken},
	}}
}

func makeRouteSig(t *testing.T, secret []byte, key, sandboxID string, port int, expiresB36 string) string {
	t.Helper()
	h8 := signature.ExpectedHex8(signature.Inner(secret, signature.CanonicalBytes(sandboxID, port, expiresB36)))
	return h8 + key
}

// --- No access verification required (GetEndpoint: empty access token) ---

func TestMatrix_NoAccessVerification_Header(t *testing.T) {
	prov := newTestProvider()
	secret := []byte{0x5e, 0x5e, 0x5e, 0x5e}
	goodSig := makeRouteSig(t, secret, "a", testIDNoAccess, 8080, "0")

	p := &Proxy{mode: ModeHeader, sandboxProvider: prov, secure: &signature.Verifier{Keys: map[string][]byte{"a": secret}}}

	t.Run("legacy two segment no header", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDNoAccess + "-8080.sandbox.gw"
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDNoAccess, h.ingressKey)
		assert.Equal(t, 8080, h.port)
	})

	t.Run("legacy two segment with OpenSandbox-Secure-Access header is ignored for routing decision", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDNoAccess + "-8080.sandbox.gw"
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "irrelevant-value")
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDNoAccess, h.ingressKey)
		assert.Equal(t, 8080, h.port)
	})

	t.Run("legacy with header field present but empty value (no access required still ok)", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDNoAccess + "-8080.sandbox.gw"
		r.Header[signature.OpenSandboxSecureAccessCanonical] = []string{""}
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDNoAccess, h.ingressKey)
	})

	t.Run("signed shape four segment without header is accepted and signature not verified", func(t *testing.T) {
		// 4+ logical segments: id-port-exp-sig; signature material is not checked when !need.
		label := fmt.Sprintf("%s-8080-0-%s", testIDNoAccess, goodSig)
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = label + ".gw"
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDNoAccess, h.ingressKey)
		assert.Equal(t, 8080, h.port)
	})
}

func TestMatrix_NoAccessVerification_URI(t *testing.T) {
	prov := newTestProvider()
	secret := []byte{0x1a, 0x1a, 0x1a, 0x1a}
	exp := strconv.FormatUint(uint64(time.Now().Add(2*time.Hour).Unix()), 36)
	goodSig := makeRouteSig(t, secret, "a", testIDNoAccess, 3000, exp)
	p := &Proxy{mode: ModeURI, sandboxProvider: prov, secure: &signature.Verifier{Keys: map[string][]byte{"a": secret}}}

	t.Run("legacy path no extra header", func(t *testing.T) {
		path := "/" + testIDNoAccess + "/3000/v1/status"
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDNoAccess, h.ingressKey)
		assert.Equal(t, 3000, h.port)
		assert.Equal(t, "/v1/status", h.requestURI)
	})

	t.Run("OSEP-shaped path re-parsed with legacy (strip must not apply)", func(t *testing.T) {
		// Syntactically valid signed prefix; for unsecured sandbox full path must be legacy-interpreted.
		path := fmt.Sprintf("/%s/3000/%s/%s/extra/segment", testIDNoAccess, exp, goodSig)
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDNoAccess, h.ingressKey)
		assert.Equal(t, 3000, h.port)
		// second segment = port, remainder of path = upstream (includes former exp+sig+rest)
		assert.Equal(t, "/"+exp+"/"+goodSig+"/extra/segment", h.requestURI)
	})

	t.Run("OSEP shape plus OpenSandbox-Secure-Access header", func(t *testing.T) {
		path := fmt.Sprintf("/%s/3000/%s/%s/api", testIDNoAccess, exp, goodSig)
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "noise")
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, "/"+exp+"/"+goodSig+"/api", h.requestURI)
	})
}

// --- Access required (GetEndpoint: non-empty SecureAccessToken) ---

func TestMatrix_AccessRequired_Header(t *testing.T) {
	prov := newTestProvider()
	secret := []byte{0x70, 0x71, 0x72, 0x73}
	exp := strconv.FormatUint(uint64(time.Now().Add(2*time.Hour).Unix()), 36)
	goodSig := makeRouteSig(t, secret, "k", testIDSecure, 7777, exp)
	badHMAC := "aabbccdd" + "k" // 9 hex chars, valid format but wrong digest

	ver := &signature.Verifier{Keys: map[string][]byte{"k": secret}}
	p := &Proxy{mode: ModeHeader, sandboxProvider: prov, secure: ver}

	t.Run("legacy two segment no header no signature -> 401", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDSecure + "-7777.gw"
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrSignatureRequired)
	})

	t.Run("legacy two segment with correct OpenSandbox-Secure-Access only", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDSecure + "-7777.gw"
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, testToken)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDSecure, h.ingressKey)
		assert.Equal(t, 7777, h.port)
	})

	t.Run("legacy two segment wrong OpenSandbox-Secure-Access", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDSecure + "-7777.gw"
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "bad-token")
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrSecureHeaderMismatch)
	})

	t.Run("legacy header field present with empty value counts as present (401 if token required)", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = testIDSecure + "-7777.gw"
		r.Header[signature.OpenSandboxSecureAccessCanonical] = []string{""}
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrSecureHeaderMismatch)
	})

	t.Run("signed host valid HMAC no header", func(t *testing.T) {
		label := fmt.Sprintf("%s-7777-%s-%s", testIDSecure, exp, goodSig)
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = label + ".gw"
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDSecure, h.ingressKey)
		assert.Equal(t, 7777, h.port)
	})

	t.Run("signed host valid HMAC but wrong OpenSandbox-Secure-Access is fail fast (no fallthrough)", func(t *testing.T) {
		label := fmt.Sprintf("%s-7777-0-%s", testIDSecure, goodSig)
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = label + ".gw"
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "nope")
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrSecureHeaderMismatch)
	})

	t.Run("signed host bad HMAC no header", func(t *testing.T) {
		label := fmt.Sprintf("%s-7777-%s-%s", testIDSecure, exp, badHMAC)
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = label + ".gw"
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrUnauthorized)
	})

	t.Run("signed host bad HMAC with correct OpenSandbox-Secure-Access passes without verifying HMAC", func(t *testing.T) {
		label := fmt.Sprintf("%s-7777-%s-%s", testIDSecure, exp, badHMAC)
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = label + ".gw"
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, testToken)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDSecure, h.ingressKey)
	})

	t.Run("signed host valid HMAC with verifier nil -> 503", func(t *testing.T) {
		label := fmt.Sprintf("%s-7777-%s-%s", testIDSecure, exp, goodSig)
		r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		r.Host = label + ".gw"
		pNoV := &Proxy{mode: ModeHeader, sandboxProvider: prov, secure: nil}
		_, code, err := pNoV.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.ErrorIs(t, err, signature.ErrVerifierNotConfigured)
	})
}

func TestMatrix_AccessRequired_URI(t *testing.T) {
	prov := newTestProvider()
	secret := []byte{0x9a, 0x9b, 0x9c, 0x9d}
	exp := strconv.FormatUint(uint64(time.Now().Add(2*time.Hour).Unix()), 36)
	goodSig := makeRouteSig(t, secret, "a", testIDSecure, 5000, exp)
	badHMAC := "badbadba" + "a"

	ver := &signature.Verifier{Keys: map[string][]byte{"a": secret}}
	p := &Proxy{mode: ModeURI, sandboxProvider: prov, secure: ver}

	orep := func() string { return fmt.Sprintf("/%s/5000/%s/%s", testIDSecure, exp, goodSig) }

	t.Run("legacy two segment no credentials -> 401", func(t *testing.T) {
		path := "/" + testIDSecure + "/5000/only/legacy"
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrSignatureRequired)
	})

	t.Run("legacy with correct OpenSandbox-Secure-Access only", func(t *testing.T) {
		path := "/" + testIDSecure + "/5000/only/legacy"
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, testToken)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDSecure, h.ingressKey)
		assert.Equal(t, 5000, h.port)
		assert.Equal(t, "/only/legacy", h.requestURI)
	})

	t.Run("signed URI valid HMAC no header", func(t *testing.T) {
		path := orep() + "/p/q"
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, "/p/q", h.requestURI)
		assert.Equal(t, testIDSecure, h.ingressKey)
		assert.Equal(t, 5000, h.port)
	})

	t.Run("signed URI valid HMAC wrong OpenSandbox-Secure-Access fail fast", func(t *testing.T) {
		path := orep() + "/x"
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, "wrong")
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrSecureHeaderMismatch)
	})

	t.Run("signed URI bad HMAC no header", func(t *testing.T) {
		path := fmt.Sprintf("/%s/5000/%s/%s/ok", testIDSecure, exp, badHMAC)
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		_, code, err := p.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.ErrorIs(t, err, signature.ErrUnauthorized)
	})

	t.Run("signed URI bad HMAC with good header (header path wins)", func(t *testing.T) {
		path := fmt.Sprintf("/%s/5000/%s/%s/ok", testIDSecure, exp, badHMAC)
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		r.Header.Set(signature.OpenSandboxSecureAccessCanonical, testToken)
		h, code, err := p.getSandboxHostDefinition(r)
		assert.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, testIDSecure, h.ingressKey)
	})

	t.Run("signed URI valid HMAC with verifier nil -> 503", func(t *testing.T) {
		path := orep() + "/m"
		r := httptest.NewRequest(http.MethodGet, "http://i"+path, nil)
		pNoV := &Proxy{mode: ModeURI, sandboxProvider: prov, secure: nil}
		_, code, err := pNoV.getSandboxHostDefinition(r)
		assert.Error(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.ErrorIs(t, err, signature.ErrVerifierNotConfigured)
	})
}
