// Copyright 2026 Alibaba Group Holding Ltd.
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
//	SET_BINDING        register the subject (deny-first), store the input
//	                   policy as pending; activate immediately when the
//	                   subject is already active (a plain input update does
//	                   not replay Hooks). A JSON-null input removes the
//	                   policy and reverts to deny-first.
//	LIFECYCLE_HOOK     sandbox.runtime-ready only confirms the deny-first
//	                   install (idempotent); sandbox.data-plane-ready applies
//	                   the pending policy -> active. Unknown Hooks are
//	                   rejected, never silently ignored.
//	REMOVE_BINDING     terminal cleanup; a fence mismatch (stale removal for
//	                   a previous instance of the same UID) is ignored.
//
// Fail-closed invariants: the subject is deny-first from registration until
// its data-plane-ready Hook succeeds, so policy delivery can be late but
// never early-open; any enforcement failure returns non-200 and the Fastlet
// retries (at-least-once, same invocationId).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
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
// MITM stack is not ready); instanceId is the replay trigger.
func (s *fleetPolicyServer) handleActionsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ready := true
	if s.mitmGate != nil && s.mitmGate.MitmPending() {
		ready = false
	}
	writeJSON(w, http.StatusOK, actionhandler.StatusResponse{
		APIVersion: constants.ActionsAPIVersion,
		Ready:      ready,
		InstanceID: s.instanceID,
	})
}

// handleActions serves POST /_fastlet/v1/actions. HTTP 200 is success; any
// other status is a failed attempt the Fastlet retries. Parse-level errors
// are 400 (permanently invalid input); enforcement failures are 500 (retried).
func (s *fleetPolicyServer) handleActions(w http.ResponseWriter, r *http.Request) {
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
	status, err := s.dispatchAction(env)
	if err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("action %s for sandbox %s failed: %v", env.Operation, env.Sandbox.UID, err))
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// dispatchAction executes one validated action and returns its HTTP status.
func (s *fleetPolicyServer) dispatchAction(env *actionhandler.Envelope) (int, error) {
	switch env.Operation {
	case actionhandler.OperationSetBinding:
		return s.applySetBinding(env)
	case actionhandler.OperationLifecycleHook:
		return s.applyLifecycleHook(env)
	case actionhandler.OperationRemoveBinding:
		return s.applyRemoveBinding(env)
	default:
		// Unreachable: ParseEnvelope validated the operation.
		return http.StatusBadRequest, fmt.Errorf("unknown operation %q", env.Operation)
	}
}

// applySetBinding registers the subject deny-first and stores the binding
// input. The policy is parsed BEFORE any state mutation so a permanently
// invalid input (400) can never leave the subject half-registered.
func (s *fleetPolicyServer) applySetBinding(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	att := env.Network()
	var pol *policy.NetworkPolicy
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
	state, err := s.reg.RegisterAndEnforce(subj, subject.SubjectKey{SourceIP: att.IP}, fence, func() error {
		return s.OnRegistered(subj, att)
	})
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("deny-first install: %w", err)
	}
	s.recordBindingState(subj, att, env.Revision.SpecGeneration)
	if env.Binding.IsRemoval() {
		// Binding removed from a still-live sandbox: the cached pushes are
		// stale for the removed binding (a re-added binding gets fresh
		// pushes) — drop them, never apply them to a policy-less subject.
		s.dropPendingPushes(subj)
		s.clearPendingPolicy(subj)
		if state == subject.StateActive {
			if err := s.revertToDenyFirst(subj, att); err != nil {
				return http.StatusInternalServerError, err
			}
		}
		log.Infof("subject %s: binding removed (deny-first)", subj)
		return http.StatusOK, nil
	}
	if state == subject.StateActive {
		// Ordinary input update on an already-ready sandbox: apply in place,
		// no Hook replay (the Fastlet does not send one for updates).
		if err := s.applyPolicy(subj, pol); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("policy apply: %w", err)
		}
	} else {
		// Deny-first until sandbox.data-plane-ready activates the subject.
		s.storePendingPolicy(subj, pol)
	}
	// Flush cached pushes LAST: a push that landed during the registration
	// window is newer intent than the binding input, so it overwrites the
	// pending policy (the lifecycle barrier still holds — it only becomes
	// effective at data-plane-ready). Vault pushes apply regardless.
	s.OnRegisteredComplete(subj, att, env.Revision.SpecGeneration)
	log.Infof("subject %s: SET_BINDING applied (state=%s)", subj, state)
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
func (s *fleetPolicyServer) applyLifecycleHook(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
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
		// Peek, do not consume: a transient nft failure must leave the
		// pending policy in place so the Fastlet's retry of the same Hook
		// succeeds instead of 409-ing forever with the subject deny-first.
		pol := s.pendingPolicy(subj)
		if pol == nil {
			// No pending policy: SET_BINDING has not landed (protocol
			// ordering violation) or the binding was removed. Fail closed —
			// the subject must never activate without its current policy.
			return http.StatusConflict, fmt.Errorf("data-plane-ready for subject %s with no pending policy", subj)
		}
		if err := s.applyPolicy(subj, pol); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("policy apply: %w", err)
		}
		s.clearPendingPolicy(subj)
		log.Infof("subject %s: data-plane-ready, policy active", subj)
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
func (s *fleetPolicyServer) applyRemoveBinding(env *actionhandler.Envelope) (int, error) {
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	registeredFence, ok := s.reg.Fence(subj)
	if ok && !registeredFence.Matches(fence) {
		log.Infof("subject %s: stale REMOVE_BINDING ignored (fence mismatch)", subj)
		return http.StatusOK, nil
	}
	if !ok {
		// Missing Handler state is success; still drop any cached pushes for
		// the dead sandbox so they can never flush into a later registration.
		s.dropSubjectState(subj)
		return http.StatusOK, nil
	}
	att, _ := s.attachment(subj)
	prev, _ := s.reg.Get(subj)
	if err := s.OnUnloaded(subj, att); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("enforcement removal: %w", err)
	}
	s.reg.Unregister(subj)
	log.Infof("subject %s: REMOVE_BINDING complete (was %s)", subj, prev)
	return http.StatusOK, nil
}
