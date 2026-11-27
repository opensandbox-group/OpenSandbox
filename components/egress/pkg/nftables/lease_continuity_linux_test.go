// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNftExpiredDomainCannotAcquireUnownedTCPLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com"}]}`)
	addr := netip.MustParseAddr("192.0.2.1")
	require.NoError(t, m.ApplyStatic(ctx, p))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
	// Advance the manager clock and model the same expiry in the kernel,
	// without waiting a minute. This is the first poll to see the flow.
	now = now.Add(61 * time.Second)
	out, err := exec.CommandContext(ctx, "nft", "delete", "element", "inet", tableName, dynAllowV4Set, "{", addr.String(), "}").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, state: "ESTABLISHED"}}, m))
	require.NotContains(t, readKernelLeaseExpiry(t, ctx), addr.String(), "a TCP poll cannot install a grant without an eligible DNS owner")
}

func TestNftReloadKeepsTCPFinalGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	wide := mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	aliasOnly := mustPolicy(t, `{"egress":[{"action":"allow","target":"alias.example.com"}]}`)
	addr := netip.MustParseAddr("192.0.2.1")
	for _, tc := range []struct {
		name         string
		removeBefore bool
		removeAfter  bool
	}{
		{name: "keep the credited owner"},
		{name: "remove the credited owner before close", removeBefore: true},
		{name: "remove the credited owner after close", removeAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManagerWithOptions(Options{})
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			require.NoError(t, m.ApplyStatic(ctx, wide))
			require.NoError(t, m.AddResolvedDomain(ctx, "primary.example.com", []ResolvedIP{{Addr: addr}}))
			require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, state: "ESTABLISHED"}}, m))
			now = now.Add(30 * time.Second)
			next := wide
			if tc.removeBefore || tc.removeAfter {
				require.NoError(t, m.AddResolvedDomain(ctx, "alias.example.com", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
			}
			if tc.removeBefore {
				next = aliasOnly
			}
			if !tc.removeAfter {
				require.NoError(t, m.ApplyStatic(ctx, next))
			}
			require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
			if tc.removeAfter {
				require.Greater(t, readKernelLeaseExpiry(t, ctx)[addr.String()], 340,
					"the credited owner still earns reconnect grace")
				require.NoError(t, m.ApplyStatic(ctx, aliasOnly))
			}
			expiry := readKernelLeaseExpiry(t, ctx)[addr.String()]
			require.Positive(t, expiry)
			if !tc.removeBefore && !tc.removeAfter {
				require.Greater(t, expiry, 340, "a close after reload earns one full reconnect grace period")
			} else {
				require.LessOrEqual(t, expiry, 120, "a new owner cannot inherit another name's pending close refresh")
			}
		})
	}
	t.Run("reload after only one credited source expired", func(t *testing.T) {
		m := NewManagerWithOptions(Options{})
		now := time.Unix(1000, 0)
		m.tracker.now = func() time.Time { return now }
		require.NoError(t, m.ApplyStatic(ctx, wide))
		require.NoError(t, m.AddResolvedDomain(ctx, "alias.example.com", []ResolvedIP{{Addr: addr}}))
		long := []ResolvedIP{{Addr: addr, TTL: 300 * time.Second}}
		require.NoError(t, m.AddResolvedDomain(ctx, "primary.example.com", long))
		require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, state: "ESTABLISHED"}}, m))
		now = now.Add(30 * time.Second)
		require.NoError(t, m.AddResolvedDomain(ctx, "primary.example.com", long))
		now = now.Add(331 * time.Second)
		require.NoError(t, m.ApplyStatic(ctx, wide))
		require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
		require.Greater(t, readKernelLeaseExpiry(t, ctx)[addr.String()], 340,
			"the surviving source still earns a full reconnect grace period")
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, aliasOnly)
		require.NoError(t, err)
		require.NotContains(t, readKernelLeaseExpiry(t, ctx), addr.String(),
			"reloading must not restore close-refresh eligibility to the expired source")
		require.True(t, m.lastDrops.MatchesFlow(addr, 443), "the expired source cannot protect a revoked flow")
	})
}

func TestNftFallbackReplansDNSLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	wide := mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"keep.example.com"}]}`)
	addr := netip.MustParseAddr("192.0.2.1")
	for _, tc := range []struct {
		name  string
		delay time.Duration
		live  bool
	}{
		{"remaining TTL", 40 * time.Second, true},
		{"expired before retry", 61 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManagerWithOptions(Options{})
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			require.NoError(t, m.ApplyStatic(ctx, wide))
			require.NoError(t, m.AddResolvedDomain(ctx, "keep.example.com", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
			require.NoError(t, m.AddResolvedDomain(ctx, "remove.example.com", []ResolvedIP{{Addr: addr, TTL: 300 * time.Second}}))
			out, err := exec.CommandContext(ctx, "nft", "delete", "table", "inet", tableName).CombinedOutput()
			require.NoError(t, err, "%s", out)
			now = now.Add(time.Minute)
			attempts := 0
			m.run = func(ctx context.Context, script string) ([]byte, error) {
				out, err := defaultRunner(ctx, script)
				attempts++
				if attempts == 1 {
					require.True(t, IsMissingTableError(err, tableName), "first attempt must get the real missing-table error: %v", err)
					now = now.Add(tc.delay)
				}
				return out, err
			}
			_, err = m.ApplyStaticComputingEstablishedDrops(ctx, wide, tight)
			require.NoError(t, err)
			require.Equal(t, 2, attempts)
			leases := readKernelLeaseExpiry(t, ctx)
			if tc.live {
				require.Contains(t, leases, addr.String())
				require.LessOrEqual(t, leases[addr.String()], 20, "retry must not add elapsed time back to the lease")
				require.False(t, m.lastDrops.MatchesFlow(addr, 443))
			} else {
				require.NotContains(t, leases, addr.String(), "a lease that expires before retry cannot be installed")
				require.True(t, m.lastDrops.MatchesFlow(addr, 443), "expiry must also remove the teardown exemption")
			}
		})
	}
}
