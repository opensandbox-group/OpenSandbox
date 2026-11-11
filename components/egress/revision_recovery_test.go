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
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func recoveryTestOwner(t *testing.T, inputs effectivePolicyInputs, store *credentialvault.Store) *policyServer {
	t.Helper()
	s := &policyServer{credentialVault: store}
	s.mu.Lock()
	err := s.initRevisionRecoveryLocked(inputs)
	s.mu.Unlock()
	require.NoError(t, err)
	return s
}

func validateRecoveryTicket(s *policyServer, ticket *revisionBootstrapTicket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateRevisionBootstrapLocked(ticket)
}

func validateRecoveryCandidate(s *policyServer, candidate *effectivePolicyCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revisionRecovery.current.Validate(s.credentialVault, candidate)
}

func TestRevisionRecoveryAuthoritativeIdentity(t *testing.T) {
	inputs := policyCandidateInputs(t)
	s := recoveryTestOwner(t, inputs, credentialvault.NewStore(nil, nil))
	_, epoch, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.Zero(t, epoch)
	first, err := s.prepareRevisionPolicyCandidate(inputs)
	require.NoError(t, err)
	require.Equal(t, int64(1), first.PolicyEpoch(), "prospective candidates must retain base+1 semantics")
	s.mu.Lock()
	err = s.replaceRevisionBaseLocked(inputs)
	s.mu.Unlock()
	require.NoError(t, err)
	require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
	require.ErrorIs(t, validateRecoveryCandidate(s, first), errStaleEffectivePolicyCandidate)
	second, err := s.prepareRevisionPolicyCandidate(inputs)
	require.NoError(t, err)
	require.Equal(t, first.Digest(), second.Digest(), "decision digest must not substitute for base identity")
	require.Equal(t, first.PolicyEpoch(), second.PolicyEpoch())
	_, epoch, fresh, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.Zero(t, epoch)
	require.NoError(t, validateRecoveryTicket(s, fresh))
	require.Positive(t, unsafe.Sizeof(*fresh.identity), "independent identities must not be zero-sized")
	s.mu.Lock()
	s.invalidateRevisionBootstrapLocked()
	s.mu.Unlock()
	require.ErrorIs(t, validateRecoveryTicket(s, fresh), errStaleRevisionBootstrap)
	require.NoError(t, validateRecoveryCandidate(s, second), "bootstrap invalidation must not publish policy")
	_, _, latest, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.NotSame(t, fresh.identity, latest.identity)
	require.NoError(t, validateRecoveryTicket(s, latest))
}

func TestRevisionRecoveryVaultABA(t *testing.T) {
	for _, kind := range []string{"store replacement", "absent ABA", "existing ABA"} {
		t.Run(kind, func(t *testing.T) {
			inputs := policyCandidateInputs(t)
			store := credentialvault.NewStore(nil, nil)
			if kind == "existing ABA" {
				_, err := store.Create(credentialvault.CreateRequest{}, inputs.user)
				require.NoError(t, err)
			}
			s := recoveryTestOwner(t, inputs, store)
			snapshot, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			candidate, err := s.prepareRevisionPolicyCandidate(inputs)
			require.NoError(t, err)
			s.mu.Lock()
			switch kind {
			case "store replacement":
				s.credentialVault = credentialvault.NewStore(nil, nil)
			case "absent ABA":
				_, err = store.Create(credentialvault.CreateRequest{}, inputs.user)
				if err == nil {
					err = store.Delete()
				}
			case "existing ABA":
				err = store.Delete()
				if err == nil {
					_, err = store.Create(credentialvault.CreateRequest{}, inputs.user)
				}
			}
			s.mu.Unlock()
			require.NoError(t, err)
			require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
			_, _, fresh, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			require.Equal(t, snapshot.Revision, fresh.vault.Snapshot().Revision)
			require.Equal(t, ticket.vault.Exists(), fresh.vault.Exists())
			require.NoError(t, validateRecoveryTicket(s, fresh))
			s.mu.Lock()
			err = s.revisionRecovery.current.Validate(s.credentialVault, candidate)
			s.mu.Unlock()
			if kind == "store replacement" {
				require.ErrorIs(t, err, credentialvault.ErrInvalidCandidate)
				currentCandidate, prepareErr := s.prepareRevisionPolicyCandidate(inputs)
				require.NoError(t, prepareErr)
				require.Same(t, s.credentialVault, currentCandidate.store)
			} else {
				require.ErrorIs(t, err, credentialvault.ErrStaleCandidate)
			}
		})
	}
}

func TestRevisionRecoverySticky(t *testing.T) {
	for _, reason := range []revisionRecoveryReason{revisionRecoveryExternalEffectsUnknown, revisionRecoverySessionCleanupFailed} {
		t.Run(fmt.Sprint(reason), func(t *testing.T) {
			inputs := policyCandidateInputs(t)
			s := recoveryTestOwner(t, inputs, policyCandidateStore(t, inputs))
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			s.mu.Lock()
			s.requireRevisionRecoveryLocked(reason)
			s.mu.Unlock()
			snapshot, epoch, rejected, err := s.captureRevisionBootstrap(context.Background())
			require.ErrorIs(t, err, errRevisionRecoveryRequired)
			require.Empty(t, snapshot)
			require.Zero(t, epoch)
			require.Nil(t, rejected)
			require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
			candidate, err := s.prepareRevisionPolicyCandidate(inputs)
			require.ErrorIs(t, err, errRevisionRecoveryRequired)
			require.Nil(t, candidate)
			// A successful ordinary readback and another invalidation must not
			// clear uncertainty belonging to this Go incarnation.
			_, err = s.credentialVault.ActiveSnapshot()
			require.NoError(t, err)
			s.mu.Lock()
			s.invalidateRevisionBootstrapLocked()
			initErr := s.initRevisionRecoveryLocked(inputs)
			replaceErr := s.replaceRevisionBaseLocked(inputs)
			s.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
			retained := s.revisionRecovery.reason
			s.mu.Unlock()
			require.ErrorIs(t, initErr, errRevisionRecoveryRequired)
			require.ErrorIs(t, replaceErr, errRevisionRecoveryRequired)
			require.Equal(t, reason, retained, "retain the first fixed recovery classification")
			_, _, rejected, err = s.captureRevisionBootstrap(context.Background())
			require.ErrorIs(t, err, errRevisionRecoveryRequired)
			require.Nil(t, rejected)
			require.NotContains(t, err.Error(), "private-policy-candidate")
		})
	}
}

type recoveryRotatingSource struct{ resolves *atomic.Int32 }

func (*recoveryRotatingSource) Type() string { return "recovery-rotating" }
func (s *recoveryRotatingSource) Resolve(context.Context) (string, error) {
	return fmt.Sprintf("private-recovery-snapshot-%d", s.resolves.Add(1)), nil
}

func TestRevisionRecoveryCapturePinnedSnapshot(t *testing.T) {
	var resolves atomic.Int32
	registry := credentialvault.NewSourceRegistry()
	registry.Register("recovery-rotating", func(json.RawMessage) (credentialvault.CredentialSource, error) {
		return &recoveryRotatingSource{resolves: &resolves}, nil
	})
	inputs := policyCandidateInputs(t)
	store := credentialvault.NewStoreWithRegistry(nil, nil, registry)
	request := integrationVaultRequest("unused-inline")
	request.Credentials[0].Source = json.RawMessage(`{"type":"recovery-rotating"}`)
	mutation, err := store.PrepareCreate(request, inputs.user)
	require.NoError(t, err)
	before, err := mutation.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = store.CommitCandidate(mutation)
	require.NoError(t, err)
	s := recoveryTestOwner(t, inputs, store)
	inputs.user.Egress[0].Target = "mutated.example.com"
	for range 3 {
		snapshot, epoch, ticket, err := s.captureRevisionBootstrap(context.Background())
		require.NoError(t, err)
		require.Equal(t, before, snapshot)
		require.Zero(t, epoch)
		require.True(t, ticket.vault.Exists())
		require.Equal(t, before.Revision, ticket.vault.Snapshot().Revision)
		require.Equal(t, int32(1), resolves.Load(), "capture must use the pinned rendered credential bytes")
		snapshot.Bindings[0].Headers[0].Value = "tampered-output"
		require.Equal(t, before, ticket.vault.Snapshot())
		require.NoError(t, validateRecoveryTicket(s, ticket))
		require.NotContains(t, fmt.Sprintf("%v %#v %v %#v", ticket, ticket, *ticket, *ticket), "private-recovery-snapshot")
	}
	_, err = s.prepareRevisionPolicyCandidate(policyCandidateInputs(t))
	require.NoError(t, err)
	require.Equal(t, int32(1), resolves.Load())
}

func TestRevisionRecoveryStartupRejectsInvalidInputsBeforeServing(t *testing.T) {
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvMitmproxyTransparent, "false")
	// The malformed address fails deterministically before any socket operation
	// if startup overlooks recovery initialization.
	srv, handler, err := startPolicyServer(&stubProxy{updated: &policy.NetworkPolicy{DefaultAction: "invalid"}}, nil, "", "invalid-address", "", nil, "", nil, nil, nil)
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.Nil(t, srv)
	require.Nil(t, handler)
}

func TestRevisionRecoveryRejectsUnrenderedVaultWithoutResolving(t *testing.T) {
	var resolves atomic.Int32
	registry := credentialvault.NewSourceRegistry()
	registry.Register("recovery-rotating", func(json.RawMessage) (credentialvault.CredentialSource, error) {
		return &recoveryRotatingSource{resolves: &resolves}, nil
	})
	inputs := policyCandidateInputs(t)
	store := credentialvault.NewStoreWithRegistry(nil, nil, registry)
	request := integrationVaultRequest("unused-inline")
	request.Credentials[0].Source = json.RawMessage(`{"type":"recovery-rotating"}`)
	_, err := store.Create(request, inputs.user)
	require.NoError(t, err)
	s := recoveryTestOwner(t, inputs, store)
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.ErrorIs(t, err, credentialvault.ErrCandidateNotRendered)
	require.ErrorIs(t, err, errInvalidRevisionBootstrap)
	require.Nil(t, ticket)
	require.Zero(t, resolves.Load())
	_, err = store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int32(1), resolves.Load())
	_, _, ticket, err = s.captureRevisionBootstrap(context.Background())
	require.ErrorIs(t, err, credentialvault.ErrCandidateNotRendered)
	require.Nil(t, ticket)
	require.Equal(t, int32(1), resolves.Load())
	require.Equal(t, revisionRecoveryNone, s.revisionRecovery.reason)
}

func TestRevisionRecoveryInvalidInputsPreserveCurrentState(t *testing.T) {
	inputs := policyCandidateInputs(t)
	invalid := policyCandidateInputs(t)
	invalid.user.DefaultAction = "private-invalid-action"
	uninitialized := &policyServer{credentialVault: credentialvault.NewStore(nil, nil)}
	uninitialized.mu.Lock()
	err := uninitialized.initRevisionRecoveryLocked(invalid)
	uninitialized.mu.Unlock()
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.Nil(t, uninitialized.revisionRecovery)
	_, _, ticket, err := uninitialized.captureRevisionBootstrap(context.Background())
	require.ErrorIs(t, err, errInvalidRevisionBootstrap)
	require.Nil(t, ticket)
	s := recoveryTestOwner(t, inputs, credentialvault.NewStore(nil, nil))
	_, _, ticket, err = s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	s.mu.Lock()
	err = s.replaceRevisionBaseLocked(invalid)
	s.mu.Unlock()
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.NoError(t, validateRecoveryTicket(s, ticket))
	candidate, err := s.prepareRevisionPolicyCandidate(invalid)
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.Nil(t, candidate)
	require.NoError(t, validateRecoveryTicket(s, ticket))
	require.Equal(t, revisionRecoveryNone, s.revisionRecovery.reason)
}

func TestRevisionRecoveryCaptureCancellationAndValidationClasses(t *testing.T) {
	inputs := policyCandidateInputs(t)
	s := recoveryTestOwner(t, inputs, credentialvault.NewStore(nil, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, epoch, ticket, err := s.captureRevisionBootstrap(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, snapshot)
	require.Zero(t, epoch)
	require.Nil(t, ticket)
	_, _, ticket, err = s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.ErrorIs(t, validateRecoveryTicket(s, nil), errInvalidRevisionBootstrap)
	malformed := *ticket
	malformed.vault = nil
	require.ErrorIs(t, validateRecoveryTicket(s, &malformed), errInvalidRevisionBootstrap)
	other := recoveryTestOwner(t, inputs, s.credentialVault)
	require.ErrorIs(t, validateRecoveryTicket(other, ticket), errStaleRevisionBootstrap)
	s.mu.Lock()
	s.requireRevisionRecoveryLocked(revisionRecoveryNone)
	s.mu.Unlock()
	require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired, "invalid reason must not become a reset")
}

func TestRevisionRecoveryCaptureRejectsIncompatibleBoundPolicy(t *testing.T) {
	inputs := policyCandidateInputs(t)
	store := policyCandidateStore(t, inputs)
	denied := effectivePolicyInputs{user: policy.DefaultDenyPolicy()}
	s := recoveryTestOwner(t, denied, store)
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.ErrorIs(t, err, errInvalidRevisionBootstrap)
	require.ErrorIs(t, err, credentialvault.ErrInvalidCandidate)
	require.Nil(t, ticket)
	require.NotContains(t, err.Error(), "api.example.com")
	require.NotContains(t, err.Error(), "private-policy-candidate")
	require.Equal(t, revisionRecoveryNone, s.revisionRecovery.reason)
}

func TestRevisionRecoveryConcurrentBarrier(t *testing.T) {
	inputs := policyCandidateInputs(t)
	s := recoveryTestOwner(t, inputs, credentialvault.NewStore(nil, nil))
	failures := make(chan error, 80)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
				if err == nil {
					err = validateRecoveryTicket(s, ticket)
					if errors.Is(err, errStaleRevisionBootstrap) {
						err = nil
					}
				}
				if err == nil {
					_, err = s.prepareRevisionPolicyCandidate(inputs)
				}
				if err != nil {
					failures <- err
				}
			}
		}()
	}
	for range 20 {
		s.mu.Lock()
		err := s.replaceRevisionBaseLocked(inputs)
		s.mu.Unlock()
		require.NoError(t, err)
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
}

func TestRevisionRecoveryStartupModeAndFrozenTelemetry(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "false")
	// Startup must use the already resolved overlay supplied by its caller.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://new-resolution.example.com:4317")
	for _, experimental := range []bool{false, true} {
		t.Run(fmt.Sprint(experimental), func(t *testing.T) {
			t.Setenv(constants.EnvExperimentalRevisionRuntime, fmt.Sprint(experimental))
			rule, err := policy.ParseValidatedEgressRule(policy.ActionAllow, "frozen-telemetry.example.com")
			require.NoError(t, err)
			allow := []policy.EgressRule{rule}
			srv, s, err := startPolicyServer(&stubProxy{}, nil, "", "127.0.0.1:0", "", nil, "", nil, allow, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			if !experimental {
				require.Nil(t, s.revisionRecovery)
				return
			}
			s.mu.Lock()
			frozen := cloneEffectivePolicyInputs(s.revisionRecovery.current.inputs)
			s.mu.Unlock()
			require.Equal(t, allow, frozen.alwaysAllow)
			require.Empty(t, frozen.telemetryAllow, "do not duplicate already resolved telemetry")
			require.Equal(t, policy.ActionDeny, frozen.user.DefaultAction, "nil startup policy defaults to deny")
			_, epoch, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			require.Zero(t, epoch)
			require.NoError(t, validateRecoveryTicket(s, ticket))
		})
	}
}

func TestRevisionRecoveryInitializationCannotResetOwner(t *testing.T) {
	inputs := policyCandidateInputs(t)
	s := recoveryTestOwner(t, inputs, credentialvault.NewStore(nil, nil))
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	s.mu.Lock()
	err = s.initRevisionRecoveryLocked(inputs)
	s.mu.Unlock()
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.NoError(t, validateRecoveryTicket(s, ticket))
	uninitialized := &policyServer{}
	uninitialized.mu.Lock()
	uninitialized.invalidateRevisionBootstrapLocked()
	err = uninitialized.replaceRevisionBaseLocked(inputs)
	uninitialized.mu.Unlock()
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	candidate, err := uninitialized.prepareRevisionPolicyCandidate(inputs)
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.Nil(t, candidate)
	uninitialized.mu.Lock()
	uninitialized.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
	err = uninitialized.initRevisionRecoveryLocked(inputs)
	uninitialized.mu.Unlock()
	require.ErrorIs(t, err, errRevisionRecoveryRequired)
	_, _, ticket, err = uninitialized.captureRevisionBootstrap(context.Background())
	require.ErrorIs(t, err, errRevisionRecoveryRequired)
	require.Nil(t, ticket)
}

// This test-only replacement exercises pointer/ABA fencing without a live owner.
// replaceRevisionBaseLocked requires the caller to hold s.mu. A replacement
// gets a fresh handle even when both policy content and epoch are unchanged.
func (s *policyServer) replaceRevisionBaseLocked(inputs effectivePolicyInputs) error {
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return err
	}
	if s.revisionRecovery == nil {
		return errInvalidEffectivePolicyCandidate
	}
	base, err := newEffectivePolicyBase(inputs, 0)
	if err != nil {
		return err
	}
	s.revisionRecovery.current = base
	s.invalidateRevisionBootstrapLocked()
	return nil
}
