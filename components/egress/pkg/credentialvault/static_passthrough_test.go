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
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
	"github.com/alibaba/opensandbox/egress/pkg/mitmpassthrough"
	"github.com/stretchr/testify/require"
)

func TestStaticPassthroughCandidates(t *testing.T) {
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"},{"action":"allow","target":"example.com"}]}`)
	for _, pair := range [][2]string{{"code.example.com", "code.example.com"}, {"*.example.com", "code.example.com"}, {"code.example.com", "*.example.com"}, {"*.sub.example.com", "*.example.com"}} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			cfg, err := mitmpassthrough.Parse(`["`+pair[0]+`"]`, true)
			require.NoError(t, err)
			store := NewLiveStore(nil, nil, cfg)
			req := testCredentialVaultRequest()
			req.Bindings[0].Match.Hosts = []string{pair[1]}
			_, err = store.PrepareCreate(req, pol)
			require.Error(t, err)
			require.Contains(t, err.Error(), fmt.Sprintf("binding %q host %q", req.Bindings[0].Name, pair[1]))
			require.Contains(t, err.Error(), "credential TLS binding overlaps static passthrough")
			require.NotContains(t, err.Error(), "secret-token")
			if pair[0] != pair[1] {
				require.NotContains(t, err.Error(), pair[0])
			}
			require.Zero(t, store.revision)
			req.Bindings[0].Match.Schemes = []string{"http"}
			candidate, err := store.PrepareCreate(req, pol)
			require.NoError(t, err)
			_, err = candidate.ActiveSnapshot(context.Background())
			require.NoError(t, err)
			_, err = store.CommitCandidate(candidate)
			require.NoError(t, err)
			tag := store.activeTag
			for _, schemes := range [][]string{nil, {"https"}, {"http", "https"}} {
				binding := req.Bindings[0]
				binding.Match.Schemes = schemes
				_, err = store.PreparePatch(MutationRequest{Bindings: &BindingMutationSet{Replace: []Binding{binding}}}, pol)
				require.Error(t, err)
				require.Contains(t, err.Error(), fmt.Sprintf("binding %q host %q", binding.Name, pair[1]))
				require.Equal(t, int64(1), store.revision)
				require.Equal(t, tag, store.activeTag)
			}
			// A malformed restored state must also fail policy freeze before rendering.
			for key, binding := range store.bindings {
				binding.Match.Schemes = []string{"https"}
				store.bindings[key] = binding
			}
			_, err = store.FreezeForPolicy(pol)
			require.ErrorIs(t, err, ErrInvalidCandidate)
			require.Contains(t, err.Error(), fmt.Sprintf("binding %q host %q", req.Bindings[0].Name, pair[1]))
			require.Equal(t, tag, store.activeTag)
		})
	}
}

func TestCredentialTLSSelectorErrorPreservesContextAndCause(t *testing.T) {
	store := NewLiveStore(nil, nil, mitmpassthrough.Config{})
	binding := testCredentialVaultRequest().Bindings[0]
	binding.Match.Schemes = []string{"https"}
	binding.Match.Hosts = []string{"invalid..example.com"}
	_, parseErr := hostselector.ParseCanonical(binding.Match.Hosts[0])
	require.Error(t, parseErr)

	// Restored bindings are revalidated without normalization during policy freeze.
	err := store.validateBindingPolicy(binding, nil)
	require.ErrorIs(t, err, parseErr)
	require.EqualError(t, err, fmt.Sprintf("binding %q host %q: invalid credential TLS selector: %v", binding.Name, binding.Match.Hosts[0], parseErr))
	require.NotContains(t, err.Error(), "secret-token")
}

func TestStaticPassthroughApexAndEmptyWildcard(t *testing.T) {
	cfg, err := mitmpassthrough.Parse(`["*.example.com"]`, true)
	require.NoError(t, err)
	store := NewLiveStore(nil, nil, cfg)
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	req := testCredentialVaultRequest()
	req.Bindings[0].Match.Hosts = []string{"example.com"}
	_, err = store.PrepareCreate(req, pol)
	require.NoError(t, err)
}

func TestStaticPassthroughEmptyWildcardLanguage(t *testing.T) {
	cfg, err := mitmpassthrough.Parse(`["*.example.com"]`, true)
	require.NoError(t, err)
	store := NewLiveStore(nil, nil, cfg)
	// A 252-byte suffix leaves no room for even a one-character subdomain.
	suffix := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 60)
	require.Len(t, suffix, 252)
	pol := testCredentialPolicy(t, `{"defaultAction":"allow","egress":[]}`)
	// Use the already-supported global policy allow grammar.
	pol.Egress = nil
	req := testCredentialVaultRequest()
	req.Bindings[0].Match.Hosts = []string{"*." + suffix}
	candidate, err := store.PrepareCreate(req, pol)
	require.NoError(t, err)
	snapshot, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	encoded, err := MarshalDecisionSnapshot(snapshot, 1)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"tlsBindingHostSelectors":[]`)
}
