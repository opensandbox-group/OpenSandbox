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

package subject

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

func TestRegistryLifecycle(t *testing.T) {
	reg := NewRegistry(nil, nil)
	s := FromSandboxUID("uid-1")
	key := SubjectKey{NetNSPath: "/var/run/netns/ns1", SourceIP: netip.MustParseAddr("10.0.0.5")}
	fence := Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}

	// absent -> denying on first observation
	state, err := reg.RegisterAndEnforce(s, key, fence, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, StateDenying, state)

	// unknown subject policy apply is rejected (pending-push signal)
	err = reg.ApplyPolicy(Subject("s-other"), nil)
	require.ErrorIs(t, err, ErrUnknownSubject)

	// apply policy -> active; effective policy carries default deny
	pol, err := parsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(s, pol))
	st, ok := reg.Get(s)
	require.True(t, ok)
	assert.Equal(t, StateActive, st)
	assert.Equal(t, "deny", reg.EffectivePolicy(s).DefaultAction)

	// same fencing re-register keeps active and does not re-run enforce
	enforceRan := false
	state, err = reg.RegisterAndEnforce(s, key, fence, func() error {
		enforceRan = true
		return nil
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateActive, state)
	assert.False(t, enforceRan, "deny-first must be skipped for active subject")

	// dispatch hot path
	got, ok := reg.Resolve(SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")})
	require.True(t, ok)
	assert.Equal(t, s, got)
	got, ok = reg.Resolve(SubjectKey{NetNSPath: "/var/run/netns/ns1"})
	require.True(t, ok)
	assert.Equal(t, s, got)

	// unload -> absent; dispatch no longer resolves
	assert.Equal(t, StateActive, reg.Unregister(s))
	_, ok = reg.Resolve(key)
	assert.False(t, ok)
}

func TestRegistryFencingRebindDiscardsPolicy(t *testing.T) {
	reg := NewRegistry(nil, nil)
	s := FromSandboxUID("uid-1")
	key := SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}
	fence1 := Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}
	fence2 := Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"}

	reg.Register(s, key, fence1)
	pol, err := parsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(s, pol))
	require.Equal(t, StateActive, mustState(reg.Get(s)))

	// rebind with a new generation: state resets to denying, policy discarded
	state := reg.Register(s, key, fence2)
	assert.Equal(t, StateDenying, state)
	assert.Nil(t, reg.EffectivePolicy(s), "policy must not survive a rebind")
}

// TestRegistryTakeoverOnSlotReuse: a registration claiming a held dispatch
// key tears the stale owner down before the successor is installed.
func TestRegistryTakeoverOnSlotReuse(t *testing.T) {
	reg := NewRegistry(nil, nil)
	ip := netip.MustParseAddr("10.0.0.5")
	key := SubjectKey{SourceIP: ip, NetNSPath: "/var/run/netns/ns1"}
	stale := FromSandboxUID("stale")
	fence := Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}

	state, err := reg.RegisterAndEnforce(stale, key, fence, nil, nil)
	require.NoError(t, err)
	require.Equal(t, StateDenying, state)
	pol, err := parsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(stale, pol))
	require.Equal(t, StateActive, mustState(reg.Get(stale)))

	successor := FromSandboxUID("successor")
	var takenOver []Subject
	enforceRan := false
	enforce := func() error {
		require.Equal(t, []Subject{stale}, takenOver, "teardown must precede the deny-first install")
		enforceRan = true
		return nil
	}
	state, err = reg.RegisterAndEnforce(successor, key, Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"}, enforce,
		func(stale Subject) error {
			takenOver = append(takenOver, stale)
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, StateDenying, state)
	assert.True(t, enforceRan)
	assert.Equal(t, []Subject{stale}, takenOver, "the stale subject must be handed to the teardown hook exactly once")

	// the stale subject is gone; the successor owns the dispatch key
	_, ok := reg.Get(stale)
	assert.False(t, ok, "the stale subject must be evicted")
	got, ok := reg.Resolve(SubjectKey{SourceIP: ip})
	require.True(t, ok)
	assert.Equal(t, successor, got)
	got, ok = reg.Resolve(SubjectKey{NetNSPath: "/var/run/netns/ns1"})
	require.True(t, ok)
	assert.Equal(t, successor, got)

	// a late REMOVE_BINDING for the stale subject is a fenced no-op
	assert.Equal(t, StateAbsent, reg.Unregister(stale))
	got, ok = reg.Resolve(SubjectKey{SourceIP: ip})
	require.True(t, ok)
	assert.Equal(t, successor, got, "the successor must be untouched by the stale unload")
}

// TestRegistryTakeoverDuplicateSetBindingIdempotent: re-claiming your own key
// is an idempotent no-op, never a takeover.
func TestRegistryTakeoverDuplicateSetBindingIdempotent(t *testing.T) {
	reg := NewRegistry(nil, nil)
	s := FromSandboxUID("uid-1")
	key := SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}
	fence := Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}

	require.Equal(t, StateDenying, reg.Register(s, key, fence))
	pol, err := parsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(s, pol))

	taken := false
	state, err := reg.RegisterAndEnforce(s, key, fence, nil, func(Subject) error {
		taken = true
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, StateActive, state)
	assert.False(t, taken, "re-claiming your own key is not a takeover")
}

// TestRegistryTakeoverCallbackFailureLeavesRegistryUntouched: a teardown
// failure fails the registration; the stale subject keeps the slot.
func TestRegistryTakeoverCallbackFailureLeavesRegistryUntouched(t *testing.T) {
	reg := NewRegistry(nil, nil)
	ip := netip.MustParseAddr("10.0.0.5")
	key := SubjectKey{SourceIP: ip}
	stale := FromSandboxUID("stale")
	require.Equal(t, StateDenying, reg.Register(stale, key, Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}))

	enforceRan := false
	_, err := reg.RegisterAndEnforce(FromSandboxUID("successor"), key, Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"},
		func() error { enforceRan = true; return nil },
		func(Subject) error { return errors.New("nft removal failed") })
	require.Error(t, err)
	assert.False(t, enforceRan, "a failed takeover must not install the successor")

	// fail-closed: the stale subject keeps the slot
	assert.Equal(t, StateDenying, mustState(reg.Get(stale)))
	got, ok := reg.Resolve(SubjectKey{SourceIP: ip})
	require.True(t, ok)
	assert.Equal(t, stale, got)
	_, ok = reg.Get(FromSandboxUID("successor"))
	assert.False(t, ok)
}

// TestRegistryTakeoverNilCallbackRefused: an observe-only registration must
// not evict a live key holder — the kernel enforcement would leak.
func TestRegistryTakeoverNilCallbackRefused(t *testing.T) {
	reg := NewRegistry(nil, nil)
	ip := netip.MustParseAddr("10.0.0.5")
	key := SubjectKey{SourceIP: ip, NetNSPath: "/var/run/netns/ns1"}
	stale := FromSandboxUID("stale")
	require.Equal(t, StateDenying, reg.Register(stale, key, Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}))

	successor := FromSandboxUID("successor")
	assert.Equal(t, StateAbsent, reg.Register(successor, key, Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"}),
		"an observe-only registration must fail against a held key, not evict silently")
	_, err := reg.RegisterAndEnforce(successor, key, Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"}, nil, nil)
	require.Error(t, err)

	// untouched: the stale subject keeps the slot
	require.Equal(t, StateDenying, mustState(reg.Get(stale)))
	got, ok := reg.Resolve(SubjectKey{SourceIP: ip})
	require.True(t, ok)
	assert.Equal(t, stale, got)
	got, ok = reg.Resolve(SubjectKey{NetNSPath: "/var/run/netns/ns1"})
	require.True(t, ok)
	assert.Equal(t, stale, got)
	_, ok = reg.Get(successor)
	assert.False(t, ok)

	// the same-fence key-change path is refused alike
	require.Equal(t, StateDenying, reg.Register(FromSandboxUID("third"), SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.9")},
		Fencing{RuntimeInstanceID: "r-3", AttachmentID: "att-3"}))
	assert.Equal(t, StateAbsent, reg.Register(FromSandboxUID("third"), key, Fencing{RuntimeInstanceID: "r-3", AttachmentID: "att-3"}))
}

// TestRegistryTakeoverMultiEvicteePartialFailureStaysConsistent: on a partial
// teardown failure the torn-down holder is evicted and the failed one stays
// registered + enforced — no subject is left registered without enforcement.
func TestRegistryTakeoverMultiEvicteePartialFailureStaysConsistent(t *testing.T) {
	reg := NewRegistry(nil, nil)
	ip := netip.MustParseAddr("10.0.0.5")
	ipHolder := FromSandboxUID("ip-holder")
	require.Equal(t, StateDenying, reg.Register(ipHolder, SubjectKey{SourceIP: ip}, Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}))
	netnsHolder := FromSandboxUID("netns-holder")
	require.Equal(t, StateDenying, reg.Register(netnsHolder, SubjectKey{NetNSPath: "/var/run/netns/ns2"}, Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"}))

	var tornDown []Subject
	_, err := reg.RegisterAndEnforce(FromSandboxUID("successor"), SubjectKey{SourceIP: ip, NetNSPath: "/var/run/netns/ns2"},
		Fencing{RuntimeInstanceID: "r-3", AttachmentID: "att-3"}, nil,
		func(stale Subject) error {
			tornDown = append(tornDown, stale)
			if stale == netnsHolder {
				return errors.New("nft removal failed")
			}
			return nil
		})
	require.Error(t, err)
	assert.Equal(t, []Subject{ipHolder, netnsHolder}, tornDown, "both holders offered for teardown, in index order")

	// the torn-down holder is evicted; the failed holder stays enforced
	_, ok := reg.Get(ipHolder)
	assert.False(t, ok)
	require.Equal(t, StateDenying, mustState(reg.Get(netnsHolder)))
	got, ok := reg.Resolve(SubjectKey{SourceIP: ip})
	assert.False(t, ok)
	got, ok = reg.Resolve(SubjectKey{NetNSPath: "/var/run/netns/ns2"})
	require.True(t, ok)
	assert.Equal(t, netnsHolder, got)
	_, ok = reg.Get(FromSandboxUID("successor"))
	assert.False(t, ok, "the registration failed whole")

	// the retry converges: only the failed holder remains to take over
	state, err := reg.RegisterAndEnforce(FromSandboxUID("successor"), SubjectKey{SourceIP: ip, NetNSPath: "/var/run/netns/ns2"},
		Fencing{RuntimeInstanceID: "r-3", AttachmentID: "att-3"}, nil,
		func(Subject) error { return nil })
	require.NoError(t, err)
	require.Equal(t, StateDenying, state)
	assert.Equal(t, FromSandboxUID("successor"), mustResolve(reg, SubjectKey{NetNSPath: "/var/run/netns/ns2"}))
}

func mustResolve(reg *MemoryRegistry, key SubjectKey) Subject {
	s, ok := reg.Resolve(key)
	if !ok {
		panic("key unresolved")
	}
	return s
}

// TestRegistryTakeoverEnforceFailureLeavesSuccessorDenying: the takeover
// succeeded but the successor's deny-first install failed — the successor
// stays registered denying (the retry re-enters through the same path) and
// the stale subject stays evicted.
func TestRegistryTakeoverEnforceFailureLeavesSuccessorDenying(t *testing.T) {
	reg := NewRegistry(nil, nil)
	ip := netip.MustParseAddr("10.0.0.5")
	key := SubjectKey{SourceIP: ip}
	stale := FromSandboxUID("stale")
	require.Equal(t, StateDenying, reg.Register(stale, key, Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}))

	successor := FromSandboxUID("successor")
	_, err := reg.RegisterAndEnforce(successor, key, Fencing{RuntimeInstanceID: "r-2", AttachmentID: "att-2"},
		func() error { return errors.New("nft busy") },
		func(Subject) error { return nil })
	require.Error(t, err)

	state, ok := reg.Get(successor)
	require.True(t, ok)
	assert.Equal(t, StateDenying, state, "the successor must be deny-first, never half-active")
	_, ok = reg.Get(stale)
	assert.False(t, ok, "the stale subject stays evicted after a torn-down takeover")
	got, _ := reg.Resolve(SubjectKey{SourceIP: ip})
	assert.Equal(t, successor, got)
}

func mustState(st State, ok bool) State {
	if !ok {
		panic("subject absent")
	}
	return st
}

func TestRegistryResolverDispatch(t *testing.T) {
	reg := NewRegistry(nil, nil)
	ips := []string{"10.0.0.5", "10.0.0.6"}
	subjects := []Subject{FromSandboxUID("a"), FromSandboxUID("b")}
	for i, ip := range ips {
		reg.Register(subjects[i], SubjectKey{SourceIP: netip.MustParseAddr(ip)}, Fencing{RuntimeInstanceID: "r-" + string(subjects[i]), AttachmentID: "att-" + string(subjects[i])})
	}
	got, ok := reg.Resolve(SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.6")})
	require.True(t, ok)
	assert.Equal(t, subjects[1], got)

	// unknown source ip -> no dispatch
	_, ok = reg.Resolve(SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.99")})
	assert.False(t, ok)
}

func TestRegistryAlwaysRulesOverlay(t *testing.T) {
	alwaysDenyRule, err := policy.ParseValidatedEgressRule("deny", "blocked.example")
	require.NoError(t, err)
	reg := NewRegistry([]policy.EgressRule{alwaysDenyRule}, nil)
	s := FromSandboxUID("a")
	reg.Register(s, SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}, Fencing{RuntimeInstanceID: "r-a", AttachmentID: "att-a"})
	pol, err := parsePolicy(`{"defaultAction":"allow"}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(s, pol))

	// effective policy denies what always-deny denies even under default allow
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "deny", eff.Evaluate("blocked.example"))
	assert.Equal(t, "allow", eff.Evaluate("ok.example"))
}

func TestRegistryUnsetPolicyReturnsToDenyFirst(t *testing.T) {
	reg := NewRegistry(nil, nil)
	s := FromSandboxUID("uid-1")
	key := SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}
	fence := Fencing{RuntimeInstanceID: "r-1", AttachmentID: "att-1"}
	reg.Register(s, key, fence)
	pol, err := parsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(s, pol))
	require.Equal(t, StateActive, mustState(reg.Get(s)))

	// Fence reports the registered identity (REMOVE_BINDING fencing)
	gotFence, ok := reg.Fence(s)
	require.True(t, ok)
	assert.Equal(t, fence, gotFence)

	// SET_BINDING(null): policy removed, back to deny-first, DNS denies
	require.NoError(t, reg.UnsetPolicy(s))
	require.Equal(t, StateDenying, mustState(reg.Get(s)))
	assert.Nil(t, reg.EffectivePolicy(s))
	assert.Nil(t, reg.UserPolicy(s))

	// unknown subject
	err = reg.UnsetPolicy(Subject("s-other"))
	require.ErrorIs(t, err, ErrUnknownSubject)

	// a later ApplyPolicy re-activates
	require.NoError(t, reg.ApplyPolicy(s, pol))
	require.Equal(t, StateActive, mustState(reg.Get(s)))
}

func parsePolicy(raw string) (*policy.NetworkPolicy, error) {
	return policy.ParsePolicy(raw)
}
