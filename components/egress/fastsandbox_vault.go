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

// Relay-only vault surface (OSEP-0022): /vault/{sandboxUid} terminates the
// FastPath handler-relay (server → FastPath → Fastlet → this loopback
// listener); the Fastlet's relay hop is the only caller. Internal, versioned
// contract — specs/egress-api.yaml (sidecar) is unchanged.
//
// Sidecar-parity semantics: POST create-only (409 on exists), PUT full
// replace with optional expectedRevision (412 on mismatch, creates when
// absent), GET the full revision INCLUDING values (server-side PATCH merges
// against it), DELETE keeps the policy. Unregistered subject → 404, never
// cached; expectedIncarnation mismatch → 412; validation failure → 400 NACK
// keeping last-good. Shares the per-subject store with the mitm addon's
// active-vault socket.
//
// With persistence enabled (OSEP-0022 stage 2) every write runs the commit
// protocol — persist → apply → ack — so the ack means durable AND applied
// (see fastsandbox_vault_persist.go).
package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// validVaultSubjectID keeps the path-derived UID opaque: bounded, no
// separators, dot-segments, or control characters — it names the subject's
// vault file once persistence lands.
func validVaultSubjectID(uid string) bool {
	if uid == "" || len(uid) > 128 || uid == "." || uid == ".." {
		return false
	}
	for _, r := range uid {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// handleVaultRelay serves /vault/{sandboxUid} for all four relay verbs.
func (s *fastSandboxPolicyServer) handleVaultRelay(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, constants.VaultRelayPathPrefix)
	if !validVaultSubjectID(uid) {
		http.Error(w, "missing or invalid sandbox uid in path", http.StatusBadRequest)
		return
	}
	subj := subject.FromSandboxUID(uid)
	switch r.Method {
	case http.MethodGet:
		full, status, msg, err := s.readRelayVault(r, subj)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if err != nil {
			writeVaultRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, full)
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		s.handleVaultRelayWrite(w, r, subj)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVaultRelayWrite serves POST (create-only), PUT (full replace) and
// DELETE.
//
// The body is parsed BEFORE the server lock: a stalled client must never be
// able to hold s.mu (healthz, addon dispatch, and all other subjects' traffic
// go through it) by dribbling bytes — the listener read timeouts are the
// second line of defense.
//
// The rest of the mutation holds s.mu, serializing per-subject writers and
// fencing rebind/terminal purges. Validation runs on a prepared candidate
// before any durable effect: a rejected request never touches memory or file
// (last-good kept, never half-open). With persistence enabled the protocol is
// persist → apply → ack, so acked ⇒ durable and applied. Persistence
// failures answer only the retryable classification (500, or 503 while
// latched); detailed I/O errors stay in the log — they carry host paths.
func (s *fastSandboxPolicyServer) handleVaultRelayWrite(w http.ResponseWriter, r *http.Request, subj subject.Subject) {
	var (
		createReq  credentialvault.CreateRequest
		replaceReq credentialvault.ReplaceRequest
	)
	switch r.Method {
	case http.MethodPost:
		if err := credentialvault.ReadJSON(r, &createReq); err != nil {
			http.Error(w, "invalid credential vault request: "+err.Error(), http.StatusBadRequest)
			return
		}
	case http.MethodPut:
		if err := credentialvault.ReadJSON(r, &replaceReq); err != nil {
			http.Error(w, "invalid credential vault replace request: "+err.Error(), http.StatusBadRequest)
			return
		}
	case http.MethodDelete:
		// No body.
	}
	// The body has been consumed; a late response must not try to read it.
	r.Body.Close()

	if s.subjectRebindingNow(subj) {
		http.Error(w, "subject rebind in progress", http.StatusServiceUnavailable)
		return
	}
	// Capture the request identity before either serialization lock. A queued
	// headerless write must not adopt a successor published while it waits.
	fence, registered := s.reg.Fence(subj)
	// Capture a response while committing; no network output holds s.mu.
	respond := func() func() {
		if s.subjectRebinding(subj) {
			return func() { http.Error(w, "subject rebind in progress", http.StatusServiceUnavailable) }
		}
		s.policyCommitMu.Lock()
		defer s.policyCommitMu.Unlock()
		effective := s.vaultValidationPolicy(subj)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.subjectRebindingLocked(subj) {
			return func() { http.Error(w, "subject rebind in progress", http.StatusServiceUnavailable) }
		}
		if !registered || !s.fenceMatchesLocked(subj, fence) {
			return func() { http.Error(w, "unknown subject", http.StatusNotFound) }
		}
		if inc := strings.TrimSpace(r.Header.Get(constants.EgressVaultIncarnationHeader)); inc != "" && inc != fence.RuntimeInstanceID {
			return func() { http.Error(w, "expectedIncarnation mismatch", http.StatusPreconditionFailed) }
		}
		if s.vaultSealedLocked(subj) {
			return func() {
				http.Error(w, "subject vault is sealed (paused); the bundle copy is already taken", http.StatusConflict)
			}
		}
		if recovery := s.vaultRecovery.Load(); recovery != nil {
			return func() {
				http.Error(w, "vault persistence recovery required: "+recovery.reason, http.StatusServiceUnavailable)
			}
		}
		store := s.vaultForLocked(subj, fence)
		if store == nil {
			return func() { http.Error(w, "unknown subject", http.StatusNotFound) }
		}
		if s.vaultSealedLocked(subj) {
			return func() {
				http.Error(w, "subject vault is sealed (paused); the bundle copy is already taken", http.StatusConflict)
			}
		}
		if entry, ok := s.vaults[subj]; ok {
			if entry.vaultStateUnknown {
				return func() { http.Error(w, "vault state is unknown; refusing mutation", http.StatusServiceUnavailable) }
			}
			if entry.fileMeta.version >= credentialvault.VaultFileVersion2 {
				return func() {
					http.Error(w, "v2 vault state is read-only until lineage-aware writes are supported", http.StatusConflict)
				}
			}
		}
		pol := effective

		var (
			candidate *credentialvault.MutationCandidate
			created   bool
			err       error
		)
		switch r.Method {
		case http.MethodPost:
			candidate, err = store.PrepareCreate(createReq, pol)
		case http.MethodPut:
			candidate, err = store.PrepareReplace(replaceReq, pol)
			created = candidate.Created()
		case http.MethodDelete:
			candidate, err = store.PrepareDelete()
		}
		if err != nil {
			return func() { writeVaultRelayError(w, err) }
		}
		defer candidate.Discard()

		// Rendering is required before commit; an unresolvable credential is a
		// NACK before anything durable happens.
		if candidate.Exists() {
			if _, err = candidate.ActiveSnapshot(r.Context()); err != nil {
				return func() { http.Error(w, "credential vault candidate cannot be rendered", http.StatusBadRequest) }
			}
		}

		if s.vaultFiles == nil {
			// Memory-only (persistence disabled): direct commit.
			state, commitErr := store.CommitCandidate(candidate)
			if commitErr != nil {
				return func() { writeVaultRelayError(w, commitErr) }
			}
			return func() { writeVaultRelayWriteResponse(w, r, state, created) }
		}

		// Persistence enabled: encode the candidate — stamped with the owning
		// runtime fence — then run the commit protocol.
		var payload []byte
		if candidate.Exists() {
			full, fullErr := candidate.Full()
			if fullErr != nil {
				log.Errorf("subject %s: vault candidate read failed: %v", subj, fullErr)
				return func() { http.Error(w, "vault persistence failure", http.StatusInternalServerError) }
			}
			payload, err = credentialvault.EncodeVaultFile(fence.RuntimeInstanceID, fence.AttachmentID, full.Revision, credentialvault.VaultFileBody{
				Credentials: full.Credentials,
				Bindings:    full.Bindings,
			})
			if err != nil {
				log.Errorf("subject %s: vault file encode failed: %v", subj, err)
				return func() { http.Error(w, "vault persistence failure", http.StatusInternalServerError) }
			}
		}
		err = s.persistVaultCandidateLocked(
			subj,
			func(fs vaultFileStore) (policy.FileMutationState, error) {
				if payload == nil {
					return fs.Delete()
				}
				return fs.SaveRaw(payload)
			},
			func() error {
				_, commitErr := store.CommitCandidate(candidate)
				return commitErr
			},
		)
		if err != nil {
			// Uncertain persist latched → 503; every other failure kept last-good
			// on both sides → retryable 500. Details stay in the log.
			log.Errorf("subject %s: vault relay write failed: %v", subj, err)
			status := http.StatusInternalServerError
			if recovery := s.vaultRecovery.Load(); recovery != nil {
				status = http.StatusServiceUnavailable
			}
			return func() { http.Error(w, "vault persistence failure", status) }
		}
		if r.Method == http.MethodDelete {
			return func() { w.WriteHeader(http.StatusNoContent) }
		}
		state, err := store.Sanitized()
		if err != nil {
			log.Errorf("subject %s: vault sanitized read failed: %v", subj, err)
			return func() { http.Error(w, "vault persistence failure", http.StatusInternalServerError) }
		}
		return func() { writeVaultRelayWriteResponse(w, r, state, created) }
	}()
	respond()
}

// writeVaultRelayWriteResponse maps the committed mutation onto the relay
// contract: POST → 201, PUT → 200 (201 when it created), DELETE → 204.
func writeVaultRelayWriteResponse(w http.ResponseWriter, r *http.Request, state credentialvault.State, created bool) {
	switch r.Method {
	case http.MethodPost:
		writeJSON(w, http.StatusCreated, state)
	case http.MethodPut:
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, state)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// readRelayVault captures an independent inline result under the subject lock;
// sending it afterwards cannot delay lifecycle state changes.
func (s *fastSandboxPolicyServer) readRelayVault(r *http.Request, subj subject.Subject) (credentialvault.FullRevision, int, string, error) {
	if s.subjectRebindingNow(subj) {
		return credentialvault.FullRevision{}, http.StatusServiceUnavailable, "subject rebind in progress", nil
	}
	fence, registered := s.reg.Fence(subj)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subjectRebindingLocked(subj) {
		return credentialvault.FullRevision{}, http.StatusServiceUnavailable, "subject rebind in progress", nil
	}
	if !registered || !s.fenceMatchesLocked(subj, fence) {
		return credentialvault.FullRevision{}, http.StatusNotFound, "unknown subject", nil
	}
	if inc := strings.TrimSpace(r.Header.Get(constants.EgressVaultIncarnationHeader)); inc != "" && inc != fence.RuntimeInstanceID {
		return credentialvault.FullRevision{}, http.StatusPreconditionFailed, "expectedIncarnation mismatch", nil
	}
	if s.vaultSealedLocked(subj) {
		return credentialvault.FullRevision{}, http.StatusConflict, "subject vault is sealed (paused); the bundle copy is already taken", nil
	}
	store := s.vaultForLocked(subj, fence)
	if store == nil {
		return credentialvault.FullRevision{}, http.StatusNotFound, "unknown subject", nil
	}
	if s.vaultSealedLocked(subj) {
		return credentialvault.FullRevision{}, http.StatusConflict, "subject vault is sealed (paused); the bundle copy is already taken", nil
	}
	if entry := s.vaults[subj]; entry.vaultStateUnknown {
		return credentialvault.FullRevision{}, http.StatusServiceUnavailable, "vault state is unknown; refusing to treat it as absent", nil
	}
	full, err := store.Full()
	return full, 0, "", err
}

// vaultSealedLocked reports whether the subject's vault is sealed (paused);
// it never materializes an entry. Callers hold s.mu.
func (s *fastSandboxPolicyServer) vaultSealedLocked(subj subject.Subject) bool {
	e, ok := s.vaults[subj]
	return ok && e.sealed
}

// vaultValidationPolicy returns the policy a vault op validates against:
// the effective policy of an active subject, or the pending policy merged
// with the always overlay while denying (a barrier-held op must not depend
// on hook timing). Call BEFORE s.mu — it reads the registry.
func (s *fastSandboxPolicyServer) vaultValidationPolicy(subj subject.Subject) *policy.NetworkPolicy {
	effective := s.reg.EffectivePolicy(subj)
	if effective != nil {
		return effective
	}
	s.mu.Lock()
	pending := s.pendingPolicies[subj]
	s.mu.Unlock()
	if pending != nil {
		return s.reg.EffectiveOf(pending)
	}
	return nil
}

// writeVaultRelayError maps store errors onto the relay contract: revision
// conflicts are 412 (sidecar PreconditionFailed parity), unlike the sidecar
// PATCH mapper which keeps 409.
func writeVaultRelayError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentialvault.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, credentialvault.ErrExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, credentialvault.ErrRevisionConflict):
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
