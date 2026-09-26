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

package fastsandboxnft

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

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
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

func testSlot(uid string, ip string) actionhandler.NetworkAttachment {
	return actionhandler.NetworkAttachment{
		IP:          netip.MustParseAddr(ip),
		Gateway:     netip.MustParseAddr("10.0.0.1"),
		PrivateCIDR: netip.MustParsePrefix("10.0.0.0/24"),
		HostVeth:    "veth" + uid,
	}
}

func TestDenyFirstInstallFailClosedShape(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	script := runner.last()

	// master chain is fail-closed: ACCEPT policy with an unmarked-drop tail
	// (the forward path never issues an explicit accept — bridge-netfilter
	// semantics), plus the prerouting mark hook chain
	require.Contains(t, script, "add chain inet opensandbox-fast-sandbox dispatch { type filter hook forward priority 0; policy accept; }")
	require.Contains(t, script, "add chain inet opensandbox-fast-sandbox marking { type filter hook prerouting priority 0; }")
	require.Contains(t, script, "delete table inet opensandbox-fast-sandbox")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch meta mark & 0x2 != 0x2 drop")
	// dispatch rule binds source IP + host veth (defense in depth)
	require.Contains(t, script, "ip saddr 10.0.0.5 jump")
	require.Contains(t, script, `add rule inet opensandbox-fast-sandbox dispatch ip saddr 10.0.0.5 jump subj_s_u_1`)
	// the prerouting mark jump reaches the subject's mark chain
	require.Contains(t, script, `add rule inet opensandbox-fast-sandbox marking ip saddr 10.0.0.5 jump mark_s_u_1`)
	// subject chains exist; deny-first = no mark rules, drop-only forward chain
	require.Contains(t, script, "subj_s_u_1")
	require.Contains(t, script, "mark_s_u_1")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1 drop")
	assert.NotContains(t, script, "meta mark set", "deny-first must not mark anything")
	// no allow elements exist yet
	require.NotContains(t, script, "add element inet opensandbox-fast-sandbox subj_s_u_1_allow")

	// second subject: table header must NOT be re-created (would kill subject 1)
	s2 := subject.FromSandboxUID("u-2")
	require.NoError(t, a.ApplyDenyFirst(ctx, s2, testSlot("u-2", "10.0.0.6")))
	script2 := runner.last()
	assert.NotContains(t, script2, "delete table inet opensandbox-fast-sandbox", "table must not be recreated")
	require.Contains(t, script2, `add rule inet opensandbox-fast-sandbox dispatch ip saddr 10.0.0.6 jump subj_s_u_2`)
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
	require.Contains(t, script, "flush chain inet opensandbox-fast-sandbox subj_s_u_1")
	require.Contains(t, script, "flush set inet opensandbox-fast-sandbox subj_s_u_1_allow_v4")
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox subj_s_u_1_allow_v4 { 10.0.0.0/24 }")
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox subj_s_u_1_deny_v4 { 1.2.3.4 }")
	// dynamic sets survive the swap (never deleted)
	assert.NotContains(t, script, "flush set inet opensandbox-fast-sandbox subj_s_u_1_dyn")
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
	require.Contains(t, script, "delete table inet opensandbox-fast-sandbox")
	require.Contains(t, script, "8.8.8.8")
	assert.NotContains(t, script, "subj_s_u_1", "removed subject must not reappear in the rebuild")
	assert.NotContains(t, script, "10.0.0.5", "removed subject's dispatch must not reappear")

	// last subject removed: swap in the empty master drop chain (fail closed)
	require.NoError(t, a.Remove(ctx, s2))
	script = runner.last()
	require.Contains(t, script, "add chain inet opensandbox-fast-sandbox dispatch { type filter hook forward priority 0; policy accept; }")
	assert.NotContains(t, script, "subj_s_u_2", "no subjects may remain after removing the last one")

	// last subject removed: whole table deleted
	require.NoError(t, a.Remove(ctx, s2))
	script = runner.last()
	require.Contains(t, script, "delete table inet opensandbox-fast-sandbox")
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
	require.Contains(t, script, "flush chain inet opensandbox-fast-sandbox subj_s_u_1")
	require.Contains(t, script, "flush set inet opensandbox-fast-sandbox subj_s_u_1_dyn_v4", "DNS leases must be wiped on rebind")
	require.Contains(t, script, `add rule inet opensandbox-fast-sandbox dispatch ip saddr 10.0.0.5 jump subj_s_u_1`, "dispatch re-added")
	assert.NotContains(t, script, "8.8.8.8", "old policy must not survive a rebind")
	assert.NotContains(t, script, "1.1.1.1", "old DNS lease must not survive a rebind")
	assert.NotContains(t, script, "delete table inet opensandbox-fast-sandbox", "reset must not touch other subjects")

	// the applier's in-memory state is deny-first again: removal deletes the
	// last subject, so the whole table goes
	require.NoError(t, a.Remove(ctx, s))
	script = runner.last()
	require.Contains(t, script, "delete table inet opensandbox-fast-sandbox")
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
	require.Contains(t, script, "delete table inet opensandbox-fast-sandbox")
	require.Contains(t, script, "add chain inet opensandbox-fast-sandbox dispatch { type filter hook forward priority 0; policy accept; }")
	assert.NotContains(t, script, "subj_s_u_1", "reset must not carry subjects")
	assert.NotContains(t, script, "10.0.0.5", "reset must not carry dispatch rules")

	// after reset the table already exists: re-registration adds only the
	// subject fragment (no delete-table header, no dispatch chain rebuild)
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	assert.NotContains(t, runner.last(), "delete table", "table must not be recreated after reset")
	assert.NotContains(t, runner.last(), "add chain inet opensandbox-fast-sandbox dispatch", "dispatch chain already exists after reset")
	require.Contains(t, runner.last(), "add chain inet opensandbox-fast-sandbox subj_s_u_1")
}

func TestApplyDenyFirstReRegistersWithNewAttachment(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	ctx := context.Background()
	s := subject.FromSandboxUID("u-1")
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	// rebind with a moved veth/IP: deny-first re-registration resets the
	// subject (flush + new dispatch rule) WITHOUT recreating the table
	att2 := testSlot("u-1", "10.0.0.9")
	att2.HostVeth = "veth-new"
	require.NoError(t, a.ApplyDenyFirst(ctx, s, att2))
	script := runner.last()
	require.Contains(t, script, "flush chain inet opensandbox-fast-sandbox subj_s_u_1")
	require.Contains(t, script, `add rule inet opensandbox-fast-sandbox dispatch ip saddr 10.0.0.9 jump subj_s_u_1`)
	assert.NotContains(t, script, "delete table", "rebind must not recreate the table")

	// unknown subject rejected on deny-first? No: deny-first installs; only
	// policy applies require a prior install.
	require.ErrorIs(t, a.ApplyPolicy(ctx, subject.FromSandboxUID("ghost"), nil), ErrUnknownSubject)
}

func TestSanitize(t *testing.T) {
	assert.Equal(t, "subj_s_abc_def", subjectChain(subject.Subject("s-abc:def")))
	assert.Equal(t, "subj_s_u_1", subjectChain(subject.FromSandboxUID("u-1")))
	assert.Equal(t, "mark_s_u_1", markChainName(subject.FromSandboxUID("u-1")))
}

// TestMarkBasedAllowShapes: the forward path never accepts explicitly — the
// per-subject prerouting mark chain marks allow/dyn members (default-deny) or
// everything (default-allow), and the master chain drops unmarked traffic.
func TestMarkBasedAllowShapes(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	// default-deny policy: allow/dyn marks, deny drops only in the forward
	// chain (no accept verdicts)
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	script := runner.last()
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox mark_s_u_1 ip daddr @subj_s_u_1_allow_v4 meta mark set 0x2")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox mark_s_u_1 ip daddr @subj_s_u_1_dyn_v4 meta mark set 0x2")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1 ip daddr @subj_s_u_1_deny_v4 drop")
	assert.NotContains(t, script, "subj_s_u_1 ip daddr @subj_s_u_1_allow_v4 accept", "forward path must not accept explicitly")
	assert.NotContains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1 accept")
	assert.NotContains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1 drop")

	// default-allow policy: unconditional mark; deny sets still drop
	pol2, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"deny","target":"9.9.9.9"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol2))
	script = runner.last()
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox mark_s_u_1 meta mark set 0x2")
	assert.NotContains(t, script, "ip daddr @subj_s_u_1_allow_v4 meta mark", "default-allow must not need set-based marks")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1 ip daddr @subj_s_u_1_deny_v4 drop")

	// deny-first reset: mark chain flushed, no mark rules, drop-only forward
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	script = runner.last()
	require.Contains(t, script, "flush chain inet opensandbox-fast-sandbox mark_s_u_1")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1 drop")
	assert.NotContains(t, script, "meta mark set", "deny-first must not mark anything")
}

func TestWriteDispatchRuleV6(t *testing.T) {
	var b strings.Builder
	writeDispatchRule(&b, subject.FromSandboxUID("u-1"), testSlot("u-1", "fd00::5"), 0)
	require.Contains(t, b.String(), `add rule inet opensandbox-fast-sandbox dispatch ip6 saddr fd00::5 jump subj_s_u_1`)
	require.Contains(t, b.String(), `add rule inet opensandbox-fast-sandbox marking ip6 saddr fd00::5 jump mark_s_u_1`)
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
	require.Contains(t, runner.last(), `add rule inet opensandbox-fast-sandbox dispatch ip saddr 10.0.0.5 jump subj_s_u_1`)

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	require.NotContains(t, runner.last(), "add rule inet opensandbox-fast-sandbox dispatch", "swap must not duplicate the dispatch rule")

	// rebind re-adds the dispatch rule for the (possibly changed) slot
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Contains(t, runner.last(), `add rule inet opensandbox-fast-sandbox dispatch ip saddr 10.0.0.5 jump subj_s_u_1`)
}

// TestApplyDenyFirstMissingTableFallback: the first install on a fresh table
// fails on `delete table` (table already gone via ApplyReset); the applier
// must retry without the delete line instead of failing forever.
func TestApplyDenyFirstMissingTableFallback(t *testing.T) {
	var attempts atomic.Int32
	runner := &fakeRunner{fail: func(script string) error {
		if strings.Contains(script, "delete table inet opensandbox-fast-sandbox") && attempts.Add(1) == 1 {
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
	require.Contains(t, runner.last(), "add table inet opensandbox-fast-sandbox")
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
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox subj_s_u_1_deny_v4 { 10.99.0.0/24 }")
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
	require.Contains(t, script, "add set inet opensandbox-fast-sandbox doh_block_v4 { type ipv4_addr; flags interval; }")
	require.Contains(t, script, "add set inet opensandbox-fast-sandbox doh_block_v6 { type ipv6_addr; flags interval; }")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch ip daddr @doh_block_v4 tcp dport 443 drop")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch ip6 daddr @doh_block_v6 tcp dport 443 drop")
	// overlapping 10.99.0.9 inside 10.99.0.0/24 is normalized away
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox doh_block_v4 { 10.99.0.0/24 }")
	assert.NotContains(t, script, "10.99.0.9", "strict subnet inside a doh blocklist CIDR must be normalized away")
	// blocklist mode is NOT strict: no bare 443 drop
	assert.NotContains(t, script, "add rule inet opensandbox-fast-sandbox dispatch tcp dport 443 drop")
}

func TestDoHStrictModeDropsAll443(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{BlockDoH443: true})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch tcp dport 443 drop")
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
	require.Contains(t, runner.last(), "add set inet opensandbox-fast-sandbox doh_block_v4", "empty-table swap must keep DoH rules")
	require.NoError(t, a.ApplyReset(ctx))
	require.Contains(t, runner.last(), "add rule inet opensandbox-fast-sandbox dispatch ip daddr @doh_block_v4 tcp dport 443 drop", "reset must keep DoH rules")
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
	require.Contains(t, script, "add chain inet opensandbox-fast-sandbox input { type filter hook input priority 0; policy accept; }")
	require.Contains(t, script, "add chain inet opensandbox-fast-sandbox subj_s_u_1_in")
	require.Contains(t, script, `add rule inet opensandbox-fast-sandbox input ip saddr 10.0.0.5 tcp dport 18081 ct status dnat jump subj_s_u_1_in`)
	// a direct (non-DNATed) connection to the mitm port must be dropped —
	// default-allow sandboxes must not bypass the transparent interception
	require.Contains(t, script, `add rule inet opensandbox-fast-sandbox input ip saddr 10.0.0.5 ip daddr 10.0.0.1 tcp dport 18081 drop`)
	// verdicts match the conntrack ORIGINAL destination (the DNATed dst is
	// the local mitm port)
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1_in ct original ip daddr @subj_s_u_1_deny_v4 drop")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1_in ct original ip daddr @subj_s_u_1_allow_v4 accept")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1_in drop", "deny-first default in the input chain")
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
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox input ct status dnat ct original ip daddr @doh_block_v4 ct original proto-dst 443 drop")
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
	require.Contains(t, script, "flush chain inet opensandbox-fast-sandbox subj_s_u_1_in")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox subj_s_u_1_in ct original ip daddr @subj_s_u_1_allow_v4 accept")
	assert.NotContains(t, script, "add rule inet opensandbox-fast-sandbox input ip saddr", "swap must not duplicate the input dispatch rule")
}

// TestUpstreamProxyForwardDrops: the chained upstream proxy endpoint is
// infrastructure — a sandbox CONNECTing it directly would relay to
// otherwise-denied destinations, so the drop rules sit profile-wide in the
// master dispatch chain, BEFORE the established accept (stale flows from a
// pre-upstream egress generation must not survive a restart) and before any
// per-subject jump.
func TestUpstreamProxyForwardDrops(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		UpstreamProxy: &UpstreamProxyEndpoint{
			Port:        3128,
			LiteralIPs:  []netip.Addr{netip.MustParseAddr("10.1.2.3"), netip.MustParseAddr("2001:db8::1")},
		},
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	require.Contains(t, script, "add set inet opensandbox-fast-sandbox upstream_proxy_v4 { type ipv4_addr; flags timeout; }")
	require.Contains(t, script, "add set inet opensandbox-fast-sandbox upstream_proxy_v6 { type ipv6_addr; flags timeout; }")
	// literal seeds are permanent: no element-level timeout
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.1.2.3 }\n")
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v6 { 2001:db8::1 }\n")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch ip daddr @upstream_proxy_v4 tcp dport 3128 drop")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch ip6 daddr @upstream_proxy_v6 tcp dport 3128 drop")
	// the drop must precede the established accept (cross-restart staleness)
	require.Less(t,
		strings.Index(script, "dispatch ip daddr @upstream_proxy_v4"),
		strings.Index(script, "dispatch ct state established,related accept"),
	)
	// no MITM: no input chain, so no input-path containment either
	assert.NotContains(t, script, "hook input")
	assert.NotContains(t, script, "input ct status dnat ct original ip daddr @upstream_proxy_v4")
}

// TestUpstreamProxyInputDropsWithMITM: intercepted CONNECTs whose ORIGINAL
// destination is the proxy endpoint die in the input chain, before its
// established accept — the forward drop never sees DNATed traffic.
func TestUpstreamProxyInputDropsWithMITM(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		MitmRedirectPort: 18081,
		UpstreamProxy:    &UpstreamProxyEndpoint{Port: 3128},
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	script := runner.last()
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox input ct status dnat ct original ip daddr @upstream_proxy_v4 ct original proto-dst 3128 drop")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox input ct status dnat ct original ip6 daddr @upstream_proxy_v6 ct original proto-dst 3128 drop")
	require.Less(t,
		strings.Index(script, "input ct status dnat ct original ip daddr @upstream_proxy_v4"),
		strings.Index(script, "input ct state established,related accept"),
		"input drop must precede the input established accept",
	)
}

func TestUpstreamProxyDisabledByDefault(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	assert.NotContains(t, runner.last(), "upstream_proxy", "no upstream proxy: no containment rules")

	// AddUpstreamProxyIPs without an endpoint is a no-op (no nft call, no error)
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.2.3.4"), TTL: time.Minute}}))
	require.Equal(t, 1, runner.count())
}

func TestAddUpstreamProxyIPs(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	require.NoError(t, a.ApplyReset(ctx))
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	// TTLs are deliberately ignored: elements are permanent so containment
	// survives egress downtime (expiry is owned by SyncUpstreamProxyIPs).
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{
		{Addr: netip.MustParseAddr("10.9.9.9"), TTL: 30 * time.Second},
		{Addr: netip.MustParseAddr("2001:db8::2"), TTL: 24 * time.Hour},
	}))
	script := runner.last()
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.9.9.9 }\n")
	require.Contains(t, script, "delete element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.9.9.9 }")
	assert.NotContains(t, script, "10.9.9.9 timeout", "DNS-learned elements must be permanent")
	assert.NotContains(t, script, "2001:db8::2 timeout", "DNS-learned elements must be permanent")

	// an empty slice is a no-op
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, nil))
	require.Equal(t, 1, runner.count())
}

// TestUpstreamProxyLearnedIPsSurviveRebuild: table rebuilds (subject removal,
// startup reset) re-seed the DNS-learned drop elements from the in-memory
// mirror, so hostname containment never lapses between the rebuild and the
// next refresh tick.
func TestUpstreamProxyLearnedIPsSurviveRebuild(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128, LiteralIPs: []netip.Addr{netip.MustParseAddr("10.1.2.3")}},
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.9.9.9"), TTL: time.Minute}}))

	// subject removal rebuilds the whole table from in-memory state
	require.NoError(t, a.Remove(ctx, s))
	script := runner.last()
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.1.2.3 }\n", "literal seed must survive")
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.9.9.9 }\n", "learned element must be re-seeded (permanent)")
	require.Contains(t, script, "add rule inet opensandbox-fast-sandbox dispatch ip daddr @upstream_proxy_v4 tcp dport 3128 drop", "drop rule must survive")

	// startup reset likewise
	require.NoError(t, a.ApplyReset(ctx))
	require.Contains(t, runner.last(), "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.9.9.9 }\n")
	require.Contains(t, runner.last(), "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.1.2.3 }\n")
}

// TestSyncUpstreamProxyIPs: the refresh loop's sync is the expiry mechanism
// for the containment — rotation removes stale addresses, and empty or
// unusable answers never prune the sets to nothing (fail-closed retention).
func TestSyncUpstreamProxyIPs(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	require.NoError(t, a.ApplyReset(ctx))
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.9.9.9")}}))

	// rotation: 10.9.9.9 out, 10.8.8.8 in
	require.NoError(t, a.SyncUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.8.8.8")}}))
	script := runner.last()
	require.Contains(t, script, "delete element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.9.9.9 }")
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.8.8.8 }")

	// the mirror follows the rotation: rebuilds drop the stale address
	require.NoError(t, a.ApplyReset(ctx))
	rebuilt := runner.last()
	require.Contains(t, rebuilt, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.8.8.8 }\n")
	assert.NotContains(t, rebuilt, "10.9.9.9")

	// empty or all-unusable answers are errors, never a prune-to-nothing
	require.Error(t, a.SyncUpstreamProxyIPs(ctx, nil))
	require.Error(t, a.SyncUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.Addr{}}}))
	require.NoError(t, a.ApplyReset(ctx))
	require.Contains(t, runner.last(), "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.8.8.8 }\n", "failed sync must keep the mirror")

	// a failed apply must not commit the rotation either
	failing := &fakeRunner{}
	b := NewApplier(failing.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	require.NoError(t, b.ApplyReset(ctx))
	require.NoError(t, b.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.9.9.9")}}))
	failing.fail = func(script string) error {
		if strings.Contains(script, "10.8.8.8") {
			return fmt.Errorf("nft apply failed")
		}
		return nil
	}
	require.Error(t, b.SyncUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.8.8.8")}}))
	require.NoError(t, b.ApplyReset(ctx))
	assert.NotContains(t, failing.last(), "10.8.8.8", "failed rotation must not leak into the rebuild")
	require.Contains(t, failing.last(), "10.9.9.9")

	// no endpoint configured: no-op
	c := NewApplier((&fakeRunner{}).Run)
	require.NoError(t, c.SyncUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.8.8.8")}}))
}

// TestStartUpstreamProxyRefreshSeedFailClosed: the first seed retries with
// bounded backoff and returns an error when the hostname cannot be
// resolved — the caller fails startup instead of serving sandboxes with an
// empty drop set.
func TestStartUpstreamProxyRefreshSeedFailClosed(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	a.upstreamSeedTimeout = 80 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// persistent lookup failure: seed retries, then errors with the cause
	err := a.StartUpstreamProxyRefresh(ctx, "proxy.test", func(context.Context, string) ([]nftables.ResolvedIP, error) {
		return nil, fmt.Errorf("dns down")
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dns down")

	// NXDOMAIN-everywhere is equally fatal for the first seed
	err = a.StartUpstreamProxyRefresh(ctx, "proxy.test", func(context.Context, string) ([]nftables.ResolvedIP, error) {
		return nil, nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no addresses")

	// nothing was seeded: no nft call ever succeeded
	require.Equal(t, 0, runner.count())
}

// TestStartUpstreamProxyRefreshSeeds: a resolvable hostname seeds the drop
// set synchronously (before the caller starts serving) as permanent
// elements.
func TestStartUpstreamProxyRefreshSeeds(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, a.StartUpstreamProxyRefresh(ctx, "proxy.test", func(context.Context, string) ([]nftables.ResolvedIP, error) {
		return []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.9.9.9"), TTL: time.Minute}}, nil
	}))
	script := runner.last()
	require.Contains(t, script, "add element inet opensandbox-fast-sandbox upstream_proxy_v4 { 10.9.9.9 }")
	assert.NotContains(t, script, "10.9.9.9 timeout", "seeded elements must be permanent")
}

// TestAddUpstreamProxyIPsFailureKeepsMirror: a failed apply must not commit
// the mirror (rebuilds stay deterministic against the kernel state).
func TestAddUpstreamProxyIPsFailureKeepsMirror(t *testing.T) {
	runner := &fakeRunner{fail: func(script string) error {
		// fail only the dynamic element update, never the table header
		if strings.Contains(script, "delete element") && strings.Contains(script, "upstream_proxy") {
			return fmt.Errorf("nft apply failed")
		}
		return nil
	}}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	require.NoError(t, a.ApplyReset(ctx))
	require.Error(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.9.9.9"), TTL: time.Minute}}))

	require.NoError(t, a.ApplyReset(ctx))
	assert.NotContains(t, runner.last(), "10.9.9.9", "failed update must not leak into the rebuild")
}
