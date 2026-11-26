// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNftClassicScopedDNSDoesNotInstallWholeAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	p := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"*.example.com","ports":[443]},{"action":"allow","target":"api.example.com","ports":[8443]},{"action":"allow","target":"control.example.net"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	ips := []ResolvedIP{
		{Addr: netip.MustParseAddr("192.0.2.1"), TTL: time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::1"), TTL: time.Minute},
	}
	require.NoError(t, m.AddResolvedDomain(ctx, "control.example.net", []ResolvedIP{
		{Addr: netip.MustParseAddr("192.0.2.99"), TTL: time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::99"), TTL: time.Minute},
	}))
	require.NoError(t, m.AddResolvedDomain(ctx, "api.example.com", ips))
	whole := readKernelAddressSets(t, ctx, dynAllowV4Set, dynAllowV6Set)
	require.ElementsMatch(t, []string{"192.0.2.99"}, whole[dynAllowV4Set], "only the independent portless grant belongs here")
	require.ElementsMatch(t, []string{"2001:db8::99"}, whole[dynAllowV6Set])
	for _, ip := range ips {
		checkScopedElement(t, ctx, ip.Addr.String(), "443", true)
		checkScopedElement(t, ctx, ip.Addr.String(), "8443", true)
		checkScopedElement(t, ctx, ip.Addr.String(), "80", false)
	}

	// Lose the acknowledgement after a real write, not before submission.
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		m.run = defaultRunner
		out, err := defaultRunner(ctx, script)
		if err == nil {
			err = &ApplyError{Effect: ApplyUnknown, Err: errors.New("lost dynamic acknowledgement")}
		}
		return out, err
	}
	require.Error(t, m.AddResolvedDomain(ctx, "unconfirmed.example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.2")}}))
	checkScopedElement(t, ctx, "192.0.2.2", "443", true)
	require.NoError(t, m.AddResolvedDomain(ctx, "api.example.com", ips))
	checkScopedElement(t, ctx, "192.0.2.2", "443", false)
	checkScopedElement(t, ctx, "192.0.2.1", "443", true)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{
		{remote: ips[0].Addr, remotePort: 443, state: "ESTABLISHED"},
	}, m))
	require.Regexp(t, `\. 443 (timeout 6m )?expires [56]m`, checkScopedElement(t, ctx, "192.0.2.1", "443", true))
	require.Regexp(t, `\. 8443 timeout 2m expires [12]m`, checkScopedElement(t, ctx, "192.0.2.1", "8443", true))

	tight := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"*.example.com","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	checkScopedElement(t, ctx, "192.0.2.1", "443", true)
	checkScopedElement(t, ctx, "192.0.2.1", "8443", false)

	now := m.tracker.now()
	m.tracker.now = func() time.Time { return now }
	late := ips[:1]
	require.NoError(t, m.AddResolvedDomain(ctx, "late.example.com", late))
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
	require.Regexp(t, `\. 443 (timeout 6m )?expires [56]m`, checkScopedElement(t, ctx, "192.0.2.1", "443", true))
	lateOnly := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"late.example.com","ports":[443]}]}`)
	now = now.Add(115 * time.Second)
	step := 2 * time.Second
	m.tracker.now = func() time.Time {
		at := now
		now = now.Add(step)
		return at
	}
	require.NoError(t, m.ApplyStatic(ctx, lateOnly))
	require.Regexp(t, `\. 443 timeout 1s\b`, checkScopedElement(t, ctx, "192.0.2.1", "443", true),
		"the late source keeps only its DNS deadline, sampled after final rendering")
}

func TestNftMixedRenewalRequiresConfirmedDeadlines(t *testing.T) {
	ctx := t.Context()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	p := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"whole.example"},{"action":"allow","target":"tcp.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	whole, scoped := netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("2001:db8::8")
	grant := func(domain string, addr netip.Addr) error {
		return m.AddResolvedDomain(ctx, domain, []ResolvedIP{{Addr: addr, TTL: time.Minute}})
	}
	require.NoError(t, grant("whole.example", whole))
	require.NoError(t, grant("tcp.example", scoped))
	now = now.Add(30 * time.Second)
	run := m.run
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		out, err := run(ctx, script)
		require.NoError(t, err, "%s", out)
		return nil, &ApplyError{Effect: ApplyUnknown, Err: context.DeadlineExceeded}
	}
	active := []tcpConnection{{remote: whole, remotePort: 443, state: "ESTABLISHED"}, {remote: scoped, remotePort: 443, state: "ESTABLISHED"}}
	require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
	m.run = run
	require.Greater(t, readKernelLeaseExpiry(t, ctx)[whole.String()], 300)
	require.Regexp(t, `expires [56]m`, checkScopedElement(t, ctx, scoped.String(), "443", true))
	other := netip.MustParseAddr("192.0.2.9")
	require.ErrorIs(t, grant("whole.example", other), errStaticStateUncertain)
	require.ErrorIs(t, m.tracker.refreshActiveConnections(ctx, nil, m), errStaticStateUncertain)
	require.NoError(t, m.ApplyStatic(ctx, p))
	leases := readKernelLeaseExpiry(t, ctx)
	require.Greater(t, leases[whole.String()], 80)
	require.LessOrEqual(t, leases[whole.String()], 90)
	require.NotContains(t, leases, other.String())
	require.Contains(t, checkScopedElement(t, ctx, scoped.String(), "443", true), "timeout 1m30s")
	require.NoError(t, grant("whole.example", other))
}

func checkScopedElement(t *testing.T, ctx context.Context, addr, port string, exists bool) string {
	t.Helper()
	return checkNftTuple(t, ctx, scopedLeaseSet(netip.MustParseAddr(addr)), addr, port, exists)
}

func checkNftTuple(t *testing.T, ctx context.Context, set, addr, port string, exists bool) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "nft", "get", "element", "inet", tableName, set, "{", addr, ".", port, "}").CombinedOutput()
	if exists {
		require.NoError(t, err, "%s", out)
	} else {
		require.Error(t, err, "%s", out)
		require.Contains(t, string(out), "No such file or directory")
	}
	return string(out)
}

func TestDomainPortWithdrawalPreservesStaticSelectorIdentity(t *testing.T) {
	old := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"api.example","ports":[443]}]}`)
	for _, tc := range []struct {
		target, learned string
		withdraw        bool
	}{
		{"fe80::1%eth0", "fe80::1", true},
		{"fe80::1", "fe80::1", false},
		{"fe80::/64", "fe80::1", false},
		{"::ffff:192.0.2.7", "192.0.2.7", true},
		{"192.0.2.7", "192.0.2.7", false},
	} {
		next := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"`+tc.target+`","ports":[443]}]}`)
		ip := netip.MustParseAddr(tc.learned)
		drops := ComputeEstablishedDrops(old, next, map[string][]netip.Addr{"api.example": {ip}})
		require.Equal(t, tc.withdraw, drops.MatchesFlow(ip, 443), "replacement target %s", tc.target)
	}
}
