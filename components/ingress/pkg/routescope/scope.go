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

// Package routescope verifies the tenant-scoped routing handle shared by the
// fleets server adapter and ingress gateway.
package routescope

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	prefix       = "f1"
	canonicalTag = "opensandbox-fleets-route-v1"
	macBytes     = 16
)

var (
	ErrInvalidScope = errors.New("route scope: invalid")
	ErrUnauthorized = errors.New("route scope: unauthorized")
)

type Scope struct {
	Namespace string
	SandboxID string
	Port      int
}

type Verifier struct {
	Keys map[string][]byte
}

func IsToken(value string) bool {
	return strings.HasPrefix(value, prefix+".")
}

func (v *Verifier) Verify(token string) (Scope, error) {
	if v == nil || len(v.Keys) == 0 {
		return Scope{}, fmt.Errorf("%w: verifier is not configured", ErrUnauthorized)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 6 || parts[0] != prefix {
		return Scope{}, fmt.Errorf("%w: malformed token", ErrInvalidScope)
	}

	namespace, err := decodeIdentity("namespace", parts[1])
	if err != nil {
		return Scope{}, err
	}
	sandboxID, err := decodeIdentity("sandbox_id", parts[2])
	if err != nil {
		return Scope{}, err
	}
	port, err := parsePort(parts[3])
	if err != nil {
		return Scope{}, err
	}
	keyID := parts[4]
	if !validKeyID(keyID) {
		return Scope{}, fmt.Errorf("%w: invalid key_id", ErrInvalidScope)
	}
	wantMAC, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil || len(wantMAC) != macBytes || base64.RawURLEncoding.EncodeToString(wantMAC) != parts[5] {
		return Scope{}, fmt.Errorf("%w: invalid mac", ErrInvalidScope)
	}
	key, ok := v.Keys[keyID]
	if !ok || len(key) == 0 {
		return Scope{}, fmt.Errorf("%w: unknown key_id", ErrUnauthorized)
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(canonicalBytes(namespace, sandboxID, port))
	gotMAC := h.Sum(nil)[:macBytes]
	if subtle.ConstantTimeCompare(gotMAC, wantMAC) != 1 {
		return Scope{}, fmt.Errorf("%w: mac mismatch", ErrUnauthorized)
	}
	return Scope{Namespace: namespace, SandboxID: sandboxID, Port: port}, nil
}

func canonicalBytes(namespace, sandboxID string, port int) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%d\n", canonicalTag, namespace, sandboxID, port))
}

func decodeIdentity(name, encoded string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return "", fmt.Errorf("%w: invalid %s", ErrInvalidScope, name)
	}
	value := string(decoded)
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: invalid %s", ErrInvalidScope, name)
		}
	}
	return value, nil
}

func parsePort(value string) (int, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, fmt.Errorf("%w: invalid port", ErrInvalidScope)
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, fmt.Errorf("%w: invalid port", ErrInvalidScope)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%w: invalid port", ErrInvalidScope)
	}
	return port, nil
}

func validKeyID(value string) bool {
	if len(value) != 1 {
		return false
	}
	c := value[0]
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')
}
