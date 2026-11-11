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

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

var (
	errRevisionPolicyPersistence = errors.New("failed to persist policy")
	errRevisionPolicyNft         = errors.New("failed to apply nftables policy")
)

// revisionMutationContext cancels on shutdown without taking the process lease.
// Mutex waits, filesystem calls and stop/reap remain cooperatively bounded.
func revisionMutationContext(parent context.Context, m *mitmTransparent) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, revisionMutationTimeout)
	if m == nil || m.shutdownCh == nil {
		return ctx, cancel
	}
	select {
	case <-m.shutdownCh:
		cancel()
	default:
		go func() {
			select {
			case <-m.shutdownCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

// revisionPolicySessionReadyLocked requires s.mu and m.mu. Incomplete startup
// is retryable and never falls back to disk/nft/Go-only experimental mutation.
func (s *policyServer) revisionPolicySessionReadyLocked(ctx context.Context, m *mitmTransparent) error {
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m != nil {
		select {
		case <-m.shutdownCh:
			return revision.ErrClosed
		default:
		}
	}
	if m == nil || s.mitmGate == nil || m.stopping || m.running == nil || m.revisionSession == nil || m.revisionOwner == nil || m.revisionOwner.stop == nil || s.mitmGate.MitmPending() {
		return revision.ErrTransportUnavailable
	}
	return nil
}

// checkRevisionPolicyAvailableLocked checks even no-op requests under s.mu.
// Real mutations recheck under their full exclusive lease before any effects.
func (s *policyServer) checkRevisionPolicyAvailableLocked(ctx context.Context) error {
	if s.revisionRecovery == nil {
		return nil
	}
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return err
	}
	m := s.revisionMitmLocked()
	if m == nil {
		return revision.ErrTransportUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.revisionPolicySessionReadyLocked(ctx, m)
}

// applyRevisionPolicyLocked is the fixed Policy effect chain. The caller holds
// s.mu and m.mu until immediately publishing the returned frozen base into the
// proxy/loader and recovery owner. No source is re-rendered or Vault committed.
// The exact local candidate, not its digest, binds all effects.
func (s *policyServer) applyRevisionPolicyLocked(ctx context.Context, m *mitmTransparent, inputs effectivePolicyInputs, persist bool) (*effectivePolicyBase, error) {
	if err := s.revisionPolicySessionReadyLocked(ctx, m); err != nil {
		return nil, err
	}
	if s.revisionRecovery == nil || s.revisionRecovery.current == nil {
		return nil, revision.ErrInvalid
	}
	current := s.revisionRecovery.current
	candidate, err := current.Prepare(s.credentialVault, inputs)
	if err != nil {
		return nil, revision.ErrInvalid
	}
	if err := current.Validate(s.credentialVault, candidate); err != nil {
		return nil, revision.ErrInvalid
	}
	base := &effectivePolicyBase{inputs: candidate.inputs, epoch: candidate.PolicyEpoch()}
	snapshot := candidate.Snapshot()
	nftPolicy := candidate.EffectivePolicy().WithExtraAllowIPs(s.nameserverIPs)
	session := m.revisionSession
	config, err := session.MitmproxyConfig()
	if err != nil || !validRevisionMutationConfig(config) {
		s.detachRevisionMutationLocked(m)
		return nil, revision.ErrTransportUnavailable
	}
	if len(candidate.payload) > config.MaxSnapshotBytes {
		return nil, revision.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	restore := func() error { return nil }
	if persist {
		restore, err = s.persistPolicyChangeLocked(base.inputs.user)
		if err != nil {
			if s.revisionRecovery.recoveryErrorLocked() != nil {
				s.detachRevisionMutationLocked(m)
			}
			return nil, errRevisionPolicyPersistence
		}
	}
	// Neither nft nor a receiver attempt has started. Only confirmed restoration
	// of the exact file/absence permits a healthy cancellation here.
	if err := ctx.Err(); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			s.detachRevisionMutationLocked(m)
		}
		return nil, err
	}
	nftCommitted := false
	if s.nft != nil {
		s.invalidateRevisionBootstrapLocked()
		if err := s.nft.ApplyStatic(ctx, nftPolicy); err != nil {
			if nftables.ApplyEffectOf(err) != nftables.ApplyUnchanged {
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			}
			if restoreErr := restore(); restoreErr != nil && s.revisionRecovery.recoveryErrorLocked() == nil {
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			}
			if s.revisionRecovery.recoveryErrorLocked() != nil {
				s.detachRevisionMutationLocked(m)
			}
			return nil, &nftables.ApplyError{Effect: nftables.ApplyEffectOf(err), Err: errRevisionPolicyNft}
		}
		nftCommitted = true
	}
	if err := ctx.Err(); err != nil {
		if nftCommitted {
			s.detachRevisionMutationLocked(m)
		} else if restoreErr := restore(); restoreErr != nil {
			s.detachRevisionMutationLocked(m)
		}
		return nil, err
	}
	if err := updateRevisionSnapshotLocked(ctx, session, config, snapshot, candidate.PolicyEpoch(), candidate.Digest()); err != nil {
		// Rejection cannot undo committed nft rules, dynamic sets, conntrack or sent
		// requests. Never replay old nft as a healthy rollback.
		if !nftCommitted && errors.Is(err, revision.ErrPrepareRejected) {
			if restoreErr := restore(); restoreErr == nil {
				return nil, err
			}
		}
		s.detachRevisionMutationLocked(m)
		return nil, revision.ErrTransportUnavailable
	}
	// A confirmed ACK wins over later cancellation. Defensive identity failure
	// must never restore an old file or epoch after receiver publication.
	if s.revisionRecovery.recoveryErrorLocked() != nil || s.revisionRecovery.current.Validate(s.credentialVault, candidate) != nil {
		s.detachRevisionMutationLocked(m)
		return nil, revision.ErrTransportUnavailable
	}
	return base, nil
}

func writeRevisionPolicyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRevisionPolicyPersistence):
		http.Error(w, errRevisionPolicyPersistence.Error(), http.StatusInternalServerError)
	case errors.Is(err, errRevisionPolicyNft):
		http.Error(w, errRevisionPolicyNft.Error(), http.StatusInternalServerError)
	case errors.Is(err, revision.ErrInvalid):
		http.Error(w, "invalid revision policy update", http.StatusBadRequest)
	default:
		http.Error(w, "revision policy publication unavailable", http.StatusServiceUnavailable)
	}
}
