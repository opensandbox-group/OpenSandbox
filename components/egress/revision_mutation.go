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
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

// mutateRevisionVault is the live Vault mutation transaction for the
// experimental revision runtime. It installs a rendered candidate snapshot on
// the receiver, reconciles an indeterminate outcome to its exact attempt, and
// finalizes the public Store only after the exact identity is confirmed. It
// does not wait for transport draining; it preserves the committed policy epoch;
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
	if s.revisionRecovery == nil || s.revisionRecovery.current == nil {
		return empty, revision.ErrInvalid
	}
	base := s.revisionRecovery.current
	inputs := base.inputs
	allow := append(append([]policy.EgressRule(nil), inputs.alwaysAllow...), inputs.telemetryAllow...)
	candidate, err := prepare(s.credentialVault, policy.MergeAlwaysOverlay(inputs.user, inputs.alwaysDeny, allow))
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
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, base.epoch)
	if err != nil {
		return empty, revision.ErrInvalid
	}
	digest := sha256.Sum256(payload)
	session := m.revisionSession
	config, err := session.MitmproxyConfig()
	if err != nil || !validRevisionMutationConfig(config) {
		s.detachRevisionMutationLocked(m)
		return empty, revision.ErrTransportUnavailable
	}
	// A rendered candidate can exceed the IPC budget even with a valid body.
	if len(payload) > config.MaxSnapshotBytes {
		return empty, revision.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := updateRevisionSnapshotLocked(ctx, session, config, snapshot, base.epoch, hex.EncodeToString(digest[:])); err != nil {
		if !errors.Is(err, revision.ErrPrepareRejected) {
			s.detachRevisionMutationLocked(m)
			return empty, revision.ErrTransportUnavailable
		}
		return empty, err
	}
	state, err := s.credentialVault.CommitCandidate(candidate)
	if err != nil {
		s.detachRevisionMutationLocked(m)
		return empty, revision.ErrTransportUnavailable
	}
	return state, nil
}

// validRevisionMutationConfig checks the identity and payload limits used by
// both mutation owners before any Policy file or kernel effect.
func validRevisionMutationConfig(config *mitmproxy.RevisionIPCConfig) bool {
	return config != nil && config.ControlGeneration != "" && config.SubjectGeneration != "" && config.MaxSnapshotBytes > 0
}

// updateRevisionSnapshotLocked installs one frozen snapshot and only reconciles
// its exact attempt. The caller holds s.mu and m.mu, owns effect classification,
// and publishes local state only after nil. A confirmed ACK wins over a later
// context cancellation. No Store/base or lifecycle state is changed here.
func updateRevisionSnapshotLocked(ctx context.Context, session revisionProcessSession, config *mitmproxy.RevisionIPCConfig,
	snapshot credentialvault.ActiveSnapshot, epoch int64, digest string,
) error {
	matches := func(id revision.Identity) bool {
		return id.ControlGeneration == config.ControlGeneration && id.SubjectGeneration == config.SubjectGeneration &&
			id.DecisionEpoch > 0 && id.VaultRevision == snapshot.Revision && id.PolicyEpoch == epoch && id.Digest == digest
	}
	attempt, err := session.Update(ctx, snapshot, epoch)
	if err != nil && !errors.Is(err, revision.ErrIndeterminate) {
		if errors.Is(err, revision.ErrPrepareRejected) {
			return revision.ErrPrepareRejected
		}
		return revision.ErrTransportUnavailable
	}
	if !matches(attempt) {
		return revision.ErrTransportUnavailable
	}
	for err != nil {
		if ctx.Err() != nil {
			return revision.ErrTransportUnavailable
		}
		var committed bool
		committed, err = session.ReconcileUpdate(ctx, attempt)
		if err == nil {
			if !committed {
				return revision.ErrPrepareRejected
			}
			break
		}
		if errors.Is(err, revision.ErrClosed) || errors.Is(err, revision.ErrTransportUnavailable) || errors.Is(err, revision.ErrInvalid) {
			return revision.ErrTransportUnavailable
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return revision.ErrTransportUnavailable
		case <-timer.C:
		}
	}
	return nil
}

// detachRevisionMutationLocked retains both owner locks until the exact child
// is stopped/reaped and its session closed. A helper may already have latched
// and contained the failure; do not repeat quarantine in that case.
func (s *policyServer) detachRevisionMutationLocked(m *mitmTransparent) {
	s.mitmGate.SetReady(false)
	if s.revisionRecovery.recoveryErrorLocked() == nil {
		s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
	}
	running, session := m.running, m.revisionSession
	m.running, m.revisionSession = nil, nil
	// Keep currentGen so the exact child's exit remains fenced by recovery.
	if running != nil {
		m.revisionOwner.stop(running)
	}
	if session != nil {
		if err := session.Close(); err != nil {
			log.Warnf("revision mutation: session cleanup failed after detach")
		}
	}
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
	case errors.Is(err, credentialvault.ErrRevisionConflict):
		return errRevisionExpectedRevision
	default:
		return revision.ErrInvalid
	}
}
