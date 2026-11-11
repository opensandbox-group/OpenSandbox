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
	"errors"
	"fmt"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// atomicPolicyFileStore is private to the policy owner. The real store owns
// storage-stage classification; runtime tests substitute outcomes at this seam.
type atomicPolicyFileStore interface {
	Snapshot() (*policy.PolicyFileSnapshot, error)
	Save(*policy.NetworkPolicy) (policy.FileMutationState, error)
	Restore(*policy.PolicyFileSnapshot) (policy.FileMutationState, error)
}

// persistPolicyChangeLocked runs under s.mu. Its restoration closure must also
// run under that barrier, before publication or another mutation can occur.
// Neither successful restoration nor a subsequent clean read clears recovery.
func (s *policyServer) persistPolicyChangeLocked(pol *policy.NetworkPolicy) (func() error, error) {
	noop := func() error { return nil }
	if s.policyFile == "" {
		return noop, nil
	}
	if s.revisionRecovery == nil {
		previous, exists, err := s.readPolicyFile()
		if err != nil {
			return nil, err
		}
		restore := func() error { return s.restorePolicyFile(previous, exists) }
		if err := s.persistPolicy(pol); err != nil {
			if restoreErr := restore(); restoreErr != nil {
				log.Errorf("policy API: restore legacy policy file after failed persist: %v", restoreErr)
			}
			return nil, err
		}
		return restore, nil
	}
	// Startup initializes this store before serving. Lazy initialization also
	// validates owners assembled by internal callers; there is no legacy fallback.
	if s.atomicPolicyFile == nil {
		store, err := policy.NewAtomicPolicyFile(s.policyFile)
		if err != nil {
			return nil, err
		}
		s.atomicPolicyFile = store
	}
	snapshot, err := s.atomicPolicyFile.Snapshot()
	if err != nil {
		return nil, err
	}
	restore := func() error {
		state, err := s.atomicPolicyFile.Restore(snapshot)
		if err == nil && state != policy.FileCommitted {
			return errors.New("atomic policy restore did not confirm durable completion")
		}
		return err
	}
	state, err := s.atomicPolicyFile.Save(pol)
	if err == nil && state == policy.FileCommitted {
		s.invalidateRevisionBootstrapLocked()
		return restore, nil
	}
	if err == nil {
		// A contradictory successful return cannot prove no effect, even if
		// the store labels it Unchanged. Only an explicit failed/no-effect
		// result permits the retryable branch below.
		state = policy.FileUnknown
		err = errors.New("atomic policy save did not confirm durable completion")
	}
	if state == policy.FileUnchanged {
		// The original never changed. Do not restore, invalidate a bootstrap,
		// apply nft, or freeze healthy writers for a known pre-rename failure.
		return nil, err
	}
	// Rename may have happened; classify before attempting best-effort restore.
	// This latches readiness/publication restrictions and drains runtime writers.
	s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
	if restoreErr := restore(); restoreErr != nil {
		log.Errorf("policy API: atomic policy restore after uncertain persist: %v", restoreErr)
		return nil, fmt.Errorf("%w (restore also failed: %v)", err, restoreErr)
	}
	return nil, err
}
