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

package sandboxnft

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// fakeRunner records every (netnsPath, script) pair and optionally fails.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []runCall
	fail    func(netnsPath, script string) error
	netnsOf func(netnsPath string) error // simulates nsenter into a gone netns
}

type runCall struct {
	netnsPath string
	script    string
}

func (r *fakeRunner) Run(_ context.Context, netnsPath, script string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runCall{netnsPath: netnsPath, script: script})
	if r.netnsOf != nil {
		if err := r.netnsOf(netnsPath); err != nil {
			return nil, err
		}
	}
	if r.fail != nil {
		return nil, r.fail(netnsPath, script)
	}
	return nil, nil
}

func (r *fakeRunner) last() runCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return runCall{}
	}
	return r.calls[len(r.calls)-1]
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
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

func TestDenyFirstInstallShape(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()

	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	call := runner.last()
	require.Equal(t, "/var/run/netns/ns-u-1", call.netnsPath, "rules must be installed via the sandbox netns path")

	script := call.script
	require.Contains(t, script, "delete table inet opensandbox-fleet-ns")
	require.Contains(t, script, "add chain inet opensandbox-fleet-ns output { type filter hook output priority 0; policy drop; }")
	// structural allowances: established, loopback, DNS scoped to the
	// gateway (sandbox DNS is addressed to the gateway:53 before the
	// Pod-netns REDIRECT; an unscoped dport-53 allowance would let a
	// denying sandbox reach any host-local resolver)
	require.Contains(t, script, "ct state established,related accept")
	require.Contains(t, script, `oifname "lo" accept`)
	require.Contains(t, script, "ip daddr 10.0.0.1 udp dport 53 accept")
	require.Contains(t, script, "ip daddr 10.0.0.1 tcp dport 53 accept")
	assert.NotContains(t, script, "add rule inet opensandbox-fleet-ns output udp dport 53 accept", "DNS exception must be gateway-scoped")
	assert.NotContains(t, script, "add rule inet opensandbox-fleet-ns output tcp dport 53 accept", "DNS exception must be gateway-scoped")
	// encrypted-DNS mirror: DoT always blocked
	require.Contains(t, script, "tcp dport 853 drop")
	require.Contains(t, script, "udp dport 853 drop")
	// empty deny-first sets, explicit drop default
	require.Contains(t, script, "add set inet opensandbox-fleet-ns allow_v4 { type ipv4_addr; flags interval; }")
	require.Contains(t, script, "add set inet opensandbox-fleet-ns dyn_v4 { type ipv4_addr; timeout 360s; }")
	assert.NotContains(t, script, "add element", "deny-first must carry no policy elements")
	assert.NotContains(t, script, "dport 443", "DoH must be off unless enabled")
	require.Contains(t, script, "add rule inet opensandbox-fleet-ns output drop")
}

func TestPolicySwapPreservesStructuralRules(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"10.0.0.0/24"},{"action":"deny","target":"1.2.3.4"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	script := runner.last().script
	require.Contains(t, script, "flush chain inet opensandbox-fleet-ns output")
	require.Contains(t, script, "flush set inet opensandbox-fleet-ns allow_v4")
	require.Contains(t, script, "add element inet opensandbox-fleet-ns allow_v4 { 10.0.0.0/24 }")
	require.Contains(t, script, "add element inet opensandbox-fleet-ns deny_v4 { 1.2.3.4 }")
	// structural rules must survive the swap (the chain was flushed)
	require.Contains(t, script, "ct state established,related accept")
	require.Contains(t, script, "ip daddr 10.0.0.1 udp dport 53 accept")
	require.Contains(t, script, "tcp dport 853 drop")
	// dynamic sets are untouched by the swap
	assert.NotContains(t, script, "flush set inet opensandbox-fleet-ns dyn")
	assert.NotContains(t, script, "flush set inet opensandbox-fleet-ns doh")
	// default-deny trailing verdict
	require.Contains(t, script, "add rule inet opensandbox-fleet-ns output drop")
}

func TestDefaultAllowPolicyTrailingAccept(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	pol, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"deny","target":"10.99.0.0/24"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	script := runner.last().script
	require.Contains(t, script, "add element inet opensandbox-fleet-ns deny_v4 { 10.99.0.0/24 }")
	assert.NotContains(t, script, "add rule inet opensandbox-fleet-ns output drop")
	require.Contains(t, script, "add rule inet opensandbox-fleet-ns output accept")
}

func TestDenyFirstResetOnReRegistration(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	// rebind: re-registration atomically resets the table to deny-first
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	script := runner.last().script
	require.Contains(t, script, "delete table inet opensandbox-fleet-ns", "rebind must rebuild the table")
	assert.NotContains(t, script, "8.8.8.8", "old policy must not survive a rebind")

	// and the applier state is deny-first again: ApplyPolicy still works
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
}

func TestUnknownSubjectRejected(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	ctx := context.Background()
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny"}`)
	require.NoError(t, err)
	require.ErrorIs(t, a.ApplyPolicy(ctx, subject.FromSandboxUID("ghost"), pol), ErrUnknownSubject)
	require.ErrorIs(t, a.AddResolvedIPs(ctx, subject.FromSandboxUID("ghost"), []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.2.3.4"), TTL: time.Minute}}), ErrUnknownSubject)
}

func TestAddResolvedIPsClampsTTL(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{
		{Addr: netip.MustParseAddr("1.2.3.4"), TTL: 30 * time.Second},   // clamps up to 90s
		{Addr: netip.MustParseAddr("2001:db8::1"), TTL: 24 * time.Hour}, // clamps down to 360s
	}))
	script := runner.last().script
	require.Contains(t, script, "1.2.3.4 timeout 90s")
	require.Contains(t, script, "2001:db8::1 timeout 360s")
	require.Contains(t, script, "add element inet opensandbox-fleet-ns dyn_v4")
	require.Contains(t, script, "add element inet opensandbox-fleet-ns dyn_v6")
}

func TestRemoveDeletesTable(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.Remove(ctx, s))
	require.Equal(t, "delete table inet opensandbox-fleet-ns\n", runner.last().script)

	// removal of an unknown subject is a no-op
	require.NoError(t, a.Remove(ctx, s))
	require.Equal(t, 2, runner.count())
}

func TestRemoveGoneNetnsIsBestEffort(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	// the netns exists during install but vanishes before the unload
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	runner.netnsOf = func(string) error {
		return fmt.Errorf("nsenter: cannot open netns: No such file or directory")
	}
	require.NoError(t, a.Remove(ctx, s), "removal of a gone netns must not fail the unload")
}

func TestDoHBlockMirror(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		BlockDoH443:    true,
		DoHBlocklistV4: []string{"203.0.113.0/24"},
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	script := runner.last().script
	require.Contains(t, script, "add set inet opensandbox-fleet-ns doh_block_v4 { type ipv4_addr; flags interval; }")
	require.Contains(t, script, "add rule inet opensandbox-fleet-ns output ip daddr @doh_block_v4 tcp dport 443 drop")
	assert.NotContains(t, script, "add rule inet opensandbox-fleet-ns output tcp dport 443 drop", "blocklist mode is not strict")

	// strict mode: no blocklist -> bare 443 drop
	runner2 := &fakeRunner{}
	a2 := NewApplier(runner2.Run, Options{BlockDoH443: true})
	require.NoError(t, a2.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Contains(t, runner2.last().script, "add rule inet opensandbox-fleet-ns output tcp dport 443 drop")
}

// TestDoHPolicySwapDoesNotReaddSets: the DoH blocklist sets are created only
// on fresh installs — a policy swap must re-add the RULES (the chain was
// flushed) but never the sets or elements (a re-add fails with "File
// exists", which would block activation under a blocklist configuration).
func TestDoHPolicySwapDoesNotReaddSets(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{
		BlockDoH443:    true,
		DoHBlocklistV4: []string{"203.0.113.0/24"},
	})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	script := runner.last().script
	require.Contains(t, script, "ip daddr @doh_block_v4 tcp dport 443 drop", "DoH rule must survive the swap")
	assert.NotContains(t, script, "add set inet opensandbox-fleet-ns doh_block", "set must not be re-created on swap")
	assert.NotContains(t, script, "add element inet opensandbox-fleet-ns doh_block", "elements must not be re-added on swap")
}

func TestApplySlotUpdateNetnsMove(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))

	// unchanged fencing, netns path moved: reinstall in the new netns WITH
	// the current policy, remove the old table best effort
	runner.mu.Lock()
	runner.calls = nil
	runner.mu.Unlock()

	newSlot := testSlot("u-1", "10.0.0.5")
	newSlot.HostNetnsPath = "/var/run/netns/ns-u-1-new"
	require.NoError(t, a.ApplySlotUpdate(ctx, s, newSlot))

	runner.mu.Lock()
	calls := append([]runCall(nil), runner.calls...)
	runner.mu.Unlock()
	require.Len(t, calls, 2, "install into the new netns + best-effort delete of the old")
	require.Equal(t, "/var/run/netns/ns-u-1-new", calls[0].netnsPath)
	require.Contains(t, calls[0].script, "8.8.8.8", "policy must be preserved across the move")
	require.Contains(t, calls[0].script, "ip daddr 10.0.0.1 udp dport 53 accept", "gateway-scoped DNS from the new slot")
	require.Equal(t, "/var/run/netns/ns-u-1", calls[1].netnsPath)
	require.Contains(t, calls[1].script, "delete table")

	// the subject stays usable in the new netns
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	runner.mu.Lock()
	last := runner.calls[len(runner.calls)-1]
	runner.mu.Unlock()
	require.Equal(t, "/var/run/netns/ns-u-1-new", last.netnsPath)
}

func TestApplySlotUpdateGatewayOnly(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	newSlot := testSlot("u-1", "10.0.0.5")
	newSlot.Gateway = netip.MustParseAddr("10.0.0.254")
	require.NoError(t, a.ApplySlotUpdate(ctx, s, newSlot))

	script := runner.last().script
	require.Contains(t, script, "ip daddr 10.0.0.254 udp dport 53 accept", "DNS exception must follow the moved gateway")
	assert.NotContains(t, script, "10.0.0.1 udp dport 53", "old gateway must not survive")
}

func TestApplySlotUpdateUnchangedIsNoop(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	count := runner.count()

	require.NoError(t, a.ApplySlotUpdate(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Equal(t, count, runner.count(), "nothing moved -> no nft transaction")

	// unknown subject is a no-op too
	require.NoError(t, a.ApplySlotUpdate(ctx, subject.FromSandboxUID("ghost"), testSlot("g", "10.0.0.9")))
	require.Equal(t, count, runner.count())
}

func TestResetWipesTables(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	runner.mu.Lock()
	runner.calls = nil
	runner.mu.Unlock()

	// a gone netns must not abort the wipe of the others
	runner.netnsOf = func(path string) error {
		if strings.Contains(path, "gone") {
			return fmt.Errorf("nsenter: cannot open netns: No such file or directory")
		}
		return nil
	}
	a.Reset(ctx, []string{"/var/run/netns/ns-a", "/var/run/netns/gone", "/var/run/netns/ns-b", "  "})

	runner.mu.Lock()
	calls := append([]runCall(nil), runner.calls...)
	runner.mu.Unlock()
	require.Len(t, calls, 3, "every non-blank path is attempted; blank paths are skipped")
	for _, call := range calls {
		require.Contains(t, call.script, "delete table")
	}
	// the unreachable netns ("gone") returned an error that Reset logged and
	// swallowed — reaching the state assertion below proves the wipe did not
	// abort on a missing netns.

	// state cleared: a subsequent policy apply is rejected, not applied
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny"}`)
	require.NoError(t, err)
	require.ErrorIs(t, a.ApplyPolicy(ctx, s, pol), ErrUnknownSubject)
}

func TestApplyDenyFirstMissingTableFallback(t *testing.T) {
	var attempts int
	runner := &fakeRunner{fail: func(_, script string) error {
		if strings.Contains(script, "delete table inet opensandbox-fleet-ns") {
			attempts++
			if attempts == 1 {
				return fmt.Errorf("nft apply failed: No such file or directory; did you mean table")
			}
		}
		return nil
	}}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.Equal(t, 1, attempts, "one failed attempt with delete-table line, then fallback")
	assert.NotContains(t, runner.last().script, "delete table", "fallback script must not contain the delete-table line")
}

func TestPolicySwapFailureKeepsState(t *testing.T) {
	var swapAttempts int
	runner := &fakeRunner{fail: func(_, script string) error {
		if strings.Contains(script, "flush chain") {
			swapAttempts++
			if swapAttempts == 1 {
				return context.DeadlineExceeded
			}
		}
		return nil
	}}
	a := NewApplier(runner.Run, Options{})
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, err)
	require.Error(t, a.ApplyPolicy(ctx, s, pol))

	// state unchanged: the retry succeeds
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
}
