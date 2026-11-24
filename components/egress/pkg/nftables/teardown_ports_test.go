// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestRemovedHostPortsKeepOriginalRangeBoundaries(t *testing.T) {
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"192.0.2.1","ports":[443,80,53]}]}`)
	old.Egress[0].PortRanges = []policy.PortRange{{Lo: 1000, Hi: 1064}, {Lo: 1065, Hi: 1130}}
	next := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","ports":[53,443,1064,1065]}]}`)
	d := ComputeEstablishedDrops(old, next, nil)
	require.Len(t, d.PortDrops, 1)
	// Adjacent source ranges must not be merged before the legacy cutoff:
	// expand the short one, but retain the whole long one if any port is denied.
	require.Equal(t, []int{443, 53, 1064}, d.PortDrops[0].Ports)
	require.Equal(t, []policy.PortRange{{Lo: 1065, Hi: 1130}}, d.PortDrops[0].Ranges)
}

func BenchmarkComputeEstablishedDropsPortWindows(b *testing.B) {
	for _, width := range []int{1, 128} {
		b.Run(fmt.Sprintf("ports-%d", width), func(b *testing.B) {
			ports := make([]int, width)
			for i := range ports {
				ports[i] = 1000 + i
			}
			oldRules := make([]policy.EgressRule, 64)
			for i := range oldRules {
				oldRules[i] = policy.EgressRule{Action: policy.ActionAllow, Target: fmt.Sprintf("192.0.2.%d", i+1), Ports: ports}
			}
			deny := []policy.EgressRule{{Action: policy.ActionDeny, Ports: []int{1000}}}
			for i := range 128 {
				unrelated := make([]int, 128)
				for j := range unrelated {
					unrelated[j] = 20000 + i + j
				}
				deny = append(deny, policy.EgressRule{Action: policy.ActionDeny, Ports: unrelated})
			}
			parse := func(rules []policy.EgressRule) *policy.NetworkPolicy {
				data, err := json.Marshal(policy.NetworkPolicy{DefaultAction: policy.ActionAllow, Egress: rules})
				require.NoError(b, err)
				p, err := policy.ParsePolicy(string(data))
				require.NoError(b, err)
				return p
			}
			old, next := parse(oldRules), parse(deny)
			addr := netip.MustParseAddr("192.0.2.1")
			d := ComputeEstablishedDrops(old, next, nil)
			require.True(b, d.MatchesFlow(addr, 1000))
			require.False(b, d.MatchesFlow(addr, 1001))
			b.ReportAllocs()
			for b.Loop() {
				ComputeEstablishedDrops(old, next, nil)
			}
		})
	}
}
