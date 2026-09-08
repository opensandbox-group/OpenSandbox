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

package subject

import (
	"net/netip"
	"sync"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// entry is the per-subject state slice: identity key, fencing, lifecycle
// state, and policy. The policy lives here (not in the kernel/DNS layer) so
// the registry is the single source for pushes, recovery, and fencing resets.
type entry struct {
	subject Subject
	key     SubjectKey
	fence   Fencing
	state   State
	user    *policy.NetworkPolicy
	// effective is user policy merged with the always-deny/allow overlay,
	// recomputed on every update. Used by DNS dispatch and nft applies.
	effective *policy.NetworkPolicy
}

// MemoryRegistry is the in-memory subject registry implementation. The
// dispatch hot path is two map lookups (source IP, netns path) under a read
// lock; every mutation is serialized by a single mutex, which is fine at the
// target density (64 subjects/Pod, point-to-point pushes).
//
// Concurrency contract between deny-first enforcement and policy apply:
// RegisterAndEnforce runs the deny-first install under the same write lock
// that ApplyPolicy uses to flip the state to active, and it skips the install
// when the subject is already active. A policy push can therefore never be
// clobbered by a later (retried) deny-first install, and a deny-first install
// is always in place before ApplyPolicy can observe the subject.
type MemoryRegistry struct {
	mu          sync.RWMutex
	bySubject   map[Subject]*entry
	byIP        map[netip.Addr]Subject
	byNetns     map[string]Subject
	alwaysDeny  []policy.EgressRule
	alwaysAllow []policy.EgressRule
}

// NewRegistry returns an empty registry. alwaysDeny/alwaysAllow are the
// operator overlay files, merged into every subject's effective policy.
func NewRegistry(alwaysDeny, alwaysAllow []policy.EgressRule) *MemoryRegistry {
	return &MemoryRegistry{
		bySubject:   make(map[Subject]*entry),
		byIP:        make(map[netip.Addr]Subject),
		byNetns:     make(map[string]Subject),
		alwaysDeny:  append([]policy.EgressRule(nil), alwaysDeny...),
		alwaysAllow: append([]policy.EgressRule(nil), alwaysAllow...),
	}
}

var _ Registry = (*MemoryRegistry)(nil)

// Register observes a slot for the subject without running platform hooks.
// See RegisterAndEnforce for the enforced variant.
func (r *MemoryRegistry) Register(s Subject, key SubjectKey, fence Fencing) State {
	state, _ := r.RegisterAndEnforce(s, key, fence, nil)
	return state
}

// RegisterAndEnforce observes a slot for the subject and, when the subject is
// not already active, runs enforce (the deny-first install) while holding the
// registry write lock. enforce must be idempotent; on failure the subject
// stays denying (fail-closed) and the caller retries.
//
// Returned state is StateDenying for a fresh/rebound subject whose enforce
// succeeded, StateActive when the subject was already active under the same
// fencing (deny-first skipped: a policy is already enforced), and
// StateAbsent when enforce is nil and the subject could not be registered.
func (r *MemoryRegistry) RegisterAndEnforce(s Subject, key SubjectKey, fence Fencing, enforce func() error) (State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.bySubject[s]; ok {
		if e.fence.Matches(fence) {
			if !keyEquals(e.key, key) {
				r.dropIndex(e)
				e.key = key
				r.addIndex(e)
			}
			if e.state == StateActive {
				return StateActive, nil
			}
		} else {
			// Rebound: discard all policy state, re-enter deny-first. A reset
			// can never carry old policy into a new sandbox.
			r.dropIndex(e)
			delete(r.bySubject, s)
		}
	}
	e := newEntry(s, key, fence)
	r.bySubject[s] = e
	r.addIndex(e)
	if enforce != nil {
		if err := enforce(); err != nil {
			return StateDenying, err
		}
	}
	return StateDenying, nil
}

func keyEquals(a, b SubjectKey) bool {
	return a.NetNSPath == b.NetNSPath && a.SourceIP == b.SourceIP &&
		a.UID == b.UID && a.Cgroup == b.Cgroup
}

func newEntry(s Subject, key SubjectKey, fence Fencing) *entry {
	return &entry{subject: s, key: key, fence: fence, state: StateDenying}
}

// addIndex registers the dispatch indexes for an entry.
func (r *MemoryRegistry) addIndex(e *entry) {
	if e.key.SourceIP.IsValid() {
		r.byIP[e.key.SourceIP] = e.subject
	}
	if e.key.NetNSPath != "" {
		r.byNetns[e.key.NetNSPath] = e.subject
	}
}

// dropIndex removes the dispatch indexes for an entry (keeps bySubject).
func (r *MemoryRegistry) dropIndex(e *entry) {
	if e.key.SourceIP.IsValid() {
		delete(r.byIP, e.key.SourceIP)
	}
	if e.key.NetNSPath != "" {
		delete(r.byNetns, e.key.NetNSPath)
	}
}

func (r *MemoryRegistry) Get(s Subject) (State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.bySubject[s]
	if !ok {
		return StateAbsent, false
	}
	return e.state, true
}

// Fence returns the fencing recorded at registration. Used to fence terminal
// cleanup (REMOVE_BINDING): a stale removal for a previous instance of the
// same UID must not unload the current subject.
func (r *MemoryRegistry) Fence(s Subject) (Fencing, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.bySubject[s]
	if !ok {
		return Fencing{}, false
	}
	return e.fence, true
}

func (r *MemoryRegistry) List() []Subject {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Subject, 0, len(r.bySubject))
	for s := range r.bySubject {
		out = append(out, s)
	}
	return out
}

// Resolve dispatches a hot-path event to its subject. The source IP is the
// primary key for fast-sandbox; the netns path is the defense-in-depth key.
func (r *MemoryRegistry) Resolve(key SubjectKey) (Subject, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if key.SourceIP.IsValid() {
		if s, ok := r.byIP[key.SourceIP]; ok {
			return s, true
		}
	}
	if key.NetNSPath != "" {
		if s, ok := r.byNetns[key.NetNSPath]; ok {
			return s, true
		}
	}
	return "", false
}

// ApplyPolicy stores the user policy and activates the subject. The subject
// must have an observed slot; otherwise ErrUnknownSubject is returned and the
// caller caches the push as pending.
func (r *MemoryRegistry) ApplyPolicy(s Subject, pol *policy.NetworkPolicy) error {
	if pol == nil {
		pol = policy.DefaultDenyPolicy()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.bySubject[s]
	if !ok {
		return ErrUnknownSubject
	}
	e.user = pol
	e.effective = policy.MergeAlwaysOverlay(pol, r.alwaysDeny, r.alwaysAllow)
	if e.state != StateActive {
		e.state = StateActive
	}
	return nil
}

// EffectiveOf merges the always rules into pol WITHOUT committing it to the
// subject. Used to apply the nft transaction before the registry state
// changes, so a failed kernel apply leaves DNS/GET on the previous policy.
func (r *MemoryRegistry) EffectiveOf(pol *policy.NetworkPolicy) *policy.NetworkPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pol == nil {
		pol = policy.DefaultDenyPolicy()
	}
	return policy.MergeAlwaysOverlay(pol, r.alwaysDeny, r.alwaysAllow)
}

// UnsetPolicy removes the subject's policy and returns it to deny-first
// (SET_BINDING with a null input: the binding was removed from a still-live
// sandbox, so the sandbox must be fully blocked again). The kernel layer must
// be reset to deny-first BEFORE this call so the transition stays fail-closed
// (nft commits first, registry follows). Returns ErrUnknownSubject when the
// subject has no observed binding.
func (r *MemoryRegistry) UnsetPolicy(s Subject) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.bySubject[s]
	if !ok {
		return ErrUnknownSubject
	}
	e.user = nil
	e.effective = nil
	e.state = StateDenying
	return nil
}

func (r *MemoryRegistry) UserPolicy(s Subject) *policy.NetworkPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.bySubject[s]
	if !ok {
		return nil
	}
	return e.user
}

func (r *MemoryRegistry) EffectivePolicy(s Subject) *policy.NetworkPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.bySubject[s]
	if !ok {
		return nil
	}
	return e.effective
}

func (r *MemoryRegistry) SetAlwaysRules(alwaysDeny, alwaysAllow []policy.EgressRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alwaysDeny = append([]policy.EgressRule(nil), alwaysDeny...)
	r.alwaysAllow = append([]policy.EgressRule(nil), alwaysAllow...)
	for _, e := range r.bySubject {
		if e.user != nil {
			e.effective = policy.MergeAlwaysOverlay(e.user, r.alwaysDeny, r.alwaysAllow)
		}
	}
}

func (r *MemoryRegistry) Unregister(s Subject) State {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.bySubject[s]
	if !ok {
		return StateAbsent
	}
	r.dropIndex(e)
	delete(r.bySubject, s)
	return e.state
}
