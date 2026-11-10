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
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// countingManager wires the manager's mint to a counter, mirroring
// newResolverCredentialManager but with an injectable clock.
func countingManager(now *time.Time, mints *atomic.Int64) *resolverCredentialManager {
	_, key, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		panic(err)
	}
	signer := &ResolverCredentialSigner{key: key, kid: "pool-a:v2"}
	manager := &resolverCredentialManager{
		mint:  signer.Mint,
		now:   func() time.Time { return *now },
		creds: make(map[credentialCacheKey]cachedCredential),
	}
	wrapped := manager.mint
	manager.mint = func(subject, audience string, at time.Time) (string, error) {
		mints.Add(1)
		return wrapped(subject, audience, at)
	}
	return manager
}

func claimOf(t *testing.T, jwt string) map[string]any {
	t.Helper()
	segment := jwtCompactPart(t, jwt, 1)
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	return claims
}

func jtiOf(t *testing.T, jwt string) string {
	t.Helper()
	return claimOf(t, jwt)["jti"].(string)
}

func jwtCompactPart(t *testing.T, jwt string, index int) string {
	t.Helper()
	var start, end int
	dots := 0
	for i := 0; i <= len(jwt); i++ {
		if i == len(jwt) || jwt[i] == '.' {
			if dots == index {
				end = i
				break
			}
			start = i + 1
			dots++
		}
	}
	return jwt[start:end]
}

// TestResolverCredentialManagerCacheAndRefresh: reuse within the 80%
// window, re-mint past it with a fresh jti, distinct audiences mint
// distinct aud claims.
func TestResolverCredentialManagerCacheAndRefresh(t *testing.T) {
	now := time.Unix(1790000000, 0)
	var mints atomic.Int64
	manager := countingManager(&now, &mints)

	first, err := manager.Credential("sbx-7f3a2c", "credential-resolver@resolver.internal")
	require.NoError(t, err)
	require.Equal(t, int64(1), mints.Load(), "first resolve mints lazily")

	// Within the 80% window (48 minutes): byte-identical reuse.
	now = now.Add(47 * time.Minute)
	again, err := manager.Credential("sbx-7f3a2c", "credential-resolver@resolver.internal")
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, int64(1), mints.Load(), "80%% window must reuse the cached JWT")

	// Past the 80% window: fresh jti, new credential.
	now = now.Add(2 * time.Minute)
	refreshed, err := manager.Credential("sbx-7f3a2c", "credential-resolver@resolver.internal")
	require.NoError(t, err)
	require.NotEqual(t, first, refreshed)
	require.NotEqual(t, jtiOf(t, first), jtiOf(t, refreshed), "re-mint carries a fresh jti")
	require.Equal(t, int64(2), mints.Load())

	// A different audience is a different cache entry with its own aud.
	other, err := manager.Credential("sbx-7f3a2c", "other-resolver@elsewhere.internal")
	require.NoError(t, err)
	require.Equal(t, int64(3), mints.Load())
	require.NotEqual(t, refreshed, other)
	require.Equal(t, "credential-resolver@resolver.internal", claimOf(t, refreshed)["aud"])
	require.Equal(t, "other-resolver@elsewhere.internal", claimOf(t, other)["aud"])

	// A different subject is a different cache entry.
	third, err := manager.Credential("sbx-othe1", "credential-resolver@resolver.internal")
	require.NoError(t, err)
	require.Equal(t, int64(4), mints.Load())
	require.Equal(t, "sbx-othe1", claimOf(t, third)["sub"])
}

// TestResolverCredentialManagerRestartMintsFresh: a new manager (process
// restart) starts with an empty cache and mints a new jti immediately.
func TestResolverCredentialManagerRestartMintsFresh(t *testing.T) {
	now := time.Unix(1790000000, 0)
	var mints1 atomic.Int64
	first := countingManager(&now, &mints1)
	jwt1, err := first.Credential("sbx-7f3a2c", "credential-resolver@resolver.internal")
	require.NoError(t, err)

	var mints2 atomic.Int64
	restarted := countingManager(&now, &mints2)
	jwt2, err := restarted.Credential("sbx-7f3a2c", "credential-resolver@resolver.internal")
	require.NoError(t, err)
	require.NotEqual(t, jtiOf(t, jwt1), jtiOf(t, jwt2), "restart mints a fresh jti immediately")
}

// TestResolverCredentialManagerConcurrentSingleMint: parallel resolves for
// one key observe exactly one mint and all receive the same JWT.
func TestResolverCredentialManagerConcurrentSingleMint(t *testing.T) {
	now := time.Unix(1790000000, 0)
	var mints atomic.Int64
	manager := countingManager(&now, &mints)

	const workers = 64
	var wg sync.WaitGroup
	jwts := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			jwt, err := manager.Credential("sbx-7f3a2c", "credential-resolver@resolver.internal")
			if err != nil {
				t.Error(err)
				return
			}
			jwts[slot] = jwt
		}(i)
	}
	wg.Wait()
	require.Equal(t, int64(1), mints.Load(), "parallel resolves must not duplicate mints")
	for _, jwt := range jwts {
		require.Equal(t, jwts[0], jwt)
	}
}

func TestResolverCredentialManagerRejectsEmptyInputs(t *testing.T) {
	now := time.Unix(1790000000, 0)
	var mints atomic.Int64
	manager := countingManager(&now, &mints)
	_, err := manager.Credential("", "aud")
	require.ErrorContains(t, err, "non-empty")
	_, err = manager.Credential("sub", "")
	require.ErrorContains(t, err, "non-empty")
	require.Equal(t, int64(0), mints.Load())
}

// TestResolverCredentialManagerBounded: the process-wide cache is capped —
// past the limit the oldest half is evicted and evicted pairs re-mint on
// next use.
func TestResolverCredentialManagerBounded(t *testing.T) {
	now := time.Unix(1790000000, 0)
	var mints atomic.Int64
	manager := countingManager(&now, &mints)
	manager.maxEntries = 8

	for i := 0; i < 20; i++ {
		_, err := manager.Credential(fmt.Sprintf("sbx-churn%02d", i), "credential-resolver@resolver.internal")
		require.NoError(t, err)
	}
	require.Len(t, manager.creds, 8, "cache must stay bounded under churn")
	require.LessOrEqual(t, mints.Load(), int64(20+8), "evictions cause re-mints, not growth")
}
