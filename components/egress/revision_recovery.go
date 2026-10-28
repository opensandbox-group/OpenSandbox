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
	"fmt"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

var (
	errInvalidRevisionBootstrap = errors.New("invalid revision bootstrap")
	errStaleRevisionBootstrap   = errors.New("stale revision bootstrap")
	errRevisionRecoveryRequired = errors.New("revision recovery required")
)

type revisionRecoveryReason uint8

const (
	revisionRecoveryNone revisionRecoveryReason = iota
	revisionRecoveryExternalEffectsUnknown
	revisionRecoverySessionCleanupFailed
	revisionRecoveryUnknown
)

func (r revisionRecoveryReason) String() string {
	switch r {
	case revisionRecoveryExternalEffectsUnknown:
		return "external effects outcome unknown"
	case revisionRecoverySessionCleanupFailed:
		return "session cleanup failed"
	default:
		return "unknown recovery reason"
	}
}

// Nonzero size ensures distinct live allocations cannot alias, unlike pointers
// to zero-sized values. This identity is private and unrelated to policyEpoch.
type revisionBootstrapIdentity struct{ marker byte }

type revisionBootstrapTicket struct {
	owner    *policyServer
	identity *revisionBootstrapIdentity
	base     *effectivePolicyBase
	store    *credentialvault.Store
	vault    *credentialvault.PolicySnapshot
}

func (revisionBootstrapTicket) String() string   { return "revisionBootstrapTicket" }
func (revisionBootstrapTicket) GoString() string { return "revisionBootstrapTicket{}" }

// revisionRecoveryState is owned by policyServer under its shared policy/Vault
// barrier. The immutable current base owns the committed policy epoch.
// There is deliberately no operation that clears a recovery reason.
type revisionRecoveryState struct {
	current  *effectivePolicyBase
	identity *revisionBootstrapIdentity
	reason   revisionRecoveryReason
}

// recoveryErrorLocked requires the caller to hold the owner's s.mu.
func (r *revisionRecoveryState) recoveryErrorLocked() error {
	if r != nil && r.reason != revisionRecoveryNone {
		return fmt.Errorf("%w: %s", errRevisionRecoveryRequired, r.reason)
	}
	return nil
}

// initRevisionRecoveryLocked requires the caller to hold s.mu. It is a one-time
// initialization, never a reset or replacement of an existing incarnation.
func (s *policyServer) initRevisionRecoveryLocked(inputs effectivePolicyInputs) error {
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return err
	}
	if s.revisionRecovery != nil {
		return errInvalidEffectivePolicyCandidate
	}
	base, err := newEffectivePolicyBase(inputs, 0)
	if err != nil {
		return err
	}
	if s.policyFile != "" {
		store, err := policy.NewAtomicPolicyFile(s.policyFile)
		if err != nil {
			return fmt.Errorf("experimental policy storage: %w", err)
		}
		s.atomicPolicyFile = store
	}
	s.revisionRecovery = &revisionRecoveryState{current: base, identity: &revisionBootstrapIdentity{}}
	return nil
}

// invalidateRevisionBootstrapLocked requires the caller to hold s.mu. It fences
// in-flight bootstraps without changing the authoritative policy or its epoch.
func (s *policyServer) invalidateRevisionBootstrapLocked() {
	if s.revisionRecovery != nil {
		s.revisionRecovery.identity = &revisionBootstrapIdentity{}
	}
}

// requireRevisionRecoveryLocked requires the caller to hold s.mu. Preserve the
// first fixed classification and invalidate every outstanding bootstrap ticket.
func (s *policyServer) requireRevisionRecoveryLocked(reason revisionRecoveryReason) {
	if s.revisionRecovery == nil {
		s.revisionRecovery = &revisionRecoveryState{}
	}
	if reason != revisionRecoveryExternalEffectsUnknown && reason != revisionRecoverySessionCleanupFailed {
		reason = revisionRecoveryUnknown
	}
	if s.revisionRecovery.reason == revisionRecoveryNone {
		s.revisionRecovery.reason = reason
	}
	s.invalidateRevisionBootstrapLocked()
	// Publish the sticky health restriction independently of optional MITM readiness.
	s.revisionRecoveryRequired.Store(true)
	s.mitmGate.SetReady(false)
	// Health and ticket restrictions precede waiting for an admitted nft writer.
	// Keep the one-way lock order s.mu -> Manager.mu; Quiesce never calls back.
	if s.nft != nil {
		s.nft.Quiesce()
	}
	if s.quarantine != nil {
		_ = s.quarantine.contain()
	}
}

func (s *policyServer) captureRevisionBootstrap(ctx context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return credentialvault.ActiveSnapshot{}, 0, nil, err
	}
	if err := ctx.Err(); err != nil {
		return credentialvault.ActiveSnapshot{}, 0, nil, err
	}
	state := s.revisionRecovery
	if state == nil || state.current == nil || state.identity == nil || s.credentialVault == nil {
		return credentialvault.ActiveSnapshot{}, 0, nil, errInvalidRevisionBootstrap
	}
	inputs := state.current.inputs
	allow := append(append([]policy.EgressRule(nil), inputs.alwaysAllow...), inputs.telemetryAllow...)
	effective := policy.MergeAlwaysOverlay(inputs.user, inputs.alwaysDeny, allow)
	vault, err := s.credentialVault.FreezeForPolicy(effective)
	if err != nil {
		// Preserve fixed error classes, never a binding/source/payload detail.
		cause := credentialvault.ErrInvalidCandidate
		if errors.Is(err, credentialvault.ErrCandidateNotRendered) {
			cause = credentialvault.ErrCandidateNotRendered
		}
		return credentialvault.ActiveSnapshot{}, 0, nil, fmt.Errorf("%w: %w", errInvalidRevisionBootstrap, cause)
	}
	ticket := &revisionBootstrapTicket{owner: s, identity: state.identity, base: state.current, store: s.credentialVault, vault: vault}
	return vault.Snapshot(), state.current.epoch, ticket, nil
}

// validateRevisionBootstrapLocked requires the caller to hold s.mu through the
// eventual readiness publication. This method alone does not reserve a ticket.
func (s *policyServer) validateRevisionBootstrapLocked(ticket *revisionBootstrapTicket) error {
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return err
	}
	state := s.revisionRecovery
	if state == nil || state.current == nil || state.identity == nil || ticket == nil ||
		ticket.owner == nil || ticket.identity == nil || ticket.base == nil || ticket.store == nil || ticket.vault == nil {
		return errInvalidRevisionBootstrap
	}
	if ticket.owner != s || ticket.identity != state.identity || ticket.base != state.current || ticket.store != s.credentialVault {
		return errStaleRevisionBootstrap
	}
	if err := s.credentialVault.ValidatePolicySnapshot(ticket.vault); err != nil {
		return errStaleRevisionBootstrap
	}
	return nil
}

func (s *policyServer) prepareRevisionPolicyCandidate(next effectivePolicyInputs) (*effectivePolicyCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return nil, err
	}
	if s.revisionRecovery == nil {
		return nil, errInvalidEffectivePolicyCandidate
	}
	return s.revisionRecovery.current.Prepare(s.credentialVault, next)
}

// publishRevisionReady is the sole experimental readiness publication point.
// Validation and ownership transfer are one step under s.mu -> m.mu. Rejection
// leaves the caller responsible for stopping/reaping and closing this result.
func publishRevisionReady(ctx context.Context, m *mitmTransparent, result *revisionLaunchResult) error {
	if ctx == nil || m == nil || result == nil || result.running == nil || result.session == nil ||
		result.generation == 0 || m.revisionOwner == nil || m.revisionOwner.server == nil {
		return errInvalidRevisionBootstrap
	}
	s := m.revisionOwner.server
	s.mu.Lock()
	defer s.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := s.validateRevisionBootstrapLocked(result.ticket); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.stopping {
		return revision.ErrClosed
	}
	if m.pending != result || m.launchGen != result.generation || m.running != nil {
		return errStaleRevisionBootstrap
	}
	if m.launchExited {
		return revision.ErrTransportUnavailable
	}
	m.running, m.revisionSession, m.currentGen = result.running, result.session, result.generation
	m.pending, m.launchGen = nil, 0
	result.running, result.session = nil, nil
	s.mitmGate.SetReady(true)
	return nil
}
