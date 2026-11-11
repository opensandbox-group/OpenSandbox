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

// Package binding holds the RuntimeBinding applied by POST /internal/init: the
// sandbox-scoped parameters (sandbox ID, API token hash, user envs, telemetry
// attributes) that are only known when a sandbox is created, resumed, or
// re-assigned from a resource pool. The binding is swapped atomically so
// every reader (auth middleware, env resolution, telemetry attribution)
// observes one consistent generation.
package binding

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
)

// AccessTokenHashPrefix marks a SHA-256 hex digest in runtime-init payloads.
const AccessTokenHashPrefix = "sha256:"

// RuntimeBinding is the sandbox-scoped runtime state applied by
// /internal/init. AccessTokenHash stores SHA-256(raw token); the raw token
// never reaches execd.
type RuntimeBinding struct {
	SandboxID  string
	Generation uint64

	AccessTokenHash [32]byte
	HasAccessToken  bool

	// Envs are the sandbox-level user envs with execd config/credential
	// names already filtered out by the caller.
	Envs map[string]string

	// TelemetryAttrs are extra observability attributes (tenant, plan, ...)
	// attached to metrics alongside sandbox_id/generation.
	TelemetryAttrs map[string]string
}

var current atomic.Pointer[RuntimeBinding]

// Current returns the active binding, or nil before the first /internal/init.
func Current() *RuntimeBinding {
	return current.Load()
}

// Apply atomically replaces the active binding and returns the previous one
// (nil on first apply).
func Apply(b *RuntimeBinding) *RuntimeBinding {
	return current.Swap(b)
}

// ParseAccessTokenHash decodes a "sha256:<hex>" digest into 32 bytes.
func ParseAccessTokenHash(raw string) ([32]byte, error) {
	var digest [32]byte
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, AccessTokenHashPrefix) {
		return digest, fmt.Errorf("access token hash must use %s<hex> format", AccessTokenHashPrefix)
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(trimmed, AccessTokenHashPrefix))
	if err != nil {
		return digest, fmt.Errorf("access token hash is not valid hex: %w", err)
	}
	if len(decoded) != len(digest) {
		return digest, fmt.Errorf("access token hash must be %d hex bytes, got %d", len(digest), len(decoded))
	}
	copy(digest[:], decoded)
	return digest, nil
}

// VerifyAccessToken reports whether the presented raw token matches the
// binding's token hash. Comparison is constant time.
func (b *RuntimeBinding) VerifyAccessToken(rawToken string) bool {
	if b == nil || !b.HasAccessToken {
		return false
	}
	digest := sha256.Sum256([]byte(rawToken))
	return hmac.Equal(digest[:], b.AccessTokenHash[:])
}

// HashAccessToken hashes a raw token into the "sha256:<hex>" form expected by
// /init callers (control-plane side helper and tests).
func HashAccessToken(rawToken string) string {
	digest := sha256.Sum256([]byte(rawToken))
	return AccessTokenHashPrefix + hex.EncodeToString(digest[:])
}
