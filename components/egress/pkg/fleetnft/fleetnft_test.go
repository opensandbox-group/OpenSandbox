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

package fleetnft

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// fakeRunner records every script and optionally fails.
type fakeRunner struct {
	mu      sync.Mutex
	scripts []string
	fail    func(script string) error
}

func (r *fakeRunner) Run(_ context.Context, script string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scripts = append(r.scripts, script)
	if r.fail != nil {
		return nil, r.fail(script)
	}
	return nil, nil
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.scripts)
}

func (r *fakeRunner) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.scripts) == 0 {
		return ""
	}
	return r.scripts[len(r.scripts)-1]
}

func testSlot(uid string, ip string) slotsource.Slot {
	return slotsource.Slot{
		ID:            "slot-" + uid,
		Phase:         slotsource.PhaseBound,
		Owner:         slotsource.Owner{SandboxUID: uid, InstanceGeneration: 1, AssignmentAttempt: 1},
		IP:            netip.MustParseAddr(ip),
		HostNetnsPath: "/var/run/netns/ns-" + uid,
		HostVeth:      "veth" + uid,
		Gateway:       netip.MustParseAddr("10.0.0.1"),
		PrivateCIDR:   netip.MustParsePrefix("10.0.0.0/24"),
		DNSPath:       "/run/fast-sandbox/network/dns/" + uid,
	}
}

func TestDenyFirstInstallFailClosedShape(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	script := runner.last()

	// master chain is fail-closed (policy drop) and installed once
	require.Contains(t, script, "add chain inet opensandbox-fleet dispatch { type filter hook forward priority 0; policy drop; }")
	require.Contains(t, script, "delete table inet opensandbox-fleet")
	// dispatch rule binds source IP + host veth (defense in depth)
	require.Contains(t, script, "ip saddr 10.0.0.5 iifname \"vethu-1\" jump")
	require.Contains(t, script, `add rule inet opensandbox-fleet dispatch ip saddr 10.0.0.5 iifname "vethu-1" jump subj_s_u_1`)
	// subject chain: empty static sets, drop policy (deny-first)
	require.Contains(t, script, "subj_s_u_1")
	// no allow elements exist yet
	require.NotContains(t, script, "add element inet opensandbox-fleet subj_s_u_1_allow")

	// second subject: table header must NOT be re-created (would kill subject 1)
	s2 := subject.FromSandboxUID("u-2")
	require.NoError(t, a.ApplyDenyFirst(ctx, s2, testSlot("u-2", "10.0.0.6")))
	script2 := runner.last()
	assert.NotContains(t, script2, "delete table inet opensandbox-fleet", "table must not be recreated")
	require.Contains(t, script2, `add rule inet opensandbox-fleet dispatch ip saddr 10.0.0.6 iifname "vethu-2" jump subj_s_u_2`)
	require.Equal(t, 2, runner.count())
}

func TestPolicySwapAtomic(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"10.0.0.0/24"},{"action":"deny","target":"1.2.3.4"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	script := runner.last()
	// swap = flush chain + flush static sets + re-add elements/rules, ONE transaction
	require.Contains(t, script, "flush chain inet opensandbox-fleet subj_s_u_1")
	require.Contains(t, script, "flush set inet opensandbox-fleet subj_s_u_1_allow_v4")
	require.Contains(t, script, "add element inet opensandbox-fleet subj_s_u_1_allow_v4 { 10.0.0.0/24 }")
	require.Contains(t, script, "add element inet opensandbox-fleet subj_s_u_1_deny_v4 { 1.2.3.4 }")
	// dynamic sets survive the swap (never deleted)
	assert.NotContains(t, script, "flush set inet opensandbox-fleet subj_s_u_1_dyn")
	require.Equal(t, 1, runner.count(), "policy swap must be a single transaction")
}

func TestPolicySwapFailureKeepsState(t *testing.T) {
	var swapAttempts atomic.Int32
	runner := &fakeRunner{fail: func(script string) error {
		if strings.Contains(script, "flush chain") && swapAttempts.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny"}`)
	require.NoError(t, err)
	require.Error(t, a.ApplyPolicy(ctx, s, pol))

	// state unchanged: a retry must produce the same swap script (idempotent)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	require.Equal(t, int32(2), swapAttempts.Load(), "first swap fails, retry succeeds")
}

func TestUnknownSubjectRejected(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	ctx := context.Background()
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny"}`)
	require.NoError(t, err)
	require.ErrorIs(t, a.ApplyPolicy(ctx, subject.FromSandboxUID("ghost"), pol), ErrUnknownSubject)
	require.ErrorIs(t, a.AddResolvedIPs(ctx, subject.FromSandboxUID("ghost"), []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.2.3.4"), TTL: time.Minute}}), ErrUnknownSubject)
}

func TestAddResolvedIPsClampsTTL(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{
		{Addr: netip.MustParseAddr("1.2.3.4"), TTL: 30 * time.Second},   // clamps up to 90s (min 60 + slack)
		{Addr: netip.MustParseAddr("2001:db8::1"), TTL: 24 * time.Hour}, // clamps down to 360s
	}))
	script := runner.last()
	require.Contains(t, script, "1.2.3.4 timeout 90s")
	require.Contains(t, script, "2001:db8::1 timeout 360s")
}

func TestRemoveRebuildsTable(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	ctx := context.Background()
	s1 := subject.FromSandboxUID("u-1")
	s2 := subject.FromSandboxUID("u-2")
	require.NoError(t, a.ApplyDenyFirst(ctx, s1, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.ApplyDenyFirst(ctx, s2, testSlot("u-2", "10.0.0.6")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s2, pol))

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	// nft deletes rules only by handle and verdict maps cannot jump to
	// chains, so Remove rebuilds the whole table: header + remaining subjects
	// (subject 2 keeps its full policy, subject 1 is gone).
	require.NoError(t, a.Remove(ctx, s1))
	script := runner.last()
	require.Contains(t, script, "delete table inet opensandbox-fleet")
	require.Contains(t, script, "8.8.8.8")
	assert.NotContains(t, script, "subj_s_u_1", "removed subject must not reappear in the rebuild")
	assert.NotContains(t, script, "10.0.0.5", "removed subject's dispatch must not reappear")

	// last subject removed: swap in the empty master drop chain (fail closed)
	require.NoError(t, a.Remove(ctx, s2))
	script = runner.last()
	require.Contains(t, script, "add chain inet opensandbox-fleet dispatch { type filter hook forward priority 0; policy drop; }")
	assert.NotContains(t, script, "subj_s_u_2", "no subjects may remain after removing the last one")

	// last subject removed: whole table deleted
	require.NoError(t, a.Remove(ctx, s2))
	script = runner.last()
	require.Contains(t, script, "delete table inet opensandbox-fleet")
}

func TestDenyFirstResetsOnReRegistration(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}}))

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	// rebind: controller re-observes the same subject -> force reset
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	script := runner.last()
	require.Contains(t, script, "flush chain inet opensandbox-fleet subj_s_u_1")
	require.Contains(t, script, "flush set inet opensandbox-fleet subj_s_u_1_dyn_v4", "DNS leases must be wiped on rebind")
	require.Contains(t, script, `add rule inet opensandbox-fleet dispatch ip saddr 10.0.0.5 iifname "vethu-1" jump subj_s_u_1`, "dispatch re-added")
	assert.NotContains(t, script, "8.8.8.8", "old policy must not survive a rebind")
	assert.NotContains(t, script, "1.1.1.1", "old DNS lease must not survive a rebind")
	assert.NotContains(t, script, "delete table inet opensandbox-fleet", "reset must not touch other subjects")

	// the applier's in-memory state is deny-first again: removal deletes the
	// last subject, so the whole table goes
	require.NoError(t, a.Remove(ctx, s))
	script = runner.last()
	require.Contains(t, script, "delete table inet opensandbox-fleet")
}

func TestApplyResetKeepsEmptyMasterDropChain(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	ctx := context.Background()
	s := subject.FromSandboxUID("u-1")
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.ApplyReset(ctx))

	// Reset swaps in an EMPTY master drop chain — the fail-closed guarantee
	// must not have a window where the drop hook is gone.
	script := runner.last()
	require.Contains(t, script, "delete table inet opensandbox-fleet")
	require.Contains(t, script, "add chain inet opensandbox-fleet dispatch { type filter hook forward priority 0; policy drop; }")
	assert.NotContains(t, script, "subj_s_u_1", "reset must not carry subjects")
	assert.NotContains(t, script, "10.0.0.5", "reset must not carry dispatch rules")

	// after reset the table already exists: re-registration adds only the
	// subject fragment (no delete-table header, no dispatch chain rebuild)
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	assert.NotContains(t, runner.last(), "delete table", "table must not be recreated after reset")
	assert.NotContains(t, runner.last(), "add chain inet opensandbox-fleet dispatch", "dispatch chain already exists after reset")
	require.Contains(t, runner.last(), "add chain inet opensandbox-fleet subj_s_u_1")
}

func TestApplyDispatchUpdate(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	ctx := context.Background()
	s := subject.FromSandboxUID("u-1")
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	// slot updated with a new veth: only the dispatch rule is appended, the
	// subject's policy content is untouched
	require.NoError(t, a.ApplyDispatchUpdate(ctx, s, testSlot("u-1", "10.0.0.5")))
	script := runner.last()
	require.Contains(t, script, `add rule inet opensandbox-fleet dispatch ip saddr 10.0.0.5 iifname "vethu-1" jump subj_s_u_1`)
	assert.NotContains(t, script, "flush", "dispatch update must not touch the subject chain")
	assert.NotContains(t, script, "subj_s_u_1 ip daddr", "dispatch update must not re-add policy rules")

	// unknown subject rejected
	require.ErrorIs(t, a.ApplyDispatchUpdate(ctx, subject.FromSandboxUID("ghost"), testSlot("g", "10.0.0.9")), ErrUnknownSubject)
}

func TestSanitize(t *testing.T) {
	assert.Equal(t, "subj_s_abc_def", subjectChain(subject.Subject("s-abc:def")))
	assert.Equal(t, "subj_s_u_1", subjectChain(subject.FromSandboxUID("u-1")))
}

func TestWriteDispatchRuleV6(t *testing.T) {
	var b strings.Builder
	writeDispatchRule(&b, subject.FromSandboxUID("u-1"), testSlot("u-1", "fd00::5"), 0)
	require.Contains(t, b.String(), `add rule inet opensandbox-fleet dispatch ip6 saddr fd00::5 iifname "vethu-1" jump subj_s_u_1`)
}

// TestIifnameBindingInDispatchRule: the host-veth binding lives in the
// dispatch rule (defense in depth against UDP spoofing) and survives policy
// swaps (the swap never touches the master chain).
func TestIifnameBindingInDispatchRule(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Contains(t, runner.last(), `add rule inet opensandbox-fleet dispatch ip saddr 10.0.0.5 iifname "vethu-1" jump subj_s_u_1`)

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	require.NotContains(t, runner.last(), "add rule inet opensandbox-fleet dispatch", "swap must not duplicate the dispatch rule")

	// rebind re-adds the dispatch rule for the (possibly changed) slot
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Contains(t, runner.last(), `iifname "vethu-1" jump subj_s_u_1`)
}

// TestApplyDenyFirstMissingTableFallback: the first install on a fresh table
// fails on `delete table` (table already gone via ApplyReset); the applier
// must retry without the delete line instead of failing forever.
func TestApplyDenyFirstMissingTableFallback(t *testing.T) {
	var attempts atomic.Int32
	runner := &fakeRunner{fail: func(script string) error {
		if strings.Contains(script, "delete table inet opensandbox-fleet") && attempts.Add(1) == 1 {
			return fmt.Errorf("nft apply failed: No such file or directory; did you mean table")
		}
		return nil
	}}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Equal(t, int32(1), attempts.Load(), "one failed attempt with delete-table line, then fallback")
	assert.NotContains(t, runner.last(), "delete table", "fallback script must not contain the delete-table line")
	require.Contains(t, runner.last(), "add table inet opensandbox-fleet")
}

// TestOverlappingIntervalsNormalized: an always-deny host inside a policy
// deny CIDR must not reach nft as "conflicting intervals specified"; the
// strict subnet is dropped before writing.
func TestOverlappingIntervalsNormalized(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	// deny.always 10.99.0.9 + policy deny 10.99.0.0/24 overlap
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"deny","target":"10.99.0.0/24"},{"action":"deny","target":"10.99.0.9"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	script := runner.last()
	require.Contains(t, script, "add element inet opensandbox-fleet subj_s_u_1_deny_v4 { 10.99.0.0/24 }")
	assert.NotContains(t, script, "10.99.0.9", "strict subnet inside a CIDR must be normalized away")
}

func TestDoHBlockRulesWithBlocklist(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		BlockDoH443:    true,
		DoHBlocklistV4: []string{"10.99.0.9", "10.99.0.0/24"},
		DoHBlocklistV6: []string{"2001:db8::/32"},
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	// global interval sets + per-family drop rules in the master chain
	require.Contains(t, script, "add set inet opensandbox-fleet doh_block_v4 { type ipv4_addr; flags interval; }")
	require.Contains(t, script, "add set inet opensandbox-fleet doh_block_v6 { type ipv6_addr; flags interval; }")
	require.Contains(t, script, "add rule inet opensandbox-fleet dispatch ip daddr @doh_block_v4 tcp dport 443 drop")
	require.Contains(t, script, "add rule inet opensandbox-fleet dispatch ip6 daddr @doh_block_v6 tcp dport 443 drop")
	// overlapping 10.99.0.9 inside 10.99.0.0/24 is normalized away
	require.Contains(t, script, "add element inet opensandbox-fleet doh_block_v4 { 10.99.0.0/24 }")
	assert.NotContains(t, script, "10.99.0.9", "strict subnet inside a doh blocklist CIDR must be normalized away")
	// blocklist mode is NOT strict: no bare 443 drop
	assert.NotContains(t, script, "add rule inet opensandbox-fleet dispatch tcp dport 443 drop")
}

func TestDoHStrictModeDropsAll443(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{BlockDoH443: true})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	require.Contains(t, script, "add rule inet opensandbox-fleet dispatch tcp dport 443 drop")
	assert.NotContains(t, script, "doh_block", "strict mode has no blocklist sets")
}

func TestDoHDisabledByDefault(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	assert.NotContains(t, script, "doh_block", "DoH-443 must be off unless enabled")
	assert.NotContains(t, script, "dport 443", "DoH-443 must be off unless enabled")
}

// TestDoHRulesSurviveRebuild: the DoH rules are part of the table header, so
// every rebuild (last-subject removal, startup reset) must keep them.
func TestDoHRulesSurviveRebuild(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{BlockDoH443: true, DoHBlocklistV4: []string{"10.99.0.2"}})
	ctx := context.Background()
	s := subject.FromSandboxUID("u-1")
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.Remove(ctx, s))
	require.Contains(t, runner.last(), "add set inet opensandbox-fleet doh_block_v4", "empty-table swap must keep DoH rules")
	require.NoError(t, a.ApplyReset(ctx))
	require.Contains(t, runner.last(), "add rule inet opensandbox-fleet dispatch ip daddr @doh_block_v4 tcp dport 443 drop", "reset must keep DoH rules")
}

// TestInputChainInstalledWithMITM: the Pod-netns INPUT enforcement chain is
// the authoritative layer for intercepted (DNATed) traffic — the forward
// hook never sees it. Deny-first must install the chain, the per-subject
// dispatch (ct status dnat on the mitm port) and the ct-original verdicts.
func TestInputChainInstalledWithMITM(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{MitmRedirectPort: 18081})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	require.Contains(t, script, "add chain inet opensandbox-fleet input { type filter hook input priority 0; policy accept; }")
	require.Contains(t, script, "add chain inet opensandbox-fleet subj_s_u_1_in")
	require.Contains(t, script, `add rule inet opensandbox-fleet input ip saddr 10.0.0.5 iifname "vethu-1" tcp dport 18081 ct status dnat jump subj_s_u_1_in`)
	// a direct (non-DNATed) connection to the mitm port must be dropped —
	// default-allow sandboxes must not bypass the transparent interception
	require.Contains(t, script, `add rule inet opensandbox-fleet input ip saddr 10.0.0.5 iifname "vethu-1" ip daddr 10.0.0.1 tcp dport 18081 drop`)
	// verdicts match the conntrack ORIGINAL destination (the DNATed dst is
	// the local mitm port)
	require.Contains(t, script, "add rule inet opensandbox-fleet subj_s_u_1_in ct original ip daddr @subj_s_u_1_deny_v4 drop")
	require.Contains(t, script, "add rule inet opensandbox-fleet subj_s_u_1_in ct original ip daddr @subj_s_u_1_allow_v4 accept")
	require.Contains(t, script, "add rule inet opensandbox-fleet subj_s_u_1_in drop", "deny-first default in the input chain")
}

// TestInputChainAbsentWithoutMITM: no MITM, no input chain — the forward
// hook is the only enforcement layer and Pod traffic is untouched.
func TestInputChainAbsentWithoutMITM(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	assert.NotContains(t, runner.last(), "hook input", "no MITM: input chain must not be installed")
	assert.NotContains(t, runner.last(), "subj_s_u_1_in", "no MITM: per-subject input chain must not exist")
}

// TestInputChainDoHBlocklist: DoH-443 blocking on the input chain matches
// the ORIGINAL port/destination (the DNATed dst is the mitm port).
func TestInputChainDoHBlocklist(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		BlockDoH443:      true,
		DoHBlocklistV4:   []string{"10.99.0.2"},
		MitmRedirectPort: 18081,
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	require.Contains(t, script, "add rule inet opensandbox-fleet input ct status dnat ct original ip daddr @doh_block_v4 ct original proto-dst 443 drop")
}

// TestInputChainPolicySwap: a policy swap rewrites the input-chain verdicts
// with the new sets (and keeps the dispatch rule untouched).
func TestInputChainPolicySwap(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{MitmRedirectPort: 18081})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	script := runner.last()
	require.Contains(t, script, "flush chain inet opensandbox-fleet subj_s_u_1_in")
	require.Contains(t, script, "add rule inet opensandbox-fleet subj_s_u_1_in ct original ip daddr @subj_s_u_1_allow_v4 accept")
	assert.NotContains(t, script, "add rule inet opensandbox-fleet input ip saddr", "swap must not duplicate the input dispatch rule")
}
