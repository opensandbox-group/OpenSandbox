// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The peer has its own network namespace so these are genuine remote TCP
// connections; loopback and the infrastructure exceptions remain untouched.
func TestNftDomainPortsKernelEstablishedTightening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startWithdrawalEchoPeer(t, ctx, "2443", "28443")
	m := NewManagerWithOptions(Options{})
	old := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"allow","target":"b.example","ports":[2443]}]}`)
	tight := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"b.example","ports":[2443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("198.18.0.2"), TTL: time.Minute}}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", ips))
	connect, echo := domainPortStreams(t)
	removeTimedGrant := func(port string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "nft", "delete", "element", "inet", tableName, scopedLeaseV4,
			"{", "198.18.0.2", ".", port, "}").CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	kept, removed := connect("2443"), connect("28443")
	removedOnReload := connect("28443")
	require.NoError(t, echo(kept))
	require.NoError(t, echo(removed))
	require.NoError(t, echo(removedOnReload))
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.NoError(t, echo(kept), "the overlapping :2443 grant must survive")
	requireWithdrawalDrop(t, ctx, removed, 255)
	removeTimedGrant("2443")
	require.NoError(t, echo(kept), "expiry of a tuple retained by the opt-in must not expose a stale cut")
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(kept), "plain rebuild must preserve the surviving endpoint")
	requireWithdrawalDrop(t, ctx, removedOnReload, 255)
	require.NoError(t, echo(connect("2443")), "no client DNS relookup is required after narrowing")
	assertNewConnectionBlocked := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp", "198.18.0.2:28443", 300*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
		require.Error(t, err, "the revoked endpoint must reject a new connection")
	}
	assertNewConnectionBlocked()

	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	fresh := connect("28443")
	require.NoError(t, echo(fresh))
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(fresh), "a plain revoke must not cut a connection established after re-allow")
	assertNewConnectionBlocked()

	m = NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	active := connect("28443")
	require.NoError(t, echo(active))
	now = now.Add(121 * time.Second)
	next := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"allow","target":"b.example","ports":[2443]},{"action":"allow","target":"unrelated.example","ports":[443]}]}`)
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, next)
	require.NoError(t, err)
	require.NoError(t, echo(active), "an unchanged port rule is not a revocation even when the DNS lease expires")

	m = NewManagerWithOptions(Options{})
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	sibling, siblingAfterRegrant, siblingAfterReload := connect("2443"), connect("2443"), connect("2443")
	for _, c := range []net.Conn{sibling, siblingAfterRegrant, siblingAfterReload} {
		require.NoError(t, echo(c))
	}
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	requireWithdrawalDrop(t, ctx, sibling, 255)
	regrant := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[28443]},{"action":"allow","target":"b.example","ports":[2443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, regrant))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	reopened := connect("28443")
	require.NoError(t, echo(reopened))
	removeTimedGrant("28443")
	require.NoError(t, echo(reopened), "expiry of a re-learned tuple must not reactivate a stale teardown against new flows")
	requireWithdrawalDrop(t, ctx, siblingAfterRegrant, 255)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(reopened), "a plain rebuild must not recreate a retired tuple cut")
	requireWithdrawalDrop(t, ctx, siblingAfterReload, 255)
	assertNewConnectionBlocked()

	// A plain replacement's deny must still win when a timed grant is
	// written for a tuple whose established cut is already installed.
	m = NewManagerWithOptions(Options{})
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	scopedDeniedStream, wholeDeniedStream := connect("28443"), connect("28443")
	require.NoError(t, echo(scopedDeniedStream))
	require.NoError(t, echo(wholeDeniedStream))
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	scopedDenied := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"deny","target":"198.18.0.2","ports":[28443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, scopedDenied))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	require.NoError(t, echo(connect("2443")), "the scoped DNS grant admits the allowed sibling")
	requireWithdrawalDrop(t, ctx, scopedDeniedStream, 255)

	wholeDenied := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"c.example"},{"action":"deny","target":"198.18.0.2","ports":[28443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, wholeDenied))
	require.NoError(t, m.AddResolvedDomain(ctx, "c.example", ips))
	require.NoError(t, echo(connect("2443")), "the whole-IP DNS grant admits the allowed sibling")
	requireWithdrawalDrop(t, ctx, wholeDeniedStream, 255)

	whole := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"d.example"},{"action":"allow","target":"control.example"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, whole))
	require.NoError(t, m.AddResolvedDomain(ctx, "control.example", []ResolvedIP{
		{Addr: netip.MustParseAddr("192.0.2.99"), TTL: time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::99"), TTL: time.Minute},
	}))
	run := m.run
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		out, err := run(ctx, script)
		require.NoError(t, err, "%s", out)
		return nil, &ApplyError{Effect: ApplyUnknown, Err: context.DeadlineExceeded}
	}
	mixed := append([]ResolvedIP{{Addr: netip.MustParseAddr("2001:db8::2"), TTL: time.Minute}}, ips...)
	require.Error(t, m.AddResolvedDomain(ctx, "d.example", mixed))
	m.run = run
	require.ErrorIs(t, m.AddResolvedDomain(ctx, "d.example", ips), errStaticStateUncertain)
	require.NoError(t, m.ApplyStatic(ctx, whole))
	restored := readKernelAddressSets(t, ctx, dynAllowV4Set, dynAllowV6Set)
	require.ElementsMatch(t, []string{"192.0.2.99"}, restored[dynAllowV4Set])
	require.ElementsMatch(t, []string{"2001:db8::99"}, restored[dynAllowV6Set])
	out, err := run(ctx, "get element inet "+tableName+" "+scopedDropV4+" { 198.18.0.2 . 28443 }\n")
	require.NoError(t, err, "%s", out)
	// Use a new DNS source so the cut must retire on this acknowledgement,
	// not from a lease staged by the preceding static transaction.
	require.NoError(t, m.AddResolvedDomain(ctx, "d.example", ips))
	wholeReopened := connect("28443")
	require.NoError(t, echo(wholeReopened))
	out, err = exec.CommandContext(ctx, "nft", "delete", "element", "inet", tableName, dynAllowV4Set,
		"{", "198.18.0.2", "}").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.NoError(t, echo(wholeReopened), "expiry of a whole-IP regrant must not expose the old exact cut")
	assertNewConnectionBlocked()
}

func TestNftDomainPortsKernelScopedCutsSurviveRepeatOptIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startWithdrawalEchoPeer(t, ctx, "2443", "28443")
	m := NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	ttl := establishedCarryTTL()
	both := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[2443]},{"action":"allow","target":"b.example","ports":[28443]}]}`)
	onlyB := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"b.example","ports":[28443]}]}`)
	neither := mustPolicy(t, `{"egress":[{"action":"allow","target":"control.example"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, both))
	addr := netip.MustParseAddr("198.18.0.2")
	ips := []ResolvedIP{{Addr: addr, TTL: time.Minute}}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", ips))
	connect, echo := domainPortStreams(t)
	old, oldAfterExpiry, recent := connect("2443"), connect("2443"), connect("28443")
	require.NoError(t, echo(old))
	require.NoError(t, echo(oldAfterExpiry))
	require.NoError(t, echo(recent))

	oldDeadline := now.Add(ttl)
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, both, onlyB)
	require.NoError(t, err)
	checkNftTuple(t, ctx, scopedDropV4, addr.String(), "2443", true)
	require.NoError(t, echo(recent), "first opt-in must leave the unrelated tuple admitted")

	now = now.Add(ttl / 2)
	run := m.run
	m.run = func(context.Context, string) ([]byte, error) {
		return nil, &ApplyError{Effect: ApplyUnchanged, Err: fmt.Errorf("second opt-in was not applied")}
	}
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, onlyB, neither)
	m.run = run
	require.Error(t, err)
	checkNftTuple(t, ctx, scopedDropV4, addr.String(), "2443", true)
	require.NoError(t, echo(recent), "failed opt-in must leave the second tuple admitted")
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, onlyB, neither)
	require.NoError(t, err)
	checkNftTuple(t, ctx, scopedDropV4, addr.String(), "28443", true)
	requireWithdrawalDrop(t, ctx, old, 255)

	// The first cut is alive during planning but expires while an unrelated
	// DNS grant is appended to the transaction. The second cut is still live.
	now = oldDeadline.Add(-3 * time.Second)
	m.tracker.now = m.now
	require.NoError(t, m.AddResolvedDomain(ctx, "control.example", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.99"), TTL: time.Minute}}))
	m.tracker.now = func() time.Time { at := now; now = now.Add(time.Second); return at }
	require.NoError(t, m.ApplyStatic(ctx, neither))
	m.tracker.now = m.now
	require.Contains(t, readKernelLeaseExpiry(t, ctx), "192.0.2.99")
	checkNftTuple(t, ctx, scopedDropV4, addr.String(), "2443", false)
	require.NoError(t, echo(oldAfterExpiry), "the first cut must expire at its original deadline")
	requireWithdrawalDrop(t, ctx, recent, 255)

	require.NoError(t, m.ApplyStatic(ctx, onlyB))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", ips))
	require.Empty(t, m.lastDrops.scopedDrops, "a genuine regrant retires the remaining cut early")
	require.NoError(t, echo(connect("28443")))
}

func domainPortStreams(t *testing.T) (func(string) net.Conn, func(net.Conn) error) {
	var mark byte = 1 // openWithdrawalStream has already checked the first echo.
	return func(port string) net.Conn { return openWithdrawalStream(t, "198.18.0.2", port) }, func(c net.Conn) error {
		mark++
		return withdrawalEcho(c, mark)
	}
}
