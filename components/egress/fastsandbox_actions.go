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

// Fastlet Sandbox Actions Handler surface (sandbox.fast.io/actions/v1,
// fast-sandbox docs/concepts/sandbox-actions.md): the Fastlet is the sole
// lifecycle dispatcher and the egress process is the Handler. It replaces
// the earlier file-driven slot-store observation: binding synchronization and
// lifecycle checkpoints arrive over the Pod-loopback HTTP endpoints instead
// of being polled from /run/fast-sandbox/network/*.json.
//
// Operation mapping:
//
//	SET_BINDING        register the subject (deny-first) and seed the input
//	                   policy on a fresh subject (an existing record
//	                   supersedes the input); activate immediately when the
//	                   subject is already active. A JSON-null input removes
//	                   the policy and reverts to deny-first. A key held by
//	                   a different live subject is a stale-subject
//	                   takeover (OSEP-0022): the previous owner goes first.
//	LIFECYCLE_HOOK     sandbox.runtime-ready only confirms the deny-first
//	                   install (idempotent); sandbox.data-plane-ready applies
//	                   the pending policy -> active. Unknown Hooks are
//	                   rejected, never silently ignored.
//	REMOVE_BINDING     terminal cleanup; a fence mismatch (stale removal for
//	                   a previous instance of the same UID) is ignored.
//	VAULT_EXPORT       copy the vault file into the snapshot bundle and seal
//	                   the subject — pause flow, before teardown.
//	VAULT_IMPORT       land the bundle re-stamped with the restored fence —
//	                   resume flow, before data-plane-ready.
//	POLICY_EXPORT      copy the recorded policy file into the snapshot bundle
//	                   and seal the subject — pause flow, before teardown.
//	POLICY_IMPORT      land the policy bundle re-stamped with the restored
//	                   fence — resume flow, before data-plane-ready.
//
// Fail-closed invariants: the subject is deny-first from registration until
// its data-plane-ready Hook succeeds, so policy delivery can be late but
// never early-open; enforcement failures return non-200 and are retryable
// (at-least-once, same invocationId). Cancellation or terminal Delete may
// stop retries; unfinished cleanup is not guaranteed to run again.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// newHandlerInstanceID mints this process's Handler incarnation id. It must
// be stable for the process lifetime and change across restarts: the Fastlet
// detects a changed instanceId, invalidates Binding readiness, and replays
// the latest SET_BINDING followed by the already-reached Hooks.
func newHandlerInstanceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal enough that a deterministic fallback
		// (pid + start time) still satisfies the incarnation contract.
		return fmt.Sprintf("egress-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("egress-%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
}

// handleActionsStatus serves GET /_fastlet/v1/actions/status: the Handler
// process incarnation probe. ready mirrors the healthz gate (false while the
// MITM stack is not ready or the vault recovery latch freezes writes);
// instanceId is the replay trigger. A per-subject degraded vault is
// deliberately not a readiness failure (it fails closed for that subject
// only) — it is listed in degradedVaults instead.
func (s *fastSandboxPolicyServer) handleActionsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.actionStatusResponse())
}

func (s *fastSandboxPolicyServer) actionStatusResponse() actionhandler.StatusResponse {
	recovery := s.vaultRecoveryActive()
	policyRecovery := s.policyRecoveryActive()
	degraded := s.degradedVaultUIDs()
	ready := true
	if s.mitmGate != nil && s.mitmGate.MitmPending() {
		ready = false
	}
	if recovery || policyRecovery {
		ready = false
	}
	resp := actionhandler.StatusResponse{
		APIVersion: constants.ActionsAPIVersion,
		Ready:      ready,
		InstanceID: s.instanceID,
		Recovery:   recovery,
		// Capability signal for the Fastlet's vault transfer: without vault
		// persistence there is no vault state to hand off, so the transfer
		// degrades (skips) rather than failing the checkpoint.
		VaultTransfer: s.vaultFiles != nil,
		// Capability signal for the Fastlet's policy transfer: without policy
		// persistence there is no recorded policy to hand off, so the
		// transfer degrades (skips) rather than failing the checkpoint.
		PolicyTransfer:      s.policyFiles != nil,
		PolicyRecovery:      policyRecovery,
		DegradedPolicies:    s.degradedPolicyUIDs(),
		DegradedVaults:      degraded,
		TemplateStateExport: true,
	}
	if s.mitmGate != nil {
		resp.MitmCaFingerprint = s.mitmGate.CAFingerprint()
		if reason, down := s.mitmGate.DownReason(); down {
			resp.MitmCaDown = true
			resp.MitmCaError = reason
		}
	}
	return resp
}

func (s *fastSandboxPolicyServer) handleActionsV2Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := s.actionStatusResponse()
	resp.APIVersion = constants.ActionsAPIVersionV2
	resp.OwnerStatus = true
	resp.VaultTransfer, resp.PolicyTransfer, resp.TemplateStateExport = false, false, false
	writeJSON(w, http.StatusOK, resp)
}

// handleActions serves POST /_fastlet/v1/actions. HTTP 200 is success; any
// other status is a failed attempt. Parse-level errors
// are 400 (permanently invalid input); enforcement failures are 500 (retried).
func (s *fastSandboxPolicyServer) handleActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBodyBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read action body: %v", err), http.StatusBadRequest)
		return
	}
	env, err := actionhandler.ParseEnvelope(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid action envelope: %v", err), http.StatusBadRequest)
		return
	}
	status, err := s.dispatchAction(r.Context(), env)
	if err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("action %s for sandbox %s failed: %v", env.Operation, env.Sandbox.UID, err))
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *fastSandboxPolicyServer) handleActionsV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBodyBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read action body: %v", err), http.StatusBadRequest)
		return
	}
	env, err := actionhandler.ParseEnvelopeV2(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid Actions v2 envelope: %v", err), http.StatusBadRequest)
		return
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	ownerLock := s.ownerLock(subj)
	ownerLock.Lock()
	defer ownerLock.Unlock()
	if err := s.validateActionsV2Owner(subj, env); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	switch env.Operation {
	case actionhandler.OperationSetBinding, actionhandler.OperationLifecycleHook, actionhandler.OperationRemoveBinding:
	default:
		http.Error(w, "snapshot and state-transfer operations are not supported by Actions v2", http.StatusNotImplemented)
		return
	}
	if env.Operation == actionhandler.OperationSetBinding && env.Binding.IsRemoval() {
		http.Error(w, "live binding removal is unsupported; use Sandbox Delete", http.StatusNotImplemented)
		return
	}
	record, err := s.readOwnerRecord(subj)
	if err != nil {
		http.Error(w, "owner persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	if env.Operation == actionhandler.OperationRemoveBinding && record != nil && record.Phase == "retired" {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	if env.Operation == actionhandler.OperationSetBinding || env.Operation == actionhandler.OperationRemoveBinding {
		phase := "active"
		if env.Operation == actionhandler.OperationRemoveBinding {
			phase = "retiring"
		}
		if err := s.writeOwnerRecord(subj, *env.Owner, phase); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	// An absent owner has no registered runtime to dispatch REMOVE to. Persist
	// a terminal fence anyway, so a delayed initial SET cannot resurrect it.
	if env.Operation == actionhandler.OperationRemoveBinding && record == nil {
		if err := s.writeOwnerRecord(subj, *env.Owner, "retired"); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	if env.Operation == actionhandler.OperationRemoveBinding && record != nil && record.VaultContract != nil {
		if err := s.retireWriterVault(subj, record); err != nil {
			http.Error(w, "Vault retirement requires recovery", http.StatusServiceUnavailable)
			return
		}
	}
	status, err := s.dispatchAction(r.Context(), env)
	if err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("Actions v2 %s for sandbox %s failed: %v", env.Operation, env.Sandbox.UID, err))
		http.Error(w, err.Error(), status)
		return
	}
	if env.Operation == actionhandler.OperationRemoveBinding {
		if err := s.writeOwnerRecord(subj, *env.Owner, "retired"); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	s.mu.Lock()
	switch env.Operation {
	case actionhandler.OperationSetBinding:
		s.actionOwners[subj] = *env.Owner
	case actionhandler.OperationRemoveBinding:
		if current, ok := s.actionOwners[subj]; ok && current.Matches(*env.Owner) {
			delete(s.actionOwners, subj)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *fastSandboxPolicyServer) validateActionsV2Owner(subj subject.Subject, env *actionhandler.Envelope) error {
	record, err := s.readOwnerRecord(subj)
	if err != nil {
		return err
	}
	if record == nil {
		if env.Operation != actionhandler.OperationSetBinding && env.Operation != actionhandler.OperationRemoveBinding {
			return fmt.Errorf("strict owner is not registered")
		}
		return s.cleanOwnerSubject(subj)
	}
	if !record.Owner.Matches(*env.Owner) {
		return fmt.Errorf("strict owner mismatch; rebind is unsupported")
	}
	if record.Phase != "active" && env.Operation != actionhandler.OperationRemoveBinding {
		return fmt.Errorf("strict owner is retiring or retired")
	}
	return nil
}

func (s *fastSandboxPolicyServer) handleActionsV2Owner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uid := strings.TrimPrefix(r.URL.Path, constants.ActionsV2OwnerPathPrefix)
	owner, err := ownerFromRequest(r)
	if !validVaultSubjectID(uid) || err != nil || owner.SandboxUID != uid {
		http.Error(w, "valid matching owner required", http.StatusBadRequest)
		return
	}
	subj := subject.FromSandboxUID(uid)
	lock := s.ownerLock(subj)
	lock.RLock()
	defer lock.RUnlock()
	record, err := s.readOwnerRecord(subj)
	if err != nil {
		http.Error(w, "owner persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	if record == nil {
		if err := s.cleanOwnerSubject(subj); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, actionhandler.OwnerObservation{State: "absent", Owner: owner})
		return
	}
	if !record.Owner.Matches(owner) {
		http.Error(w, "owner mismatch", http.StatusConflict)
		return
	}
	state := "current"
	if record.Phase == "retired" {
		state = "retired"
	}
	writeJSON(w, http.StatusOK, actionhandler.OwnerObservation{State: state, Owner: owner})
}

func (s *fastSandboxPolicyServer) subjectHasLiveStateLocked(subj subject.Subject) bool {
	_, rebind := s.rebinds[subj]
	_, vault := s.vaults[subj]
	_, policy := s.policies[subj]
	_, pending := s.pendingPolicies[subj]
	_, attachment := s.subjAtt[subj]
	_, fence := s.subjFence[subj]
	return rebind || vault || policy || pending || attachment || fence
}

func (s *fastSandboxPolicyServer) subjectHasPersistentState(subj subject.Subject) (bool, error) {
	if s.vaultFiles != nil {
		fs, err := s.vaultFiles.storeFor(subj)
		if err != nil {
			return false, err
		}
		snapshot, err := fs.Snapshot()
		if err != nil {
			return false, err
		}
		if snapshot.Exists() || s.vaultFiles.isSealed(subj) {
			return true, nil
		}
	}
	if s.policyFiles != nil {
		fs, err := s.policyFiles.storeFor(subj)
		if err != nil {
			return false, err
		}
		snapshot, err := fs.Snapshot()
		if err != nil {
			return false, err
		}
		if snapshot.Exists() || s.policyFiles.isSealed(subj) {
			return true, nil
		}
	}
	return false, nil
}

func ownerFromRequest(r *http.Request) (actionhandler.Owner, error) {
	encoded := strings.TrimSpace(r.Header.Get(constants.EgressActionsOwnerHeader))
	if encoded == "" {
		return actionhandler.Owner{}, fmt.Errorf("owner header is required")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return actionhandler.Owner{}, err
	}
	var owner actionhandler.Owner
	if err := json.Unmarshal(decoded, &owner); err != nil {
		return owner, err
	}
	if !owner.Valid() || strings.TrimSpace(r.Header.Get(constants.EgressActionsSandboxUIDHeader)) != owner.SandboxUID {
		return owner, fmt.Errorf("owner header is incomplete or disagrees with Sandbox UID header")
	}
	return owner, nil
}

func (s *fastSandboxPolicyServer) validateRelayOwner(w http.ResponseWriter, r *http.Request, uid string) bool {
	if s.actionsAPIVersion != constants.ActionsAPIVersionV2 {
		return true
	}
	owner, err := ownerFromRequest(r)
	if err != nil || owner.SandboxUID != uid {
		http.Error(w, "relay owner must match the path Sandbox UID", http.StatusBadRequest)
		return false
	}
	subj := subject.FromSandboxUID(uid)
	record, recordErr := s.readOwnerRecord(subj)
	if recordErr != nil || record == nil || record.Phase != "active" || !record.Owner.Matches(owner) {
		http.Error(w, "strict owner is not active", http.StatusPreconditionFailed)
		return false
	}
	s.mu.Lock()
	current, found := s.actionOwners[subj]
	s.mu.Unlock()
	if !found || !current.Matches(owner) {
		http.Error(w, "relay owner is not the current registered owner", http.StatusPreconditionFailed)
		return false
	}
	return true
}

// dispatchAction validates effect-free input before entering the cancellable
// action gate. Every runtime registry writer is reached through this gate;
// snapshot transfers retain their own Vault transaction/guard semantics.
func (s *fastSandboxPolicyServer) dispatchAction(ctx context.Context, env *actionhandler.Envelope) (int, error) {
	var pol *policy.NetworkPolicy
	switch env.Operation {
	case actionhandler.OperationVaultExport:
		return s.applyVaultExport(env)
	case actionhandler.OperationVaultImport:
		return s.applyVaultImport(env)
	case actionhandler.OperationPolicyExport:
		return s.applyPolicyExport(env)
	case actionhandler.OperationPolicyImport:
		return s.applyPolicyImport(env)
	case actionhandler.OperationSetBinding:
		if !env.Binding.IsRemoval() {
			input, err := env.Binding.InputString()
			if err != nil {
				return http.StatusBadRequest, err
			}
			pol, err = policy.ParsePolicy(input)
			if err != nil {
				return http.StatusBadRequest, fmt.Errorf("invalid binding input policy: %v", err)
			}
		}
	case actionhandler.OperationLifecycleHook, actionhandler.OperationRemoveBinding:
	default:
		return http.StatusBadRequest, fmt.Errorf("unknown operation %q", env.Operation)
	}
	release, err := s.acquireAction(ctx)
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("action canceled: %w", err)
	}
	defer release()
	switch env.Operation {
	case actionhandler.OperationSetBinding:
		return s.applySetBinding(ctx, env, pol)
	case actionhandler.OperationLifecycleHook:
		return s.applyLifecycleHook(env)
	default:
		return s.applyRemoveBinding(env)
	}
}

// applySetBinding holds the action gate. The policy has already been parsed;
// same-fence network rejection precedes every effect, including Vault reload.
func (s *fastSandboxPolicyServer) applySetBinding(ctx context.Context, env *actionhandler.Envelope, pol *policy.NetworkPolicy) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	att := env.Network()
	// Serialize registration and rebind cleanup with policy and vault commits.
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()
	old, registered := s.reg.Fence(subj)
	oldState, _ := s.reg.Get(subj)
	if stale, ok := s.reg.Resolve(subject.SubjectKey{SourceIP: att.IP}); ok && stale != subj {
		s.mu.Lock()
		status, purgeErr := s.vaultPurgeStatusLocked(stale)
		s.mu.Unlock()
		if purgeErr != nil {
			return status, fmt.Errorf("stale subject cleanup blocked: %w", purgeErr)
		}
	}
	s.mu.Lock()
	// Waiting for an in-progress Vault commit may outlive the request too.
	if err := s.actionCanceled(ctx); err != nil {
		s.mu.Unlock()
		return http.StatusServiceUnavailable, fmt.Errorf("action canceled: %w", err)
	}
	oldAtt, knownNetwork := s.subjAtt[subj]
	knownNetwork = knownNetwork && oldAtt.IP.IsValid()
	token := s.rebinds[subj]
	// A previous attempt may have purged bookkeeping before old-IP removal
	// failed. Its token still carries the actual old network for comparisons.
	if !knownNetwork && token != nil && registered && token.old.Matches(old) && token.oldNetwork.IP.IsValid() {
		oldAtt, knownNetwork = token.oldNetwork, true
	}
	if token == nil && registered && old.Matches(fence) && !knownNetwork {
		if oldState == subject.StateActive {
			// Successful registration always records an attempt before becoming
			// active. Missing metadata here is an internal inconsistency, not a
			// reason to mutate a live subject or acknowledge an uninstalled update.
			s.mu.Unlock()
			return http.StatusInternalServerError, fmt.Errorf("active subject %s has no registered network", subj)
		}
	}
	willPurge := (token != nil && (!token.expectedNew.Matches(fence) || !token.purged)) || (token == nil && registered && !old.Matches(fence))
	if willPurge {
		status, purgeErr := s.vaultPurgeStatusLocked(subj)
		if purgeErr != nil {
			s.mu.Unlock()
			return status, fmt.Errorf("subject rebind blocked: %w", purgeErr)
		}
	}
	// A null binding may omit attachment only for its own known identity.
	// Never register a new fence using the previous instance's network.
	if env.Binding.IsRemoval() && env.Attachment == nil {
		if token != nil && token.expectedNew.Matches(fence) && token.newNetwork.IP.IsValid() {
			att = token.newNetwork
		} else if token == nil && registered && old.Matches(fence) {
			att = oldAtt
		} else if registered || token != nil {
			s.mu.Unlock()
			return http.StatusBadRequest, fmt.Errorf("attachment required for registration identity")
		}
	}
	if (token != nil && token.expectedNew.Matches(fence) && !sameBindingNetwork(token.newNetwork, att)) ||
		(registered && old.Matches(fence) && knownNetwork && !sameBindingNetwork(oldAtt, att)) {
		s.mu.Unlock()
		return http.StatusConflict, fmt.Errorf("same-fence network change for subject %s", subj)
	}
	if token != nil && !token.expectedNew.Matches(fence) {
		// The latest valid, uncanceled SET supersedes an abandoned intent. Its
		// new token guards a full reset of actual residual state, even when the
		// previous registration failed before publishing any registry entry.
		token = &subjectRebind{old: old, expectedNew: fence, oldNetwork: oldAtt, newNetwork: att, needsReset: true}
		s.rebinds[subj] = token
	} else if token == nil && registered && !old.Matches(fence) {
		token = &subjectRebind{old: old, expectedNew: fence, oldNetwork: oldAtt, newNetwork: att, removeOld: !knownNetwork || oldAtt.IP != att.IP}
		s.rebinds[subj] = token
	}
	needsReset := token != nil && token.needsReset
	s.mu.Unlock()
	defer s.logPendingRebind(subj, token)
	if token != nil {
		if needsReset {
			if err := s.unloadEnforcement(subj); err != nil {
				return http.StatusInternalServerError, fmt.Errorf("superseded registration reset: %w", err)
			}
			s.reg.Unregister(subj)
			s.mu.Lock()
			if err := s.dropSubjectStateLocked(subj); err != nil { // preserve this exact token through C publication
				status := s.vaultPurgeFailureStatusLocked(subj)
				s.mu.Unlock()
				return status, fmt.Errorf("subject reset purge: %w", err)
			}
			token.needsReset = false
			token.oldRemoved = true
			token.purged = true
			s.mu.Unlock()
		}
		s.mu.Lock()
		if !token.purged {
			if err := s.dropSubjectStateLocked(subj); err != nil {
				status := s.vaultPurgeFailureStatusLocked(subj)
				s.mu.Unlock()
				return status, fmt.Errorf("subject rebind purge: %w", err)
			}
			token.purged = true
		}
		removeOld := token.removeOld && !token.oldRemoved
		s.mu.Unlock()
		if removeOld {
			nftCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
			err := s.nft.Remove(nftCtx, subj)
			cancel()
			if err != nil {
				return http.StatusInternalServerError, fmt.Errorf("old network removal: %w", err)
			}
			s.mu.Lock()
			token.oldRemoved = true
			s.mu.Unlock()
		}
	} else if registered && !knownNetwork {
		// Legacy/unknown attempt metadata cannot prove reset will remove old IP
		// dispatch. A conservative Remove is retryable; do not invent a network.
		if err := s.unloadEnforcement(subj); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("unknown network removal: %w", err)
		}
		if err := s.reg.UnsetPolicy(subj); err != nil {
			return http.StatusInternalServerError, err
		}
	}
	if !env.Binding.IsRemoval() {
		// Policy first: its reload result is the validation policy for the
		// vault reload (a template clone's vault hosts are covered by the
		// landed template policy, not by the replayed bootstrap input). A
		// degraded policy record degrades the VAULT with it (fail-closed,
		// file kept) instead of seeding the deny fallback — a deny-seeded
		// vault validation would quarantine a healthy vault.
		seed, policyDegraded := s.rehydratePolicyReload(subj, fence)
		if seed == nil {
			seed = pol
		}
		s.rehydrateVaultReload(subj, fence, seed, policyDegraded)
	}
	setBindingStarted := time.Now()
	installStarted := time.Now()
	// Record even failed first installs: partial kernel effects otherwise have
	// no network identity for a later same-fence retry or changed-fence rebind.
	s.recordBindingState(subj, att)
	var victims []subject.Subject
	state, err := s.reg.RegisterAndEnforce(subj, subject.SubjectKey{SourceIP: att.IP}, fence, func() error {
		return s.OnRegistered(subj, att)
	}, func(stale subject.Subject) error {
		victims = append(victims, stale)
		return s.takeoverStaleSubject(stale)
	})
	// Callback teardown never takes mu. Only purge actual evictions after the
	// registry lock is released, even if successor enforcement failed. The
	// action gate prevents a victim being re-registered before this purge.
	for _, victim := range victims {
		if _, exists := s.reg.Fence(victim); !exists {
			s.dropSubjectState(victim)
		}
	}
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("deny-first install: %w", err)
	}
	denyFirstInstall := time.Since(installStarted)
	s.recordRegisteredFence(subj, fence)
	if env.Binding.IsRemoval() {
		// Binding removed from a still-live sandbox: drop the pending policy
		// (a re-added binding gets a fresh input — never apply the removed
		// binding's policy to a policy-less subject) and purge the recorded
		// policy, so deny-first is also the durable state.
		s.clearPendingPolicy(subj)
		if err := s.clearPolicyRecordLocked(subj); err != nil {
			// The durable removal did not confirm: fail the action so the
			// Fastlet's retry re-runs the purge/tombstone instead of acking
			// a removal whose file may still carry the removed policy.
			return http.StatusInternalServerError, fmt.Errorf("policy record removal: %w", err)
		}
		if state == subject.StateActive {
			if err := s.revertToDenyFirst(subj, att); err != nil {
				return http.StatusInternalServerError, err
			}
		}
		s.finishRebind(subj, token)
		log.Infof("subject %s: binding removed (deny-first)", subj)
		return http.StatusOK, nil
	}
	// Install the binding policy (or honor the file-backed record that
	// supersedes it) through the policy commit protocol.
	policyStarted := time.Now()
	if err := s.applyBindingPolicyLocked(subj, fence, pol, state == subject.StateActive); err != nil {
		log.Errorf("subject %s: binding policy commit failed: %v", subj, err)
		return http.StatusInternalServerError, fmt.Errorf("binding policy commit: %w", err)
	}
	s.finishRebind(subj, token)
	policyCommit := time.Since(policyStarted)
	log.Infof("subject %s: SET_BINDING applied (state=%s) total=%dms denyFirstInstall=%dms policyCommit=%dms",
		subj, state, time.Since(setBindingStarted).Milliseconds(), denyFirstInstall.Milliseconds(), policyCommit.Milliseconds())
	return http.StatusOK, nil
}

// applyLifecycleHook processes a reached lifecycle checkpoint. runtime-ready
// confirms the deny-first install (idempotent re-registration — a replay
// after an egress restart re-enters the subject through SET_BINDING first);
// data-plane-ready applies the pending policy and activates the subject.
//
// Both Hooks are fenced against the registered identity: a delayed Hook from
// a previous instance of the same UID must never consume the replacement
// sandbox's pending policy or activate it before its own data plane is ready
// (fail closed; the Fastlet retries with the current revision).
func (s *fastSandboxPolicyServer) applyLifecycleHook(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	s.mu.Lock()
	rebinding := s.subjectRebindingLocked(subj)
	s.mu.Unlock()
	if rebinding {
		return http.StatusServiceUnavailable, fmt.Errorf("subject %s registration is unfinished", subj)
	}
	regFence, ok := s.reg.Fence(subj)
	if !ok {
		return http.StatusConflict, fmt.Errorf("%s for unregistered subject %s", env.Hook.Name, subj)
	}
	if !regFence.Matches(fence) {
		return http.StatusConflict, fmt.Errorf("stale %s (fence mismatch) for subject %s", env.Hook.Name, subj)
	}
	switch env.Hook.Name {
	case constants.HookRuntimeReady:
		return http.StatusOK, nil
	case constants.HookDataPlaneReady:
		// Serialized with the policy relay commits (policyCommitMu): a
		// relay write that passed its guards before this hook must either
		// land first (the activation below then applies the relayed
		// pending policy) or after (its apply-time state re-read then sees
		// Active and updates enforcement in place) — it can never land
		// after the activation as a pending-only write that nothing will
		// ever consume.
		s.policyCommitMu.Lock()
		defer s.policyCommitMu.Unlock()
		// Fence re-validation AND the pending peek under one s.mu hold
		// (policyCommitMu → s.mu, the established order): fenceMatchesLocked
		// reads subjFence, which terminal cleanup deletes under s.mu. An
		// unsynchronized read here would race that delete. The pre-lock fence check above cannot see a rebind or
		// removal that completes while this Hook waits on policyCommitMu —
		// a stale Hook must never activate the successor.
		s.mu.Lock()
		stale := !s.fenceMatchesLocked(subj, fence)
		pol := s.pendingPolicies[subj]
		s.mu.Unlock()
		if stale {
			return http.StatusConflict, fmt.Errorf("stale %s (fence mismatch) for subject %s", env.Hook.Name, subj)
		}
		if pol == nil {
			// No pending policy: SET_BINDING has not landed (protocol
			// ordering violation) or the binding was removed. Fail closed —
			// the subject must never activate without its current policy.
			return http.StatusConflict, fmt.Errorf("data-plane-ready for subject %s with no pending policy", subj)
		}
		applyStarted := time.Now()
		if err := s.applyPolicy(subj, pol); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("policy apply: %w", err)
		}
		s.clearPendingPolicy(subj)
		log.Infof("subject %s: data-plane-ready, policy active applyPolicy=%dms", subj, time.Since(applyStarted).Milliseconds())
		return http.StatusOK, nil
	default:
		// Unreachable: ParseEnvelope validated the Hook name.
		return http.StatusBadRequest, fmt.Errorf("unknown lifecycle hook %q", env.Hook.Name)
	}
}

// applyRemoveBinding performs terminal cleanup. Missing Handler state is
// success; a fence mismatch (a stale removal for a previous instance of the
// same UID) is ignored so it can never unload the current sandbox's subject.
//
// The subject stays registered until enforcement removal succeeds: a
// transient failure returns 500 and the retried REMOVE_BINDING resumes
// cleanup instead of succeeding with stale rules left in the kernel.
//
// Final local purge follows Unregister. A relay that already validated the old
// fence may finish before this purge; later relay requests find no identity.
func (s *fastSandboxPolicyServer) applyRemoveBinding(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	s.mu.Lock()
	token := s.rebinds[subj]
	s.mu.Unlock()
	if token != nil && !token.expectedNew.Matches(fence) {
		log.Infof("subject %s: stale REMOVE_BINDING ignored (pending rebind fence mismatch)", subj)
		return http.StatusOK, nil
	}
	defer s.logPendingRebind(subj, token)
	registeredFence, ok := s.reg.Fence(subj)
	if token == nil && ok && !registeredFence.Matches(fence) {
		log.Infof("subject %s: stale REMOVE_BINDING ignored (fence mismatch)", subj)
		return http.StatusOK, nil
	}
	s.mu.Lock()
	purgeStatus, purgeErr := s.vaultPurgeStatusLocked(subj)
	s.mu.Unlock()
	if purgeErr != nil {
		return purgeStatus, fmt.Errorf("binding removal blocked: %w", purgeErr)
	}
	if token == nil && !ok {
		s.policyCommitMu.Lock()
		s.dropSubjectState(subj)
		s.policyCommitMu.Unlock()
		return http.StatusOK, nil
	}
	// expectedNew REMOVE cancels an unfinished registration intent, including
	// prepublication failure where the registry still contains old (or no)
	// identity but kernel effects may remain. Failure retains token and state.
	if err := s.unloadEnforcement(subj); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("enforcement removal: %w", err)
	}
	prev := s.reg.Unregister(subj)
	// Terminal file cleanup remains best-effort after Unregister. A failed
	// delete leaves the file fenced to the old identity; this acknowledgement
	// does not promise automatic cleanup retry.
	s.policyCommitMu.Lock()
	s.mu.Lock()
	if err := s.dropSubjectStateLocked(subj); err != nil {
		status := s.vaultPurgeFailureStatusLocked(subj)
		s.mu.Unlock()
		s.policyCommitMu.Unlock()
		return status, fmt.Errorf("terminal state purge: %w", err)
	}
	cleared := token != nil && s.rebinds[subj] == token
	if cleared {
		delete(s.rebinds, subj)
	}
	s.mu.Unlock()
	s.policyCommitMu.Unlock()
	if cleared {
		log.Infof("subject_rebind_cleared")
	}
	log.Infof("subject %s: REMOVE_BINDING complete (was %s)", subj, prev)
	return http.StatusOK, nil
}
