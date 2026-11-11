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
	"net/http"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// These exercise the real owner and nft Manager classification paths. The
// boundary fixture records containment requests; it is not packet-isolation
// evidence, which requires the separate privileged runtime tests.
func TestRuntimeQuarantineAlwaysRulesUsesExistingEffectClassification(t *testing.T) {
	for _, outcome := range []string{"success", "unchanged", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			fence := runtimeQuarantineFixture(s)
			loader := &stagedTestAlwaysLoader{
				candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "new.example.com")},
				pending:        true,
			}
			s.alwaysLoader = loader
			var applyErr error
			switch outcome {
			case "unchanged":
				applyErr = &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: errors.New("nft did not start")}
			case "unknown":
				applyErr = errors.New("nft result lost after start")
			}
			nftCalls := 0
			manager := nftables.NewManagerWithRunnerAndOptions(func(context.Context, string) ([]byte, error) {
				nftCalls++
				require.Empty(t, fence.calls, "normal always-rule publication must not preemptively fence")
				return nil, applyErr
			}, nftables.Options{QuiesceOnApplyFailure: true})
			s.nft = manager
			oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)

			changed, err := s.reloadAlwaysRules()
			require.Equal(t, 1, nftCalls)
			if outcome == "success" {
				require.NoError(t, err)
				require.True(t, changed)
				require.False(t, loader.pending)
				require.NotSame(t, oldBase, s.revisionRecovery.current)
				require.Equal(t, loader.allow, s.proxy.(*stubProxy).allow)
			} else {
				require.Error(t, err)
				require.False(t, changed)
				require.True(t, loader.pending)
				require.Empty(t, loader.allow)
				require.Empty(t, s.proxy.(*stubProxy).allow)
				require.Same(t, oldBase, s.revisionRecovery.current)
				require.Same(t, oldProxy, s.proxy.CurrentPolicy())
			}
			if outcome == "unknown" {
				require.Equal(t, nftables.ApplyUnknown, nftables.ApplyEffectOf(err))
				require.True(t, s.revisionRecoveryRequired.Load())
				require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
				require.Equal(t, []string{"namespace", "ensure", "namespace"}, fence.calls)
				require.Equal(t, http.StatusServiceUnavailable, recoveryHealthzProbe(s).Code)
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
				require.ErrorIs(t, manager.AddResolvedDomain(context.Background(), "api.example.com", quiescenceIPs()), nftables.ErrQuiesced)
				applyErr = nil
				changed, err = s.reloadAlwaysRules()
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
				require.False(t, changed)
				require.Equal(t, 1, nftCalls, "recovery must reject a clean retry before another effect")
				return
			}
			require.False(t, s.revisionRecoveryRequired.Load())
			require.Equal(t, revisionRecoveryNone, s.revisionRecovery.reason)
			require.Empty(t, fence.calls)
			require.Equal(t, quarantineInactive, packetQuarantineState(s.quarantine.state.Load()))
			require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code)
			// #2167 invalidates attempted-publication tickets even when nft did
			// not start, without latching recovery or changing active state.
			require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
			if outcome == "unchanged" {
				require.Equal(t, nftables.ApplyUnchanged, nftables.ApplyEffectOf(err))
				applyErr = nil
				changed, err = s.reloadAlwaysRules()
				require.NoError(t, err)
				require.True(t, changed)
				require.Equal(t, 2, nftCalls)
				require.NotSame(t, oldBase, s.revisionRecovery.current)
				require.Empty(t, fence.calls)
			}
		})
	}
}

func TestRuntimeQuarantineAtomicPolicyFailuresUseExistingRecovery(t *testing.T) {
	for _, outcome := range []string{"snapshot", "pre-rename", "post-rename-restored", "post-rename-restore-failed"} {
		t.Run(outcome, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			fence := runtimeQuarantineFixture(s)
			nftCalls := 0
			manager := nftables.NewManagerWithRunner(func(context.Context, string) ([]byte, error) {
				nftCalls++
				require.Empty(t, fence.calls)
				return nil, nil
			})
			s.nft = manager
			store := &faultPolicyStore{state: policy.FileUnchanged}
			s.policyFile, s.atomicPolicyFile = "injected-policy.json", store
			cause := errors.New("injected file failure")
			switch outcome {
			case "snapshot":
				store.snapshotErr = &policy.FileMutationError{Phase: "directory-sync-preflight", Err: cause}
			case "pre-rename":
				store.saveErr = &policy.FileMutationError{Phase: "file-sync", Err: cause}
			default:
				store.state = policy.FileUnknown
				store.saveErr = &policy.FileMutationError{Phase: "directory-sync", Err: cause}
				if outcome == "post-rename-restore-failed" {
					store.restoreErr = errors.New("restore also failed")
				}
			}
			store.onSave = func() { require.Empty(t, fence.calls, "file preparation must not introduce a fence") }
			store.onRestore = func() {
				require.True(t, s.revisionRecoveryRequired.Load(), "existing recovery must latch before best-effort restore")
				require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
				require.Equal(t, []string{"namespace", "ensure", "namespace"}, fence.calls)
				require.ErrorIs(t, manager.AddResolvedDomain(context.Background(), "api.example.com", quiescenceIPs()), nftables.ErrQuiesced)
			}
			oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)

			response := applyEffectRequest(s, http.MethodPost)
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			require.Zero(t, nftCalls, "file failure precedes static nft publication")
			require.Same(t, oldBase, s.revisionRecovery.current)
			require.Same(t, oldProxy, s.proxy.CurrentPolicy())
			if outcome == "post-rename-restored" || outcome == "post-rename-restore-failed" {
				require.Equal(t, 1, store.saves)
				require.Equal(t, 1, store.restores)
				require.True(t, s.revisionRecoveryRequired.Load())
				require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
				require.Equal(t, []string{"namespace", "ensure", "namespace"}, fence.calls)
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
				require.Equal(t, http.StatusServiceUnavailable, recoveryHealthzProbe(s).Code)
				require.Equal(t, http.StatusServiceUnavailable, applyEffectRequest(s, http.MethodPost).Code)
				require.Equal(t, 1, store.saves, "even successful restoration must not reopen publication")
				require.Zero(t, nftCalls)
				return
			}
			require.False(t, s.revisionRecoveryRequired.Load())
			require.Equal(t, revisionRecoveryNone, s.revisionRecovery.reason)
			require.Zero(t, store.restores)
			require.Empty(t, fence.calls)
			require.Equal(t, quarantineInactive, packetQuarantineState(s.quarantine.state.Load()))
			require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code)
			require.NoError(t, validateRecoveryTicket(s, ticket), "known pre-effect file failures preserve the bootstrap ticket")
			if outcome == "snapshot" {
				require.Zero(t, store.saves)
			} else {
				require.Equal(t, 1, store.saves)
			}
			store.snapshotErr, store.saveErr, store.state = nil, nil, policy.FileCommitted
			require.Equal(t, http.StatusOK, applyEffectRequest(s, http.MethodPost).Code)
			require.Equal(t, 1, nftCalls)
			require.NotSame(t, oldBase, s.revisionRecovery.current)
			require.Empty(t, fence.calls)
		})
	}
}
