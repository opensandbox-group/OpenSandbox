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

package credentialvault

import (
	"fmt"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// PolicySnapshot is the immutable, already-rendered Vault portion of a
// policy-only candidate. It owns no publication or credential-source lifecycle.
type PolicySnapshot struct {
	owner       *Store
	mutationTag string
	exists      bool
	snapshot    ActiveSnapshot
}

func (PolicySnapshot) String() string   { return "credentialvault.PolicySnapshot" }
func (PolicySnapshot) GoString() string { return "credentialvault.PolicySnapshot{}" }
func (s *PolicySnapshot) Exists() bool  { return s != nil && s.exists }
func (s *PolicySnapshot) Snapshot() ActiveSnapshot {
	if s == nil {
		return ActiveSnapshot{}
	}
	return cloneActiveSnapshot(s.snapshot)
}

// FreezeForPolicy validates every binding against a frozen effective policy and
// captures its rendered snapshot and mutation identity under one Store lock.
// The caller must own its policy/Vault barrier and keep pol immutable. No source
// is resolved: a bound Vault requires the pinned snapshot from CommitCandidate.
// Legacy Create/Patch (and ActiveSnapshot reads) do not satisfy that prerequisite.
// An empty Vault can be captured without rendering, retaining its public revision.
func (v *Store) FreezeForPolicy(pol *policy.NetworkPolicy) (*PolicySnapshot, error) {
	if v == nil {
		return nil, ErrInvalidCandidate
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.validateCandidate(v.credentials, v.bindings, pol); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCandidate, err)
	}
	for _, binding := range v.bindings {
		for _, host := range binding.Match.Hosts {
			if !pol.AllowsEntireHostSelector(host) {
				return nil, fmt.Errorf("binding %q host %q is not entirely allowed by egress policy: %w", binding.Name, host, ErrInvalidCandidate)
			}
		}
	}
	snapshot := ActiveSnapshot{Revision: v.revision}
	if v.activeSnapshot != nil {
		snapshot = cloneActiveSnapshot(*v.activeSnapshot)
	} else if len(v.bindings) > 0 {
		return nil, ErrCandidateNotRendered
	}
	return &PolicySnapshot{owner: v, mutationTag: v.mutationTag, exists: v.exists, snapshot: snapshot}, nil
}

// ValidatePolicySnapshot checks owner and private mutation identity, detecting
// public-revision ABA including absent -> create -> delete. This is a point-in-
// time check only: a future publication owner must hold the shared policy/Vault
// barrier across preparation, validation, and any external transaction.
func (v *Store) ValidatePolicySnapshot(s *PolicySnapshot) error {
	if v == nil || s == nil || s.owner != v {
		return ErrInvalidCandidate
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if s.mutationTag != v.mutationTag {
		return ErrStaleCandidate
	}
	return nil
}
