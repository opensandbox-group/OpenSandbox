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

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

type stubProxy struct {
	updated *policy.NetworkPolicy
	deny    []policy.EgressRule
	allow   []policy.EgressRule
}

func (s *stubProxy) CurrentPolicy() *policy.NetworkPolicy {
	return s.updated
}

func (s *stubProxy) UpdatePolicy(p *policy.NetworkPolicy) {
	s.updated = p
}

func (s *stubProxy) UpdateAlwaysRules(alwaysDeny, alwaysAllow []policy.EgressRule) {
	s.deny = append([]policy.EgressRule(nil), alwaysDeny...)
	s.allow = append([]policy.EgressRule(nil), alwaysAllow...)
}

type stubNft struct {
	err     error
	calls   int
	applied *policy.NetworkPolicy
}

func (s *stubNft) ApplyStatic(_ context.Context, p *policy.NetworkPolicy) error {
	s.calls++
	s.applied = p
	return s.err
}

func (s *stubNft) AddResolvedIPs(_ context.Context, _ []nftables.ResolvedIP) error {
	return nil
}

func (s *stubNft) RemoveEnforcement(_ context.Context) error {
	return nil
}

func TestHandlePolicy_AlwaysDenyMergedIntoNft(t *testing.T) {
	deny, err := policy.ParseValidatedEgressRule(policy.ActionDeny, "9.9.9.9")
	require.NoError(t, err)
	proxy := &stubProxy{}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}
	srv.setAlwaysRules([]policy.EgressRule{deny}, nil)

	body := `{"defaultAction":"deny","egress":[{"action":"allow","target":"9.9.9.9"}]}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 OK")
	require.NotNil(t, nft.applied, "expected nft applied")
	_, _, denyV4, _ := nft.applied.StaticIPSets()
	require.Contains(t, denyV4, "9.9.9.9", "always deny must appear in nft static deny set")
	require.Len(t, proxy.updated.Egress, 1, "persisted/user policy must not include always rules")
	require.Equal(t, "9.9.9.9", proxy.updated.Egress[0].Target)
}

func TestHandlePolicy_AppliesNftAndUpdatesProxy(t *testing.T) {
	proxy := &stubProxy{}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 OK")
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json", "expected json response")
	require.Equal(t, 1, nft.calls, "expected nft ApplyStatic called once")
	require.NotNil(t, proxy.updated, "expected proxy policy to be updated")
	require.Equal(t, policy.ActionDeny, proxy.updated.DefaultAction, "unexpected defaultAction")
}

func TestHandlePolicy_NftFailureReturns500(t *testing.T) {
	proxy := &stubProxy{}
	nft := &stubNft{err: errors.New("boom")}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, "expected 500")
	require.Equal(t, 1, nft.calls, "expected nft ApplyStatic called once")
	require.Nil(t, proxy.updated, "expected proxy policy not updated on nft failure")
}

func TestHandleGet_ReturnsEnforcementMode(t *testing.T) {
	proxy := &stubProxy{updated: policy.DefaultDenyPolicy()}
	srv := &policyServer{proxy: proxy, nft: nil, enforcementMode: "dns"}

	req := httptest.NewRequest(http.MethodGet, "/policy", nil)
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `"enforcementMode":"dns"`, "expected enforcementMode dns in response")
}

func TestHandlePatch_MergesAndApplies(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionAllow, Target: "example.com"},
			{Action: policy.ActionDeny, Target: "*.example.com"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `[{"action":"deny","target":"blocked.com"},{"action":"allow","target":"example.com"}]`
	req := httptest.NewRequest(http.MethodPatch, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200")
	require.Equal(t, 1, nft.calls, "expected nft ApplyStatic called once")
	require.NotNil(t, proxy.updated, "expected proxy policy to be updated")
	require.Equal(t, policy.ActionDeny, proxy.updated.DefaultAction, "default action should be preserved")
	require.Len(t, proxy.updated.Egress, 3, "expected 3 egress rules")
	require.Equal(t, policy.ActionDeny, proxy.updated.Egress[0].Action, "first rule action mismatch")
	require.Equal(t, "blocked.com", proxy.updated.Egress[0].Target, "first rule target mismatch")
	require.Equal(t, policy.ActionAllow, proxy.updated.Egress[1].Action, "second rule action mismatch")
	require.Equal(t, "example.com", proxy.updated.Egress[1].Target, "second rule target mismatch")
	require.Equal(t, policy.ActionDeny, proxy.updated.Egress[2].Action, "base wildcard rule action mismatch")
	require.Equal(t, "*.example.com", proxy.updated.Egress[2].Target, "base wildcard rule target mismatch")
}

func TestHandlePatch_DomainCaseOverride(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionDeny, Target: "Example.COM"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `[{"action":"allow","target":"example.com"}]`
	req := httptest.NewRequest(http.MethodPatch, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200")
	require.NotNil(t, proxy.updated, "expected proxy policy to be updated")
	require.Len(t, proxy.updated.Egress, 1, "expected deduped rule count 1")
	require.Equal(t, policy.ActionAllow, proxy.updated.Egress[0].Action, "expected allow action")
	require.Equal(t, "example.com", proxy.updated.Egress[0].Target, "expected allow example.com to override")
}

func TestMaxEgressRulesFromEnv(t *testing.T) {
	old := os.Getenv(constants.EnvMaxEgressRules)
	defer func() { _ = os.Setenv(constants.EnvMaxEgressRules, old) }()

	require.NoError(t, os.Unsetenv(constants.EnvMaxEgressRules))
	require.Equal(t, constants.DefaultMaxEgressRules, maxEgressRulesFromEnv(), "empty env uses default")

	require.NoError(t, os.Setenv(constants.EnvMaxEgressRules, "0"))
	require.Equal(t, 0, maxEgressRulesFromEnv(), "0 means unlimited")

	require.NoError(t, os.Setenv(constants.EnvMaxEgressRules, "100"))
	require.Equal(t, 100, maxEgressRulesFromEnv())

	require.NoError(t, os.Setenv(constants.EnvMaxEgressRules, "not-a-number"))
	require.Equal(t, constants.DefaultMaxEgressRules, maxEgressRulesFromEnv(), "invalid falls back to default")

	require.NoError(t, os.Setenv(constants.EnvMaxEgressRules, "-1"))
	require.Equal(t, constants.DefaultMaxEgressRules, maxEgressRulesFromEnv(), "negative falls back to default")
}

func TestHandlePatch_RejectsWhenOverMaxEgressRules(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionAllow, Target: "a.example.com"},
			{Action: policy.ActionAllow, Target: "b.example.com"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft", maxEgressRules: 2}

	body := `[{"action":"allow","target":"c.example.com"}]`
	req := httptest.NewRequest(http.MethodPatch, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, "expected 400 when merged egress exceeds max")
	require.Equal(t, 0, nft.calls, "nft should not apply on rejection")
	require.Len(t, proxy.updated.Egress, 2, "policy should be unchanged")
}

func TestHandlePost_RejectsWhenOverMaxEgressRules(t *testing.T) {
	proxy := &stubProxy{}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft", maxEgressRules: 1}

	body := `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"},{"action":"allow","target":"8.8.8.8"}]}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, "expected 400")
	require.Nil(t, proxy.updated, "policy should not update")
	require.Equal(t, 0, nft.calls, "nft should not apply")
}
