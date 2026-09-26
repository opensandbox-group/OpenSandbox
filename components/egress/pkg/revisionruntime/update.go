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

package revisionruntime

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

// Update installs a post-bootstrap decision snapshot on this session's
// receiver. It is an internal transaction primitive and does not publish any
// public Vault state. A future caller must keep its candidate unpublished while
// holding the shared mutation barrier. A successful return confirms this exact
// identity and permits finalization. If Update returns ErrIndeterminate with a
// non-zero attempt, only ReconcileUpdate of that exact recorded attempt can
// permit finalization or discarding. ErrClosed and ErrTransportUnavailable are
// terminal session failures: callers must stop the exact child, close the
// session, discard the unpublished candidate, and start a fresh session from
// the prior public state. They must never finalize the candidate on those
// errors.
func (s *ProcessSession) Update(
	ctx context.Context,
	snapshot credentialvault.ActiveSnapshot,
	effectivePolicyEpoch int64,
) (revision.Identity, error) {
	coordinator, err := s.Coordinator()
	if err != nil {
		return revision.Identity{}, err
	}
	if err := ctx.Err(); err != nil {
		return revision.Identity{}, err
	}
	if err := s.beginPostBootstrapOperation(); err != nil {
		return revision.Identity{}, err
	}
	defer s.finishPostBootstrapOperation()
	if err := s.checkLocalFence(); err != nil {
		return revision.Identity{}, err
	}
	previous, err := coordinator.Confirmed()
	if err != nil {
		return revision.Identity{}, err
	}
	if previous == nil {
		return revision.Identity{}, revision.ErrIndeterminate
	}

	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, effectivePolicyEpoch)
	if err != nil {
		return revision.Identity{}, err
	}
	identity, err := coordinator.Apply(ctx, snapshot.Revision, effectivePolicyEpoch, payload)
	if err != nil {
		if err == revision.ErrIndeterminate && identity != (revision.Identity{}) {
			s.recordPendingUpdate(identity, *previous)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.closed {
				return revision.Identity{}, revision.ErrClosed
			}
			if !s.parentPathMatches() {
				return revision.Identity{}, revision.ErrTransportUnavailable
			}
			return identity, err
		}
		return revision.Identity{}, err
	}
	confirmed, err := coordinator.Confirmed()
	if err != nil {
		return revision.Identity{}, err
	}
	if confirmed == nil || *confirmed != identity {
		return revision.Identity{}, revision.ErrIndeterminate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return revision.Identity{}, revision.ErrClosed
	}
	if !s.parentPathMatches() {
		return revision.Identity{}, revision.ErrTransportUnavailable
	}
	return identity, nil
}

// ReconcileUpdate resolves only the exact outstanding Update attempt recorded
// by this session, using metadata-only coordinator reconciliation. The returned
// bool is true only if that attempt is confirmed active, false only if its exact
// frozen previous identity remains active, and an error means the caller must
// keep its mutation blocked. ErrClosed and ErrTransportUnavailable are terminal
// session failures; callers must stop the exact child, close the session,
// discard the unpublished candidate, and start a fresh session from the prior
// public state.
func (s *ProcessSession) ReconcileUpdate(
	ctx context.Context,
	attempt revision.Identity,
) (bool, error) {
	if !s.validUpdateAttempt(attempt) {
		return false, revision.ErrInvalid
	}
	coordinator, err := s.Coordinator()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	previous, err := s.beginReconcileUpdate(attempt)
	if err != nil {
		return false, err
	}
	defer s.finishPostBootstrapOperation()
	if err := s.checkLocalFence(); err != nil {
		return false, err
	}

	resolved, err := coordinator.Reconcile(ctx)
	if err != nil {
		return false, err
	}
	if resolved == nil {
		return false, revision.ErrIndeterminate
	}
	confirmed, err := coordinator.Confirmed()
	if err != nil {
		return false, err
	}
	if confirmed == nil || *confirmed != *resolved {
		return false, revision.ErrIndeterminate
	}
	activated := *resolved == attempt
	if !activated && *resolved != previous {
		return false, revision.ErrIndeterminate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, revision.ErrClosed
	}
	if !s.parentPathMatches() {
		return false, revision.ErrTransportUnavailable
	}
	s.bootstrapMu.Lock()
	if s.pendingUpdate == nil || *s.pendingUpdate != attempt ||
		s.previousUpdate == nil || *s.previousUpdate != previous {
		s.bootstrapMu.Unlock()
		return false, revision.ErrIndeterminate
	}
	s.pendingUpdate = nil
	s.previousUpdate = nil
	s.bootstrapMu.Unlock()
	return activated, nil
}

func (s *ProcessSession) validUpdateAttempt(attempt revision.Identity) bool {
	return attempt.ControlGeneration == s.launch.ControlGeneration &&
		attempt.SubjectGeneration == s.launch.SubjectGeneration &&
		attempt.DecisionEpoch > 0 && attempt.VaultRevision >= 0 && attempt.PolicyEpoch >= 0 &&
		validUpdateDigest(attempt.Digest)
}

func validUpdateDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func (s *ProcessSession) beginPostBootstrapOperation() error {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	switch s.bootstrap {
	case bootstrapComplete:
		if s.pendingUpdate != nil {
			return revision.ErrIndeterminate
		}
		s.bootstrap = bootstrapRunning
		return nil
	case bootstrapRunning:
		return revision.ErrBusy
	default:
		return revision.ErrIndeterminate
	}
}

func (s *ProcessSession) beginReconcileUpdate(attempt revision.Identity) (revision.Identity, error) {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	switch s.bootstrap {
	case bootstrapRunning:
		return revision.Identity{}, revision.ErrBusy
	case bootstrapComplete:
		if s.pendingUpdate == nil || *s.pendingUpdate != attempt || s.previousUpdate == nil {
			return revision.Identity{}, revision.ErrIndeterminate
		}
		previous := *s.previousUpdate
		s.bootstrap = bootstrapRunning
		return previous, nil
	default:
		return revision.Identity{}, revision.ErrIndeterminate
	}
}

func (s *ProcessSession) recordPendingUpdate(attempt, previous revision.Identity) {
	s.bootstrapMu.Lock()
	attemptCopy := attempt
	previousCopy := previous
	s.pendingUpdate = &attemptCopy
	s.previousUpdate = &previousCopy
	s.bootstrapMu.Unlock()
}

func (s *ProcessSession) finishPostBootstrapOperation() {
	s.bootstrapMu.Lock()
	s.bootstrap = bootstrapComplete
	s.bootstrapMu.Unlock()
}

func (s *ProcessSession) checkLocalFence() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return revision.ErrClosed
	}
	if !s.parentPathMatches() {
		return revision.ErrTransportUnavailable
	}
	return nil
}
