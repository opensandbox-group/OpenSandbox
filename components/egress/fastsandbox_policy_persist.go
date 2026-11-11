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

// Egress-held policy persistence: one file per subject under
// <egress-volume>/policy/, mirroring the vault persistence machinery
// (fastsandbox_vault_persist.go). The recorded policy is the recovery
// source for an egress restart — at SET_BINDING replay the committed file
// supersedes the replayed binding input, which carries only the original
// bootstrap policy — and the bundle source for the POLICY_EXPORT /
// POLICY_IMPORT snapshot transfer. Ordering is persist → apply → ack.
//
// Durability is tiered by replayability: the bootstrap candidate
// (applyBindingPolicyLocked) publishes non-durably — rev 1 is byte-identical
// to the binding input, which the fastlet re-issues on every egress instance
// change, so a lost rename is reconstructed exactly by replay; relay writes
// and the removal tombstone record state that outruns the binding input and
// stay fully durable (acked ⇒ durable for them). For the bootstrap ack the
// guarantee is "recorded and enforced (deny-first)", not "durable". The
// three-state classification, snapshot restore, and recovery latch remain
// the same as the vault files.
//
// TODO(encryption-at-rest): template-borne state blobs are stored as-is.
// When encryption lands it protects the store-side reader only — CLONE
// ISOLATION is determined by KEY SCOPING (per-tenant/pool keys), so design
// the key hierarchy with the template-distribution flow in mind.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// policyRecoveryState is the recovery latch payload.
type policyRecoveryState struct {
	reason string
}

// policyPersistence owns the per-subject policy files: <dir>/<sandboxUid>.json,
// with lazily constructed, cached file stores. The file store seam is the
// same vaultFileStore contract — one atomic-file abstraction serves both
// state kinds.
type policyPersistence struct {
	dir string
	// newFile is injectable for tests; the default is the atomic file machinery.
	newFile func(path string) (vaultFileStore, error)

	mu    sync.Mutex
	files map[subject.Subject]vaultFileStore
}

// newPolicyPersistence prepares the policy directory (0700) and sweeps
// staged leftovers: startup loads the committed files only.
func newPolicyPersistence(dir string) (*policyPersistence, error) {
	if err := prepareStateDir(dir, "policy"); err != nil {
		return nil, err
	}
	return &policyPersistence{
		dir:     dir,
		newFile: func(path string) (vaultFileStore, error) { return policy.NewAtomicPolicyFile(path) },
		files:   make(map[subject.Subject]vaultFileStore),
	}, nil
}

func (p *policyPersistence) pathFor(subj subject.Subject) (string, error) {
	uid := vaultFileUID(subj)
	// The relay validates the path-derived UID; envelope UIDs get the same check.
	if !validVaultSubjectID(uid) {
		return "", fmt.Errorf("subject uid %q cannot name a policy file", uid)
	}
	return filepath.Join(p.dir, uid+".json"), nil
}

// storeFor returns the subject's file store, constructing it on first use.
func (p *policyPersistence) storeFor(subj subject.Subject) (vaultFileStore, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if fs, ok := p.files[subj]; ok {
		return fs, nil
	}
	path, err := p.pathFor(subj)
	if err != nil {
		return nil, err
	}
	fs, err := p.newFile(path)
	if err != nil {
		return nil, err
	}
	p.files[subj] = fs
	return fs, nil
}

// purge removes the subject's file and evicts the cached store. An
// already-absent file is a complete purge; an uncertain outcome is reported —
// the leftover stays fenced out by its recorded runtime fence either way.
// The seal marker (if any) goes with it.
func (p *policyPersistence) purge(subj subject.Subject) error {
	fs, err := p.storeFor(subj)
	if err != nil {
		return err
	}
	state, err := fs.Delete()
	if err == nil || state == policy.FileUnchanged {
		p.mu.Lock()
		delete(p.files, subj)
		p.mu.Unlock()
	}
	if err != nil {
		return err
	}
	if state == policy.FileUnknown {
		return errors.New("policy file unlink outcome uncertain")
	}
	return p.unseal(subj)
}

// markerFor names the durable seal marker: export writes it before acking
// sealed, and a handler restart between export and REMOVE_BINDING restores
// the seal from it — otherwise binding replay would treat the bundle-backed
// state as live while the bundle copy is already taken.
func (p *policyPersistence) markerFor(subj subject.Subject) (string, error) {
	path, err := p.pathFor(subj)
	if err != nil {
		return "", err
	}
	return path + ".sealed", nil
}

// seal writes the marker durably. Presence is the signal; no fsync — the
// marker only needs to survive a container restart, and a torn write still
// exists.
func (p *policyPersistence) seal(subj subject.Subject) error {
	path, err := p.markerFor(subj)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte("sealed"), 0o600)
}

// unseal removes the marker; an absent marker is a complete unseal.
func (p *policyPersistence) unseal(subj subject.Subject) error {
	path, err := p.markerFor(subj)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// isSealed reports whether the durable seal marker exists.
func (p *policyPersistence) isSealed(subj subject.Subject) bool {
	path, err := p.markerFor(subj)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// reservesPath reports whether a driver-provided snapshot path resolves
// inside the policy persistence directory — reserved for per-subject policy
// files (an export target there could clobber another subject's policy).
func (p *policyPersistence) reservesPath(path string) bool {
	return reservesPathUnder(p.dir, path)
}

// SetPolicyFiles wires persistence under the relay handlers. Policy
// persistence is REQUIRED in the fast-sandbox profile (startup fails
// without it), so every assembled server carries a store.
func (s *fastSandboxPolicyServer) SetPolicyFiles(pp *policyPersistence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policyFiles = pp
}

// reservesStatePath reports whether a driver-provided snapshot path
// resolves inside ANY per-subject state directory — the vault and policy
// directories are siblings on one volume, and an export target under the
// other kind's directory could clobber its state (the rehydrate would then
// quarantine the corrupted file).
func (s *fastSandboxPolicyServer) reservesStatePath(path string) bool {
	// Policy persistence is required in this profile; vault persistence
	// stays optional.
	if s.policyFiles.reservesPath(path) {
		return true
	}
	return s.vaultFiles != nil && s.vaultFiles.reservesPath(path)
}

// purgePolicyFile removes the subject's persisted policy (rebind or terminal
// cleanup). Best effort: a failed purge leaves a file that its recorded
// fence keeps out of every later registration.
func (s *fastSandboxPolicyServer) purgePolicyFile(subj subject.Subject) {
	if s.policyFiles == nil {
		return
	}
	if err := s.policyFiles.purge(subj); err != nil {
		log.Errorf("subject %s: policy file purge failed (leftover stays fenced out): %v", subj, err)
	}
}

// persistPolicyCandidateLocked runs the commit protocol; the caller
// serializes per-subject writers on policyCommitMu for the whole operation
// (relay writes and SET_BINDING commits both take it). persist performs the
// durable effect and returns its three-state classification; apply commits
// the memory mutation and must leave the live state untouched on failure (a
// failed apply only needs the file rolled back). The apply closure must not
// hold s.mu around enforcement (applyPolicy takes the registry write lock,
// which serializes against takeover paths that take s.mu).
func (s *fastSandboxPolicyServer) persistPolicyCandidateLocked(
	subj subject.Subject,
	persist func(vaultFileStore) (policy.FileMutationState, error),
	apply func() error,
) error {
	fs, err := s.policyFiles.storeFor(subj)
	if err != nil {
		return fmt.Errorf("policy file store unavailable: %w", err)
	}
	before, err := fs.Snapshot()
	if err != nil {
		return fmt.Errorf("policy file snapshot: %w", err)
	}
	state, err := persist(fs)
	switch {
	case err == nil && state == policy.FileCommitted:
		// Rename + parent-dir fsync done.
	case err == nil && state == policy.FileUnchanged:
		// Delete of an already-absent file (SaveRaw never returns this pair).
	case err == nil:
		// A success that did not confirm an effect is uncertain.
		state = policy.FileUnknown
		err = errors.New("policy file persist did not confirm durable completion")
	}
	if err != nil && state == policy.FileUnchanged {
		// Pre-rename failure: last-good intact — NACK without restore or latch.
		return err
	}
	if err != nil {
		// Publication attempted, durability unconfirmed: latch first (only a
		// restart reconciles), then restore so disk agrees with the untouched
		// live state.
		s.requirePolicyRecoveryLocked("policy file persist outcome uncertain")
		if restoreErr := restoreVaultFile(fs, before); restoreErr != nil {
			log.Errorf("subject %s: policy file restore after uncertain persist: %v", subj, restoreErr)
			return fmt.Errorf("%w (restore also failed: %v)", err, restoreErr)
		}
		return err
	}
	// Durable. A failed apply rolls the file back — no latch while the
	// restore is confirmed.
	if applyErr := apply(); applyErr != nil {
		if restoreErr := restoreVaultFile(fs, before); restoreErr != nil {
			s.requirePolicyRecoveryLocked("policy file restore after failed apply was inconclusive")
			log.Errorf("subject %s: policy file restore after failed apply: %v", subj, restoreErr)
			return fmt.Errorf("policy apply failed (%v) and restore failed (%v)", applyErr, restoreErr)
		}
		return fmt.Errorf("policy apply: %w", applyErr)
	}
	return nil
}

// requirePolicyRecoveryLocked latches recovery (first reason wins): policy
// writes freeze until the process restarts and reconciles from the committed
// files. The latch is an atomic so the healthz probe reads it lock-free.
func (s *fastSandboxPolicyServer) requirePolicyRecoveryLocked(reason string) {
	if s.policyRecovery.Load() == nil {
		s.policyRecovery.CompareAndSwap(nil, &policyRecoveryState{reason: reason})
		log.Errorf("policy persistence recovery latched (writes frozen until restart): %s", reason)
	}
}

// policyRecoveryActive reports whether the recovery latch freezes writes.
func (s *fastSandboxPolicyServer) policyRecoveryActive() bool {
	return s.policyRecovery.Load() != nil
}

// policyHealthIssue is non-empty only for the process-level failure — the
// recovery latch.
func (s *fastSandboxPolicyServer) policyHealthIssue() string {
	if state := s.policyRecovery.Load(); state != nil {
		return "policy persistence recovery required: " + state.reason
	}
	return ""
}

// clearPolicyRecordLocked removes the recorded policy (binding removal on a
// still-live sandbox). A failed purge falls back to a default-deny
// tombstone — a leftover file would otherwise resurrect the removed policy
// at the next same-fence registration; a tombstone persist failure latches
// recovery. The caller holds policyCommitMu.
func (s *fastSandboxPolicyServer) clearPolicyRecordLocked(subj subject.Subject) error {
	s.mu.Lock()
	entry := s.policies[subj]
	pp := s.policyFiles
	s.mu.Unlock()
	if entry == nil {
		// A previous attempt may have cleared the memory record but failed
		// the durable purge: retry the idempotent cleanup so the acked
		// removal is real (the Fastlet's retry lands here).
		if pp != nil {
			if err := pp.purge(subj); err != nil {
				return fmt.Errorf("policy file purge retry: %w", err)
			}
		}
		return nil
	}
	s.mu.Lock()
	delete(s.policies, subj)
	delete(s.pendingPolicies, subj)
	s.mu.Unlock()
	if pp == nil {
		return nil
	}
	if err := pp.purge(subj); err == nil {
		return nil
	} else {
		log.Warnf("subject %s: policy file purge failed (%v); persisting a deny tombstone instead", subj, err)
	}
	deny := policy.DefaultDenyPolicy()
	raw, err := json.Marshal(deny)
	if err != nil {
		return fmt.Errorf("deny tombstone marshal: %w", err)
	}
	rev := entry.revision + 1
	payload, err := policy.EncodePolicyFile(entry.fence.RuntimeInstanceID, entry.fence.AttachmentID, rev, raw)
	if err != nil {
		return fmt.Errorf("deny tombstone encode: %w", err)
	}
	err = s.persistPolicyCandidateLocked(
		subj,
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRaw(payload)
		},
		func() error {
			s.mu.Lock()
			s.pendingPolicies[subj] = deny
			s.policies[subj] = &policyEntry{revision: rev, fence: entry.fence}
			s.mu.Unlock()
			return nil
		},
	)
	if err != nil {
		log.Errorf("subject %s: deny tombstone persist failed: %v", subj, err)
		return fmt.Errorf("deny tombstone persist: %w", err)
	}
	log.Infof("subject %s: deny tombstone persisted after purge failure (revision %d)", subj, rev)
	return nil
}

// rehydratePolicyReload is the egress-restart recovery path: at
// (re)registration of a subject with no recorded policy entry, load its
// committed file, fenced by the full runtime identity:
//
//   - fence match → the file is authoritative (the replayed input carries
//     only the bootstrap policy, never relay mutations)
//   - mismatch → stale, purged
//   - unreadable, a newer schema, or unparseable → the subject DEGRADES
//     fail-closed (pending default-deny, healed by a relay PUT) — the
//     bootstrap input could be more permissive than the unreadable runtime
//     change, so it must not seed
//
// Removal SET_BINDINGS skip the reload (clearPolicyRecordLocked). A live
// record for this exact fence wins (duplicate SET_BINDING). Runs BEFORE
// RegisterAndEnforce, mirroring rehydrateVaultReload.
// Returns the policy the subject must run per the reload: the file-backed
// policy, default-deny when degraded, or nil when the binding input seeds
// (fresh subject).
func (s *fastSandboxPolicyServer) rehydratePolicyReload(subj subject.Subject, fence subject.Fencing) (seed *policy.NetworkPolicy, degraded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.policies[subj]; e != nil {
		if e.fence == fence {
			return nil, false
		}
		delete(s.policies, subj)
	}
	fs, err := s.policyFiles.storeFor(subj)
	if err != nil {
		s.degradePolicyReloadLocked(subj, fence, fmt.Sprintf("policy file store unavailable: %v", err))
		return policy.DefaultDenyPolicy(), true
	}
	snap, err := fs.Snapshot()
	if err != nil {
		s.degradePolicyReloadLocked(subj, fence, fmt.Sprintf("policy file unreadable: %v", err))
		return policy.DefaultDenyPolicy(), true
	}
	if !snap.Exists() {
		// Fresh subject: the binding input seeds. A durable seal marker (an
		// export that sealed a file-less subject) still gates the relay
		// ops — the seed entry carries it (caller holds s.mu).
		if s.policyFiles.isSealed(subj) {
			s.policies[subj] = &policyEntry{fence: fence, sealed: true}
			s.pendingPolicies[subj] = policy.DefaultDenyPolicy()
			log.Infof("subject %s: seal restored from marker (file-less subject)", subj)
			return policy.DefaultDenyPolicy(), true
		}
		return nil, false
	}
	pf, body, decErr := policy.DecodePolicyFile(snap.Data())
	if decErr != nil {
		// Fail closed: the file may carry an acknowledged runtime change
		// (e.g. a tightening) that this egress can no longer read — seeding
		// the bootstrap input would silently undo it. Corrupt files are
		// quarantined; newer-schema files stay for the newer writer. A
		// relay PUT heals the subject.
		if !errors.Is(decErr, policy.ErrUnsupportedPolicyFileVersion) {
			if _, delErr := fs.Delete(); delErr != nil {
				log.Errorf("subject %s: unreadable policy file quarantine failed (file left in place): %v", subj, delErr)
			}
		}
		s.degradePolicyReloadLocked(subj, fence, fmt.Sprintf("policy file unusable (%v)", decErr))
		return policy.DefaultDenyPolicy(), true
	}
	recorded := subject.Fencing{RuntimeInstanceID: pf.Incarnation, AttachmentID: pf.AttachmentID}
	if !recorded.Matches(fence) {
		// Stale: discard durably. A failed purge is harmless — the recorded
		// fence mismatches every later registration regardless.
		if _, delErr := fs.Delete(); delErr != nil {
			log.Warnf("subject %s: stale policy file purge failed (stays fenced out): %v", subj, delErr)
		} else {
			log.Infof("subject %s: stale policy file discarded (fence %s/%s ≠ %s/%s)", subj, pf.Incarnation, pf.AttachmentID, fence.RuntimeInstanceID, fence.AttachmentID)
		}
		return nil, false
	}
	pol, parseErr := policy.ParsePolicy(string(body))
	if parseErr != nil {
		if _, delErr := fs.Delete(); delErr != nil {
			log.Errorf("subject %s: unparseable policy file quarantine failed (file left in place): %v", subj, delErr)
			s.degradePolicyReloadLocked(subj, fence, fmt.Sprintf("policy file quarantine failed: %v", delErr))
			return policy.DefaultDenyPolicy(), true
		}
		s.degradePolicyReloadLocked(subj, fence, fmt.Sprintf("policy file content rejected (%v)", parseErr))
		return policy.DefaultDenyPolicy(), true
	}
	entry := &policyEntry{revision: pf.Revision, fence: fence}
	// A durable seal marker (export acked, teardown not yet delivered)
	// restores the sealed entry across a restart: relay ops stay 409 until
	// REMOVE_BINDING purges or an import unseals.
	if s.policyFiles.isSealed(subj) {
		entry.sealed = true
		log.Infof("subject %s: policy seal restored from marker (exported bundle pending)", subj)
	}
	s.policies[subj] = entry
	s.pendingPolicies[subj] = pol
	log.Infof("subject %s: policy reloaded from file (revision %d, incarnation %s); binding input superseded", subj, pf.Revision, pf.Incarnation)
	return pol, false
}

// applyBindingPolicyLocked seeds the binding's policy for a freshly
// registered subject (record absent). A subject with an existing record
// never reads its binding input again — the recorded policy is
// authoritative, like the vault slice — so binding replays are policy
// no-ops. The caller holds policyCommitMu.
func (s *fastSandboxPolicyServer) applyBindingPolicyLocked(subj subject.Subject, fence subject.Fencing, pol *policy.NetworkPolicy, stateActive bool) error {
	s.mu.Lock()
	entry := s.policies[subj]
	if entry != nil && entry.fence != fence {
		// A stale record (its purge outcome was uncertain, or a rebind
		// interleaved the purge) must never supersede the fresh
		// registration's input: purge it durably and seed from the input.
		delete(s.policies, subj)
		pp := s.policyFiles
		s.mu.Unlock()
		if pp != nil {
			if err := pp.purge(subj); err != nil {
				log.Errorf("subject %s: stale policy record purge failed (leftover stays fenced out): %v", subj, err)
			}
		}
		log.Infof("subject %s: stale policy record discarded (fence mismatch), binding input seeds", subj)
		s.mu.Lock()
		entry = nil
	}
	s.mu.Unlock()
	if entry != nil {
		log.Infof("subject %s: binding input superseded by the recorded policy (revision %d)", subj, entry.revision)
		return nil
	}

	const rev = int64(1)
	raw, err := json.Marshal(pol)
	if err != nil {
		return fmt.Errorf("policy marshal: %w", err)
	}
	apply := func() error {
		if stateActive {
			// Enforcement first: a failed apply leaves the record untouched
			// (the commit protocol rolls the file back). Called without
			// s.mu — it takes the registry write lock.
			if err := s.applyPolicy(subj, pol); err != nil {
				return err
			}
		}
		s.mu.Lock()
		// The pending slot mirrors what is enforced (same rationale as the
		// relay PUT: a replayed data-plane-ready must be idempotent).
		s.pendingPolicies[subj] = pol
		s.policies[subj] = &policyEntry{revision: rev, fence: fence}
		s.mu.Unlock()
		return nil
	}
	payload, err := policy.EncodePolicyFile(fence.RuntimeInstanceID, fence.AttachmentID, rev, raw)
	if err != nil {
		return fmt.Errorf("policy file encode: %w", err)
	}
	// The bootstrap seed publishes NON-durably: rev 1 is byte-identical to
	// the binding input, which the fastlet re-issues on every egress
	// instance change — a crash that loses the rename is reconstructed
	// exactly by that replay (deny-first nft already enforces meanwhile).
	// Everything that outruns the binding input stays durable: relay
	// writes (SaveRaw) and the removal tombstone in
	// clearPolicyRecordLocked.
	return s.persistPolicyCandidateLocked(
		subj,
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRawNonDurable(payload)
		},
		apply,
	)
}

// degradePolicyReloadLocked materializes the subject's record as a degraded
// default-deny entry: the pending policy is default-deny (the
// data-plane-ready barrier activates deny, never a stale or permissive
// policy) and the degraded mark surfaces on the actions status until a
// successful relay write heals it. Callers hold s.mu — the reload runs
// inside rehydratePolicyReload, before RegisterAndEnforce (same point as
// the healthy reload).
func (s *fastSandboxPolicyServer) degradePolicyReloadLocked(subj subject.Subject, fence subject.Fencing, reason string) {
	entry := &policyEntry{fence: fence, degraded: reason}
	if s.policyFiles.isSealed(subj) {
		entry.sealed = true
	}
	s.policies[subj] = entry
	s.pendingPolicies[subj] = policy.DefaultDenyPolicy()
	log.Warnf("subject %s: policy degraded fail-closed (%s); default-deny pending until a relay write heals it", subj, reason)
}

// degradedPolicyUIDs lists the subjects with a degraded policy record, as
// raw sandbox UIDs, sorted for stable surfacing.
func (s *fastSandboxPolicyServer) degradedPolicyUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for subj, e := range s.policies {
		if e.degraded != "" {
			out = append(out, vaultFileUID(subj))
		}
	}
	sort.Strings(out)
	return out
}
