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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

// mutateRevisionVault is the live Vault mutation transaction for the
// experimental revision runtime. It installs a rendered candidate snapshot on
// the receiver, reconciles an indeterminate outcome to its exact attempt, and
// finalizes the public Store only after the exact identity is confirmed. It
// does not provide transport drain acknowledgements or policy epoch updates;
// connection draining is owned by the addon's decision registry deadlines.
// prepare must only prepare a candidate from the supplied Store and policy; it
// must not publish state or reenter policy/lifecycle methods. The caller must
// supply a deadline context canceled when sidecar shutdown begins. A prepare
// failure is sanitized to the fixed public error vocabulary so client-facing
// status codes survive without exposing arbitrary prepare error text.
//
// Lock order is policy barrier -> exclusive process lease. Both stay held from
// candidate preparation through local finalization or terminal cleanup. Unlike
// withRevisionMutationSession, this owner can detach failed resources without a
// lock upgrade. A terminal detach first latches sticky recovery (its remote
// effects are unprovable), then stops/reaps the exact child before closing its
// session and discarding the candidate; the same sidecar never restarts from
// prior state. It never calls lifecycle lock methods. IPC is context-bounded,
// but existing stop/reap has no hard recovery deadline.
func (s *policyServer) mutateRevisionVault(
	ctx context.Context, m *mitmTransparent,
	prepare func(*credentialvault.Store, *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error),
) (credentialvault.State, error) {
	empty := credentialvault.State{}
	if s == nil || m == nil || ctx == nil || prepare == nil || s.credentialVault == nil || s.proxy == nil || s.mitmGate == nil {
		return empty, revision.ErrInvalid
	}
	if _, ok := ctx.Deadline(); !ok {
		return empty, revision.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return empty, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if m.stopping {
		return empty, revision.ErrClosed
	}
	if m.running == nil || m.revisionSession == nil || m.revisionOwner == nil || m.revisionOwner.stop == nil || s.mitmGate.MitmPending() {
		return empty, revision.ErrTransportUnavailable
	}
	candidate, err := prepare(s.credentialVault, s.effectivePolicy())
	if err != nil {
		if candidate != nil {
			candidate.Discard()
		}
		return empty, sanitizePrepareError(err)
	}
	if candidate == nil {
		return empty, revision.ErrInvalid
	}
	defer candidate.Discard()
	snapshot, err := candidate.ActiveSnapshot(ctx)
	if err != nil {
		return empty, revision.ErrInvalid
	}
	// Policy epoch remains the bootstrap-reserved zero until policy integration.
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, 0)
	if err != nil {
		return empty, revision.ErrInvalid
	}
	digest := sha256.Sum256(payload)
	session := m.revisionSession
	detach := func() (credentialvault.State, error) {
		s.mitmGate.SetReady(false)
		// A terminal detach cannot prove the remote outcome, so it latches
		// sticky recovery BEFORE detaching/stopping: no same-sidecar restart,
		// every outstanding bootstrap ticket is fenced, nft is quiesced and
		// quarantine contains. Known prepare rejection and an exact-previous
		// reconcile are the only nonsticky outcomes and never reach here.
		s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
		running := m.running
		m.running, m.revisionSession = nil, nil
		// Keep currentGen: the exact child's exit event is fenced by the same
		// recovery latch, which any fresh bootstrap must wait on via s.mu.
		// Concurrent shutdown also waits for this lease.
		m.revisionOwner.stop(running)
		// Close failures remain a failed transaction. Do not expose error text that
		// may contain credentials or filesystem details, or restore readiness here.
		// The detach latch already quiesced/contained, so a second latch attempt
		// must not repeat those effects; it only fires when nothing latched yet.
		if err := session.Close(); err != nil && s.revisionRecovery != nil &&
			s.revisionRecovery.recoveryErrorLocked() == nil {
			s.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
		}
		return empty, revision.ErrTransportUnavailable
	}
	config, err := session.MitmproxyConfig()
	if err != nil || config == nil {
		return detach()
	}
	matches := func(id revision.Identity) bool {
		return config.ControlGeneration != "" && config.SubjectGeneration != "" &&
			id.ControlGeneration == config.ControlGeneration && id.SubjectGeneration == config.SubjectGeneration &&
			id.DecisionEpoch > 0 && id.VaultRevision == snapshot.Revision && id.PolicyEpoch == 0 &&
			id.Digest == hex.EncodeToString(digest[:])
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	attempt, err := session.Update(ctx, snapshot, 0)
	if err != nil && !errors.Is(err, revision.ErrIndeterminate) {
		if errors.Is(err, revision.ErrPrepareRejected) {
			return empty, revision.ErrPrepareRejected
		}
		return detach()
	}
	if !matches(attempt) {
		return detach()
	}
	for err != nil {
		if ctx.Err() != nil {
			return detach()
		}
		var committed bool
		committed, err = session.ReconcileUpdate(ctx, attempt)
		if err == nil {
			if !committed {
				return empty, revision.ErrPrepareRejected
			}
			break
		}
		if errors.Is(err, revision.ErrClosed) || errors.Is(err, revision.ErrTransportUnavailable) || errors.Is(err, revision.ErrInvalid) {
			return detach()
		}
		// Do not spin on unavailable readback or send Update again. Reconcile only
		// this exact attempt until confirmation or the caller's bounded deadline.
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return detach()
		case <-timer.C:
		}
	}
	state, err := s.credentialVault.CommitCandidate(candidate)
	if err != nil {
		return detach()
	}
	return state, nil
}

// errRevisionExpectedRevision carries the optimistic-concurrency conflict in
// the fixed public vocabulary; the numeric detail never crosses this boundary.
var errRevisionExpectedRevision = errors.New("expectedRevision does not match the current revision")

// sanitizePrepareError maps a prepare failure onto the fixed public error
// vocabulary. Arbitrary prepare error text never crosses this boundary, so
// a leaking prepare callback cannot disclose rendered data; only the
// client-facing status classes (not-found, exists, revision conflict) survive.
func sanitizePrepareError(err error) error {
	switch {
	case errors.Is(err, credentialvault.ErrNotFound):
		return credentialvault.ErrNotFound
	case errors.Is(err, credentialvault.ErrExists):
		return credentialvault.ErrExists
	case strings.Contains(err.Error(), "expectedRevision"):
		return errRevisionExpectedRevision
	default:
		return revision.ErrInvalid
	}
}
