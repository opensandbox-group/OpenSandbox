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
	"fmt"
	"sync"
	"time"
)

// resolverCredentialManager caches one credential per (subject, resolver
// origin) and re-mints at 80% of the TTL, so no request ever meets an
// expired credential and the superseded one stays valid at resolvers until
// its own exp (the 12-minute overlap bounds replay; OSEP-0025 Rotation).
// Minting is lazy on first resolve; a process restart starts empty and mints
// fresh jtis immediately. The manager implements CredentialProvider.
type resolverCredentialManager struct {
	mint func(subject, audience string, now time.Time) (string, error)
	now  func() time.Time

	mu    sync.Mutex
	creds map[credentialCacheKey]cachedCredential
}

// credentialRefreshFraction is the re-mint point within the TTL window.
const credentialRefreshFraction = 0.8

type credentialCacheKey struct {
	subject  string
	audience string
}

type cachedCredential struct {
	jwt      string
	mintedAt time.Time
}

var _ CredentialProvider = (*resolverCredentialManager)(nil)

// newResolverCredentialManager binds the manager to a loaded signer.
func newResolverCredentialManager(signer *ResolverCredentialSigner) *resolverCredentialManager {
	return &resolverCredentialManager{
		mint:  signer.Mint,
		now:   time.Now,
		creds: make(map[credentialCacheKey]cachedCredential),
	}
}

// Credential returns the cached JWT for (subject, audience), re-minting once
// the cached one passes 80% of its TTL. Minting happens under the cache
// lock: parallel resolves for the same key see one mint, and the ~µs Ed25519
// sign cost never blocks unrelated keys for long.
func (m *resolverCredentialManager) Credential(subject, audience string) (string, error) {
	if subject == "" || audience == "" {
		return "", fmt.Errorf("resolver credential: subject and audience must be non-empty")
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	key := credentialCacheKey{subject: subject, audience: audience}
	if cached, ok := m.creds[key]; ok {
		if now.Sub(cached.mintedAt) < time.Duration(credentialRefreshFraction*float64(CredentialTTLMax)) {
			return cached.jwt, nil
		}
	}
	jwt, err := m.mint(subject, audience, now)
	if err != nil {
		return "", err
	}
	m.creds[key] = cachedCredential{jwt: jwt, mintedAt: now}
	return jwt, nil
}
