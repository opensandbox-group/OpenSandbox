// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestNftDomainAnswerDoesNotShortenSharedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, p))
	addr := netip.MustParseAddr("192.0.2.1")
	require.NoError(t, m.AddResolvedDomain(ctx, "long.example.com", []ResolvedIP{{Addr: addr, TTL: 300 * time.Second}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "short.example.com", []ResolvedIP{{Addr: addr, TTL: time.Second}}))
	leases := readKernelLeaseExpiry(t, ctx)
	require.Contains(t, leases, addr.String())
	require.Greater(t, leases[addr.String()], 240, "the shorter answer must not replace a live long lease")
}

func TestNftPolicyReplacementKeepsOnlySurvivingDomainLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	wide, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	tight, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"keep.example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, wide))
	shared := netip.MustParseAddr("192.0.2.1")
	removed := netip.MustParseAddr("192.0.2.2")
	unattributed := netip.MustParseAddr("192.0.2.3")
	require.NoError(t, m.AddResolvedDomain(ctx, "keep.example.com", []ResolvedIP{{Addr: shared, TTL: 120 * time.Second}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "remove.example.com", []ResolvedIP{{Addr: shared, TTL: 300 * time.Second}, {Addr: removed, TTL: 300 * time.Second}}))
	require.NoError(t, m.AddResolvedIPs(ctx, []ResolvedIP{{Addr: unattributed, TTL: 300 * time.Second}}))
	before := readKernelLeaseExpiry(t, ctx)
	require.Len(t, before, 3)
	require.Greater(t, before[shared.String()], 240)

	now = now.Add(70 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	after := readKernelLeaseExpiry(t, ctx)
	require.Len(t, after, 1, "the revoked and unattributed leases must not survive")
	require.Contains(t, after, shared.String())
	require.Greater(t, after[shared.String()], 90, "the still-permitted source keeps its remaining lease")
	require.LessOrEqual(t, after[shared.String()], 110, "replacement cannot renew or inherit the revoked source's longer lease")
}

func TestNftSharedIPTeardownCountsOnlyDroppedLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	const keep = "keep.example.com"
	wide := mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"keep.example.com"}]}`)
	shared := netip.MustParseAddr("192.0.2.1")
	flow := []byte("ipv4 2 tcp 6 431999 ESTABLISHED src=10.0.0.2 dst=192.0.2.1 sport=12345 dport=443 src=192.0.2.1 dst=10.0.0.2 sport=443 dport=12345 [ASSURED]\n")
	for _, tc := range []struct {
		name         string
		elapsed      time.Duration
		retired      bool
		earlyTCP     bool
		refreshTCP   bool
		clockStep    time.Duration
		maxRemaining int
		removedFirst bool
		want         int
	}{
		{name: "live retired owner", elapsed: time.Minute, retired: true, want: 0},
		{name: "expired retired owner", elapsed: 121 * time.Second, retired: true, want: 1},
		{name: "expired retired owner with unrelated TCP", elapsed: 121 * time.Second, retired: true, refreshTCP: true, want: 1},
		{name: "expired renewable owner in DNS backoff", elapsed: 121 * time.Second, want: 1},
		{name: "previously credited owner after a delayed TCP poll", elapsed: 361 * time.Second, retired: true, earlyTCP: true, refreshTCP: true, want: 0},
		{name: "lease expires while rendering replacement", elapsed: 110 * time.Second, retired: true, clockStep: 11 * time.Second, want: 1},
		{name: "lease expires while assembling timed elements", elapsed: 110 * time.Second, retired: true, clockStep: 6 * time.Second, want: 1},
		{name: "live lease loses time while assembling timed elements", elapsed: 110 * time.Second, retired: true, clockStep: 3 * time.Second, maxRemaining: 4, want: 0},
		{name: "revoked sources do not delay the replacement", elapsed: 59 * time.Second, retired: true, clockStep: 2 * time.Second, removedFirst: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManagerWithOptions(Options{})
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			require.NoError(t, m.ApplyStatic(ctx, wide))
			if tc.removedFirst {
				for i := range 4 {
					addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(10 + i)})
					require.NoError(t, m.AddResolvedDomain(ctx, "remove.example.com", []ResolvedIP{{Addr: addr, TTL: time.Duration(i) * time.Second}}))
				}
			}
			require.NoError(t, m.AddResolvedDomain(ctx, keep, []ResolvedIP{{Addr: shared, TTL: time.Minute}}))
			active := []tcpConnection{{remote: shared, state: "ESTABLISHED"}}
			if tc.retired {
				m.applyDomainRefresh(ctx, keep, m.domains[keep], nil)
			}
			if tc.earlyTCP {
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
			}
			now = now.Add(tc.elapsed)
			if !tc.retired {
				m.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
					return nil, errors.New("resolver temporarily unavailable")
				})
				require.True(t, m.domains[keep].retryAt.After(now), "expired observation must be in retry backoff")
			}
			if !tc.removedFirst {
				require.NoError(t, m.AddResolvedDomain(ctx, "remove.example.com", []ResolvedIP{{Addr: shared, TTL: 300 * time.Second}}))
			}
			if tc.refreshTCP {
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
			}
			if tc.clockStep != 0 {
				m.tracker.now = func() time.Time {
					snapshotAt := now
					now = now.Add(tc.clockStep)
					return snapshotAt
				}
			}
			if tc.removedFirst {
				budget := now.Add(3 * tc.clockStep)
				m.run = func(ctx context.Context, script string) ([]byte, error) {
					require.False(t, now.After(budget), "expired revoked sources must not delay submission")
					return defaultRunner(ctx, script)
				}
			}
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, tight)
			require.NoError(t, err)
			m.run = defaultRunner
			m.tracker.now = func() time.Time { return now }
			leases := readKernelLeaseExpiry(t, ctx)
			remaining, retained := leases[shared.String()]
			require.Equal(t, tc.want == 0, retained, "only a still-live source may keep the shared grant")
			if tc.maxRemaining != 0 {
				require.Positive(t, remaining)
				require.LessOrEqual(t, remaining, tc.maxRemaining, "kernel timeout must use the remaining time after assembly")
			}
			if tc.removedFirst {
				require.Len(t, leases, 1)
				for i := range 4 {
					addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(10 + i)})
					require.True(t, m.lastDrops.MatchesFlow(addr, 443), "expired revoked sources remain teardown targets")
				}
			}
			require.Equal(t, tc.want, countMatchingConntrackFlowsData(flow, m.lastDrops),
				"the conntrack count must agree with whether the shared grant survives")
			require.NoError(t, m.ApplyStatic(ctx, tight))
			require.Equal(t, tc.want, countMatchingConntrackFlowsData(flow, m.lastDrops),
				"ordinary reload must not turn an expired source into a keep")
		})
	}
}

func TestNftUnknownPolicyApplyFencesDNSUntilVerifiedReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	allow, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, allow))
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		m.run = defaultRunner
		out, err := defaultRunner(ctx, script)
		if err != nil {
			return out, err
		}
		return out, &ApplyError{Effect: ApplyUnknown, Err: errors.New("lost nft apply acknowledgement")}
	}
	require.Error(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.4"), TTL: time.Minute}}
	require.Error(t, m.AddResolvedDomain(ctx, "example.com", ips), "an old-policy answer cannot be granted after an unacknowledged replacement")
	require.Empty(t, readKernelLeaseExpiry(t, ctx))
	require.NoError(t, m.ApplyStatic(ctx, allow))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", ips))
	require.Contains(t, readKernelLeaseExpiry(t, ctx), ips[0].Addr.String())
}

func readKernelLeaseExpiry(t *testing.T, ctx context.Context) map[string]int {
	t.Helper()

	data, err := exec.CommandContext(ctx, "nft", "-j", "list", "set", "inet", tableName, dynAllowV4Set).CombinedOutput()
	require.NoError(t, err, "%s", data)
	var result struct {
		Nftables []struct {
			Set struct {
				Elem []struct {
					Elem struct {
						Val     string
						Expires int
					}
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal(data, &result))
	leases := make(map[string]int)
	for _, entry := range result.Nftables {
		for _, wrapper := range entry.Set.Elem {
			leases[wrapper.Elem.Val] = wrapper.Elem.Expires
		}
	}
	return leases
}
