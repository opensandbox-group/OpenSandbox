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

package signature

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testExpiresB36(t *testing.T) string {
	t.Helper()
	return strconv.FormatUint(uint64(time.Now().Add(1*time.Hour).Unix()), 36)
}

func TestParseRouteToken_RightSplit(t *testing.T) {
	e := "1a2b3c"
	id, port, ex, sig, err := ParseRouteToken("alpha-beta-8080-" + e + "-abcdef12k")
	assert.NoError(t, err)
	assert.Equal(t, "alpha-beta", id)
	assert.Equal(t, 8080, port)
	assert.Equal(t, e, ex)
	assert.Equal(t, "abcdef12k", sig)

	id, port, ex, sig, err = ParseRouteToken("sandbox-8080")
	assert.NoError(t, err)
	assert.Equal(t, "sandbox", id)
	assert.Equal(t, 8080, port)
	assert.Equal(t, "", ex)
	assert.Equal(t, "", sig)

	_, _, _, _, err = ParseRouteToken("only-two")
	assert.Error(t, err)
}

func TestParseRouteToken_LeadingZeroPort(t *testing.T) {
	_, _, _, _, err := ParseRouteToken("sb-08080-0-abcdef12k")
	assert.Error(t, err)
}

func TestInnerAndExpectedHex8(t *testing.T) {
	secret := []byte{0x01, 0x02, 0x03}
	exp := "0"
	canonical := CanonicalBytes("sb", 42, exp)
	inner := Inner(secret, canonical)
	assert.Len(t, inner, 4+len(secret)+4+len(canonical))
	h := ExpectedHex8(inner)
	assert.Len(t, h, 8)
	for _, c := range h {
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'), "got %q", h)
	}
}

func TestVerifySignature_OKAnd401(t *testing.T) {
	secret := []byte("test-secret-bytes")
	sb := "my-sandbox"
	port := 9000
	exp := testExpiresB36(t)
	hex8 := ExpectedHex8(Inner(secret, CanonicalBytes(sb, port, exp)))
	sig := hex8 + "z"

	v := &Verifier{Keys: map[string][]byte{"z": secret}}
	assert.NoError(t, v.VerifySignature(sig, sb, port, exp))

	badSig := "00000000" + "z"
	err := v.VerifySignature(badSig, sb, port, exp)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnauthorized))

	bad10 := hex8 + "zz"
	err = v.VerifySignature(bad10, sb, port, exp)
	assert.Error(t, err)
}

func TestVerifySignature_ExpiryComparisonAvoidsUint64Overflow(t *testing.T) {
	secret := []byte("test-secret-bytes")
	sb := "my-sandbox"
	port := 9000
	exp := strconv.FormatUint(^uint64(0), 36)
	hex8 := ExpectedHex8(Inner(secret, CanonicalBytes(sb, port, exp)))
	sig := hex8 + "z"

	v := &Verifier{Keys: map[string][]byte{"z": secret}}
	assert.NoError(t, v.VerifySignature(sig, sb, port, exp))
}

func TestHTTPStatusForErr(t *testing.T) {
	assert.Equal(t, http.StatusUnauthorized, HTTPStatusForErr(fmt.Errorf("%w: x", ErrUnauthorized)))
	assert.Equal(t, http.StatusUnauthorized, HTTPStatusForErr(ErrAccessExpired))
	assert.Equal(t, http.StatusBadRequest, HTTPStatusForErr(fmt.Errorf("bad format")))
	assert.Equal(t, 0, HTTPStatusForErr(nil))
	assert.Equal(t, http.StatusUnauthorized, HTTPStatusForIngressErr(ErrSecureHeaderMismatch))
	assert.Equal(t, http.StatusUnauthorized, HTTPStatusForIngressErr(ErrSignatureRequired))
	assert.Equal(t, http.StatusServiceUnavailable, HTTPStatusForIngressErr(ErrVerifierNotConfigured))
}

func TestCheckIngressSecureAccess(t *testing.T) {
	secret := []byte("k")
	v := &Verifier{Keys: map[string][]byte{"z": secret}}
	sb, port := "s", 1
	e := testExpiresB36(t)
	sig := ExpectedHex8(Inner(secret, CanonicalBytes(sb, port, e))) + "z"

	assert.NoError(t, CheckIngressSecureAccess(IngressAccessInput{
		Secure:    false,
		Signature: sig,
		SandboxID: sb,
		Port:      port,
		Verifier:  v,
	}))

	assert.NoError(t, CheckIngressSecureAccess(IngressAccessInput{
		Secure:                    true,
		ExpectedAccessToken:       "tok",
		SecureAccessHeaderPresent: true,
		RequestedAccessToken:      "tok",
		Signature:                 "bad",
		SandboxID:                 sb,
		Port:                      port,
	}))

	assert.ErrorIs(t, CheckIngressSecureAccess(IngressAccessInput{
		Secure:                    true,
		ExpectedAccessToken:       "tok",
		SecureAccessHeaderPresent: true,
		RequestedAccessToken:      "nope",
		Signature:                 sig,
		SandboxID:                 sb,
		Port:                      port,
		Verifier:                  v,
	}), ErrSecureHeaderMismatch)

	assert.ErrorIs(t, CheckIngressSecureAccess(IngressAccessInput{
		Secure:              true,
		ExpectedAccessToken: "tok",
		Signature:           sig,
		ExpiresB36:          e,
		SandboxID:           sb,
		Port:                port,
		Verifier:            nil,
	}), ErrVerifierNotConfigured)

	assert.NoError(t, CheckIngressSecureAccess(IngressAccessInput{
		Secure:              true,
		ExpectedAccessToken: "tok",
		Signature:           sig,
		ExpiresB36:          e,
		SandboxID:           sb,
		Port:                port,
		Verifier:            v,
	}))

	assert.ErrorIs(t, CheckIngressSecureAccess(IngressAccessInput{
		Secure:              true,
		ExpectedAccessToken: "tok",
		Signature:           "",
		ExpiresB36:          "",
		SandboxID:           sb,
		Port:                port,
	}), ErrSignatureRequired)
}

func TestParseKeys(t *testing.T) {
	raw := []byte{0xab, 0xcd}
	keys, err := ParseKeys("k=" + base64.StdEncoding.EncodeToString(raw))
	assert.NoError(t, err)
	assert.Equal(t, raw, keys["k"])

	_, err = ParseKeys("")
	assert.Error(t, err)

	_, err = ParseKeys("K=" + base64.StdEncoding.EncodeToString(raw))
	assert.Error(t, err)
}
