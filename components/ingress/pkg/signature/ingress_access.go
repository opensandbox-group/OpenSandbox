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
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

const (
	// OpenSandboxSecureAccessHeader is the OSEP-0011 header field name; use
	// OpenSandboxSecureAccessCanonical when looking up the HTTP header map.
	OpenSandboxSecureAccessHeader = "OpenSandbox-Secure-Access"
)

var (
	OpenSandboxSecureAccessCanonical = http.CanonicalHeaderKey(OpenSandboxSecureAccessHeader)

	ErrSecureHeaderMismatch  = errors.New("signature: secure access header mismatch")
	ErrSignatureRequired     = errors.New("signature: signature required for this sandbox")
	ErrVerifierNotConfigured = errors.New("signature: ingress verifier not configured")
)

// SecureAccessHeaderInfo reports field presence: present iff the header field
// is sent (values may be empty) and the trimmed first value for comparison.
func SecureAccessHeaderInfo(r *http.Request) (present bool, value string) {
	if r == nil {
		return false, ""
	}
	vs := r.Header.Values(OpenSandboxSecureAccessCanonical)
	if len(vs) == 0 {
		return false, ""
	}
	return true, strings.TrimSpace(vs[0])
}

// SecureAccessHeaderFromRequest returns the trimmed first field value, or "" if absent.
func SecureAccessHeaderFromRequest(r *http.Request) string {
	_, v := SecureAccessHeaderInfo(r)
	return v
}

func secureAccessTokenEqualConstantTime(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type IngressAccessInput struct {
	Secure                    bool
	ExpectedAccessToken       string
	SecureAccessHeaderPresent bool
	RequestedAccessToken      string
	ExpiresB36                string
	Signature                 string
	SandboxID                 string
	Port                      int
	Verifier                  *Verifier
}

// CheckIngressSecureAccess applies OSEP-0011: if OpenSandbox-Secure-Access is
// present, compare to annotation token (constant-time) and 401 on mismatch
// (no route-signature fallback). If absent, verify route signature+expiry.
func CheckIngressSecureAccess(in IngressAccessInput) error {
	if !in.Secure {
		return nil
	}

	at := strings.TrimSpace(in.ExpectedAccessToken)
	if in.SecureAccessHeaderPresent {
		if secureAccessTokenEqualConstantTime(in.RequestedAccessToken, at) {
			return nil
		}
		return ErrSecureHeaderMismatch
	}
	if in.Signature != "" && strings.TrimSpace(in.ExpiresB36) != "" {
		if in.Verifier == nil || !in.Verifier.Enabled() {
			return ErrVerifierNotConfigured
		}
		return in.Verifier.VerifySignature(in.Signature, in.SandboxID, in.Port, in.ExpiresB36)
	}
	return ErrSignatureRequired
}

func HTTPStatusForIngressErr(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrUnauthorized) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrAccessExpired) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrSecureHeaderMismatch) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrSignatureRequired) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrVerifierNotConfigured) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}
