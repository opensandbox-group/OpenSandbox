// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWholeDNSHistoryCapacityIsTransactional(t *testing.T) {
	ctx := t.Context()
	writes := 0
	m := NewManagerWithRunnerAndOptions(func(context.Context, string) ([]byte, error) {
		writes++
		return nil, nil
	}, Options{})
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	grow, merge := netip.MustParseAddr("198.18.0.0"), netip.MustParseAddr("198.18.0.1")
	answer := []ResolvedIP{{Addr: grow}, {Addr: merge}}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", answer[:1]))

	// Model acknowledged history at capacity, without a two-million-element nft fixture.
	// Each host owns 255 disjoint TCP intervals and the portless companion.
	const hosts = maxResolvedDomains * maxDomainAddresses
	prior := wholeDNSEstablishedKeep{ports: make(tcpSpans, maxWholeDNSHistoryEntries/hosts-1), other: true}
	for i := range prior.ports {
		prior.ports[i].Lo, prior.ports[i].Hi = 2*i+1, 2*i+1
	}
	extra := netip.MustParseAddr("2001:db8::1")
	d := EstablishedDrops{guardWholeDNS: true, wholeDNSKeeps: make(map[netip.Addr]wholeDNSEstablishedKeep, hosts), PortDrops: []EstablishedPortDrop{
		{Addr: grow, Ports: []int{65535}}, // One additional disjoint interval.
		{Addr: merge, Ports: []int{2}},    // Connects existing intervals 1 and 3.
		{Addr: extra, Ports: []int{443}},
	}}
	for i := range hosts {
		d.wholeDNSKeeps[netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)})] = prior
	}
	m.setLastDropsLocked(d, m.now())
	r := m.wholeDNSReadmission
	require.Equal(t, maxWholeDNSHistoryEntries, r.spans)
	// Both traversal orders must fit. Do not let the randomized Go map order
	// decide whether this regression actually exercises growth before coalescing.
	for _, order := range [][]netip.Addr{{grow, merge}, {merge, grow}} {
		plan := wholeDNSReadmissionPlan{hosts: hosts, spans: r.spans}
		for _, addr := range order {
			require.NoError(t, plan.stage(d, addr, r.admitted(addr, r.uncovered(d, addr))))
		}
		require.Equal(t, maxWholeDNSHistoryEntries, plan.spans)
	}

	before := writes
	require.ErrorContains(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: extra}}), "too many whole DNS hosts")
	require.NotContains(t, m.tracker.dynamicIPs, extra)
	require.ErrorContains(t, m.AddResolvedDomain(ctx, "a.example", answer[:1]), "too many whole DNS established exceptions")
	require.ErrorContains(t, m.ApplyStatic(ctx, p), "too many whole DNS established exceptions")
	require.Equal(t, before, writes, "a capacity refusal must not reach nft")
	require.True(t, m.lastDrops.MatchesFlow(grow, 65535))
	require.True(t, m.lastDrops.MatchesFlow(merge, 2))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", answer))
	require.Equal(t, before+1, writes, "the two grants belong to one transaction")
	require.False(t, m.lastDrops.MatchesFlow(grow, 65535))
	require.False(t, m.lastDrops.MatchesFlow(merge, 2))
	require.Equal(t, maxWholeDNSHistoryEntries, m.wholeDNSReadmission.spans)
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"allow"}`)))
	require.Empty(t, m.lastDrops.wholeDNSKeeps, "retiring the withdrawals releases their budget")
}
