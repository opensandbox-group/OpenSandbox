// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmpassthrough"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// Exercise the real mutation owner rather than only Store candidate validation.
// Rejection must precede rendering/publication and all external effects.
func TestStaticPassthroughVaultMutationRejectsBeforeEffects(t *testing.T) {
	for _, overlap := range []struct{ name, static, host string }{
		{"exact", "api.example.com", "api.example.com"},
		{"static-wildcard", "*.example.com", "deep.api.example.com"},
		{"binding-wildcard", "api.example.com", "*.example.com"},
		{"nested-wildcard", "*.api.example.com", "*.example.com"},
	} {
		for _, operation := range []string{"create", "patch"} {
			t.Run(overlap.name+"/"+operation, func(t *testing.T) {
				s, m, session, nft := revisionPolicyFixture(t)
				encoded, err := json.Marshal([]string{overlap.static})
				require.NoError(t, err)
				config, err := mitmpassthrough.Parse(string(encoded), true)
				require.NoError(t, err)
				s.credentialVault = credentialvault.NewLiveStore(s.mitmGate, func() bool { return true }, config)
				policyJSON, err := json.Marshal(map[string]any{"defaultAction": "deny", "egress": []map[string]string{{"action": "allow", "target": overlap.host}}})
				require.NoError(t, err)
				pol := testCredentialVaultPolicy(t, string(policyJSON))
				setMutationFixturePolicy(t, s, pol)
				file := &faultPolicyStore{state: policy.FileCommitted}
				s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
				request := credentialvault.CreateRequest{
					Credentials: []credentialvault.Credential{{Name: "token", Source: []byte(`{"type":"inline","value":"private-static-test"}`)}},
					Bindings:    []credentialvault.Binding{{Name: "api", Match: credentialvault.Match{Hosts: []string{overlap.host}}, Auth: credentialvault.Auth{Type: "apiKey", Name: "X-API-Key", Credential: "token"}}},
				}
				if operation == "patch" {
					seed := request
					seed.Bindings = nil
					_, err = s.credentialVault.Create(seed, pol)
					require.NoError(t, err)
				}
				before, tag, _, beforeErr := s.credentialVault.ActiveSnapshotIfChanged(context.Background(), "")
				base := s.revisionRecovery.current
				var prepareErr error
				_, err = s.mutateRevisionVault(mutationContext(t), m, func(store *credentialvault.Store, p *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
					var candidate *credentialvault.MutationCandidate
					if operation == "patch" {
						candidate, prepareErr = store.PreparePatch(credentialvault.MutationRequest{Bindings: &credentialvault.BindingMutationSet{Add: request.Bindings}}, p)
					} else {
						candidate, prepareErr = store.PrepareCreate(request, p)
					}
					return candidate, prepareErr
				})
				require.ErrorContains(t, prepareErr, "overlaps static passthrough")
				require.ErrorContains(t, prepareErr, overlap.host)
				require.Error(t, err)
				require.NotContains(t, err.Error(), overlap.host)
				after, afterTag, _, afterErr := s.credentialVault.ActiveSnapshotIfChanged(context.Background(), "")
				require.Equal(t, beforeErr, afterErr)
				require.Equal(t, before, after)
				require.Equal(t, tag, afterTag)
				require.Same(t, base, s.revisionRecovery.current)
				require.Zero(t, session.updateCalls)
				require.Zero(t, session.reconcileUpdateCalls)
				require.Zero(t, file.saves)
				require.Zero(t, file.restores)
				require.Zero(t, nft.calls)
				require.False(t, s.revisionRecoveryRequired.Load())
				require.False(t, s.mitmGate.MitmPending())
				// Identical destination and policy are valid when only HTTP is
				// eligible, proving the preceding failure was TLS overlap.
				request.Bindings[0].Match.Schemes = []string{"http"}
				accepted, err := s.mutateRevisionVault(mutationContext(t), m, func(store *credentialvault.Store, p *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
					if operation == "patch" {
						return store.PreparePatch(credentialvault.MutationRequest{Bindings: &credentialvault.BindingMutationSet{Add: request.Bindings}}, p)
					}
					return store.PrepareCreate(request, p)
				})
				require.NoError(t, err)
				require.Positive(t, accepted.Revision)
				require.Equal(t, 1, session.updateCalls)

			})
		}
	}
}

// The public handler must retain the fixed 400 mapping without disclosing hosts.
func TestStaticPassthroughHandlerOverlapIsBadRequest(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			s, _, session, nft := revisionPolicyFixture(t)
			config, err := mitmpassthrough.Parse(`["code.example.com"]`, true)
			require.NoError(t, err)
			s.credentialVault = credentialvault.NewLiveStore(s.mitmGate, func() bool { return true }, config)
			setMutationFixturePolicy(t, s, liveWritePolicy(t, true))
			request := testCredentialVaultRequest()
			request.Bindings[0].Name = "private-binding-sentinel"
			body, err := json.Marshal(request)
			require.NoError(t, err)
			if method == http.MethodPatch {
				seed := request
				seed.Bindings = nil
				_, err := s.credentialVault.Create(seed, s.proxy.CurrentPolicy())
				require.NoError(t, err)
				expected := int64(1)
				body, err = json.Marshal(credentialvault.MutationRequest{ExpectedRevision: &expected, Bindings: &credentialvault.BindingMutationSet{Add: request.Bindings}})
				require.NoError(t, err)
			}
			before, tag, _, beforeErr := s.credentialVault.ActiveSnapshotIfChanged(context.Background(), "")
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			response := liveWriteDo(t, s, method, string(body))
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.NotContains(t, response.Body.String(), "code.example.com")
			require.NotContains(t, response.Body.String(), "private-binding-sentinel")
			after, afterTag, _, afterErr := s.credentialVault.ActiveSnapshotIfChanged(context.Background(), "")
			require.Equal(t, beforeErr, afterErr)
			require.Equal(t, before, after)
			require.Equal(t, tag, afterTag)
			require.Zero(t, session.updateCalls)
			require.Zero(t, file.saves)
			require.Zero(t, nft.calls)
		})
	}
}
