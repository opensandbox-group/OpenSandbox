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

// Policy snapshot transfer across pause/resume, mirroring the vault snapshot
// transfer (fastsandbox_vault_snapshot.go). POLICY_EXPORT copies the
// subject's recorded policy file into the snapshot bundle strictly before the
// runtime teardown and seals the subject; POLICY_IMPORT lands the bundle
// file re-stamped with the restored fence — the only authorized incarnation
// transfer — through the same commit protocol as the relay writes, marking
// the recorded policy so later binding replays never override it. The
// invocation side (driver/Fastlet hooks, snapshot manifest) is platform
// work in the fast-sandbox repository.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// maxPolicySnapshotBytes bounds a bundle read: the largest valid policy file
// is a max relay body plus the encoded envelope — anything over this is not
// a v1 policy file.
const maxPolicySnapshotBytes = policy.MaxEncodedPolicyFileBytes

var errPolicySnapshotTooLarge = fmt.Errorf("policy bundle file exceeds %d bytes", maxPolicySnapshotBytes)

// applyPolicyExport copies the recorded policy file into the bundle target
// and seals the subject (a subject without a policy file just seals).
// Failures leave it unsealed and enforcement unchanged — the driver fails
// the pause checkpoint and retries. Three phases, the guarded ones
// serialized with the relay commits: guards + source image, the copy, then
// re-check + seal (source changed mid-copy → 409; the recovery latch
// rejects the export in both guarded phases).
func (s *fastSandboxPolicyServer) applyPolicyExport(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	target := strings.TrimSpace(env.Snapshot.Path)
	if s.reservesStatePath(target) {
		return http.StatusBadRequest, fmt.Errorf("policy bundle target %q must not live under a per-subject state directory", target)
	}
	// A subject that never registered (a sandbox created without an egress
	// binding, or after terminal cleanup) has no policy to carry: 200 so the
	// checkpoint proceeds — a 404 would fail every pause of a binding-less
	// sandbox. A registered subject under a stale fence still fails below.
	// Registry reads before the lock acquisitions (lock order: registry →
	// policyCommitMu → s.mu, see selectRelayPolicy).
	regFence, registered := s.reg.Fence(subj)
	if !registered {
		log.Infof("policy export: subject %s not registered; no policy to carry, checkpoint proceeds without it", subj)
		return http.StatusOK, nil
	}

	// Phase 1: guards + consistent source image, serialized with the policy
	// relay commits (policyCommitMu). A relay writer that passed its guards
	// but has not committed yet is either flushed before the snapshot (the
	// copy carries its write) or blocked until phase 3.
	s.policyCommitMu.Lock()
	s.mu.Lock()
	snap, status, err := s.policyExportSourceImageLocked(subj, fence, regFence, registered)
	s.mu.Unlock()
	s.policyCommitMu.Unlock()
	if err != nil {
		return status, err
	}

	// Phase 2: no lock held across the driver-owned write. A relay commit
	// here is caught by the phase-3 re-check (the copied image no longer
	// matches the live file → 409, no seal).
	if snap.Exists() {
		bundle, err := s.policyFiles.newFile(target)
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("policy bundle target unavailable: %w", err)
		}
		state, err := bundle.SaveRaw(snap.Data())
		if err != nil || state != policy.FileCommitted {
			return http.StatusInternalServerError, fmt.Errorf("policy bundle copy failed (state %v): %v", state, err)
		}
	}

	// Phase 3: seal only against the exact copied image, again serialized
	// with the relay commits — between the re-check and the seal no writer
	// can pass the guards (they now observe the seal) or commit. The fence
	// is re-read fresh (registry before s.mu) so a rebind during the copy
	// fails the export instead of sealing the wrong subject's bytes.
	regFence, registered = s.reg.Fence(subj)
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	live, status, err := s.policyExportSourceImageLocked(subj, fence, regFence, registered)
	if err != nil {
		return status, err
	}
	if live.Exists() != snap.Exists() || (snap.Exists() && !bytes.Equal(live.Data(), snap.Data())) {
		return http.StatusConflict, fmt.Errorf("policy file changed during export for subject %s; bundle copy not sealed", subj)
	}
	if !snap.Exists() {
		// Nothing to carry: a previous attempt may have left stale bytes at
		// the target — remove them, or a later import would restore a policy
		// deleted during the pause.
		bundle, err := s.policyFiles.newFile(target)
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("policy bundle target unavailable: %w", err)
		}
		state, err := bundle.Delete()
		if err != nil || (state != policy.FileCommitted && state != policy.FileUnchanged) {
			return http.StatusInternalServerError, fmt.Errorf("policy bundle removal failed (state %v): %v", state, err)
		}
	}
	// Seal-free template carry: the record was copied verbatim and the
	// source keeps full relay CRUD — no latch, bookkeeping untouched.
	if !env.Snapshot.Sealed() {
		if snap.Exists() {
			log.Infof("subject %s: policy exported to snapshot bundle, seal-free (template carry)", subj)
		} else {
			log.Infof("subject %s: no policy file to export, seal-free (template carry)", subj)
		}
		return http.StatusOK, nil
	}
	// Durable seal first: a restart before REMOVE_BINDING must reload the
	// entry sealed, or relay writes would stale the taken bundle.
	if err := s.policyFiles.seal(subj); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("policy seal marker: %w", err)
	}
	e := s.policies[subj]
	if e == nil || e.fence != fence {
		e = &policyEntry{fence: fence}
	}
	e.sealed = true
	s.policies[subj] = e
	if snap.Exists() {
		log.Infof("subject %s: policy exported to snapshot bundle, subject sealed", subj)
	} else {
		log.Infof("subject %s: no policy file to export, subject sealed", subj)
	}
	return http.StatusOK, nil
}

// policyExportSourceImageLocked runs the export guards and returns a
// consistent image of the subject's policy file. regFence/registered are
// the caller's pre-read of the registration (lock order: registry →
// policyCommitMu → s.mu, see applyPolicyExport). Callers hold s.mu.
func (s *fastSandboxPolicyServer) policyExportSourceImageLocked(subj subject.Subject, fence subject.Fencing, regFence subject.Fencing, registered bool) (*policy.PolicyFileSnapshot, int, error) {
	if s.subjectRebindingLocked(subj) {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("subject rebind in progress")
	}
	if !registered {
		return nil, http.StatusNotFound, fmt.Errorf("policy export for unknown subject %s", subj)
	}
	if !regFence.Matches(fence) {
		return nil, http.StatusConflict, fmt.Errorf("stale policy export (fence mismatch) for subject %s", subj)
	}
	if !s.fenceMatchesLocked(subj, fence) {
		// Purged between the pre-read and the lock — the stale envelope
		// lost the race with a rebind. The registered-fence mismatch above
		// already covers the plain stale case.
		return nil, http.StatusNotFound, fmt.Errorf("policy export for unknown subject %s", subj)
	}
	if recovery := s.policyRecovery.Load(); recovery != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("policy persistence recovery required: %s", recovery.reason)
	}
	fs, err := s.policyFiles.storeFor(subj)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("policy file store unavailable: %w", err)
	}
	snap, err := fs.Snapshot()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("policy file snapshot: %w", err)
	}
	if snap.Exists() {
		// Carry only bytes attributable to the registered fence: a leftover
		// from a previous incarnation (purge failed), a corrupt file, or a
		// newer schema is treated as absent — never exported, or import
		// would re-stamp a foreign policy as current.
		pf, _, decErr := policy.DecodePolicyFile(snap.Data())
		if decErr != nil || pf.Incarnation != regFence.RuntimeInstanceID || pf.AttachmentID != regFence.AttachmentID {
			what := "unattributable fence"
			if errors.Is(decErr, policy.ErrUnsupportedPolicyFileVersion) {
				what = "newer schema"
			} else if decErr != nil {
				what = "unreadable"
			}
			log.Infof("subject %s: policy file not carried (%s)", subj, what)
			return policy.NewFileSnapshot("", nil, false), 0, nil
		}
	}
	return snap, 0, nil
}

// applyPolicyImport lands the bundle as the subject's policy file,
// re-stamped with the restored fence from the envelope (the only authorized
// incarnation transfer) and installs it file-backed. Delivery is
// at-least-once: a local revision newer than the bundle acks idempotently.
// Unregistered (before SET_BINDING), only the file lands — the
// registration-time reload validates it; registered, the policy installs
// into the live record and a rejected bundle is a 400 with last-good kept.
func (s *fastSandboxPolicyServer) applyPolicyImport(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	source := strings.TrimSpace(env.Snapshot.Path)
	if s.reservesStatePath(source) {
		return http.StatusBadRequest, fmt.Errorf("policy bundle source %q must not live under a per-subject state directory", source)
	}

	// Read before locking; a missing/oversized/unreadable source is
	// permanent — the fastlet retry cannot repair it.
	data, err := readPolicyBundle(source)
	if errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound, fmt.Errorf("policy bundle file %q does not exist", source)
	}
	if errors.Is(err, errPolicySnapshotTooLarge) {
		return http.StatusBadRequest, err
	}
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("policy bundle read failed: %w", err)
	}
	pf, body, err := policy.DecodePolicyFile(data)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid policy bundle file: %v", err)
	}
	pol, parseErr := policy.ParsePolicy(string(body))
	if parseErr != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid policy bundle content: %v", parseErr)
	}
	// The authorized incarnation transfer.
	payload, err := policy.EncodePolicyFile(fence.RuntimeInstanceID, fence.AttachmentID, pf.Revision, body)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("policy bundle re-stamp failed: %w", err)
	}

	if s.subjectRebinding(subj) {
		return http.StatusServiceUnavailable, fmt.Errorf("subject rebind in progress")
	}
	// Serialize the commit against relay writes; guards run under s.mu
	// inside (the ordering policyCommitMu → s.mu is the commit path's).
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()

	// The registration fence re-validation happens under s.mu against our
	// own recorded fence (subjFence) — no registry read is needed in this
	// section, so the registry → policyCommitMu → s.mu order is preserved
	// trivially (see applyPolicyExport). An unknown fence (no record) means
	// the subject is not registered: the bundle lands for the next
	// registration.
	s.mu.Lock()
	if s.subjectRebindingLocked(subj) {
		s.mu.Unlock()
		return http.StatusServiceUnavailable, fmt.Errorf("subject rebind in progress")
	}
	// The latch freezes every policy file mutation; only a restart
	// reconciles (relay-write parity).
	if recovery := s.policyRecovery.Load(); recovery != nil {
		s.mu.Unlock()
		return http.StatusServiceUnavailable, fmt.Errorf("policy persistence recovery required: %s", recovery.reason)
	}
	// At-least-once delivery: never roll a newer local revision back. The
	// guard requires the stored fence to match the envelope — a newer file
	// under an old incarnation is stale local state the import must
	// supersede, not ack.
	if fs, fsErr := s.policyFiles.storeFor(subj); fsErr == nil {
		if cur, snapErr := fs.Snapshot(); snapErr == nil && cur.Exists() {
			if curPF, _, decErr := policy.DecodePolicyFile(cur.Data()); decErr == nil &&
				curPF.Incarnation == fence.RuntimeInstanceID && curPF.AttachmentID == fence.AttachmentID &&
				curPF.Revision > pf.Revision {
				s.mu.Unlock()
				log.Infof("subject %s: policy import superseded (local revision %d > bundle revision %d), idempotent ack", subj, curPF.Revision, pf.Revision)
				return http.StatusOK, nil
			}
		}
	}
	registered := s.fenceMatchesLocked(subj, fence)
	if _, known := s.subjFence[subj]; known && !registered {
		s.mu.Unlock()
		return http.StatusConflict, fmt.Errorf("stale policy import (fence mismatch) for subject %s", subj)
	}
	sealed := false
	if e := s.policies[subj]; e != nil && e.sealed {
		sealed = true
	}
	s.mu.Unlock()

	err = s.persistPolicyCandidateLocked(
		subj,
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRaw(payload)
		},
		func() error {
			if !registered {
				// Before SET_BINDING: registration reloads and validates.
				s.mu.Lock()
				delete(s.policies, subj)
				s.mu.Unlock()
				return s.policyFiles.unseal(subj)
			}
			err := s.installImportedPolicyLocked(subj, fence, pf.Revision, pol, sealed)
			if err != nil && sealed {
				// The unseal already happened; restore the durable seal so
				// disk and memory agree the bundle is still the truth (the
				// commit protocol rolls the file back to the sealed image;
				// the in-memory entry was never modified).
				if sealErr := s.policyFiles.seal(subj); sealErr != nil {
					log.Errorf("subject %s: policy seal marker restore failed after import apply failure: %v", subj, sealErr)
				}
			}
			return err
		},
	)
	if err != nil {
		log.Errorf("subject %s: policy import failed: %v", subj, err)
		status := http.StatusInternalServerError
		if recovery := s.policyRecovery.Load(); recovery != nil {
			status = http.StatusServiceUnavailable
		}
		return status, errors.New("policy import persistence failure")
	}
	if registered {
		log.Infof("subject %s: policy imported from snapshot bundle (revision %d, re-stamped to %s/%s)",
			subj, pf.Revision, fence.RuntimeInstanceID, fence.AttachmentID)
	} else {
		log.Infof("subject %s: policy bundle landed for the next registration (revision %d, re-stamped to %s/%s)",
			subj, pf.Revision, fence.RuntimeInstanceID, fence.AttachmentID)
	}
	return http.StatusOK, nil
}

// installImportedPolicyLocked binds the imported revision into the live
// per-subject record under the restored fence, replacing any stale record,
// and unseals it. Ordering inside the commit protocol's apply step: the
// durable unseal FIRST (a failure here mutates nothing live — the commit
// protocol rolls the file back and the seal stays), then the state/fence
// re-read, then enforcement, then the record. Enforcement runs WITHOUT
// s.mu (it takes the registry write lock); the s.mu suffix is the
// docstring's "Locked" sense: map mutations under s.mu.
func (s *fastSandboxPolicyServer) installImportedPolicyLocked(subj subject.Subject, fence subject.Fencing, revision int64, pol *policy.NetworkPolicy, wasSealed bool) error {
	if wasSealed {
		if err := s.policyFiles.unseal(subj); err != nil {
			return err
		}
	}
	state, err := s.policyCommitTarget(subj, fence)
	if err != nil {
		return err
	}
	if state == subject.StateActive {
		// Enforcement first: a failed apply leaves the record untouched
		// (the commit protocol rolls the file back).
		if err := s.applyPolicy(subj, pol); err != nil {
			return err
		}
	}
	s.mu.Lock()
	// The pending slot mirrors what is enforced (same rationale as the
	// relay PUT: a replayed data-plane-ready must be idempotent).
	s.pendingPolicies[subj] = pol
	s.policies[subj] = &policyEntry{revision: revision, fence: fence}
	s.mu.Unlock()
	return nil
}

// readPolicyBundle reads the driver-staged bundle source with a hard size
// cap — a read-only lookup in the driver's trust domain, so plain bounded
// I/O suffices (writes keep the hardened atomic machinery).
func readPolicyBundle(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPolicySnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPolicySnapshotBytes {
		return nil, errPolicySnapshotTooLarge
	}
	return data, nil
}
