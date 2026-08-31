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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

func TestRegistryLifecycle(t *testing.T) {
	reg := NewRegistry(nil, nil)
	s := FromSandboxUID("uid-1")
	key := SubjectKey{NetNSPath: "/var/run/netns/ns1", SourceIP: netip.MustParseAddr("10.0.0.5")}
	fence := Fencing{SandboxUID: "uid-1", InstanceGeneration: 1, AssignmentAttempt: 1}

	// absent -> denying on first observation
	state, err := reg.RegisterAndEnforce(s, key, fence, nil)
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
	})
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
	fence1 := Fencing{SandboxUID: "uid-1", InstanceGeneration: 1, AssignmentAttempt: 1}
	fence2 := Fencing{SandboxUID: "uid-1", InstanceGeneration: 2, AssignmentAttempt: 1}

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
		reg.Register(subjects[i], SubjectKey{SourceIP: netip.MustParseAddr(ip)}, Fencing{SandboxUID: string(subjects[i])})
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
	reg.Register(s, SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}, Fencing{SandboxUID: "a"})
	pol, err := parsePolicy(`{"defaultAction":"allow"}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(s, pol))

	// effective policy denies what always-deny denies even under default allow
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "deny", eff.Evaluate("blocked.example"))
	assert.Equal(t, "allow", eff.Evaluate("ok.example"))
}

func parsePolicy(raw string) (*policy.NetworkPolicy, error) {
	return policy.ParsePolicy(raw)
}
