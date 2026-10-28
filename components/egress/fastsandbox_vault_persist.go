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

// Egress-held vault persistence (OSEP-0022 stage 2): one file per subject
// under <egress-volume>/vault/, so an egress restart recovers locally — no
// server re-push exists or is needed.
//
// TODO(encryption-at-rest): template-borne vault blobs are stored as-is.
// When encryption lands it protects the store-side reader only — CLONE
// ISOLATION is determined by KEY SCOPING (per-tenant/pool keys), so design
// the key hierarchy with the template-distribution flow in mind. Ordering is persist → apply → ack
// (acked ⇒ durable and applied), reusing the policy-persistence machinery's
// three-state classification, snapshot restore, and recovery latch.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// vaultRecoveryState is the recovery latch payload.
type vaultRecoveryState struct {
	reason string
}

// vaultFileStore is the storage seam for one subject's vault file; the real
// store is policy.AtomicPolicyFile, and tests substitute outcomes here.
// Callers must serialize operations across a transaction — the relay write
// path holds the server lock for exactly that.
type vaultFileStore interface {
	Snapshot() (*policy.PolicyFileSnapshot, error)
	SaveRaw(data []byte) (policy.FileMutationState, error)
	// SaveRawNonDurable publishes by rename without durability barriers —
	// only valid for content the caller can reconstruct by replay (the
	// atomic store documents the contract). Everything that outruns its
	// replay source must use SaveRaw.
	SaveRawNonDurable(data []byte) (policy.FileMutationState, error)
	Restore(*policy.PolicyFileSnapshot) (policy.FileMutationState, error)
	Delete() (policy.FileMutationState, error)
}

// vaultPersistence owns the per-subject vault files: <dir>/<sandboxUid>.json,
// with lazily constructed, cached file stores.
type vaultPersistence struct {
	dir string
	// newFile is injectable for tests; the default is the atomic file machinery.
	newFile func(path string) (vaultFileStore, error)

	mu    sync.Mutex
	files map[subject.Subject]vaultFileStore
}

// newVaultPersistence prepares the vault directory (0700 — v1 is plaintext,
// the volume joins the credential trust surface) and sweeps staged leftovers:
// startup loads the committed files only.
func newVaultPersistence(dir string) (*vaultPersistence, error) {
	if err := prepareStateDir(dir, "vault"); err != nil {
		return nil, err
	}
	return &vaultPersistence{
		dir:     dir,
		newFile: func(path string) (vaultFileStore, error) { return policy.NewAtomicPolicyFile(path) },
		files:   make(map[subject.Subject]vaultFileStore),
	}, nil
}

// prepareStateDir validates and prepares one per-subject state directory
// (0700, no symlink, no traversal) for the atomic file machinery: it sweeps
// staging leftovers a crashed writer left behind, so startup loads the
// committed files only. noun names the state kind in errors and logs
// ("vault", "policy").
func prepareStateDir(dir, noun string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("%s persistence directory is empty", noun)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s persistence directory %q must be absolute", noun, dir)
	}
	for _, part := range strings.Split(dir, string(os.PathSeparator)) {
		if part == ".." {
			return fmt.Errorf("%s persistence directory %q must not traverse parents", noun, dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare %s directory %s: %w", noun, dir, err)
	}
	// Reject a symlinked state directory BEFORE chmod/sweep act through it
	// (the per-file atomic machinery re-validates no-follow on every real
	// op; this keeps startup from mutating a link target first).
	if info, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("inspect %s directory %s: %w", noun, dir, err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s persistence directory %q must not be a symlink", noun, dir)
	} else if !info.IsDir() {
		return fmt.Errorf("%s persistence directory %q is not a directory", noun, dir)
	}
	// An existing directory may carry wider bits from a previous generation.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict %s directory %s: %w", noun, dir, err)
	}
	// The volume root must pass the atomic machinery's trusted-ancestor
	// check; kubelet creates emptyDir roots 0777, which fails it.
	if err := restrictVolumeRoot(dir, noun); err != nil {
		return err
	}
	if err := sweepStateTempFiles(dir, noun); err != nil {
		return fmt.Errorf("sweep staged %s leftovers in %s: %w", noun, dir, err)
	}
	return nil
}

// sweepStateTempFiles unlinks staging files a crashed writer left behind
// (rename never attempted — unconfirmed candidate state).
func sweepStateTempFiles(dir, noun string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, policy.AtomicTempPrefix) || !strings.HasSuffix(name, policy.AtomicTempSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("discard staged leftover %s: %w", name, err)
		}
		log.Infof("%s persistence: discarded staged leftover %s", noun, name)
	}
	return nil
}

// stateDirNames are the basenames of the per-subject state directories
// sharing one egress volume root, registered at assembly from the CONFIGURED
// paths (the basename is deployment-owned). restrictVolumeRoot treats
// registered siblings as ours — otherwise the second initializer skips the
// volume-root restriction and the atomic machinery rejects every file op on
// the untrusted ancestor.
var stateDirNames = map[string]bool{
	"vault":  true,
	"policy": true,
}

// registerStateDirName records a configured state directory basename.
func registerStateDirName(dir string) {
	stateDirNames[filepath.Base(filepath.Clean(dir))] = true
}

// restrictVolumeRoot clears group/world-write bits from the state volume
// root: kubelet creates emptyDir roots 0777, which the atomic machinery
// rejects as an untrusted ancestor. Registered siblings belong to us; a
// root holding any other entry is left untouched (the manifest's job).
func restrictVolumeRoot(dir, noun string) error {
	parent := filepath.Dir(dir)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect %s volume root %s: %w", noun, parent, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 == 0 {
		return nil
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && st.Uid != uint32(os.Geteuid()) {
		return nil // not ours to restrict
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("list %s volume root %s: %w", noun, parent, err)
	}
	base := filepath.Base(dir)
	for _, entry := range entries {
		if entry.Name() == base || (entry.IsDir() && stateDirNames[entry.Name()]) {
			continue
		}
		log.Infof("%s persistence: volume root %s holds foreign entry %q; not restricting (fix permissions in the manifest)", noun, parent, entry.Name())
		return nil
	}
	if err := os.Chmod(parent, info.Mode().Perm()&^0o022); err != nil {
		return fmt.Errorf("restrict %s volume root %s: %w", noun, parent, err)
	}
	log.Infof("%s persistence: restricted group/world-writable volume root %s (kubelet emptyDir default)", noun, parent)
	return nil
}

// vaultFileUID strips the internal "s-" prefix: the file contract names files
// <sandboxUid>.json.
func vaultFileUID(subj subject.Subject) string {
	return strings.TrimPrefix(string(subj), "s-")
}

func (p *vaultPersistence) pathFor(subj subject.Subject) (string, error) {
	uid := vaultFileUID(subj)
	// The relay validates the path-derived UID; envelope UIDs get the same check.
	if !validVaultSubjectID(uid) {
		return "", fmt.Errorf("subject uid %q cannot name a vault file", uid)
	}
	return filepath.Join(p.dir, uid+".json"), nil
}

// storeFor returns the subject's file store, constructing it on first use.
func (p *vaultPersistence) storeFor(subj subject.Subject) (vaultFileStore, error) {
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
func (p *vaultPersistence) purge(subj subject.Subject) error {
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
		return errors.New("vault file unlink outcome uncertain")
	}
	return p.unseal(subj)
}

// markerFor names the durable seal marker: export writes it before acking
// sealed, and a handler restart between export and REMOVE_BINDING restores
// the seal from it — otherwise binding replay would reload the vault writable
// with a bundle already taken.
func (p *vaultPersistence) markerFor(subj subject.Subject) (string, error) {
	path, err := p.pathFor(subj)
	if err != nil {
		return "", err
	}
	return path + ".sealed", nil
}

// seal writes the marker durably. Presence is the signal; no fsync — the
// marker only needs to survive a container restart, and a torn write still
// exists.
func (p *vaultPersistence) seal(subj subject.Subject) error {
	path, err := p.markerFor(subj)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte("sealed"), 0o600)
}

// unseal removes the marker; an absent marker is a complete unseal.
func (p *vaultPersistence) unseal(subj subject.Subject) error {
	path, err := p.markerFor(subj)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// isSealed reports whether the durable seal marker exists.
func (p *vaultPersistence) isSealed(subj subject.Subject) bool {
	path, err := p.markerFor(subj)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// inspectLazyVaultStateLocked discovers only whether a lazy vault must stay
// unavailable to the v1 relay path. Removal bindings have no policy against
// which to reload legacy contents, so this deliberately does not import or
// quarantine v1 files. Callers hold s.mu.
func (s *fastSandboxPolicyServer) inspectLazyVaultStateLocked(subj subject.Subject, entry *vaultEntry) {
	if s.vaultFiles == nil || entry.vaultStateProtected {
		return
	}
	fileStore, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		entry.degraded = fmt.Sprintf("vault file store unavailable: %v", err)
		entry.vaultStateUnknown = true
		return
	}
	snapshot, err := fileStore.Snapshot()
	if err != nil {
		entry.degraded = fmt.Sprintf("vault file unreadable: %v", err)
		entry.vaultStateUnknown = true
		return
	}
	entry.degraded = ""
	entry.vaultStateUnknown = false
	if !snapshot.Exists() {
		return
	}
	vf, _, decodeErr := credentialvault.DecodeVaultFile(snapshot.Data())
	if decodeErr == nil && vf.Version == credentialvault.VaultFileVersion {
		return
	}
	entry.vaultStateProtected = true
	entry.vaultStateUnknown = true
	entry.degraded = "unreadable or non-v1 vault state requires a lineage-aware reload"
	entry.fileMeta = vaultFileMetadata{
		version: vf.Version, revision: vf.Revision, state: vf.State, lineage: vf.StateLineage,
		stateRevision: vf.StateRevision, initializationID: vf.InitializationID,
	}
}

// SetVaultFiles wires persistence under the relay handlers; nil (the default)
// keeps the vault memory-only.
func (s *fastSandboxPolicyServer) SetVaultFiles(vp *vaultPersistence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vaultFiles = vp
}

// purgeVaultFile removes the subject's persisted slice (rebind or terminal
// cleanup). Best effort: a failed purge leaves a file that its recorded fence
// keeps out of every later registration.
func (s *fastSandboxPolicyServer) purgeVaultFile(subj subject.Subject) error {
	if s.vaultFiles == nil {
		return nil
	}
	err := s.vaultFiles.purge(subj)
	if err != nil {
		log.Errorf("subject %s: vault file purge failed (leftover stays fenced out): %v", subj, err)
	}
	return err
}

// vaultPurgeStatusLocked protects the remaining lifecycle writer: unlike
// relay/snapshot handlers, terminal REMOVE and rebind cleanup delete the file.
// Callers hold s.mu and the action/commit gate before invoking it.
func (s *fastSandboxPolicyServer) vaultPurgeStatusLocked(subj subject.Subject) (int, error) {
	if entry, ok := s.vaults[subj]; ok {
		if entry.vaultStateUnknown {
			return http.StatusServiceUnavailable, errors.New("vault state is unknown; refusing lifecycle purge")
		}
		if entry.fileMeta.version >= credentialvault.VaultFileVersion2 {
			return http.StatusConflict, errors.New("v2 vault state is read-only until lineage-aware lifecycle writes are supported")
		}
	}
	if s.vaultFiles == nil {
		return 0, nil
	}
	fileStore, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("vault purge state unavailable: %w", err)
	}
	snapshot, err := fileStore.Snapshot()
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("vault purge snapshot unavailable: %w", err)
	}
	if !snapshot.Exists() {
		return 0, nil
	}
	vf, _, decodeErr := credentialvault.DecodeVaultFile(snapshot.Data())
	if errors.Is(decodeErr, credentialvault.ErrUnsupportedFileVersion) {
		return http.StatusServiceUnavailable, errors.New("vault file schema is unknown; refusing lifecycle purge")
	}
	if decodeErr == nil && vf.Version >= credentialvault.VaultFileVersion2 {
		return http.StatusConflict, errors.New("v2 vault state is read-only until lineage-aware lifecycle writes are supported")
	}
	// Corrupt v1 files retain the pre-existing terminal cleanup behavior. A
	// recognized v2/unknown schema is never deleted through this v1 path.
	return 0, nil
}

// vaultPurgeFailureStatusLocked preserves a specific v2/unknown refusal. If a
// transient preflight snapshot error clears during this recheck, status 0 must
// still become a valid retryable HTTP error for the failed preflight.
func (s *fastSandboxPolicyServer) vaultPurgeFailureStatusLocked(subj subject.Subject) int {
	status, _ := s.vaultPurgeStatusLocked(subj)
	if status < 100 || status > 599 {
		return http.StatusInternalServerError
	}
	return status
}

// persistVaultCandidateLocked runs the commit protocol; callers hold s.mu for
// the whole operation, which serializes per-subject writers. persist performs
// the durable effect and returns its three-state classification; apply
// commits the prepared candidate and must leave the store untouched on
// failure (a failed apply only needs the file rolled back).
func (s *fastSandboxPolicyServer) persistVaultCandidateLocked(
	subj subject.Subject,
	persist func(vaultFileStore) (policy.FileMutationState, error),
	apply func() error,
) error {
	fs, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return fmt.Errorf("vault file store unavailable: %w", err)
	}
	before, err := fs.Snapshot()
	if err != nil {
		return fmt.Errorf("vault file snapshot: %w", err)
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
		err = errors.New("vault file persist did not confirm durable completion")
	}
	if err != nil && state == policy.FileUnchanged {
		// Pre-rename failure: last-good intact — NACK without restore or latch.
		return err
	}
	if err != nil {
		// Publication attempted, durability unconfirmed: latch first (only a
		// restart reconciles), then restore so disk agrees with the untouched
		// live state.
		s.requireVaultRecoveryLocked("vault file persist outcome uncertain")
		if restoreErr := restoreVaultFile(fs, before); restoreErr != nil {
			log.Errorf("subject %s: vault file restore after uncertain persist: %v", subj, restoreErr)
			return fmt.Errorf("%w (restore also failed: %v)", err, restoreErr)
		}
		return err
	}
	// Durable. A failed apply rolls the file back — no latch while the
	// restore is confirmed.
	if applyErr := apply(); applyErr != nil {
		if restoreErr := restoreVaultFile(fs, before); restoreErr != nil {
			s.requireVaultRecoveryLocked("vault file restore after failed apply was inconclusive")
			log.Errorf("subject %s: vault file restore after failed apply: %v", subj, restoreErr)
			return fmt.Errorf("vault apply failed (%v) and restore failed (%v)", applyErr, restoreErr)
		}
		return fmt.Errorf("vault apply: %w", applyErr)
	}
	// Durable AND applied: a degraded vault recovers here.
	s.clearVaultDegradedLocked(subj)
	return nil
}

// restoreVaultFile reinstates the previous image and requires the machinery
// to confirm durable completion.
func restoreVaultFile(fs vaultFileStore, before *policy.PolicyFileSnapshot) error {
	state, err := fs.Restore(before)
	if err == nil && state != policy.FileCommitted {
		err = errors.New("vault file restore did not confirm durable completion")
	}
	return err
}

// requireVaultRecoveryLocked latches recovery (first reason wins): vault
// writes freeze until the process restarts and reconciles from the committed
// files. The latch is an atomic so the healthz probe reads it lock-free.
func (s *fastSandboxPolicyServer) requireVaultRecoveryLocked(reason string) {
	if s.vaultRecovery.Load() == nil {
		s.vaultRecovery.CompareAndSwap(nil, &vaultRecoveryState{reason: reason})
		log.Errorf("vault persistence recovery latched (writes frozen until restart): %s", reason)
	}
}

// vaultRecoveryActive reports whether the recovery latch freezes writes.
func (s *fastSandboxPolicyServer) vaultRecoveryActive() bool {
	return s.vaultRecovery.Load() != nil
}

// clearVaultDegradedLocked clears the subject's degraded mark after a
// successful relay write — a corrupt-file subject recovers without recreation.
func (s *fastSandboxPolicyServer) clearVaultDegradedLocked(subj subject.Subject) {
	if e, ok := s.vaults[subj]; ok {
		e.degraded = ""
		s.vaults[subj] = e
	}
}

// degradedVaultUIDs lists the subjects with a degraded vault, as raw sandbox
// UIDs, sorted for stable surfacing.
func (s *fastSandboxPolicyServer) degradedVaultUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degradedVaultUIDsLocked()
}

// vaultHealthIssue is non-empty only for the process-level failure — the
// recovery latch. A per-subject degraded vault surfaces via the status
// payload's degradedVaults instead (see rehydrateVaultReload).
func (s *fastSandboxPolicyServer) vaultHealthIssue() string {
	if state := s.vaultRecovery.Load(); state != nil {
		return "vault persistence recovery required: " + state.reason
	}
	return ""
}

// degradedVaultUIDsLocked implements degradedVaultUIDs; callers hold s.mu.
func (s *fastSandboxPolicyServer) degradedVaultUIDsLocked() []string {
	var out []string
	for subj, e := range s.vaults {
		if e.degraded != "" {
			out = append(out, vaultFileUID(subj))
		}
	}
	sort.Strings(out)
	return out
}

// rehydrateVaultReload is the egress-restart recovery path: at (re)registration
// of a subject with no live vault slice, load its committed file, fenced by
// the full runtime identity:
//
//   - fence match → reload at the exact file revision
//   - mismatch → stale, purged (a reset — or an attachment-only rebind whose
//     purge outcome was uncertain — never inherits the predecessor's vault)
//   - corrupt/unreadable, or content the binding policy rejects → the
//     subject's vault is degraded (interception fails closed, policy
//     unaffected) and the file is quarantined so a restart cannot re-degrade
//     on it — except an unknown version, preserved for the newer writer. A
//     successful relay write clears the mark
//
// Removal SET_BINDINGS skip the reload: with no policy to revalidate against,
// the file stays fenced on disk until the next registration or terminal purge.
// An existing slice for this exact fence wins (duplicate SET_BINDING); one
// left under a different fence goes through the same file rules as a fresh
// registration.
// policyDegraded reports that the recorded policy itself degraded at this
// restart: the vault then degrades WITHOUT validation and WITHOUT
// quarantine (a deny-seeded validation would quarantine a HEALTHY vault
// whose binding hosts the tightened/unknown policy no longer covers —
// silently destroying persisted credentials on every restart until a
// policy/vault relay write heals it).
func (s *fastSandboxPolicyServer) rehydrateVaultReload(subj subject.Subject, fence subject.Fencing, pol *policy.NetworkPolicy, policyDegraded bool) {
	if s.vaultFiles == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	protectedBefore := false
	previousMeta := vaultFileMetadata{}
	if e, live := s.vaults[subj]; live {
		if e.fence == fence && !e.vaultStateUnknown {
			return
		}
		if e.fence == fence {
			protectedBefore = e.vaultStateProtected
			previousMeta = e.fileMeta
		}
		delete(s.vaults, subj)
	}
	entry := vaultEntry{
		store: credentialvault.NewStore(nil, func() bool { return true }), fence: fence,
		fileMeta: previousMeta, vaultStateProtected: protectedBefore,
	}
	fs, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		entry.degraded = fmt.Sprintf("vault file store unavailable: %v", err)
		entry.vaultStateUnknown = true
		s.vaults[subj] = entry
		log.Warnf("subject %s: %s", subj, entry.degraded)
		return
	}
	snap, err := fs.Snapshot()
	switch {
	case err != nil:
		entry.degraded = fmt.Sprintf("vault file unreadable: %v", err)
		entry.vaultStateUnknown = true
		log.Warnf("subject %s: %s", subj, entry.degraded)
	case !snap.Exists():
		if entry.vaultStateProtected {
			// A previously observed v2/unsupported file disappearing is not
			// evidence of a fresh v1 subject. Keep its provenance sticky for
			// this fence so a relay write cannot replace it with v1.
			entry.degraded = "previously protected vault state disappeared; refusing legacy fallback"
			entry.vaultStateUnknown = true
			log.Warnf("subject %s: %s", subj, entry.degraded)
		}
		// Otherwise a fresh subject has nothing persisted to recover.
	default:
		vf, body, decErr := credentialvault.DecodeVaultFile(snap.Data())
		unsupported := errors.Is(decErr, credentialvault.ErrUnsupportedFileVersion)
		if vf.Version >= credentialvault.VaultFileVersion2 || unsupported {
			entry.vaultStateProtected = true
		}
		legacyDowngrade := protectedBefore && vf.Version < credentialvault.VaultFileVersion2 && !unsupported
		if !legacyDowngrade {
			entry.fileMeta = vaultFileMetadata{
				version: vf.Version, revision: vf.Revision, state: vf.State, lineage: vf.StateLineage,
				stateRevision: vf.StateRevision, initializationID: vf.InitializationID,
			}
		}
		switch {
		case legacyDowngrade:
			entry.degraded = "previously protected vault state was replaced by a legacy file; refusing downgrade"
			entry.vaultStateUnknown = true
			log.Warnf("subject %s: %s", subj, entry.degraded)
		case decErr != nil && errors.Is(decErr, credentialvault.ErrUnsupportedFileVersion):
			// A newer or unreadable v2 state must remain available for a
			// compatible reader; never quarantine or expose it as absent.
			entry.degraded = fmt.Sprintf("vault file version not readable by this egress: %v", decErr)
			entry.vaultStateUnknown = true
			log.Warnf("subject %s: %s (file preserved for the newer writer)", subj, entry.degraded)
		case decErr != nil:
			// Structurally corrupt: quarantine so the degradation cannot
			// outlive this process via the file.
			entry.degraded = fmt.Sprintf("corrupt vault file: %v", decErr)
			s.quarantineVaultFile(subj, fs)
			log.Warnf("subject %s: %s", subj, entry.degraded)
		case policyDegraded:
			// The recorded policy degraded at this restart: hold the vault
			// degraded WITHOUT validating it against the deny fallback (a
			// deny-seeded validation would quarantine a HEALTHY vault) and
			// WITHOUT quarantining the file — a policy/vault relay write
			// heals the slice.
			entry.degraded = "held degraded: the recorded policy is degraded at this restart"
			entry.vaultStateUnknown = vf.Version >= credentialvault.VaultFileVersion2
			log.Warnf("subject %s: %s (file kept)", subj, entry.degraded)
		case vf.Version == credentialvault.VaultFileVersion2 && vf.State == credentialvault.FileStateEmpty:
			// Empty is an explicit initialization decision, not a legacy
			// absence. This stage cannot verify its mode/owner contract.
			entry.degraded = "explicit empty vault state requires the strict mode protocol"
			entry.vaultStateUnknown = true
			log.Warnf("subject %s: %s (file kept)", subj, entry.degraded)
		case (subject.Fencing{RuntimeInstanceID: vf.Incarnation, AttachmentID: vf.AttachmentID}).Matches(fence):
			// Same-instance recovery: revalidate against the replayed binding
			// policy, install at the exact file revision. A validation
			// failure (e.g. the recorded policy was tightened and no longer
			// covers a binding host) DEGRADES the slice but KEEPS the file —
			// runtime relay commits never re-validate the vault, so
			// policy/vault divergence at restart is a normal state, and
			// quarantining here would silently destroy persisted
			// credentials. A policy/vault relay write heals the slice; a
			// policy heal + restart revives it from the kept file.
			if vf.Version == credentialvault.VaultFileVersion2 && vf.State == credentialvault.FileStateDeleted {
				// The persisted tombstone is known absence. Keep the store
				// empty so the public GET remains 404, and keep the file as
				// the source of truth until v2 writes are implemented.
				log.Infof("subject %s: deleted v2 vault state reloaded (stateRevision %d)", subj, vf.StateRevision)
			} else if err := entry.store.Reload(vf.Revision, credentialvault.CreateRequest(body), pol); err != nil {
				entry.degraded = fmt.Sprintf("vault file reload rejected: %v", err)
				entry.vaultStateUnknown = vf.Version >= credentialvault.VaultFileVersion2
				log.Warnf("subject %s: %s (file kept; a policy/vault relay write heals the slice)", subj, entry.degraded)
			} else {
				log.Infof("subject %s: vault reloaded from file (version %d, revision %d, incarnation %s)", subj, vf.Version, vf.Revision, vf.Incarnation)
			}
		default:
			if vf.Version >= credentialvault.VaultFileVersion2 {
				// A v2 file belongs to an unknown lineage/owner here. Preserve
				// it and block relay reads/writes; this stage cannot safely
				// infer a transfer or purge authorization from the file alone.
				entry.degraded = "v2 vault file owner does not match the current runtime fence"
				entry.vaultStateUnknown = true
				log.Warnf("subject %s: %s (file preserved)", subj, entry.degraded)
			} else {
				// Legacy v1 stale cleanup retains its existing behavior.
				if _, delErr := fs.Delete(); delErr != nil {
					log.Warnf("subject %s: stale vault file purge failed (stays fenced out): %v", subj, delErr)
				} else {
					log.Infof("subject %s: stale vault file discarded (fence %s/%s ≠ %s/%s)", subj, vf.Incarnation, vf.AttachmentID, fence.RuntimeInstanceID, fence.AttachmentID)
				}
			}
		}
	}
	// A durable seal marker (export acked, teardown not yet delivered)
	// restores the sealed entry across a restart: vault ops stay 409 until
	// REMOVE_BINDING purges or an import unseals.
	if s.vaultFiles.isSealed(subj) {
		entry.sealed = true
		log.Infof("subject %s: seal restored from marker (exported bundle pending)", subj)
	}
	s.vaults[subj] = entry
}

// quarantineVaultFile unlinks a file that can never load again. Best effort:
// on failure the file stays and the subject remains degraded.
func (s *fastSandboxPolicyServer) quarantineVaultFile(subj subject.Subject, fs vaultFileStore) {
	if _, err := fs.Delete(); err != nil {
		log.Errorf("subject %s: vault file quarantine failed (file left in place): %v", subj, err)
	}
}
