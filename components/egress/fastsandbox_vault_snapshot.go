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

// Vault snapshot transfer across pause/resume (OSEP-0022 "Vault Lifecycle").
// VAULT_EXPORT copies the subject's vault file into the snapshot bundle
// strictly before the runtime teardown and seals the subject; VAULT_IMPORT
// lands the bundle file re-stamped with the restored fence — the only
// authorized incarnation transfer — through the same commit protocol as the
// relay writes. The invocation side (driver/Fastlet hooks, snapshot manifest)
// is platform work: until it lands this slice is unit-test only.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// maxVaultSnapshotBytes bounds a bundle read: the largest valid vault file
// is a max relay body plus the encoded envelope — anything over this is not
// a v1 vault file.
const maxVaultSnapshotBytes = credentialvault.MaxEncodedVaultFileBytes

var errVaultSnapshotTooLarge = fmt.Errorf("vault bundle file exceeds %d bytes", maxVaultSnapshotBytes)

// reservesPath reports whether a driver-provided snapshot path resolves
// inside the vault persistence directory — reserved for per-subject vault
// files (an export target there could clobber another subject's vault).
func (p *vaultPersistence) reservesPath(path string) bool {
	return reservesPathUnder(p.dir, path)
}

// reservesPathUnder is the shared path-reservation check for the per-subject
// state directories: only the parent chain may be outside; children whose
// names begin with dots (e.g. "..victim.json") are inside the reserved
// subtree. Uncomparable paths fail closed.
func reservesPathUnder(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		// Uncomparable — fail closed.
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// applyVaultExport copies the subject's vault file into the bundle target and
// marks the subject sealed (SnapshotTransfer.Seal absent/true — the
// checkpoint flow); Seal=false is the seal-free template carry: the record
// is copied verbatim and the source keeps full relay CRUD. A subject
// without a vault file just seals (checkpoint) / exports nothing
// (template). Any
// failure leaves the subject unsealed and active; what the pause flow does
// with that is the caller's call — the fast-sandbox driver fails the pause
// checkpoint, so the teardown never runs without a bundle. A retried export
// re-copies idempotently (while sealed no vault op can change the source).
//
// Three phases: guards + a consistent source image under s.mu, the
// driver-owned copy without the lock (it may be slow; every action and vault
// op goes through s.mu), then a re-check under the lock — sealed only if the
// source is still the exact copied image, else 409. The recovery latch
// rejects the export in both guarded phases: with on-disk state unconfirmed,
// the copy plus the teardown purge would make uncertain bytes the only
// surviving vault content.
func (s *fastSandboxPolicyServer) applyVaultExport(env *actionhandler.Envelope) (int, error) {
	if s.vaultFiles == nil {
		return http.StatusInternalServerError, errors.New("vault export requires vault persistence (OPENSANDBOX_EGRESS_VAULT_DIR)")
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	target := strings.TrimSpace(env.Snapshot.Path)
	if s.reservesStatePath(target) {
		return http.StatusBadRequest, fmt.Errorf("vault bundle target %q must not live under a per-subject state directory", target)
	}
	// A subject that never registered (a sandbox created without an egress
	// binding, or after terminal cleanup) has no vault to carry: 200 so the
	// checkpoint proceeds — a 404 would fail every pause of a binding-less
	// sandbox. A registered subject under a stale fence still fails below.
	// Registry reads before the lock acquisitions (lock order: registry →
	// s.mu, see handleVaultRelayWrite).
	regFence, registered := s.reg.Fence(subj)
	if !registered {
		log.Infof("vault export: subject %s not registered; no vault to carry, checkpoint proceeds without it", subj)
		return http.StatusOK, nil
	}

	// Phase 1: guards + consistent source image.
	s.mu.Lock()
	snap, status, err := s.exportSourceImageLocked(subj, fence, regFence, registered)
	if err != nil {
		s.mu.Unlock()
		return status, err
	}
	s.mu.Unlock()

	// Phase 2: no lock held across the driver-owned write.
	if snap.Exists() {
		bundle, err := s.vaultFiles.newFile(target)
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("vault bundle target unavailable: %w", err)
		}
		state, err := bundle.SaveRaw(snap.Data())
		if err != nil || state != policy.FileCommitted {
			return http.StatusInternalServerError, fmt.Errorf("vault bundle copy failed (state %v): %v", state, err)
		}
	}

	// Phase 3: re-check + seal; the fence is re-read fresh so a rebind
	// during the copy fails the export.
	regFence, registered = s.reg.Fence(subj)
	s.mu.Lock()
	defer s.mu.Unlock()
	live, status, err := s.exportSourceImageLocked(subj, fence, regFence, registered)
	if err != nil {
		return status, err
	}
	if live.Exists() != snap.Exists() || (snap.Exists() && !bytes.Equal(live.Data(), snap.Data())) {
		return http.StatusConflict, fmt.Errorf("vault file changed during export for subject %s; bundle copy not sealed", subj)
	}
	if !snap.Exists() {
		// Nothing to carry: a previous attempt may have left stale bytes at
		// the target — remove them, or a later import would restore
		// credentials deleted during the pause.
		bundle, err := s.vaultFiles.newFile(target)
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("vault bundle target unavailable: %w", err)
		}
		state, err := bundle.Delete()
		if err != nil || (state != policy.FileCommitted && state != policy.FileUnchanged) {
			return http.StatusInternalServerError, fmt.Errorf("vault bundle removal failed (state %v): %v", state, err)
		}
	}
	// Seal-free template carry: the record was copied verbatim and the
	// source keeps full relay CRUD — no latch, bookkeeping untouched. (The
	// vault blob is carried as-is; encryption at rest is a deferred TODO in
	// the vault persist layer, and clone isolation is key scoping, not the
	// blob.)
	if !env.Snapshot.Sealed() {
		if snap.Exists() {
			log.Infof("subject %s: vault exported to snapshot bundle, seal-free (template carry)", subj)
		} else {
			log.Infof("subject %s: no vault file to export, seal-free (template carry)", subj)
		}
		return http.StatusOK, nil
	}
	// Durable seal first: a restart before REMOVE_BINDING must reload the
	// entry sealed, or relay writes would stale the taken bundle.
	if err := s.vaultFiles.seal(subj); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault seal marker: %w", err)
	}
	s.vaultForLocked(subj, fence)
	e := s.vaults[subj]
	e.sealed = true
	s.vaults[subj] = e
	if snap.Exists() {
		log.Infof("subject %s: vault exported to snapshot bundle, subject sealed", subj)
	} else {
		log.Infof("subject %s: no vault file to export, subject sealed", subj)
	}
	return http.StatusOK, nil
}

// exportSourceImageLocked runs the export guards and returns a consistent
// image of the subject's vault file. regFence/registered are the caller's
// pre-read of the registration (lock order: registry → s.mu, see
// applyVaultExport). Callers hold s.mu.
func (s *fastSandboxPolicyServer) exportSourceImageLocked(subj subject.Subject, fence subject.Fencing, regFence subject.Fencing, registered bool) (*policy.PolicyFileSnapshot, int, error) {
	if s.subjectRebindingLocked(subj) {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("subject rebind in progress")
	}
	if !registered {
		return nil, http.StatusNotFound, fmt.Errorf("vault export for unknown subject %s", subj)
	}
	if !regFence.Matches(fence) {
		return nil, http.StatusConflict, fmt.Errorf("stale vault export (fence mismatch) for subject %s", subj)
	}
	if !s.fenceMatchesLocked(subj, fence) {
		// Purged between the pre-read and the lock — the stale envelope
		// lost the race with a rebind. The registered-fence mismatch above
		// already covers the plain stale case.
		return nil, http.StatusNotFound, fmt.Errorf("vault export for unknown subject %s", subj)
	}
	if recovery := s.vaultRecovery.Load(); recovery != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("vault persistence recovery required: %s", recovery.reason)
	}
	if entry, ok := s.vaults[subj]; ok {
		if entry.vaultStateUnknown {
			return nil, http.StatusServiceUnavailable, errors.New("vault state is unknown; refusing snapshot export")
		}
		if entry.fileMeta.version >= credentialvault.VaultFileVersion2 {
			return nil, http.StatusConflict, errors.New("v2 vault state is read-only until lineage-aware restore is supported")
		}
	}
	fs, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("vault file store unavailable: %w", err)
	}
	snap, err := fs.Snapshot()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("vault file snapshot: %w", err)
	}
	if snap.Exists() {
		// The current reader can expose v2 state read-only but cannot restore
		// its ordering lineage. Refuse before copying, sealing, or deleting a
		// stale bundle; treating it as absent would make a successful snapshot
		// impossible to restore.
		vf, _, decErr := credentialvault.DecodeVaultFile(snap.Data())
		if errors.Is(decErr, credentialvault.ErrUnsupportedFileVersion) || (decErr == nil && vf.Version >= credentialvault.VaultFileVersion2) {
			return nil, http.StatusConflict, fmt.Errorf("v2 or unknown vault state cannot be exported before lineage-aware restore is supported")
		}
		// Carry only bytes attributable to the registered fence: a leftover
		// from a previous incarnation (purge failed) or a corrupt v1 file is
		// treated as absent, preserving the legacy behavior.
		if decErr != nil || vf.Incarnation != regFence.RuntimeInstanceID || vf.AttachmentID != regFence.AttachmentID {
			what := "unattributable fence"
			if decErr != nil {
				what = "unreadable"
			}
			log.Infof("subject %s: vault file not carried (%s)", subj, what)
			return policy.NewFileSnapshot("", nil, false), 0, nil
		}
	}
	return snap, 0, nil
}

// applyVaultImport lands the bundle file as the subject's vault file,
// re-stamped with the restored fence from the envelope (restore may land on
// any Pod, so the recorded incarnation is replaced, not compared). The file
// goes through the commit protocol; the vault activates at the same
// data-plane-ready barrier as policy.
//
// The bundle is read and decoded before the lock. Delivery is at-least-once:
// a local revision newer than the bundle acks idempotently — relay writes
// after the first import are never rolled back. Unregistered (before
// SET_BINDING), only the file lands and the registration-time reload
// validates it; registered under the envelope fence, the content is
// validated on a scratch store before anything durable happens, and a
// rejected bundle is a 400 with last-good kept.
func (s *fastSandboxPolicyServer) applyVaultImport(env *actionhandler.Envelope) (int, error) {
	if s.vaultFiles == nil {
		return http.StatusInternalServerError, errors.New("vault import requires vault persistence (OPENSANDBOX_EGRESS_VAULT_DIR)")
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	source := strings.TrimSpace(env.Snapshot.Path)
	if s.reservesStatePath(source) {
		return http.StatusBadRequest, fmt.Errorf("vault bundle source %q must not live under a per-subject state directory", source)
	}

	// Read before locking; a missing/oversized/unreadable source is
	// permanent — the fastlet retry cannot repair it.
	data, err := readVaultBundle(source)
	if errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound, fmt.Errorf("vault bundle file %q does not exist", source)
	}
	if errors.Is(err, errVaultSnapshotTooLarge) {
		return http.StatusBadRequest, err
	}
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault bundle read failed: %w", err)
	}
	vf, body, err := credentialvault.DecodeVaultFile(data)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid vault bundle file: %v", err)
	}
	if vf.Version != credentialvault.VaultFileVersion {
		return http.StatusConflict, fmt.Errorf("v%d vault bundles are read-only until lineage-aware import is supported", vf.Version)
	}
	// The authorized incarnation transfer.
	payload, err := credentialvault.EncodeVaultFile(fence.RuntimeInstanceID, fence.AttachmentID, vf.Revision, body)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault bundle re-stamp failed: %w", err)
	}

	// The whole import holds policyCommitMu (vault-write parity with
	// handleVaultRelayWrite): the validation policy and the registration
	// fence cannot change between the pre-reads and the commit. Lock
	// order: policyCommitMu → registry → s.mu.
	if s.subjectRebinding(subj) {
		return http.StatusServiceUnavailable, fmt.Errorf("subject rebind in progress")
	}
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()
	validation := s.vaultValidationPolicy(subj)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subjectRebindingLocked(subj) {
		return http.StatusServiceUnavailable, fmt.Errorf("subject rebind in progress")
	}
	// The latch freezes every vault file mutation; only a restart
	// reconciles (relay-write parity).
	if recovery := s.vaultRecovery.Load(); recovery != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("vault persistence recovery required: %s", recovery.reason)
	}
	if entry, ok := s.vaults[subj]; ok {
		if entry.vaultStateUnknown {
			return http.StatusServiceUnavailable, errors.New("vault state is unknown; refusing snapshot import")
		}
		if entry.fileMeta.version >= credentialvault.VaultFileVersion2 {
			return http.StatusConflict, errors.New("v2 vault state is read-only until lineage-aware writes are supported")
		}
	}
	// At-least-once delivery: never roll a newer local revision back. The
	// guard requires the stored fence to match the envelope — a newer file
	// under an old incarnation is stale local state the import must
	// supersede, not ack.
	fileStore, fsErr := s.vaultFiles.storeFor(subj)
	if fsErr != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault file store unavailable: %w", fsErr)
	}
	current, snapErr := fileStore.Snapshot()
	if snapErr != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault file snapshot: %w", snapErr)
	}
	if current.Exists() {
		curVF, _, decErr := credentialvault.DecodeVaultFile(current.Data())
		if decErr != nil && errors.Is(decErr, credentialvault.ErrUnsupportedFileVersion) {
			return http.StatusConflict, errors.New("local vault file uses an unsupported schema; refusing to overwrite it")
		}
		if decErr == nil && curVF.Version != credentialvault.VaultFileVersion {
			return http.StatusConflict, errors.New("local v2 vault state is read-only until lineage-aware writes are supported")
		}
		if decErr == nil && curVF.Incarnation == fence.RuntimeInstanceID && curVF.AttachmentID == fence.AttachmentID && curVF.Revision > vf.Revision {
			log.Infof("subject %s: vault import superseded (local revision %d > bundle revision %d), idempotent ack", subj, curVF.Revision, vf.Revision)
			return http.StatusOK, nil
		}
	}
	// Commit-time fence re-validation against our own record (subjFence —
	// see fenceMatchesLocked): an unknown fence means unregistered (the
	// bundle lands for the next registration), a mismatched one is stale.
	registered := s.fenceMatchesLocked(subj, fence)
	if _, known := s.subjFence[subj]; known && !registered {
		return http.StatusConflict, fmt.Errorf("stale vault import (fence mismatch) for subject %s", subj)
	}
	if registered {
		// Validate before any durable effect — a permanently broken bundle
		// is a 400 with last-good kept, never half-open.
		if err := validateImportedVault(vf, body, validation); err != nil {
			return http.StatusBadRequest, fmt.Errorf("vault bundle rejected: %v", err)
		}
	}
	err = s.persistVaultCandidateLocked(
		subj,
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRaw(payload)
		},
		func() error {
			if !registered {
				// Before SET_BINDING: registration reloads and validates.
				return s.vaultFiles.unseal(subj)
			}
			return s.installImportedVaultLocked(subj, fence, vf, body, validation)
		},
	)
	if err != nil {
		log.Errorf("subject %s: vault import failed: %v", subj, err)
		status := http.StatusInternalServerError
		if recovery := s.vaultRecovery.Load(); recovery != nil {
			status = http.StatusServiceUnavailable
		}
		return status, errors.New("vault import persistence failure")
	}
	if registered {
		log.Infof("subject %s: vault imported from snapshot bundle (revision %d, re-stamped to %s/%s)",
			subj, vf.Revision, fence.RuntimeInstanceID, fence.AttachmentID)
	} else {
		log.Infof("subject %s: vault bundle landed for the next registration (revision %d, re-stamped to %s/%s)",
			subj, vf.Revision, fence.RuntimeInstanceID, fence.AttachmentID)
	}
	return http.StatusOK, nil
}

// installImportedVaultLocked binds the imported revision into the live
// per-subject slice under the restored fence, replacing any stale entry.
// The slice is only replaced once Reload accepts — a failure leaves the live
// entry untouched while the commit protocol rolls the file back.
// validation is the caller's pre-computed vault validation policy
// (lock order: registry → s.mu, see vaultValidationPolicy).
func (s *fastSandboxPolicyServer) installImportedVaultLocked(subj subject.Subject, fence subject.Fencing, vf credentialvault.VaultFile, body credentialvault.VaultFileBody, validation *policy.NetworkPolicy) error {
	pol := validation
	entry, ok := s.vaults[subj]
	if !ok || entry.fence != fence {
		entry = vaultEntry{store: credentialvault.NewStore(nil, func() bool { return true }), fence: fence}
	}
	if err := entry.store.Reload(vf.Revision, credentialvault.CreateRequest(body), pol); err != nil {
		return err
	}
	// Unseal only after the store accepted — a failed apply rolls the file
	// back and keeps the seal.
	if err := s.vaultFiles.unseal(subj); err != nil {
		return err
	}
	entry.sealed = false
	s.vaults[subj] = entry
	return nil
}

// validateImportedVault runs the imported revision through the exact Reload
// the live install will use, on a scratch store.
func validateImportedVault(vf credentialvault.VaultFile, body credentialvault.VaultFileBody, pol *policy.NetworkPolicy) error {
	scratch := credentialvault.NewStore(nil, func() bool { return true })
	return scratch.Reload(vf.Revision, credentialvault.CreateRequest(body), pol)
}

// readVaultBundle reads the driver-staged bundle source with a hard size
// cap — a read-only lookup in the driver's trust domain, so plain bounded
// I/O suffices (writes keep the hardened atomic machinery).
func readVaultBundle(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxVaultSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxVaultSnapshotBytes {
		return nil, errVaultSnapshotTooLarge
	}
	return data, nil
}
