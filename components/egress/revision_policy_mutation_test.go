// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

func revisionPolicyFixture(t *testing.T) (*policyServer, *mitmTransparent, *mutationTestSession, *stubNft) {
	t.Helper()
	s, m, session := liveWriteFixture(t)
	pol := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"},{"action":"allow","target":"spare.example.com"}]}`)
	setMutationFixturePolicy(t, s, pol)
	s.nft = &stubNft{}
	return s, m, session, s.nft.(*stubNft)
}

func revisionPolicyDo(t *testing.T, s *policyServer, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	return revisionPolicyDoContext(t, context.Background(), s, method, body)
}

func revisionPolicyDoContext(t *testing.T, ctx context.Context, s *policyServer, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	s.handlePolicy(response, httptest.NewRequest(method, "/policy", strings.NewReader(body)).WithContext(ctx))
	return response
}

type revisionPolicyObservedProxy struct {
	stubProxy
	onPolicy func(*policy.NetworkPolicy)
	onAlways func()
}

func (p *revisionPolicyObservedProxy) UpdatePolicy(pol *policy.NetworkPolicy) {
	if p.onPolicy != nil {
		p.onPolicy(pol)
	}
	p.stubProxy.UpdatePolicy(pol)
}

func (p *revisionPolicyObservedProxy) UpdateAlwaysRules(deny, allow []policy.EgressRule) {
	if p.onAlways != nil {
		p.onAlways()
	}
	p.stubProxy.UpdateAlwaysRules(deny, allow)
}

func TestRevisionPolicyHandlersPublishOnlyAfterExactConfirmation(t *testing.T) {
	for _, tc := range []struct{ name, method, body string }{
		{"post", http.MethodPost, `{"defaultAction":"allow"}`},
		{"put", http.MethodPut, `{"defaultAction":"allow"}`},
		{"patch", http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`},
		{"delete", http.MethodDelete, `["spare.example.com"]`},
		{"reset", http.MethodPost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, session, nft := revisionPolicyFixture(t)
			oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
			before, err := s.credentialVault.FreezeForPolicy(oldProxy)
			require.NoError(t, err)
			events := []string{}
			file := &faultPolicyStore{state: policy.FileCommitted, onSave: func() { events = append(events, "save") }}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			nft.onApply = func(*policy.NetworkPolicy) {
				events = append(events, "nft")
				require.Same(t, oldBase, s.revisionRecovery.current)
				require.Same(t, oldProxy, s.proxy.CurrentPolicy())
			}
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				events = append(events, "update")
				require.Same(t, oldBase, s.revisionRecovery.current)
				require.Same(t, oldProxy, s.proxy.CurrentPolicy())
				require.Equal(t, int64(1), epoch)
				require.Equal(t, before.Snapshot(), snapshot)
				return mutationIdentity(t, snapshot, epoch), nil
			}
			s.proxy = &revisionPolicyObservedProxy{stubProxy: stubProxy{updated: oldProxy}, onPolicy: func(*policy.NetworkPolicy) { events = append(events, "proxy") }}
			response := revisionPolicyDo(t, s, tc.method, tc.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, []string{"save", "nft", "update", "proxy"}, events)
			require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
			require.NoError(t, s.credentialVault.ValidatePolicySnapshot(before), "Policy must not mutate the Vault identity")
			require.Zero(t, file.restores)
			require.Equal(t, 1, session.updateCalls)
			snapshot, epoch, _, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			require.Equal(t, int64(1), epoch)
			require.Equal(t, before.Snapshot(), snapshot)
		})
	}
}

func TestRevisionPolicyPreservesAbsentEmptyAndBoundVault(t *testing.T) {
	for _, kind := range []string{"absent", "empty", "bound"} {
		t.Run(kind, func(t *testing.T) {
			s, _, session, _ := revisionPolicyFixture(t)
			switch kind {
			case "empty":
				_, err := s.credentialVault.Create(credentialvault.CreateRequest{}, s.proxy.CurrentPolicy())
				require.NoError(t, err)
			case "bound":
				revisionIntegrationCreateRendered(t, s, "private-pinned-policy-token")
			}
			frozen, err := s.credentialVault.FreezeForPolicy(s.proxy.CurrentPolicy())
			require.NoError(t, err)
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				require.Equal(t, frozen.Snapshot(), snapshot)
				return mutationIdentity(t, snapshot, epoch), nil
			}
			response := revisionPolicyDo(t, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			after, err := s.credentialVault.FreezeForPolicy(s.proxy.CurrentPolicy())
			require.NoError(t, err)
			require.Equal(t, frozen.Exists(), after.Exists())
			require.Equal(t, frozen.Snapshot(), after.Snapshot())
			require.NoError(t, s.credentialVault.ValidatePolicySnapshot(frozen))
			require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
		})
	}
}

func TestRevisionPolicyRejectsBoundDenyBeforeEffects(t *testing.T) {
	for _, kind := range []string{"post", "patch", "delete", "reset", "always-deny"} {
		t.Run(kind, func(t *testing.T) {
			s, _, session, nft := revisionPolicyFixture(t)
			revisionIntegrationCreateRendered(t, s, "private-pinned-policy-token")
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			old := s.revisionRecovery.current
			s.nameserverIPs = []netip.Addr{netip.MustParseAddr("192.0.2.53")}
			method, body := http.MethodPost, `{"defaultAction":"deny"}`
			switch kind {
			case "patch":
				method, body = http.MethodPatch, `[{"action":"deny","target":"api.example.com"}]`
			case "delete":
				method, body = http.MethodDelete, `["api.example.com"]`
			case "reset":
				body = ""
			case "always-deny":
				loader := &stagedTestAlwaysLoader{candidateDeny: []policy.EgressRule{mustRule(t, policy.ActionDeny, "api.example.com")}, pending: true}
				s.alwaysLoader = loader
				changed, err := s.reloadAlwaysRules()
				require.Error(t, err)
				require.False(t, changed)
				require.True(t, loader.pending)
			}
			if kind != "always-deny" {
				response := revisionPolicyDo(t, s, method, body)
				require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			}
			require.Same(t, old, s.revisionRecovery.current)
			require.Zero(t, file.saves)
			require.Zero(t, nft.calls)
			require.Zero(t, session.updateCalls)
			require.False(t, s.revisionRecoveryRequired.Load())
		})
	}
}

func TestRevisionPolicyRejectsUnconfirmedIdentity(t *testing.T) {
	for _, field := range []string{"control", "subject", "decision", "vault", "policy", "digest", "zero-uncertain"} {
		t.Run(field, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			old := s.revisionRecovery.current
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				id := mutationIdentity(t, snapshot, epoch)
				switch field {
				case "control":
					id.ControlGeneration = "other"
				case "subject":
					id.SubjectGeneration = "other"
				case "decision":
					id.DecisionEpoch = 0
				case "vault":
					id.VaultRevision++
				case "policy":
					id.PolicyEpoch++
				case "digest":
					id.Digest = "private-invalid-digest"
				case "zero-uncertain":
					return revision.Identity{}, revision.ErrIndeterminate
				}
				return id, nil
			}
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.NotContains(t, response.Body.String(), "private")
			require.Same(t, old, s.revisionRecovery.current)
			require.True(t, s.revisionRecoveryRequired.Load())
			require.Equal(t, 1, nft.calls, "must never replay old nft rules")
			require.Equal(t, 1, nft.quiesces)
			require.Equal(t, 1, stops)
			require.Equal(t, 1, session.closeCalls)
			require.Zero(t, session.reconcileUpdateCalls)
			m.shutdown(time.Millisecond)
			require.Equal(t, 1, session.closeCalls)
		})
	}
}

func TestRevisionPolicyReconcilesOnlyExactAttempt(t *testing.T) {
	for _, outcome := range []string{"committed", "aborted", "unknown", "terminal", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			old := s.revisionRecovery.current
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			var attempt revision.Identity
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				if outcome == "rejected" {
					return revision.Identity{}, revision.ErrPrepareRejected
				}
				attempt = mutationIdentity(t, snapshot, epoch)
				return attempt, revision.ErrIndeterminate
			}
			session.reconcile = func(ctx context.Context, id revision.Identity) (bool, error) {
				require.Equal(t, attempt, id)
				require.Same(t, old, s.revisionRecovery.current)
				switch outcome {
				case "unknown":
					<-ctx.Done()
					return false, ctx.Err()
				case "terminal":
					return false, revision.ErrClosed
				case "aborted":
					return false, nil
				default:
					if session.reconcileUpdateCalls < 3 {
						return false, revision.ErrIndeterminate
					}
					return true, nil
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			response := revisionPolicyDoContext(t, ctx, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, 1, session.updateCalls)
			require.Equal(t, 1, nft.calls)
			if outcome == "committed" {
				require.Equal(t, http.StatusOK, response.Code)
				require.Equal(t, 3, session.reconcileUpdateCalls)
				require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
				require.Zero(t, stops)
			} else {
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
				require.Same(t, old, s.revisionRecovery.current)
				require.True(t, s.revisionRecoveryRequired.Load(), "nft success makes even a confirmed receiver abort sticky")
				require.Equal(t, 1, stops)
				require.Equal(t, 1, session.closeCalls)
			}
		})
	}
}

func TestRevisionPolicyCancellationBoundaries(t *testing.T) {
	for _, point := range []string{"before", "after-save", "after-save-restore-fails", "after-nft", "after-ack", "after-reconcile"} {
		t.Run(point, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			old := s.revisionRecovery.current
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			switch point {
			case "before":
				cancel()
			case "after-save", "after-save-restore-fails":
				file.onSave = cancel
			case "after-nft":
				nft.onApply = func(*policy.NetworkPolicy) { cancel() }
			}
			if point == "after-save-restore-fails" {
				file.restoreErr = errors.New("private-restore-failure")
			}
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				id := mutationIdentity(t, snapshot, epoch)
				if point == "after-reconcile" {
					return id, revision.ErrIndeterminate
				}
				cancel()
				return id, nil
			}
			session.reconcile = func(context.Context, revision.Identity) (bool, error) { cancel(); return true, nil }
			response := revisionPolicyDoContext(t, ctx, s, http.MethodPost, `{"defaultAction":"allow"}`)
			if point == "after-ack" || point == "after-reconcile" {
				require.Equal(t, http.StatusOK, response.Code, "confirmed commit must win cancellation")
				require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
				require.Zero(t, file.restores)
				require.Zero(t, stops)
				return
			}
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Same(t, old, s.revisionRecovery.current)
			require.Zero(t, session.updateCalls)
			sticky := point == "after-nft" || point == "after-save-restore-fails"
			require.Equal(t, sticky, s.revisionRecoveryRequired.Load())
			require.Equal(t, map[bool]int{true: 1, false: 0}[sticky], stops)
			if point == "before" {
				require.Zero(t, file.saves)
			}
			if strings.HasPrefix(point, "after-save") {
				require.Equal(t, 1, file.restores)
				require.Zero(t, nft.calls)
			}
			if point == "after-nft" {
				require.Equal(t, 1, nft.calls)
				require.Zero(t, file.restores, "committed kernel effects cannot be undone by restoring a file")
			}
		})
	}
}

func TestRevisionPolicyPreflightRequiresReadySession(t *testing.T) {
	for _, kind := range []string{"unattached", "stopping", "not-ready", "missing-session", "missing-stop", "overflow", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			want := http.StatusServiceUnavailable
			switch kind {
			case "unattached":
				s.mitm = nil
			case "stopping":
				m.stopping = true
			case "not-ready":
				s.mitmGate.SetReady(false)
			case "missing-session":
				m.revisionSession = nil
			case "missing-stop":
				m.revisionOwner.stop = nil
			case "overflow":
				s.revisionRecovery.current.epoch = math.MaxInt64
				want = http.StatusBadRequest
			case "oversized":
				session.config.MaxSnapshotBytes = 1
				want = http.StatusBadRequest
			}
			old := s.revisionRecovery.current
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, want, response.Code, response.Body.String())
			require.Same(t, old, s.revisionRecovery.current)
			require.Zero(t, file.saves)
			require.Zero(t, nft.calls)
			require.Zero(t, session.updateCalls)
			require.Zero(t, session.closeCalls)
			require.False(t, s.revisionRecoveryRequired.Load())
		})
	}
}

func TestRevisionPolicySameDigestDoesNotMixCandidateEffects(t *testing.T) {
	s, _, session, nft := revisionPolicyFixture(t)
	base := s.revisionRecovery.current
	firstInputs := effectivePolicyInputs{user: testCredentialVaultPolicy(t, `{"defaultAction":"allow"}`)}
	secondInputs := effectivePolicyInputs{user: testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"second.example.com"}]}`)}
	first, err := base.Prepare(s.credentialVault, firstInputs)
	require.NoError(t, err)
	second, err := base.Prepare(s.credentialVault, secondInputs)
	require.NoError(t, err)
	require.Equal(t, first.Digest(), second.Digest(), "wire digest intentionally does not identify Policy rules")
	// Fail before nft executes, then retry a distinct candidate at the same epoch.
	nft.err = &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: errors.New("not started")}
	response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Zero(t, session.updateCalls)
	require.Same(t, base, s.revisionRecovery.current)
	nft.err = nil
	response = revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"deny","egress":[{"action":"allow","target":"second.example.com"}]}`)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, 2, nft.calls)
	require.Equal(t, policy.ActionDeny, nft.applied.Evaluate("unrelated.example.com"))
	require.Equal(t, policy.ActionAllow, nft.applied.Evaluate("second.example.com"))
	require.Equal(t, policy.ActionDeny, s.proxy.CurrentPolicy().Evaluate("unrelated.example.com"))
	require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
	require.ErrorIs(t, s.revisionRecovery.current.Validate(s.credentialVault, first), errStaleEffectivePolicyCandidate)
}

func TestRevisionPolicyThenVaultReloadThenVaultKeepsEpoch(t *testing.T) {
	s, _, session, _ := revisionPolicyFixture(t)
	revisionIntegrationCreateRendered(t, s, "private-initial")
	var epochs []int64
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		epochs = append(epochs, epoch)
		return mutationIdentity(t, snapshot, epoch), nil
	}
	require.Equal(t, http.StatusOK, revisionPolicyDo(t, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`).Code)
	patch := func(expected int64, token string) {
		body, err := json.Marshal(credentialvault.MutationRequest{ExpectedRevision: &expected, Credentials: &credentialvault.CredentialMutationSet{Replace: integrationVaultRequest(token).Credentials}})
		require.NoError(t, err)
		response := liveWriteDo(t, s, http.MethodPatch, string(body))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
	patch(1, "private-second")
	loader := &stagedTestAlwaysLoader{candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "reload.example.com")}, pending: true}
	s.alwaysLoader = loader
	changed, err := s.reloadAlwaysRules()
	require.NoError(t, err)
	require.True(t, changed)
	patch(2, "private-third")
	require.Equal(t, []int64{1, 1, 2, 2}, epochs)
	snapshot, epoch, _, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), epoch)
	require.Equal(t, int64(3), snapshot.Revision)
}

func TestRevisionPolicyNoOpsCannotBypassRecoveryOrReadiness(t *testing.T) {
	for _, state := range []string{"healthy", "recovery", "not-ready"} {
		t.Run(state, func(t *testing.T) {
			s, _, session, nft := revisionPolicyFixture(t)
			s.alwaysLoader = &stagedTestAlwaysLoader{}
			old := s.revisionRecovery.current
			switch state {
			case "recovery":
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			case "not-ready":
				s.mitmGate.SetReady(false)
			}
			response := revisionPolicyDo(t, s, http.MethodDelete, `["missing.example.com"]`)
			changed, err := s.reloadAlwaysRules()
			require.False(t, changed)
			if state == "healthy" {
				require.Equal(t, http.StatusOK, response.Code)
				require.NoError(t, err)
			} else {
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
				require.Error(t, err)
			}
			require.Same(t, old, s.revisionRecovery.current)
			require.Zero(t, nft.calls)
			require.Zero(t, session.updateCalls)
		})
	}
}

// Hold the loader after its apply callback returned, before accepted metadata
// and proxy publication. The process lease must span this entire interval.
type revisionPolicyFinalizationLoader struct {
	recoveryLockedAlwaysLoader
	afterApply func()
	onSet      func()
}

func (l *revisionPolicyFinalizationLoader) RefreshIfDueWithApply(now time.Time, apply func([]policy.EgressRule, []policy.EgressRule) error) ([]policy.EgressRule, []policy.EgressRule, bool, error) {
	return l.recoveryLockedAlwaysLoader.RefreshIfDueWithApply(now, func(deny, allow []policy.EgressRule) error {
		if err := apply(deny, allow); err != nil {
			return err
		}
		if l.afterApply != nil {
			l.afterApply()
		}
		return nil
	})
}

func (l *revisionPolicyFinalizationLoader) SetCurrentRules(deny, allow []policy.EgressRule) {
	if l.onSet != nil {
		l.onSet()
	}
	l.recoveryLockedAlwaysLoader.SetCurrentRules(deny, allow)
}

func TestRevisionPolicyReloadHoldsLeaseThroughFinalization(t *testing.T) {
	s, m, session, _ := revisionPolicyFixture(t)
	old := s.revisionRecovery.current
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	loader := &revisionPolicyFinalizationLoader{recoveryLockedAlwaysLoader: recoveryLockedAlwaysLoader{t: t, stagedTestAlwaysLoader: stagedTestAlwaysLoader{candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "reload.example.com")}, pending: true}}}
	loader.afterApply = func() { close(entered); <-release }
	assertLease := func() {
		locked := !m.mu.TryLock()
		if !locked {
			m.mu.Unlock()
		}
		require.True(t, locked, "reload finalization must still own the exact process")
		require.Same(t, old, s.revisionRecovery.current)
	}
	loader.onSet = assertLease
	s.alwaysLoader = loader
	s.proxy = &revisionPolicyObservedProxy{stubProxy: stubProxy{updated: s.proxy.CurrentPolicy()}, onAlways: assertLease}
	done := make(chan error, 1)
	go func() { _, err := s.reloadAlwaysRules(); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reload did not reach post-ACK finalization")
	}
	require.Equal(t, 1, session.updateCalls)
	assertLease()
	locked := !s.mu.TryLock()
	if !locked {
		s.mu.Unlock()
	}
	require.True(t, locked)
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("reload did not finish")
	}
	require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
}

func TestRevisionPolicyFileAndNftFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		fileState                        policy.FileMutationState
		snapshotFail, saveFail           bool
		restoreState                     policy.FileMutationState
		restoreFail                      bool
		nftEffect                        nftables.ApplyEffect
		nftFail, sticky                  bool
		wantSaves, wantRestores, wantNft int
	}{
		{name: "snapshot", snapshotFail: true},
		{name: "save-unchanged", fileState: policy.FileUnchanged, saveFail: true, wantSaves: 1},
		{name: "save-unknown-restored", fileState: policy.FileUnknown, saveFail: true, restoreState: policy.FileCommitted, sticky: true, wantSaves: 1, wantRestores: 1},
		{name: "save-unconfirmed-success", fileState: policy.FileUnknown, restoreState: policy.FileCommitted, sticky: true, wantSaves: 1, wantRestores: 1},
		{name: "save-unchanged-success-invalid", fileState: policy.FileUnchanged, restoreState: policy.FileCommitted, sticky: true, wantSaves: 1, wantRestores: 1},
		{name: "nft-unchanged-restored", fileState: policy.FileCommitted, restoreState: policy.FileCommitted, nftFail: true, nftEffect: nftables.ApplyUnchanged, wantSaves: 1, wantRestores: 1, wantNft: 1},
		{name: "nft-unchanged-restore-fails", fileState: policy.FileCommitted, restoreState: policy.FileCommitted, restoreFail: true, nftFail: true, nftEffect: nftables.ApplyUnchanged, sticky: true, wantSaves: 1, wantRestores: 1, wantNft: 1},
		{name: "nft-unchanged-restore-unconfirmed", fileState: policy.FileCommitted, restoreState: policy.FileUnknown, nftFail: true, nftEffect: nftables.ApplyUnchanged, sticky: true, wantSaves: 1, wantRestores: 1, wantNft: 1},
		{name: "nft-unchanged-restore-unchanged", fileState: policy.FileCommitted, restoreState: policy.FileUnchanged, nftFail: true, nftEffect: nftables.ApplyUnchanged, sticky: true, wantSaves: 1, wantRestores: 1, wantNft: 1},
		{name: "nft-unknown-restored", fileState: policy.FileCommitted, restoreState: policy.FileCommitted, nftFail: true, nftEffect: nftables.ApplyUnknown, sticky: true, wantSaves: 1, wantRestores: 1, wantNft: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
			fence := runtimeQuarantineFixture(s)
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			file := &faultPolicyStore{state: tc.fileState, restoreState: &tc.restoreState}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			injected := errors.New("private-effect-diagnostic")
			if tc.snapshotFail {
				file.snapshotErr = injected
			}
			if tc.saveFail {
				file.saveErr = injected
			}
			if tc.restoreFail {
				file.restoreErr = injected
			}
			if tc.nftFail {
				nft.err = &nftables.ApplyError{Effect: tc.nftEffect, Err: injected}
			}
			file.onRestore = func() {
				unknownBeforeRestore := tc.fileState == policy.FileUnknown || (!tc.saveFail && tc.fileState != policy.FileCommitted && !tc.snapshotFail) || (tc.nftFail && tc.nftEffect == nftables.ApplyUnknown)
				require.Equal(t, unknownBeforeRestore, s.revisionRecoveryRequired.Load(), "unknown effects must latch before restoration")
			}
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, http.StatusInternalServerError, response.Code)
			require.NotContains(t, response.Body.String(), "private-effect-diagnostic")
			require.Same(t, oldBase, s.revisionRecovery.current)
			require.Same(t, oldProxy, s.proxy.CurrentPolicy())
			require.Equal(t, tc.wantSaves, file.saves)
			require.Equal(t, tc.wantRestores, file.restores)
			require.Equal(t, tc.wantNft, nft.calls)
			require.Zero(t, session.updateCalls)
			require.Equal(t, tc.sticky, s.revisionRecoveryRequired.Load())
			if tc.sticky {
				require.Equal(t, 1, stops)
				require.Equal(t, 1, session.closeCalls)
				require.Equal(t, 1, nft.quiesces)
				require.Equal(t, []string{"namespace", "ensure", "namespace"}, fence.calls, "one failure must not repeat containment")
				_, _, _, err := s.captureRevisionBootstrap(context.Background())
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
			} else {
				require.Zero(t, stops)
				require.Zero(t, session.closeCalls)
				require.Empty(t, fence.calls)
			}
		})
	}
}

func TestRevisionPolicyNoNftConfirmedRejectionRequiresFileRestore(t *testing.T) {
	for _, restore := range []string{"confirmed", "failed", "unknown"} {
		t.Run(restore, func(t *testing.T) {
			s, m, session, _ := revisionPolicyFixture(t)
			s.nft = nil
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			if restore == "failed" {
				file.restoreErr = errors.New("restore failed")
			}
			if restore == "unknown" {
				state := policy.FileUnknown
				file.restoreState = &state
			}
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
				return revision.Identity{}, revision.ErrPrepareRejected
			}
			old := s.revisionRecovery.current
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Same(t, old, s.revisionRecovery.current)
			require.Equal(t, 1, file.restores)
			require.Equal(t, restore != "confirmed", s.revisionRecoveryRequired.Load())
			require.Equal(t, map[bool]int{true: 1, false: 0}[restore != "confirmed"], stops)
		})
	}
}

type revisionPolicyRotatingSource struct{ calls int }

func (*revisionPolicyRotatingSource) Type() string { return "policy-rotation-test" }
func (s *revisionPolicyRotatingSource) Resolve(context.Context) (string, error) {
	s.calls++
	if s.calls == 1 {
		return "private-pinned-first", nil
	}
	return "private-must-not-read-second", nil
}

func TestRevisionPolicyDoesNotResolvePinnedCredentialAgain(t *testing.T) {
	s, _, session, _ := revisionPolicyFixture(t)
	source := &revisionPolicyRotatingSource{}
	registry := credentialvault.NewSourceRegistry()
	registry.Register(source.Type(), func(json.RawMessage) (credentialvault.CredentialSource, error) { return source, nil })
	s.credentialVault = credentialvault.NewStoreWithRegistry(nil, func() bool { return true }, registry)
	request := integrationVaultRequest("unused")
	request.Credentials[0].Source = json.RawMessage(`{"type":"policy-rotation-test"}`)
	candidate, err := s.credentialVault.PrepareCreate(request, s.proxy.CurrentPolicy())
	require.NoError(t, err)
	before, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = s.credentialVault.CommitCandidate(candidate)
	require.NoError(t, err)
	require.Equal(t, 1, source.calls)
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		require.Equal(t, before, snapshot)
		return mutationIdentity(t, snapshot, epoch), nil
	}
	require.Equal(t, http.StatusOK, revisionPolicyDo(t, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`).Code)
	require.Equal(t, 1, source.calls)
	after, err := s.credentialVault.FreezeForPolicy(s.proxy.CurrentPolicy())
	require.NoError(t, err)
	require.Equal(t, before, after.Snapshot())
}

func TestRevisionPolicyFinalValidationRejectsStaleBaseAndVaultABA(t *testing.T) {
	for _, kind := range []string{"base", "store", "vault-ABA"} {
		t.Run(kind, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			oldProxy := s.proxy.CurrentPolicy()
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				switch kind {
				case "base":
					base, err := newEffectivePolicyBase(s.revisionRecovery.current.inputs, s.revisionRecovery.current.epoch)
					require.NoError(t, err)
					s.revisionRecovery.current = base
				case "store":
					s.credentialVault = credentialvault.NewStore(nil, nil)
				case "vault-ABA":
					_, err := s.credentialVault.Create(credentialvault.CreateRequest{}, oldProxy)
					require.NoError(t, err)
					require.NoError(t, s.credentialVault.Delete())
				}
				return mutationIdentity(t, snapshot, epoch), nil
			}
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Same(t, oldProxy, s.proxy.CurrentPolicy())
			require.Zero(t, s.revisionRecovery.current.epoch)
			require.Equal(t, 1, nft.calls)
			require.Zero(t, file.restores, "never restore old state after confirmed receiver publication")
			require.Equal(t, 1, stops)
			require.Equal(t, 1, session.closeCalls)
			require.True(t, s.revisionRecoveryRequired.Load())
		})
	}
}

func TestRevisionPolicyCleanupRetainsBothBarriers(t *testing.T) {
	for _, contender := range []string{"policy", "vault", "reload", "bootstrap", "exit", "shutdown"} {
		t.Run(contender, func(t *testing.T) {
			s, m, session, _ := revisionPolicyFixture(t)
			s.alwaysLoader = &stagedTestAlwaysLoader{}
			stopping, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			running := m.running
			stops := 0
			m.revisionOwner.stop = func(got *mitmproxy.Running) {
				require.Same(t, running, got)
				require.Nil(t, m.running)
				require.Nil(t, m.revisionSession)
				require.True(t, s.mitmGate.MitmPending())
				stops++
				close(stopping)
				<-release
			}
			session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
				return revision.Identity{}, revision.ErrPrepareRejected
			}
			done := make(chan int, 1)
			go func() { done <- revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`).Code }()
			select {
			case <-stopping:
			case <-time.After(time.Second):
				t.Fatal("terminal stop did not start")
			}
			policyLocked := !s.mu.TryLock()
			if !policyLocked {
				s.mu.Unlock()
			}
			processLocked := !m.mu.TryLock()
			if !processLocked {
				m.mu.Unlock()
			}
			require.True(t, policyLocked)
			require.True(t, processLocked)
			entered, contended := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(contended)
				close(entered)
				switch contender {
				case "policy":
					require.Equal(t, http.StatusServiceUnavailable, revisionPolicyDo(t, s, http.MethodDelete, `["missing.example.com"]`).Code)
				case "vault":
					require.Equal(t, http.StatusServiceUnavailable, liveWriteDo(t, s, http.MethodPost, `{}`).Code)
				case "reload":
					_, err := s.reloadAlwaysRules()
					require.ErrorIs(t, err, errRevisionRecoveryRequired)
				case "bootstrap":
					_, _, _, err := s.captureRevisionBootstrap(context.Background())
					require.ErrorIs(t, err, errRevisionRecoveryRequired)
				case "exit":
					m.closeRevisionSession(m.currentGen)
				case "shutdown":
					m.shutdown(time.Millisecond)
				}
			}()
			<-entered
			// Both actual mutexes were probed while stop was blocked; no timing
			// window or assumed scheduler delay is needed to prove exclusion.
			select {
			case <-contended:
				t.Fatal("contender passed terminal cleanup")
			default:
			}
			unblock()
			select {
			case code := <-done:
				require.Equal(t, http.StatusServiceUnavailable, code)
			case <-time.After(time.Second):
				t.Fatal("mutation did not finish cleanup")
			}
			select {
			case <-contended:
			case <-time.After(time.Second):
				t.Fatal("contender did not resume")
			}
			require.Equal(t, 1, stops)
			require.Equal(t, 1, session.closeCalls)
		})
	}
}

func TestRevisionPolicyShutdownSignalCancelsIPCWithoutProcessLease(t *testing.T) {
	for _, kind := range []string{"policy", "vault", "reload"} {
		t.Run(kind, func(t *testing.T) {
			s, m, session, _ := revisionPolicyFixture(t)
			m.shutdownCh = make(chan struct{})
			m.revisionOwner.stop = func(*mitmproxy.Running) {}
			entered := make(chan struct{})
			session.update = func(ctx context.Context, _ credentialvault.ActiveSnapshot, _ int64) (revision.Identity, error) {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), revisionMutationTimeout)
				close(entered)
				<-ctx.Done()
				return revision.Identity{}, ctx.Err()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch kind {
				case "policy":
					require.Equal(t, http.StatusServiceUnavailable, revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`).Code)
				case "vault":
					require.Equal(t, http.StatusServiceUnavailable, liveWriteDo(t, s, http.MethodPost, `{}`).Code)
				case "reload":
					s.alwaysLoader = &stagedTestAlwaysLoader{candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "reload.example.com")}, pending: true}
					_, err := s.reloadAlwaysRules()
					require.Error(t, err)
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("IPC did not start")
			}
			locked := !m.mu.TryLock()
			if !locked {
				m.mu.Unlock()
			}
			require.True(t, locked)
			m.signalShutdown()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("shutdown signal did not interrupt IPC while the lease was held")
			}
			require.True(t, s.revisionRecoveryRequired.Load())
			require.Equal(t, 1, session.closeCalls)
		})
	}
}

func TestRevisionPolicyEquivalentWritesStillAdvanceEpoch(t *testing.T) {
	s, _, session, nft := revisionPolicyFixture(t)
	for wantEpoch := int64(1); wantEpoch <= 2; wantEpoch++ {
		response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, wantEpoch, s.revisionRecovery.current.epoch)
	}
	require.Equal(t, 2, session.updateCalls)
	require.Equal(t, 2, nft.calls)
}

func TestRevisionPolicyInvalidSessionIdentityDetachesBeforeEffects(t *testing.T) {
	for _, field := range []string{"control", "subject", "size"} {
		t.Run(field, func(t *testing.T) {
			s, m, session, nft := revisionPolicyFixture(t)
			file := &faultPolicyStore{state: policy.FileCommitted}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", file
			old := s.revisionRecovery.current
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			switch field {
			case "control":
				session.config.ControlGeneration = ""
			case "subject":
				session.config.SubjectGeneration = ""
			case "size":
				session.config.MaxSnapshotBytes = 0
			}
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Same(t, old, s.revisionRecovery.current)
			require.Zero(t, file.saves)
			require.Zero(t, nft.calls)
			require.Zero(t, session.updateCalls)
			require.Equal(t, 1, stops)
			require.Equal(t, 1, session.closeCalls)
			require.True(t, s.revisionRecoveryRequired.Load(), "invalid session identity is not merely unready startup")
		})
	}
}

func TestRevisionPolicyPatchDeleteUseAuthoritativeBaseNotMutableProxy(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			s, _, _, nft := revisionPolicyFixture(t)
			// Simulate mutation of the public proxy view by an internal reader.
			// The committed base owns a separate frozen policy and must remain
			// the only source for a subsequent read-modify-write mutation.
			view := s.proxy.CurrentPolicy()
			view.DefaultAction = policy.ActionAllow
			view.Egress[0].Target = "unpublished.example.com"
			body := `[{"action":"allow","target":"new.example.com"}]`
			if method == http.MethodDelete {
				body = `["spare.example.com"]`
			}
			response := revisionPolicyDo(t, s, method, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			for _, pol := range []*policy.NetworkPolicy{s.proxy.CurrentPolicy(), s.revisionRecovery.current.inputs.user, nft.applied} {
				require.Equal(t, policy.ActionDeny, pol.DefaultAction)
				require.Equal(t, policy.ActionAllow, pol.Evaluate("api.example.com"))
				require.Equal(t, policy.ActionDeny, pol.Evaluate("unpublished.example.com"))
			}
			if method == http.MethodDelete {
				require.Equal(t, policy.ActionDeny, s.proxy.CurrentPolicy().Evaluate("spare.example.com"))
			} else {
				require.Equal(t, policy.ActionAllow, s.proxy.CurrentPolicy().Evaluate("new.example.com"))
			}
		})
	}
}

func TestRevisionPolicyAPIIgnoresUnpublishedLoaderOverlay(t *testing.T) {
	s, _, session, nft := revisionPolicyFixture(t)
	revisionIntegrationCreateRendered(t, s, "private-authoritative-token")
	// This loader view has not passed its reload callback. Importing it from a
	// Policy API write could both invalidate the binding and grant extra egress.
	s.alwaysLoader = &stagedTestAlwaysLoader{
		deny:  []policy.EgressRule{mustRule(t, policy.ActionDeny, "api.example.com")},
		allow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "unpublished.example.com")},
	}
	response := revisionPolicyDo(t, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, policy.ActionAllow, nft.applied.Evaluate("api.example.com"))
	require.Equal(t, policy.ActionDeny, nft.applied.Evaluate("unpublished.example.com"))
	require.Empty(t, s.revisionRecovery.current.inputs.alwaysDeny)
	require.Empty(t, s.revisionRecovery.current.inputs.alwaysAllow)
	require.Equal(t, 1, session.updateCalls)
}
