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

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// subjectRebind protects only one unfinished same-UID registration intent.
// The allocated pointer is its exact, non-reusable identity. Stages record
// completed cleanup, not assumptions based on the registry's new fence:
// RegisterAndEnforce publishes that fence before deny-first can fail.
// Identity fields are immutable. Map/stage access is under mu; only an
// action-gate holder changes stages.
type subjectRebind struct {
	old, expectedNew       subject.Fencing
	oldNetwork, newNetwork actionhandler.NetworkAttachment
	needsReset             bool // a newer SET superseded an unfinished intent
	removeOld              bool // old IP differs, or its network is unknown
	oldRemoved             bool
	purged                 bool
}

// subjectRebindingLocked is shared by Vault entry points. Callers hold mu.
func (s *fastSandboxPolicyServer) subjectRebindingLocked(subj subject.Subject) bool {
	return s.rebinds[subj] != nil
}

func (s *fastSandboxPolicyServer) actionCanceled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.ctx.Err()
}

func (s *fastSandboxPolicyServer) acquireAction(ctx context.Context) (func(), error) {
	if err := s.actionCanceled(ctx); err != nil {
		return nil, err
	}
	select {
	case s.actionGate <- struct{}{}:
		// Cancellation and gate availability can become ready together. Never
		// let a canceled queued SET replace the current registration intent.
		if err := s.actionCanceled(ctx); err != nil {
			<-s.actionGate
			return nil, err
		}
		return func() { <-s.actionGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func sameBindingNetwork(a, b actionhandler.NetworkAttachment) bool {
	return a.IP == b.IP && a.Gateway == b.Gateway
}

func (s *fastSandboxPolicyServer) finishRebind(subj subject.Subject, token *subjectRebind) {
	if token == nil {
		return
	}
	s.mu.Lock()
	cleared := s.rebinds[subj] == token
	if cleared {
		delete(s.rebinds, subj)
	}
	s.mu.Unlock()
	if cleared {
		log.Infof("subject_rebind_cleared")
	}
}

// logPendingRebind runs once at the end of an attempted SET/REMOVE, never on
// polling Vault/HOOK reads. Fixed events contain no identity or request data.
func (s *fastSandboxPolicyServer) logPendingRebind(subj subject.Subject, token *subjectRebind) {
	if token == nil {
		return
	}
	s.mu.Lock()
	pending := s.rebinds[subj] == token
	s.mu.Unlock()
	if pending {
		log.Warnf("subject_rebind_pending")
	}
}

// subjectRebinding permits fail-fast rejection before waiting on a policy commit.
// Commit paths must repeat this check under mu after acquiring their commit lock.
func (s *fastSandboxPolicyServer) subjectRebinding(subj subject.Subject) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subjectRebindingLocked(subj)
}

// subjectRebindingNow is only an early rejection hint. It never queues before
// the request's registry pre-read; the authoritative guard is repeated under mu.
func (s *fastSandboxPolicyServer) subjectRebindingNow(subj subject.Subject) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	return s.subjectRebindingLocked(subj)
}
