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
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

func TestParseConntrackTCPEntries(t *testing.T) {
	data := []byte(`ipv4 2 tcp 6 431999 ESTABLISHED src=10.10.0.5 dst=1.1.1.1 sport=51234 dport=443 packets=1 bytes=60 src=1.1.1.1 dst=10.10.0.5 sport=443 dport=51234 [ASSURED] mark=0 zone=0 use=2
ipv4 2 tcp 6 30 TIME_WAIT src=10.10.0.5 dst=1.1.1.2 sport=51235 dport=443 packets=1 bytes=60 src=1.1.1.2 dst=10.10.0.5 sport=443 dport=51235 [ASSURED] mark=0 zone=0 use=2
ipv6 10 tcp 6 431999 SYN_SENT src=fd00::5 dst=2001:db8::1 sport=40000 dport=8080 packets=0 bytes=0 src=2001:db8::1 dst=fd00::5 sport=8080 dport=40000 [ASSURED] mark=0 zone=0 use=2
ipv4 2 udp 17 29 src=10.10.0.5 dst=10.0.0.1 sport=53234 dport=53 packets=1 bytes=60 src=10.0.0.1 dst=10.10.0.5 sport=53 dport=53234 [UNREPLIED] mark=0 zone=0 use=2
garbage line without fields
`)
	entries := parseConntrackTCPEntries(data)
	require.Len(t, entries, 3, "only TCP entries with parseable src/dst are kept")

	assert.Equal(t, "10.10.0.5", entries[0].src.String())
	assert.Equal(t, "1.1.1.1", entries[0].dst.String())
	assert.Equal(t, "ESTABLISHED", entries[0].state)

	assert.Equal(t, "TIME_WAIT", entries[1].state)

	assert.Equal(t, "fd00::5", entries[2].src.String())
	assert.Equal(t, "2001:db8::1", entries[2].dst.String())
	assert.Equal(t, "SYN_SENT", entries[2].state)
}

func TestActiveConntrackTCPState(t *testing.T) {
	for _, state := range []string{"ESTABLISHED", "SYN_SENT", "SYN_RECV", "FIN_WAIT", "CLOSE_WAIT", "CLOSING", "LAST_ACK"} {
		assert.True(t, activeConntrackTCPState(state), "%s must be active", state)
	}
	for _, state := range []string{"TIME_WAIT", "CLOSE", "LISTEN", "NONE", "UNKNOWN"} {
		assert.False(t, activeConntrackTCPState(state), "%s must not be active", state)
	}
}

func TestRefreshPlanRenewsActiveAndPrunes(t *testing.T) {
	now := time.Now()
	st := &refreshState{
		dyn: map[netip.Addr]time.Time{
			netip.MustParseAddr("1.1.1.1"): now.Add(60 * time.Second),  // active -> renew
			netip.MustParseAddr("2.2.2.2"): now.Add(-10 * time.Second), // expired, active -> renew
			netip.MustParseAddr("3.3.3.3"): now.Add(-10 * time.Second), // expired, idle -> prune
		},
		prev: nil,
	}
	active := map[netip.Addr]struct{}{
		netip.MustParseAddr("1.1.1.1"): {},
		netip.MustParseAddr("2.2.2.2"): {},
	}

	ips := st.plan(active, now)
	got := make(map[string]bool)
	for _, ip := range ips {
		got[ip.Addr.String()] = true
		require.Equal(t, time.Duration(dynSetTimeoutS)*time.Second, ip.TTL, "refresh re-adds with the full set timeout")
	}
	assert.True(t, got["1.1.1.1"], "active lease must be renewed")
	assert.True(t, got["2.2.2.2"], "expired-but-active lease must be renewed")
	assert.False(t, got["3.3.3.3"], "expired idle lease must be pruned, not refreshed")

	// the mirror now reflects the refresh
	assert.NotContains(t, st.dyn, netip.MustParseAddr("3.3.3.3"), "pruned lease must leave the mirror")
	assert.Equal(t, map[netip.Addr]struct{}{netip.MustParseAddr("1.1.1.1"): {}, netip.MustParseAddr("2.2.2.2"): {}}, st.prev)
}

func TestRefreshPlanFinalRefreshOnActivityEnd(t *testing.T) {
	now := time.Now()
	addr := netip.MustParseAddr("1.1.1.1")
	st := &refreshState{
		dyn:  map[netip.Addr]time.Time{addr: now.Add(300 * time.Second)},
		prev: map[netip.Addr]struct{}{addr: {}},
	}

	// connection closed between ticks: one final refresh makes the set
	// timeout the reconnect grace period
	ips := st.plan(nil, now)
	require.Len(t, ips, 1)
	assert.Equal(t, addr, ips[0].Addr)

	// next tick without activity and without a new lease: nothing to refresh
	st.dyn[addr] = now.Add(-1 * time.Second)
	assert.Empty(t, st.plan(nil, now.Add(2*time.Second)), "expired lease with no activity and no prior activity must not refresh")
	assert.NotContains(t, st.dyn, addr, "fully idle expired lease must be pruned")
}

func TestRefreshTickBucketsAndApplies(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s1 := subject.FromSandboxUID("u-1")
	s2 := subject.FromSandboxUID("u-2")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s1, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.ApplyDenyFirst(ctx, s2, testSlot("u-2", "10.0.0.6")))

	// seed leases for both subjects (mirrors the dyn sets in the kernel)
	require.NoError(t, a.AddResolvedIPs(ctx, s1, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}}))
	require.NoError(t, a.AddResolvedIPs(ctx, s2, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("2.2.2.2"), TTL: time.Minute}}))

	a.conntrack = func(context.Context) ([]conntrackEntry, error) {
		return []conntrackEntry{
			{src: netip.MustParseAddr("10.0.0.5"), dst: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"},
			{src: netip.MustParseAddr("10.0.0.6"), dst: netip.MustParseAddr("2.2.2.2"), state: "ESTABLISHED"},
			// unknown source: ignored (fail closed, no subject)
			{src: netip.MustParseAddr("10.0.0.99"), dst: netip.MustParseAddr("9.9.9.9"), state: "ESTABLISHED"},
			// inactive state: ignored
			{src: netip.MustParseAddr("10.0.0.5"), dst: netip.MustParseAddr("8.8.8.8"), state: "TIME_WAIT"},
		}, nil
	}

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	var mirrored []string
	a.sandboxMir = func(_ context.Context, s subject.Subject, ips []nftables.ResolvedIP) error {
		for _, ip := range ips {
			mirrored = append(mirrored, string(s)+"="+ip.Addr.String())
		}
		return nil
	}
	a.refreshTick(ctx)

	scripts := runner.scripts
	require.Len(t, scripts, 1, "all subjects' refreshes are batched into ONE nft transaction")
	joined := scripts[0]
	require.Contains(t, joined, "1.1.1.1 timeout 360s")
	require.Contains(t, joined, "2.2.2.2 timeout 360s")
	assert.NotContains(t, joined, "8.8.8.8", "inactive connections must not refresh")
	assert.NotContains(t, joined, "9.9.9.9", "unknown sources must not refresh")
	require.Contains(t, joined, "subj_s_u_1_dyn_v4", "refresh must target the subject's dynamic sets")
	require.Contains(t, joined, "subj_s_u_2_dyn_v4")

	require.ElementsMatch(t, []string{"s-u-1=1.1.1.1", "s-u-2=2.2.2.2"}, mirrored, "sandbox mirror must receive the same refreshed IPs")
}

// TestRefreshTickPendingRedelivery: when the Pod apply succeeds but the
// sandbox mirror fails, the IPs become pending and the next tick redelivers
// them UNCONDITIONALLY (no conntrack activity required), so a transient
// mirror miss never leaves the subject self-locked until re-resolution.
func TestRefreshTickPendingRedelivery(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}}))

	a.conntrack = func(context.Context) ([]conntrackEntry, error) { return nil, nil }
	mirrorFail := true
	a.sandboxMir = func(context.Context, subject.Subject, []nftables.ResolvedIP) error {
		if mirrorFail {
			return context.DeadlineExceeded
		}
		return nil
	}

	// tick 1: no active connections, nothing to refresh yet
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()
	a.refreshTick(ctx)
	assert.Empty(t, runner.scripts, "no activity and no pending -> no refresh")

	// tick 2: an active connection appears; the mirror fails -> pending
	a.conntrack = func(context.Context) ([]conntrackEntry, error) {
		return []conntrackEntry{{src: netip.MustParseAddr("10.0.0.5"), dst: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"}}, nil
	}
	a.refreshTick(ctx)
	require.Len(t, runner.scripts, 1, "active connection refreshed")
	require.NotNil(t, a.states[s].pending, "failed mirror delivery must mark the IPs pending")

	// tick 3: connection gone, conntrack empty — pending forces redelivery
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()
	a.conntrack = func(context.Context) ([]conntrackEntry, error) { return nil, nil }
	mirrorFail = false
	a.refreshTick(ctx)
	require.Len(t, runner.scripts, 1, "pending IPs must be redelivered without conntrack activity")
	require.Contains(t, runner.scripts[0], "1.1.1.1 timeout 360s")
	assert.Empty(t, a.states[s].pending, "successful redelivery clears the pending set")

	// tick 4: nothing owed anymore
	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()
	a.refreshTick(ctx)
	assert.Empty(t, runner.scripts)
}

func TestRefreshTickConntrackErrorClearsPrev(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))

	addr := netip.MustParseAddr("1.1.1.1")
	a.states[s] = &refreshState{
		dyn:  map[netip.Addr]time.Time{addr: time.Now().Add(30 * time.Second)},
		prev: map[netip.Addr]struct{}{addr: {}},
	}
	a.conntrack = func(context.Context) ([]conntrackEntry, error) {
		return nil, context.DeadlineExceeded
	}

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	a.refreshTick(ctx)
	assert.Empty(t, runner.scripts, "no refresh without conntrack data")
	assert.Empty(t, a.states[s].prev, "previous activity must be cleared so no stale lease lingers")
}

func TestRefreshTickNoActiveConnectionsSkipsSubject(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}}))

	a.conntrack = func(context.Context) ([]conntrackEntry, error) { return nil, nil }

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	a.refreshTick(ctx)
	assert.Empty(t, runner.scripts, "no active connections -> no refresh scripts")
}

// TestRefreshTickUsesReboundDispatchIP: after a fencing rebind moved the
// subject IP (new attachment -> ApplyDenyFirst re-registration), the refresh
// bucketing must match conntrack entries against the NEW source IP —
// otherwise active connections from the new IP never renew their leases.
func TestRefreshTickUsesReboundDispatchIP(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run)
	s := subject.FromSandboxUID("u-1")
	ctx := context.Background()
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("u-1", "10.0.0.5")))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}}))

	// the subject's IP moved (rebind: deny-first re-registration wipes the
	// old leases, then the sandbox re-learns its DNS after re-activation)
	moved := testSlot("u-1", "10.0.0.7")
	require.NoError(t, a.ApplyDenyFirst(ctx, s, moved))
	require.NoError(t, a.AddResolvedIPs(ctx, s, []nftables.ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}}))

	a.conntrack = func(context.Context) ([]conntrackEntry, error) {
		return []conntrackEntry{
			{src: netip.MustParseAddr("10.0.0.7"), dst: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"},
		}, nil
	}

	runner.mu.Lock()
	runner.scripts = nil
	runner.mu.Unlock()

	a.refreshTick(ctx)
	require.Len(t, runner.scripts, 1, "the moved source IP must still bucket to the subject")
	require.Contains(t, runner.scripts[0], "1.1.1.1 timeout 360s")
}
