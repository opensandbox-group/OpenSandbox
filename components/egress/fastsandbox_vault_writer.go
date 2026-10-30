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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

const vaultWriterVersion = "sandbox.fast.io/vault-writer/v2"
const vaultInitializationPrefix = "/_fastlet/v2/vault-initialization/"
const vaultStatePrefix = "/_fastlet/v2/relay/vault-state/"

type strictVaultContract struct {
	Version          string `json:"version"`
	InitializationID string `json:"initializationId"`
	Lineage          string `json:"lineage"`
}

func (c strictVaultContract) valid() bool {
	return c.Version == vaultWriterVersion && validVaultSubjectID(c.InitializationID) && validVaultSubjectID(c.Lineage) && strings.TrimSpace(c.InitializationID) == c.InitializationID && strings.TrimSpace(c.Lineage) == c.Lineage
}

type vaultStateToken struct {
	Lineage  string `json:"lineage"`
	Sequence uint64 `json:"sequence"`
}
type vaultStateReply struct {
	HandlerInstanceID string                        `json:"handlerInstanceId"`
	Version           string                        `json:"version"`
	Owner             actionhandler.Owner           `json:"owner"`
	InitializationID  string                        `json:"initializationId"`
	State             credentialvault.FileState     `json:"state"`
	Token             vaultStateToken               `json:"token"`
	Vault             *credentialvault.FullRevision `json:"vault,omitempty"`
}

func (s *fastSandboxPolicyServer) writerFile(subj subject.Subject, record *strictOwnerRecord) (credentialvault.VaultFile, credentialvault.VaultFileBody, error) {
	if s.vaultFiles == nil || record == nil || record.VaultContract == nil {
		return credentialvault.VaultFile{}, credentialvault.VaultFileBody{}, errors.New("writer persistence unavailable")
	}
	f, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return credentialvault.VaultFile{}, credentialvault.VaultFileBody{}, err
	}
	snap, err := f.Snapshot()
	if err != nil {
		return credentialvault.VaultFile{}, credentialvault.VaultFileBody{}, err
	}
	if !snap.Exists() {
		return credentialvault.VaultFile{}, credentialvault.VaultFileBody{}, errors.New("initialized Vault file is missing")
	}
	vf, body, err := credentialvault.DecodeVaultFile(snap.Data())
	c := record.VaultContract
	if err == nil && (vf.Version != 2 || vf.InitializationID != c.InitializationID || vf.StateLineage != c.Lineage || vf.Incarnation != record.Owner.RuntimeInstanceID || vf.AttachmentID != record.Owner.AttachmentID) {
		err = errors.New("Vault contract or owner mismatch")
	}
	return vf, body, err
}

// readWriterOwner validates request identity before consulting durable authority.
// The caller holds the owner stripe. Returning the record avoids a second read
// whose failure could otherwise bypass live-state invalidation.
func (s *fastSandboxPolicyServer) readWriterOwner(w http.ResponseWriter, r *http.Request, uid string, contract *strictVaultContract) *strictOwnerRecord {
	owner, err := ownerFromRequest(r)
	if err != nil || owner.SandboxUID != uid {
		http.Error(w, "relay owner must match the path Sandbox UID", 400)
		return nil
	}
	subj := subject.FromSandboxUID(uid)
	s.mu.Lock()
	current, found := s.actionOwners[subj]
	entry := s.vaults[subj]
	s.mu.Unlock()
	if !found || !current.Matches(owner) {
		http.Error(w, "relay owner is not the current registered owner", 412)
		return nil
	}
	// A bad initialization request cannot invalidate a legitimate live Vault,
	// even when the durable owner record happens to be unavailable.
	if contract != nil && entry.fileMeta.version == 2 && (contract.InitializationID != entry.fileMeta.initializationID || contract.Lineage != entry.fileMeta.lineage) {
		http.Error(w, "initialization contract mismatch", 412)
		return nil
	}
	record, err := s.readOwnerRecord(subj)
	if err != nil || record == nil {
		s.policyCommitMu.Lock()
		s.mu.Lock()
		current, found = s.actionOwners[subj]
		entry, exists := s.vaults[subj]
		expected := subject.Fencing{RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID}
		if found && current.Matches(owner) && exists && entry.fence == expected && s.fenceMatchesLocked(subj, expected) && entry.v2Verified {
			entry.v2Verified = false
			entry.vaultStateUnknown = true
			entry.store = credentialvault.NewStore(nil, func() bool { return true })
			s.vaults[subj] = entry
		}
		s.mu.Unlock()
		s.policyCommitMu.Unlock()
		http.Error(w, "strict owner authority unavailable", 412)
		return nil
	}
	if record.Phase != "active" || !record.Owner.Matches(owner) {
		http.Error(w, "strict owner is not active", 412)
		return nil
	}
	return record
}

// Only the trusted manager may call this endpoint. Fresh is a transient CAS
// grant from the paired control plane, not a persisted replay permission.
// A pre-existing contract always refuses fresh, including after partial init.
func (s *fastSandboxPolicyServer) handleVaultInitialization(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	uid := strings.TrimPrefix(r.URL.Path, vaultInitializationPrefix)
	if !validVaultSubjectID(uid) {
		http.Error(w, "invalid UID", 400)
		return
	}
	var req struct {
		Contract          strictVaultContract `json:"contract"`
		HandlerInstanceID string              `json:"handlerInstanceId"`
		Mode              string              `json:"mode"`
		Fresh             bool                `json:"fresh"`
	}
	if err := credentialvault.ReadJSON(r, &req); err != nil || !req.Contract.valid() || req.Mode != "legacy" {
		http.Error(w, "invalid initialization contract", 400)
		return
	}
	if req.HandlerInstanceID != s.instanceID {
		http.Error(w, "Handler incarnation changed", 412)
		return
	}
	subj := subject.FromSandboxUID(uid)
	gate := s.ownerLock(subj)
	gate.Lock()
	defer gate.Unlock()
	record := s.readWriterOwner(w, r, uid, &req.Contract)
	if record == nil {
		return
	}
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()
	pol := s.vaultValidationPolicy(subj)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vaultRecoveryActive() || s.vaultFiles == nil || s.subjectRebindingLocked(subj) || s.vaultSealedLocked(subj) {
		http.Error(w, "Vault initialization unavailable", 503)
		return
	}
	// Registry's published mirror is checked without adopting a new owner.
	expected := subject.Fencing{RuntimeInstanceID: record.Owner.RuntimeInstanceID, AttachmentID: record.Owner.AttachmentID}
	if !s.fenceMatchesLocked(subj, expected) {
		http.Error(w, "owner not registered", 412)
		return
	}
	if req.Fresh {
		if record.VaultContract != nil {
			http.Error(w, "initialization grant already consumed; verify instead", 409)
			return
		}
		fs, openErr := s.vaultFiles.storeFor(subj)
		if openErr != nil {
			http.Error(w, "Vault persistence unavailable", 503)
			return
		}
		snap, snapErr := fs.Snapshot()
		if snapErr != nil || snap.Exists() {
			http.Error(w, "fresh requires a confirmed empty source", 409)
			return
		}
		if entry, ok := s.vaults[subj]; ok {
			_, fullErr := entry.store.Full()
			if !errors.Is(fullErr, credentialvault.ErrNotFound) || entry.vaultStateUnknown || entry.vaultStateProtected {
				http.Error(w, "fresh cannot replace existing state", 409)
				return
			}
		}
		// Persist identity before publishing empty. Any gap remains unavailable.
		record.VaultContract = &req.Contract
		data, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			http.Error(w, "invalid owner record", 500)
			return
		}
		// ownerFile takes s.mu; release only while the exclusive owner stripe and
		// policy commit gate prevent competing management operations.
		s.mu.Unlock()
		ownerFile, ownerErr := s.ownerFile(subj)
		s.mu.Lock()
		if ownerErr != nil {
			http.Error(w, "owner persistence unavailable", 503)
			return
		}
		state, saveErr := ownerFile.SaveRaw(data)
		if saveErr != nil || state != policy.FileCommitted {
			if state != policy.FileUnchanged {
				s.requireVaultRecoveryLocked("initialization owner persistence uncertain")
			}
			http.Error(w, "initialization owner persistence not confirmed", 503)
			return
		}
		s.vaults[subj] = vaultEntry{store: credentialvault.NewStore(nil, func() bool { return true }), fence: expected, vaultStateUnknown: true, vaultStateProtected: true}
		payload, encodeErr := credentialvault.EncodeVaultFileV2(credentialvault.VaultFile{Incarnation: expected.RuntimeInstanceID, AttachmentID: expected.AttachmentID, State: credentialvault.FileStateEmpty, StateLineage: req.Contract.Lineage, StateRevision: 1, InitializationID: req.Contract.InitializationID}, credentialvault.VaultFileBody{})
		if encodeErr != nil {
			http.Error(w, "invalid initial state", 500)
			return
		}
		state, saveErr = fs.SaveRaw(payload)
		if saveErr != nil || state != policy.FileCommitted {
			if state != policy.FileUnchanged {
				s.requireVaultRecoveryLocked("initial Vault persistence uncertain")
			}
			http.Error(w, "initial Vault persistence not confirmed; verify required", 503)
			return
		}
	} else if record.VaultContract == nil || *record.VaultContract != req.Contract {
		http.Error(w, "initialization contract mismatch", 412)
		return
	}
	s.vaults[subj] = vaultEntry{store: credentialvault.NewStore(nil, func() bool { return true }), fence: expected, vaultStateUnknown: true, vaultStateProtected: true}
	vf, body, err := s.writerFile(subj, record)
	if err != nil {
		http.Error(w, "Vault verification requires recovery", 503)
		return
	}
	store := credentialvault.NewStore(nil, func() bool { return true })
	if vf.State == credentialvault.FileStateActive {
		if err := store.Reload(vf.Revision, credentialvault.CreateRequest(body), pol); err != nil {
			http.Error(w, "Vault verification rejected", 503)
			return
		}
	}
	s.vaults[subj] = vaultEntry{store: store, fence: expected, fileMeta: metadataForWriter(vf), vaultStateProtected: true, v2Verified: true}
	writeJSON(w, 200, s.writerReply(record, vf, nil))
}

func metadataForWriter(v credentialvault.VaultFile) vaultFileMetadata {
	return vaultFileMetadata{version: v.Version, revision: v.Revision, state: v.State, lineage: v.StateLineage, stateRevision: v.StateRevision, initializationID: v.InitializationID}
}
func (s *fastSandboxPolicyServer) writerReply(record *strictOwnerRecord, vf credentialvault.VaultFile, full *credentialvault.FullRevision) vaultStateReply {
	return vaultStateReply{HandlerInstanceID: s.instanceID, Version: vaultWriterVersion, Owner: record.Owner, InitializationID: vf.InitializationID, State: vf.State, Token: vaultStateToken{vf.StateLineage, vf.StateRevision}, Vault: full}
}

// Internal state reads expose the token for empty/deleted as well as active.
// Mutations carry the token captured before the caller constructed the write.
func (s *fastSandboxPolicyServer) handleVaultState(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, vaultStatePrefix)
	if !validVaultSubjectID(uid) {
		http.Error(w, "invalid UID", 400)
		return
	}
	var req struct {
		ExpectedState vaultStateToken                `json:"expectedState"`
		Vault         credentialvault.ReplaceRequest `json:"vault"`
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		if err := credentialvault.ReadJSON(r, &req); err != nil {
			http.Error(w, "invalid conditional mutation", 400)
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	subj := subject.FromSandboxUID(uid)
	gate := s.ownerLock(subj)
	gate.RLock()
	defer gate.RUnlock()
	record := s.readWriterOwner(w, r, uid, nil)
	if record == nil {
		return
	}
	if record.VaultContract == nil {
		http.Error(w, "Vault writer not selected", 404)
		return
	}
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()
	pol := s.vaultValidationPolicy(subj)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.vaults[subj]
	if !ok || !entry.v2Verified || entry.vaultStateUnknown || entry.sealed || s.vaultRecoveryActive() || s.subjectRebindingLocked(subj) {
		http.Error(w, "Vault verification required", 503)
		return
	}
	vf, _, err := s.writerFile(subj, record)
	if err != nil || metadataForWriter(vf) != entry.fileMeta {
		entry.v2Verified = false
		entry.vaultStateUnknown = true
		entry.store = credentialvault.NewStore(nil, func() bool { return true })
		s.vaults[subj] = entry
		http.Error(w, "Vault persistent state requires recovery", 503)
		return
	}
	if r.Method == http.MethodGet {
		var full *credentialvault.FullRevision
		if vf.State == credentialvault.FileStateActive {
			v, err := entry.store.Full()
			if err != nil {
				writeVaultRelayError(w, err)
				return
			}
			full = &v
		}
		writeJSON(w, 200, s.writerReply(record, vf, full))
		return
	}
	if req.ExpectedState != (vaultStateToken{vf.StateLineage, vf.StateRevision}) {
		http.Error(w, "private state precondition mismatch", 412)
		return
	}
	if vf.StateRevision == math.MaxUint64 || r.Method == http.MethodPut && vf.Revision == math.MaxInt64 {
		http.Error(w, "Vault sequence exhausted", 409)
		return
	}
	var candidate *credentialvault.MutationCandidate
	switch r.Method {
	case http.MethodPost:
		candidate, err = entry.store.PrepareCreate(req.Vault.CreateRequest, pol)
	case http.MethodPut:
		candidate, err = entry.store.PrepareReplace(req.Vault, pol)
	case http.MethodDelete:
		candidate, err = entry.store.PrepareDelete()
	}
	if err != nil {
		writeVaultRelayError(w, err)
		return
	}
	defer candidate.Discard()
	if candidate.Exists() {
		if _, err = candidate.ActiveSnapshot(r.Context()); err != nil {
			http.Error(w, "candidate cannot be rendered", 400)
			return
		}
	}
	vf.StateRevision++
	vf.State = credentialvault.FileStateDeleted
	vf.Revision = 0
	body := credentialvault.VaultFileBody{}
	if candidate.Exists() {
		full, err := candidate.Full()
		if err != nil {
			writeVaultRelayError(w, err)
			return
		}
		vf.State = credentialvault.FileStateActive
		vf.Revision = full.Revision
		body = credentialvault.VaultFileBody{Credentials: full.Credentials, Bindings: full.Bindings}
	}
	payload, err := credentialvault.EncodeVaultFileV2(vf, body)
	if err != nil {
		http.Error(w, "invalid Vault state", 500)
		return
	}
	var state credentialvault.State
	err = s.persistVaultCandidateLocked(subj, func(f vaultFileStore) (policy.FileMutationState, error) { return f.SaveRaw(payload) }, func() error {
		var commitErr error
		state, commitErr = entry.store.CommitCandidate(candidate)
		if commitErr == nil {
			entry.fileMeta = metadataForWriter(vf)
			s.vaults[subj] = entry
		}
		return commitErr
	})
	if err != nil {
		http.Error(w, "Vault persistence not confirmed", 503)
		return
	}
	writeVaultRelayWriteResponse(w, r, state, candidate.Created())
}

// Called under the exclusive owner stripe after durable phase=retiring.
// Keep the owner record indefinitely, but remove credential bytes only after
// matching the contract. Missing bytes are acceptable for a retried removal.
func (s *fastSandboxPolicyServer) retireWriterVault(subj subject.Subject, record *strictOwnerRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vaultFiles == nil || s.vaultRecoveryActive() {
		return errors.New("Vault recovery required")
	}
	f, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return err
	}
	snap, err := f.Snapshot()
	if err != nil {
		return err
	}
	if snap.Exists() {
		if _, _, err := s.writerFile(subj, record); err != nil {
			return err
		}
		state, err := f.Delete()
		if err != nil || state != policy.FileCommitted {
			if state != policy.FileUnchanged {
				s.requireVaultRecoveryLocked("Vault retirement persistence uncertain")
			}
			return fmt.Errorf("Vault removal not confirmed: %v", err)
		}
	}
	delete(s.vaults, subj)
	return nil
}
