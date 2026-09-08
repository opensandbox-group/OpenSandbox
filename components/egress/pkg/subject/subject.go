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

// Package subject implements the multi-sandbox egress Subject abstraction: one opaque
// identifier per sandbox owning an isolated slice of policy, credentials, and
// kernel rules, dispatched by platform-provided identity keys.
//
// The package owns the in-process state machine (absent -> denying -> active)
// and the dispatch hot path (identity key -> Subject). It does not touch
// kernel rules, DNS, or HTTP surfaces; those are wired by the caller through
// the lifecycle hooks. Subject lifecycle is driven by the fast-sandbox
// Sandbox Actions Handler protocol (pkg/actionhandler): the Fastlet delivers
// SET_BINDING / LIFECYCLE_HOOK / REMOVE_BINDING and the caller translates
// them into registry transitions.
package subject

import (
	"errors"
	"net/netip"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// Subject is the opaque unit of policy, credential, and rule ownership.
type Subject string

// FromSandboxUID derives the subject for a fast-sandbox sandbox UID.
func FromSandboxUID(sandboxUID string) Subject {
	return Subject("s-" + sandboxUID)
}

// Fencing is the identity fence from the action revision. A change in either
// field means the sandbox was rebound: all prior state for the subject must
// be discarded — a reset can never carry old policy into a new sandbox.
//
// SpecGeneration is deliberately NOT part of the fence: it bumps on every
// spec update (including policy updates), and a policy update must update the
// policy in place, never reset the subject to deny-first.
type Fencing struct {
	RuntimeInstanceID string
	AttachmentID      string
}

// Matches reports whether f and other describe the same sandbox instance.
func (f Fencing) Matches(other Fencing) bool {
	return f.RuntimeInstanceID == other.RuntimeInstanceID &&
		f.AttachmentID == other.AttachmentID
}

// FromRevision derives the identity fence from an action envelope revision.
func FromRevision(env *actionhandler.Envelope) Fencing {
	return Fencing{
		RuntimeInstanceID: env.Revision.RuntimeInstanceID,
		AttachmentID:      env.Revision.AttachmentID,
	}
}

// State is the subject lifecycle state.
type State int

const (
	// StateAbsent: no binding observed for this subject.
	StateAbsent State = iota
	// StateDenying: binding observed, deny-first rules installed, policy not
	// yet applied (or removed). Traffic is fully blocked.
	StateDenying
	// StateActive: policy landed (data-plane-ready), traffic flows per policy.
	StateActive
)

func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateDenying:
		return "denying"
	case StateActive:
		return "active"
	default:
		return "unknown"
	}
}

// SubjectKey is the platform-provided identity material used to dispatch a
// hot-path event (packet, DNS query) to a subject. The registry indexes on
// the key fields the adapter fills in.
type SubjectKey struct {
	NetNSPath string     // fast-sandbox: sandbox netns path (defense in depth)
	SourceIP  netip.Addr // fast-sandbox: dispatch key (ip saddr)
	UID       uint32     // bwrap setpriv
	Cgroup    string     // bwrap userns (future)
}

// ErrUnknownSubject is returned when an operation targets a subject with no
// observed binding. Callers treat it as the signal to cache the push as
// pending.
var ErrUnknownSubject = errors.New("subject not registered")

// Resolver is the hot path: pure lookup, must be cheap and race-free.
type Resolver interface {
	Resolve(key SubjectKey) (Subject, bool)
}

// Registry is the subject state store. All methods are safe for concurrent
// use. RegisterAndEnforce/ApplyPolicy/UnsetPolicy/Unregister drive the state
// machine; Resolve is the dispatch hot path.
type Registry interface {
	Resolver
	// RegisterAndEnforce observes a binding for the subject and runs the
	// deny-first install (via enforce) under the same lock that ApplyPolicy
	// uses, so a policy push can never be clobbered by a retried install.
	// enforce is skipped when the subject is already active. Returns the
	// state after registration.
	RegisterAndEnforce(s Subject, key SubjectKey, fence Fencing, enforce func() error) (State, error)
	// Register is RegisterAndEnforce without platform hooks.
	Register(s Subject, key SubjectKey, fence Fencing) State
	// Get returns the current state.
	Get(s Subject) (State, bool)
	// Fence returns the fencing recorded at registration.
	Fence(s Subject) (Fencing, bool)
	// List returns all subjects with an observed binding.
	List() []Subject
	// ApplyPolicy stores the user policy and moves the subject to active.
	// Returns ErrUnknownSubject when the binding has not been observed.
	ApplyPolicy(s Subject, pol *policy.NetworkPolicy) error
	// UnsetPolicy removes the user policy and returns the subject to
	// deny-first (binding removed from a still-live sandbox). Returns
	// ErrUnknownSubject when the subject has no binding.
	UnsetPolicy(s Subject) error
	// EffectiveOf merges the always rules into pol without committing it
	// (used to apply nft before the registry state changes).
	EffectiveOf(pol *policy.NetworkPolicy) *policy.NetworkPolicy
	// UserPolicy returns the stored user policy (without the always overlay),
	// or nil for unknown subjects.
	UserPolicy(s Subject) *policy.NetworkPolicy
	// EffectivePolicy returns the always-rule merged policy for a subject,
	// or nil for unknown subjects. Nil while the subject is denying.
	EffectivePolicy(s Subject) *policy.NetworkPolicy
	// SetAlwaysRules replaces the always-deny/always-allow overlay used when
	// computing effective policies.
	SetAlwaysRules(alwaysDeny, alwaysAllow []policy.EgressRule)
	// Unregister drops the subject (binding removed). Returns the prior state.
	Unregister(s Subject) State
}

// LifecycleHooks are invoked by the caller at subject transitions driven by
// the action protocol (the fleet control plane translates SET_BINDING,
// LIFECYCLE_HOOK, and REMOVE_BINDING into these calls). The caller installs
// the platform adapters here (deny-first nft rules, gateway DNS redirect,
// pending-push flush). A hook error must be treated as fail-closed: the
// subject stays denying until the hook succeeds.
type LifecycleHooks interface {
	// OnRegistered fires when a binding is observed (SET_BINDING) and the
	// subject entered denying. It must install deny-first enforcement; on
	// failure the caller retries (the action is a failed attempt) and the
	// subject never activates. Runs while the registry holds its write lock
	// (atomic with ApplyPolicy), so it must not call back into registry
	// methods.
	OnRegistered(s Subject, att actionhandler.NetworkAttachment) error
	// OnRegisteredComplete fires once registration (deny-first install)
	// succeeded and the registry lock is released. It is the place for
	// best-effort follow-ups that would deadlock inside OnRegistered, e.g.
	// flushing a cached pending credential push for this subject. specGen is
	// the sandbox spec generation recorded at SET_BINDING, used to drop
	// stale pending pushes (X-Fast-Sandbox-Generation fencing).
	OnRegisteredComplete(s Subject, att actionhandler.NetworkAttachment, specGen uint64)
	// OnUnloaded fires when the binding is removed terminally (REMOVE_BINDING
	// or the sandbox's binding is gone). Enforcement must be removed; att is
	// the last observed network attachment (for gateway refcounts etc.).
	OnUnloaded(s Subject, att actionhandler.NetworkAttachment) error
}
