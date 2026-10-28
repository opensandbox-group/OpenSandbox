// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

type runtimeQuarantineFake struct {
	calls       []string
	fail        string
	checks      int
	failCheckAt int
}

func (f *runtimeQuarantineFake) Check() error {
	f.calls = append(f.calls, "namespace")
	f.checks++
	if f.fail == "namespace" || (f.failCheckAt > 0 && f.checks >= f.failCheckAt) {
		return errors.New("original namespace mismatch")
	}
	return nil
}
func (f *runtimeQuarantineFake) Close() error { return nil }
func (f *runtimeQuarantineFake) Ensure(context.Context) error {
	f.calls = append(f.calls, "ensure")
	if f.fail == "ensure" {
		return errors.New("installation unconfirmed")
	}
	return nil
}
func runtimeQuarantineFixture(s *policyServer) *runtimeQuarantineFake {
	f := &runtimeQuarantineFake{}
	s.quarantine = &revisionQuarantine{fence: f, target: f}
	return f
}
func TestRuntimeQuarantineRequiresOriginalNamespaceAndReadback(t *testing.T) {
	for _, stage := range []string{"success", "namespace", "ensure", "after-readback"} {
		t.Run(stage, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			f := runtimeQuarantineFixture(s)
			f.fail = stage
			if stage == "after-readback" {
				f.failCheckAt = 2
			}
			s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			require.True(t, s.revisionRecoveryRequired.Load())
			require.ErrorIs(t, s.revisionRecovery.recoveryErrorLocked(), errRevisionRecoveryRequired)
			want := quarantineUnknown
			if stage == "success" {
				want = quarantineConfirmed
			}
			require.Equal(t, want, packetQuarantineState(s.quarantine.state.Load()))
			if stage == "namespace" {
				require.NotContains(t, f.calls, "ensure")
			} else {
				require.Contains(t, f.calls, "ensure")
			}
			if stage == "success" {
				require.Equal(t, []string{"namespace", "ensure", "namespace"}, f.calls)
			}
		})
	}
}
func TestRuntimeQuarantineNewFailureClearsOldConfirmation(t *testing.T) {
	s := recoveryPolicyFixture(t)
	f := runtimeQuarantineFixture(s)
	s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
	require.Equal(t, quarantineConfirmed, packetQuarantineState(s.quarantine.state.Load()))
	f.fail = "ensure"
	s.requireRevisionRecoveryLocked(revisionRecoveryUnknown)
	require.Equal(t, quarantineUnknown, packetQuarantineState(s.quarantine.state.Load()))
	require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
}
func TestRuntimeQuarantineNormalVaultOutcomesDoNotFence(t *testing.T) {
	for _, outcome := range []string{"success", "prepare-rejected", "readback-aborted"} {
		t.Run(outcome, func(t *testing.T) {
			s, m, session := mutationFixture(t)
			f := runtimeQuarantineFixture(s)
			switch outcome {
			case "prepare-rejected":
				session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
					return revision.Identity{}, revision.ErrPrepareRejected
				}
			case "readback-aborted":
				session.update = func(_ context.Context, snap credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
					return mutationIdentity(t, snap, epoch), revision.ErrIndeterminate
				}
				session.reconcile = func(context.Context, revision.Identity) (bool, error) { return false, nil }
			}
			_, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
			if outcome == "success" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, revision.ErrPrepareRejected)
			}
			require.Empty(t, f.calls)
			require.False(t, s.revisionRecoveryRequired.Load())
			require.Equal(t, quarantineInactive, packetQuarantineState(s.quarantine.state.Load()))
		})
	}
}
func TestRuntimeQuarantinePolicyUsesExistingEffectClassification(t *testing.T) {
	for _, effect := range []string{"success", "unchanged", "unknown"} {
		t.Run(effect, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			f := runtimeQuarantineFixture(s)
			s.nft = nftables.NewManagerWithRunner(func(context.Context, string) ([]byte, error) {
				require.Empty(t, f.calls, "ordinary publication must not preemptively fence")
				switch effect {
				case "unchanged":
					return nil, &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: errors.New("not started")}
				case "unknown":
					return nil, errors.New("lost result")
				default:
					return nil, nil
				}
			})
			w := httptest.NewRecorder()
			s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
			if effect == "unknown" {
				require.True(t, s.revisionRecoveryRequired.Load())
				require.Contains(t, f.calls, "ensure")
				require.Equal(t, quarantineConfirmed, packetQuarantineState(s.quarantine.state.Load()))
			} else {
				require.False(t, s.revisionRecoveryRequired.Load())
				require.Empty(t, f.calls)
				if effect == "success" {
					require.Equal(t, http.StatusOK, w.Code)
				}
			}
		})
	}
}
func TestRuntimeQuarantineOnlyCurrentOwnedChildExitContains(t *testing.T) {
	for _, which := range []string{"current", "old", "detached", "stopping"} {
		t.Run(which, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			f := runtimeQuarantineFixture(s)
			m := &mitmTransparent{currentGen: 7, running: &mitmproxy.Running{}, revisionOwner: &revisionLaunchOwner{server: s}}
			gen := uint64(7)
			switch which {
			case "old":
				gen = 6
			case "detached":
				m.running = nil
			case "stopping":
				m.stopping = true
			}
			m.recordExit(gen)
			if which == "current" {
				require.Contains(t, f.calls, "ensure")
				require.True(t, s.revisionRecoveryRequired.Load())
			} else {
				require.Empty(t, f.calls)
				require.False(t, s.revisionRecoveryRequired.Load())
			}
		})
	}
}
func TestRuntimeQuarantineBlocksStaleBootstrap(t *testing.T) {
	s := recoveryPolicyFixture(t)
	runtimeQuarantineFixture(s)
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	m, result := quiescencePendingResult(s, ticket)
	s.requireRevisionRecoveryLocked(revisionRecoveryUnknown)
	require.ErrorIs(t, publishRevisionReady(context.Background(), m, result), errRevisionRecoveryRequired)
	require.True(t, s.mitmGate.MitmPending())
	require.Nil(t, m.running)
}

func TestRuntimeQuarantineActivationPreservesLegacyAndDNSOnly(t *testing.T) {
	for _, cfg := range []struct{ flag, mode string }{{"false", "dns+nft"}, {"true", "dns"}, {"true", "invalid"}} {
		t.Run(cfg.flag+"/"+cfg.mode, func(t *testing.T) {
			t.Setenv("OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME", cfg.flag)
			t.Setenv("OPENSANDBOX_EGRESS_MODE", cfg.mode)
			q, err := newRevisionQuarantine()
			require.NoError(t, err)
			require.Nil(t, q)
		})
	}
}
