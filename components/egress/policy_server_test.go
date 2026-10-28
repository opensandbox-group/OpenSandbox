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

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
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

type stagedTestAlwaysLoader struct {
	deny, allow    []policy.EgressRule
	candidateDeny  []policy.EgressRule
	candidateAllow []policy.EgressRule
	pending        bool
}

func (l *stagedTestAlwaysLoader) CurrentRules() (deny, allow []policy.EgressRule) {
	return append([]policy.EgressRule(nil), l.deny...), append([]policy.EgressRule(nil), l.allow...)
}

func (l *stagedTestAlwaysLoader) SetCurrentRules(deny, allow []policy.EgressRule) {
	l.deny = append([]policy.EgressRule(nil), deny...)
	l.allow = append([]policy.EgressRule(nil), allow...)
}

func (l *stagedTestAlwaysLoader) RefreshIfDueWithApply(_ time.Time, apply func(deny, allow []policy.EgressRule) error) ([]policy.EgressRule, []policy.EgressRule, bool, error) {
	if !l.pending {
		deny, allow := l.CurrentRules()
		return deny, allow, false, nil
	}
	deny := append([]policy.EgressRule(nil), l.candidateDeny...)
	allow := append([]policy.EgressRule(nil), l.candidateAllow...)
	if apply != nil {
		if err := apply(append([]policy.EgressRule(nil), deny...), append([]policy.EgressRule(nil), allow...)); err != nil {
			return nil, nil, false, err
		}
	}
	l.pending = false
	return deny, allow, true, nil
}

type stubNft struct {
	err         error
	calls       int
	quiesces    int
	applied     *policy.NetworkPolicy
	onApply     func(*policy.NetworkPolicy)
	deadline    time.Time
	hasDeadline bool
}

func (s *stubNft) Quiesce() { s.quiesces++ }

func (s *stubNft) ApplyStatic(ctx context.Context, p *policy.NetworkPolicy) error {
	s.calls++
	s.applied = p
	s.deadline, s.hasDeadline = ctx.Deadline()
	if s.onApply != nil {
		s.onApply(p)
	}
	return s.err
}

func TestReloadAlwaysRules_NftFailurePreservesRulesAndRetriesBeforeProxyPublish(t *testing.T) {
	oldDeny := []policy.EgressRule{mustRule(t, policy.ActionDeny, "1.1.1.1")}
	oldAllow := []policy.EgressRule{mustRule(t, policy.ActionAllow, "2.2.2.2")}
	newDeny := []policy.EgressRule{mustRule(t, policy.ActionDeny, "3.3.3.3")}
	loader := &stagedTestAlwaysLoader{deny: oldDeny, allow: oldAllow, candidateDeny: newDeny, candidateAllow: oldAllow, pending: true}
	proxy := &stubProxy{}
	proxy.UpdateAlwaysRules(oldDeny, oldAllow)
	nft := &stubNft{err: errors.New("nft apply failed")}
	nft.onApply = func(applied *policy.NetworkPolicy) {
		_, _, denyV4, _ := applied.StaticIPSets()
		require.Contains(t, denyV4, "3.3.3.3", "nft must receive the staged candidate before it is published in memory")
		deny, allow := loader.CurrentRules()
		require.Equal(t, oldDeny, deny, "loader rules must not publish before nft accepts the candidate")
		require.Equal(t, oldAllow, allow)
		require.Equal(t, oldDeny, proxy.deny, "proxy rules must not publish before nft accepts the candidate")
		require.Equal(t, oldAllow, proxy.allow)
	}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft", alwaysLoader: loader}

	srv.reloadAlwaysRulesJob()
	deny, allow := loader.CurrentRules()
	require.Equal(t, oldDeny, deny, "nft failure must preserve loader rules")
	require.Equal(t, oldAllow, allow)
	require.Equal(t, oldDeny, proxy.deny, "nft failure must preserve proxy rules")
	require.Equal(t, oldAllow, proxy.allow)

	nft.err = nil
	srv.reloadAlwaysRulesJob()
	deny, allow = loader.CurrentRules()
	require.Equal(t, newDeny, deny)
	require.Equal(t, oldAllow, allow)
	require.Equal(t, newDeny, proxy.deny)
	require.Equal(t, oldAllow, proxy.allow)
}

func TestReloadAlwaysRules_UsesSameTelemetryCandidateForNftAndMemory(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector-a.example:4318/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("HOST_IP", "")

	fileAllow := []policy.EgressRule{mustRule(t, policy.ActionAllow, "file-allow.example")}
	loader := &stagedTestAlwaysLoader{candidateAllow: fileAllow, pending: true}
	proxy := &stubProxy{}
	nft := &stubNft{}
	nft.onApply = func(*policy.NetworkPolicy) {
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector-b.example:4318/v1/metrics")
	}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft", alwaysLoader: loader}

	srv.reloadAlwaysRulesJob()

	require.NotNil(t, nft.applied)
	_, _, denyV4, _ := nft.applied.StaticIPSets()
	require.Empty(t, denyV4)
	require.Equal(t, []policy.EgressRule{
		mustRule(t, policy.ActionAllow, "file-allow.example"),
		mustRule(t, policy.ActionAllow, "collector-a.example"),
	}, proxy.allow, "memory rules must preserve the telemetry candidate applied to nft")
	require.Equal(t, proxy.allow, nft.applied.Egress, "nft and proxy must publish the same effective allow rules")
	deny, allow := loader.CurrentRules()
	require.Nil(t, deny)
	require.Equal(t, proxy.allow, allow, "loader current rules must use the same effective candidate")
}

func TestReloadAlwaysRules_NftApplyHasThirtySecondDeadline(t *testing.T) {
	deny := []policy.EgressRule{mustRule(t, policy.ActionDeny, "blocked.example")}
	loader := &stagedTestAlwaysLoader{candidateDeny: deny, pending: true}
	nft := &stubNft{}
	srv := &policyServer{proxy: &stubProxy{}, nft: nft, enforcementMode: "dns+nft", alwaysLoader: loader}

	srv.reloadAlwaysRulesJob()

	require.True(t, nft.hasDeadline, "periodic nft application must have a finite deadline")
	remaining := time.Until(nft.deadline)
	require.Greater(t, remaining, 20*time.Second, "the deadline should retain the existing 30-second policy apply budget")
	require.LessOrEqual(t, remaining, 30*time.Second)
}

func TestReloadAlwaysRules_HoldsVaultMutationBarrierDuringNftApply(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	deny := []policy.EgressRule{mustRule(t, policy.ActionDeny, "code.example.com")}
	loader := &stagedTestAlwaysLoader{candidateDeny: deny, pending: true}
	proxy := &stubProxy{updated: testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)}
	nft := &blockingVaultPolicyNft{entered: make(chan struct{}, 1), release: make(chan struct{})}
	srv := &policyServer{
		proxy:           proxy,
		nft:             nft,
		enforcementMode: "dns+nft",
		alwaysLoader:    loader,
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}
	t.Cleanup(nft.unblock)

	reloadDone := make(chan struct{})
	go func() {
		srv.reloadAlwaysRulesJob()
		close(reloadDone)
	}()
	select {
	case <-nft.entered:
	case <-time.After(time.Second):
		t.Fatal("periodic reload did not reach nft apply")
	}
	if srv.mu.TryLock() {
		srv.mu.Unlock()
		t.Fatal("policy mutex should remain held while periodic nft apply is blocked")
	}
	nft.unblock()
	<-reloadDone

	body := `{"credentials":[{"name":"gitlab-token","source":{"type":"inline","value":"secret-token"}}],"bindings":[{"name":"gitlab-api","match":{"hosts":["code.example.com"],"methods":["GET"],"paths":["/api/v8/*"]},"auth":{"type":"apiKey","name":"PRIVATE-TOKEN","credential":"gitlab-token"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:4321"
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "is not allowed by egress policy")
}

func (s *stubNft) AddResolvedDomain(_ context.Context, _ string, _ []nftables.ResolvedIP) error {
	return nil
}

func (s *stubNft) AddUpstreamProxyIPs(_ context.Context, _ []nftables.ResolvedIP) error {
	return nil
}

func (s *stubNft) StartConnectionRefresh(context.Context) {}

func (s *stubNft) StartDomainRefresh(context.Context, func(context.Context, string) ([]nftables.ResolvedIP, error)) {
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

func TestHandlePolicy_NftFailureRestoresPolicyFile(t *testing.T) {
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	previous := []byte(`{"defaultAction":"deny","egress":[{"action":"allow","target":"2.2.2.2"}]}`)
	require.NoError(t, os.WriteFile(policyFile, previous, 0o600))
	srv := &policyServer{
		proxy:           &stubProxy{},
		nft:             &stubNft{err: errors.New("boom")},
		enforcementMode: "dns+nft",
		policyFile:      policyFile,
	}

	body := `{"defaultAction":"allow"}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Result().StatusCode)
	got, err := os.ReadFile(policyFile)
	require.NoError(t, err)
	require.Equal(t, string(previous), string(got), "rejected policy must not stay on disk")
}

func TestHandlePolicy_NftFailureRemovesNewPolicyFile(t *testing.T) {
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	srv := &policyServer{
		proxy:           &stubProxy{},
		nft:             &stubNft{err: errors.New("boom")},
		enforcementMode: "dns+nft",
		policyFile:      policyFile,
	}

	body := `{"defaultAction":"allow"}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Result().StatusCode)
	_, err := os.Stat(policyFile)
	require.True(t, os.IsNotExist(err), "rejected policy must not stay on disk")
}

func TestHandlePolicy_SuccessPersistsPolicyFile(t *testing.T) {
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	srv := &policyServer{
		proxy:           &stubProxy{},
		nft:             &stubNft{},
		enforcementMode: "dns+nft",
		policyFile:      policyFile,
	}

	body := `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	data, err := os.ReadFile(policyFile)
	require.NoError(t, err)
	saved, err := policy.ParsePolicy(string(data))
	require.NoError(t, err)
	require.Equal(t, policy.ActionDeny, saved.DefaultAction)
	require.Len(t, saved.Egress, 1)
	require.Equal(t, "1.1.1.1", saved.Egress[0].Target)
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

func TestHandlePatch_DistinctPortScopedRulesCoexist(t *testing.T) {
	// mergeKey used to fold every target-less rule to the same "" key, so a
	// second port-only rule silently evicted the first instead of coexisting
	// with it. Two port-only rules with different Ports must both survive a
	// single PATCH payload.
	initial := &policy.NetworkPolicy{DefaultAction: policy.ActionDeny}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `[{"action":"deny","ports":[25]},{"action":"allow","ports":[443,80]}]`
	req := httptest.NewRequest(http.MethodPatch, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200")
	require.Len(t, proxy.updated.Egress, 2, "both port-scoped rules should be kept, not deduped against each other")

	// A follow-up PATCH adding a third, differently-scoped port rule must not
	// evict the two already-applied port rules.
	body2 := `[{"action":"deny","target":"10.0.0.5","ports":[22]}]`
	req2 := httptest.NewRequest(http.MethodPatch, "/policy", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	srv.handlePolicy(w2, req2)

	require.Equal(t, http.StatusOK, w2.Result().StatusCode, "expected 200")
	require.Len(t, proxy.updated.Egress, 3, "the two prior port-scoped rules must survive a later unrelated patch")
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

func TestHandleDelete_RemovesMatchingTargets(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionAllow, Target: "example.com"},
			{Action: policy.ActionDeny, Target: "blocked.com"},
			{Action: policy.ActionAllow, Target: "keep.com"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `["blocked.com","nonexistent.com"]`
	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 OK")
	require.Equal(t, 1, nft.calls, "expected nft ApplyStatic called once")
	require.NotNil(t, proxy.updated, "expected proxy policy updated")
	require.Equal(t, policy.ActionDeny, proxy.updated.DefaultAction, "defaultAction should be preserved")
	require.Len(t, proxy.updated.Egress, 2, "expected 2 rules remaining after delete")
	require.Equal(t, policy.ActionAllow, proxy.updated.Egress[0].Action)
	require.Equal(t, "example.com", proxy.updated.Egress[0].Target)
	require.Equal(t, policy.ActionAllow, proxy.updated.Egress[1].Action)
	require.Equal(t, "keep.com", proxy.updated.Egress[1].Target)
}

func TestHandleDelete_CaseInsensitiveMatch(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionAllow, Target: "Example.COM"},
			{Action: policy.ActionDeny, Target: "Blocked.COM"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `["example.com"]`
	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 OK")
	require.NotNil(t, proxy.updated)
	require.Len(t, proxy.updated.Egress, 1, "expected 1 rule remaining")
	require.Equal(t, "Blocked.COM", proxy.updated.Egress[0].Target, "unmatched rule should remain")
}

func TestHandleDelete_NoMatchReturns200(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionAllow, Target: "keep.com"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `["nonexistent.com"]`
	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 OK even when no targets match")
	require.Equal(t, 0, nft.calls, "nft should not be called when nothing changes")
	require.Len(t, proxy.updated.Egress, 1, "policy should be unchanged")
}

func TestHandleDelete_EmptyBodyReturns400(t *testing.T) {
	proxy := &stubProxy{updated: policy.DefaultDenyPolicy()}
	srv := &policyServer{proxy: proxy, nft: nil, enforcementMode: "dns"}

	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(""))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "expected 400 for empty body")
}

func TestHandleDelete_EmptyArrayReturns400(t *testing.T) {
	proxy := &stubProxy{updated: policy.DefaultDenyPolicy()}
	srv := &policyServer{proxy: proxy, nft: nil, enforcementMode: "dns"}

	body := `[]`
	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "expected 400 for empty array")
}

func TestHandleDelete_InvalidJSONReturns400(t *testing.T) {
	proxy := &stubProxy{updated: policy.DefaultDenyPolicy()}
	srv := &policyServer{proxy: proxy, nft: nil, enforcementMode: "dns"}

	body := `not-json`
	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "expected 400 for invalid JSON")
}

func TestHandleDelete_NftFailureReturns500(t *testing.T) {
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionAllow, Target: "example.com"},
		},
	}
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{err: errors.New("nft apply failed")}
	srv := &policyServer{proxy: proxy, nft: nft, enforcementMode: "dns+nft"}

	body := `["example.com"]`
	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handlePolicy(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, "expected 500 on nft failure")
	require.Equal(t, 1, nft.calls, "expected nft ApplyStatic called once")
	require.Len(t, proxy.updated.Egress, 1, "proxy should not be updated on nft failure")
	require.Equal(t, "example.com", proxy.updated.Egress[0].Target, "original rule should remain")
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

func mustRule(t *testing.T, action, target string) policy.EgressRule {
	t.Helper()
	r, err := policy.ParseValidatedEgressRule(action, target)
	require.NoError(t, err)
	return r
}

func TestFingerprintRules_OrderIndependent(t *testing.T) {
	a := mustRule(t, policy.ActionDeny, "1.1.1.1")
	b := mustRule(t, policy.ActionDeny, "2.2.2.2")
	c := mustRule(t, policy.ActionAllow, "example.com")
	d := mustRule(t, policy.ActionAllow, "foo.test")

	fp1 := fingerprintRules([]policy.EgressRule{a, b}, []policy.EgressRule{c, d})
	fp2 := fingerprintRules([]policy.EgressRule{b, a}, []policy.EgressRule{d, c})
	require.Equal(t, fp1, fp2, "fingerprint must be order-independent within each set")
}

func TestFingerprintRules_DenyAllowSetsDistinct(t *testing.T) {
	denyX := mustRule(t, policy.ActionDeny, "1.1.1.1")
	allowX := mustRule(t, policy.ActionAllow, "1.1.1.1")

	fpDeny := fingerprintRules([]policy.EgressRule{denyX}, nil)
	fpAllow := fingerprintRules(nil, []policy.EgressRule{allowX})
	require.NotEqual(t, fpDeny, fpAllow, "deny X and allow X must not collide via set separator")
}

func TestFingerprintRules_DetectsAddRemove(t *testing.T) {
	a := mustRule(t, policy.ActionDeny, "1.1.1.1")
	b := mustRule(t, policy.ActionDeny, "2.2.2.2")

	fp1 := fingerprintRules([]policy.EgressRule{a}, nil)
	fp2 := fingerprintRules([]policy.EgressRule{a, b}, nil)
	require.NotEqual(t, fp1, fp2, "adding a rule must change fingerprint")

	fp3 := fingerprintRules([]policy.EgressRule{a, b}, nil)
	fp4 := fingerprintRules([]policy.EgressRule{a}, nil)
	require.NotEqual(t, fp3, fp4, "removing a rule must change fingerprint")
}

func TestFingerprintRules_DetectsActionChange(t *testing.T) {
	deny := mustRule(t, policy.ActionDeny, "1.1.1.1")
	allow := mustRule(t, policy.ActionAllow, "1.1.1.1")

	fp1 := fingerprintRules([]policy.EgressRule{deny}, nil)
	fp2 := fingerprintRules([]policy.EgressRule{allow}, nil)
	require.NotEqual(t, fp1, fp2, "flipping action on same target must change fingerprint")
}

func TestFingerprintRules_EmptyStable(t *testing.T) {
	fp1 := fingerprintRules(nil, nil)
	fp2 := fingerprintRules([]policy.EgressRule{}, []policy.EgressRule{})
	require.Equal(t, fp1, fp2, "nil and empty slices must produce same fingerprint")
}

func recoveryPolicyFixture(t *testing.T) *policyServer {
	t.Helper()
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	inputs := policyCandidateInputs(t)
	s := recoveryTestOwner(t, inputs, credentialvault.NewStore(nil, nil))
	s.proxy = &stubProxy{updated: inputs.user}
	s.mitmGate = mitmproxy.NewHealthGate()
	s.mitmGate.SetReady(true)
	s.alwaysLoader = &stagedTestAlwaysLoader{}
	attachPolicyTestSession(t, s)
	return s
}

func attachPolicyTestSession(t *testing.T, s *policyServer) {
	t.Helper()
	session := &mutationTestSession{fakeRevisionProcessSession: fakeRevisionProcessSession{config: validFakeRevisionIPCConfig()}}
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		return mutationIdentity(t, snapshot, epoch), nil
	}
	s.mitm = &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: session, currentGen: 1,
		revisionOwner: &revisionLaunchOwner{server: s, stop: func(*mitmproxy.Running) {}}}
}

func TestRevisionRecoveryPolicyEffectClassification(t *testing.T) {
	for _, kind := range []string{"json", "binding", "read-file", "unpublished-overlay-ignored", "persist", "nft-no-file", "nft-restored-file", "success-no-effects", "success-same-content", "success-reset", "success-nft", "success-file", "legacy-failure"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS != "linux" && (kind == "success-file" || kind == "nft-restored-file") {
				t.Skip("real experimental atomic policy storage requires Linux")
			}
			s := recoveryPolicyFixture(t)
			nft := &stubNft{}
			s.nft = nft
			body := `{"defaultAction":"allow"}`
			wantDefault := policy.ActionAllow
			wantStatus := http.StatusOK
			wantRecovery, wantChanged := false, true
			var previous []byte
			switch kind {
			case "json":
				body, wantStatus, wantChanged = "invalid", http.StatusBadRequest, false
			case "binding":
				s.credentialVault = policyCandidateStore(t, policyCandidateInputs(t))
				body, wantStatus, wantChanged = `{"defaultAction":"deny"}`, http.StatusBadRequest, false
			case "read-file":
				s.policyFile = t.TempDir()
				wantStatus, wantChanged = http.StatusInternalServerError, false
			case "unpublished-overlay-ignored":
				// Only an acknowledged reload can replace the frozen overlay.
				s.alwaysLoader.SetCurrentRules(nil, []policy.EgressRule{{Action: "invalid", Target: "invalid.example"}})
			case "persist":
				s.policyFile = filepath.Join(t.TempDir(), "missing", "policy.json")
				wantStatus, wantChanged = http.StatusInternalServerError, false
			case "nft-no-file", "nft-restored-file":
				nft.err = errors.New("private-nft-error")
				wantStatus, wantRecovery, wantChanged = http.StatusInternalServerError, true, false
				if kind == "nft-restored-file" {
					s.policyFile = filepath.Join(t.TempDir(), "policy.json")
					previous = []byte(`{"defaultAction":"deny"}`)
					require.NoError(t, os.WriteFile(s.policyFile, previous, 0o600))
				}
			case "success-no-effects":
				s.nft = nil
			case "success-same-content":
				s.nft = nil
				body = `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`
				wantDefault = policy.ActionDeny
			case "success-reset":
				s.nft = nil
				body, wantDefault = "", policy.ActionDeny
			case "success-file":
				s.nft = nil
				s.policyFile = filepath.Join(t.TempDir(), "policy.json")
			case "legacy-failure":
				s.revisionRecovery = nil
				nft.err = errors.New("legacy-nft-error")
				wantStatus, wantChanged = http.StatusInternalServerError, false
			}
			var ticket *revisionBootstrapTicket
			var candidate *effectivePolicyCandidate
			var original *effectivePolicyBase
			if s.revisionRecovery != nil {
				original = s.revisionRecovery.current
				_, _, ticket, _ = s.captureRevisionBootstrap(context.Background())
				var err error
				candidate, err = s.prepareRevisionPolicyCandidate(original.inputs)
				require.NoError(t, err)
				nft.onApply = func(*policy.NetworkPolicy) {
					require.ErrorIs(t, s.validateRevisionBootstrapLocked(ticket), errStaleRevisionBootstrap, "invalidate before the real nft attempt")
					require.Same(t, original, s.revisionRecovery.current, "publication follows effects")
				}
			}
			w := httptest.NewRecorder()
			s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body)))
			require.Equal(t, wantStatus, w.Code)
			if kind == "legacy-failure" {
				require.Nil(t, s.revisionRecovery)
				require.False(t, s.mitmGate.MitmPending())
				require.Zero(t, nft.quiesces)
				return
			}
			if wantRecovery {
				require.Equal(t, 1, nft.quiesces)
				require.True(t, s.mitmGate.MitmPending())
				require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
				_, err := s.prepareRevisionPolicyCandidate(original.inputs)
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
				// A best-effort disk restoration and a later healthy nft cannot clear the latch.
				if previous != nil {
					actual, err := os.ReadFile(s.policyFile)
					require.NoError(t, err)
					require.Equal(t, previous, actual)
				}
				nft.err = nil
				calls := nft.calls
				s.policyFile = ""
				w = httptest.NewRecorder()
				s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(body)))
				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				require.Equal(t, calls, nft.calls)
				require.True(t, s.mitmGate.MitmPending())
			} else {
				require.Zero(t, nft.quiesces)
				require.Equal(t, revisionRecoveryNone, s.revisionRecovery.reason)
				require.False(t, s.mitmGate.MitmPending())
				if wantChanged {
					require.NotSame(t, original, s.revisionRecovery.current)
					require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
					require.ErrorIs(t, validateRecoveryCandidate(s, candidate), errStaleEffectivePolicyCandidate)
					require.Equal(t, wantDefault, s.revisionRecovery.current.inputs.user.DefaultAction)
					require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
					// The proxy exposes a mutable pointer; it must not alias the frozen base.
					s.proxy.(*stubProxy).updated.DefaultAction = "tampered"
					require.Equal(t, wantDefault, s.revisionRecovery.current.inputs.user.DefaultAction)
				} else {
					require.NoError(t, validateRecoveryTicket(s, ticket))
					require.Zero(t, nft.calls, "pure errors must precede external effects")
				}
			}
			require.Same(t, original, candidate.base)
			if !wantChanged {
				require.Same(t, original, s.revisionRecovery.current)
			}
			if kind == "unpublished-overlay-ignored" {
				_, err := os.Stat(s.policyFile)
				require.True(t, os.IsNotExist(err), "prebuild failure must occur before saving")
			}
		})
	}
}

// The production loader invokes apply under its own write lock. This fixture
// detects reentry without letting a bad callback deadlock the test process.
type recoveryLockedAlwaysLoader struct {
	stagedTestAlwaysLoader
	mu         sync.Mutex
	t          *testing.T
	refreshErr error
}

func (l *recoveryLockedAlwaysLoader) CurrentRules() ([]policy.EgressRule, []policy.EgressRule) {
	if !l.mu.TryLock() {
		l.t.Error("always callback reentered CurrentRules while loader lock was held")
		return nil, nil
	}
	defer l.mu.Unlock()
	return l.stagedTestAlwaysLoader.CurrentRules()
}
func (l *recoveryLockedAlwaysLoader) SetCurrentRules(deny, allow []policy.EgressRule) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stagedTestAlwaysLoader.SetCurrentRules(deny, allow)
}
func (l *recoveryLockedAlwaysLoader) RefreshIfDueWithApply(now time.Time, apply func([]policy.EgressRule, []policy.EgressRule) error) ([]policy.EgressRule, []policy.EgressRule, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refreshErr != nil {
		return nil, nil, false, l.refreshErr
	}
	return l.stagedTestAlwaysLoader.RefreshIfDueWithApply(now, apply)
}

func TestRevisionRecoveryAlwaysCallback(t *testing.T) {
	for _, kind := range []string{"unchanged", "parse-error", "unpublished-overlay-ignored", "binding", "nameserver-binding", "nft-failure", "success-nft", "success-no-nft"} {
		t.Run(kind, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			loader := &recoveryLockedAlwaysLoader{t: t, stagedTestAlwaysLoader: stagedTestAlwaysLoader{
				candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "file-allow.example")}, pending: true,
			}}
			s.alwaysLoader = loader
			nft := &stubNft{}
			s.nft = nft
			t.Setenv("OTEL_SDK_DISABLED", "")
			t.Setenv("OTEL_METRICS_EXPORTER", "")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector-a.example:4318/v1/metrics")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("HOST_IP", "")
			switch kind {
			case "unchanged":
				loader.pending = false
			case "parse-error":
				loader.refreshErr = errors.New("parse failed")
			case "unpublished-overlay-ignored":
				loader.candidateAllow = []policy.EgressRule{{Action: "invalid", Target: "invalid.example"}}
			case "binding", "nameserver-binding":
				s.credentialVault = policyCandidateStore(t, policyCandidateInputs(t))
				loader.candidateDeny = []policy.EgressRule{mustRule(t, policy.ActionDeny, "api.example.com")}
				if kind == "nameserver-binding" {
					s.nameserverIPs = []netip.Addr{netip.MustParseAddr("192.0.2.53")}
				}
			case "success-nft":
				s.nameserverIPs = []netip.Addr{netip.MustParseAddr("192.0.2.53")}
			case "nft-failure":
				nft.err = errors.New("private-nft-detail")
			case "success-no-nft":
				s.nft = nil
			}
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			original := s.revisionRecovery.current
			nft.onApply = func(*policy.NetworkPolicy) {
				require.ErrorIs(t, s.validateRevisionBootstrapLocked(ticket), errStaleRevisionBootstrap)
				require.Same(t, original, s.revisionRecovery.current)
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector-b.example:4318/v1/metrics")
			}
			changed, err := s.reloadAlwaysRules()
			if kind == "nft-failure" {
				require.Error(t, err)
				require.False(t, changed)
				require.True(t, s.mitmGate.MitmPending())
				require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
				nft.err = nil
				_, err = s.reloadAlwaysRules()
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
				require.Equal(t, 1, nft.calls)
			} else if strings.HasPrefix(kind, "success") {
				require.NoError(t, err)
				require.True(t, changed)
				require.NotSame(t, original, s.revisionRecovery.current)
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
				inputs := s.revisionRecovery.current.inputs
				effectiveAllow := append(append([]policy.EgressRule(nil), inputs.alwaysAllow...), inputs.telemetryAllow...)
				require.Equal(t, []policy.EgressRule{mustRule(t, policy.ActionAllow, "file-allow.example"), mustRule(t, policy.ActionAllow, "collector-a.example")}, effectiveAllow)
				require.Equal(t, effectiveAllow, s.proxy.(*stubProxy).allow)
				_, allow := loader.CurrentRules()
				require.Equal(t, effectiveAllow, allow)
				if s.nft != nil {
					require.Equal(t, effectiveAllow, nft.applied.Egress[:len(effectiveAllow)])
					nftAllowV4, _, _, _ := nft.applied.StaticIPSets()
					require.Contains(t, nftAllowV4, "192.0.2.53")
					basePolicy := policy.MergeAlwaysOverlay(inputs.user, inputs.alwaysDeny, effectiveAllow)
					baseAllowV4, _, _, _ := basePolicy.StaticIPSets()
					require.NotContains(t, baseAllowV4, "192.0.2.53", "nameserver allowances belong only to nft derivation")
				}
				require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
				require.False(t, s.mitmGate.MitmPending())
			} else {
				if kind == "unchanged" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.False(t, changed)
				require.Same(t, original, s.revisionRecovery.current)
				require.NoError(t, validateRecoveryTicket(s, ticket))
				require.Zero(t, nft.calls)
				require.False(t, s.mitmGate.MitmPending())
			}
		})
	}
}

func TestRevisionRecoveryLegacyAlwaysWithoutPolicyRead(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	s := &policyServer{alwaysLoader: &stagedTestAlwaysLoader{pending: true}}
	// The legacy nil-nft callback has never required a user-policy source.
	// UpdateAlwaysRules still needs a proxy, but CurrentPolicy is unnecessary.
	s.proxy = &recoveryNoCurrentPolicyProxy{stubProxy: stubProxy{}}
	changed, err := s.reloadAlwaysRules()
	require.NoError(t, err)
	require.True(t, changed)
	require.Nil(t, s.revisionRecovery)
}

type recoveryNoCurrentPolicyProxy struct{ stubProxy }

func (*recoveryNoCurrentPolicyProxy) CurrentPolicy() *policy.NetworkPolicy {
	panic("legacy nil-nft callback must not request CurrentPolicy")
}

func TestHandleDelete_PortOnlyRulesViaEmptyTarget(t *testing.T) {
	// Port-only rules have no target; an explicit empty entry in the DELETE
	// list addresses them (all of them), so they are not permanently stuck.
	initial := &policy.NetworkPolicy{
		DefaultAction: policy.ActionDeny,
		Egress: []policy.EgressRule{
			{Action: policy.ActionDeny, Ports: []int{25}},
			{Action: policy.ActionAllow, Target: "example.com"},
			{Action: policy.ActionAllow, Ports: []int{53}},
		},
	}
	proxy := &stubProxy{updated: initial}
	srv := &policyServer{proxy: proxy, nft: &stubNft{}, enforcementMode: "dns+nft"}

	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(`["example.com", ""]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	require.Len(t, proxy.updated.Egress, 0, "port-only rules go with the empty entry, domain rule with its target")
}
