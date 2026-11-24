// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCountMatchingConntrackFlowsDataPreservesDestinationFamily(t *testing.T) {
	native := "ipv4 2 tcp 6 299 ESTABLISHED src=192.0.2.20 dst=192.0.2.10 sport=48000 dport=443\n"
	mapped := "ipv6 10 tcp 6 299 ESTABLISHED src=2001:db8::20 dst=::ffff:192.0.2.10 sport=48000 dport=443\n"
	for _, tt := range []struct{ target, matched, spared string }{
		{"::ffff:192.0.2.10/128", mapped, native},
		{"192.0.2.10", native, mapped},
	} {
		t.Run(tt.target, func(t *testing.T) {
			p := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"`+tt.target+`","ports":[443]}]}`)
			drops := ComputeEstablishedDrops(nil, p, nil)
			require.Equal(t, 1, countMatchingConntrackFlowsData([]byte(tt.matched), drops))
			require.Zero(t, countMatchingConntrackFlowsData([]byte(tt.spared), drops))
		})
	}
}

func TestApplyStatic_MappedExactRulesKeepTheirFamily(t *testing.T) {
	mapped := netip.MustParseAddr("::ffff:192.0.2.10")
	native := mapped.Unmap()
	for _, target := range []string{mapped.String(), mapped.String() + "/128"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			m, _ := recordingManager()
			old := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"allow","target":"192.0.2.10"},{"action":"allow","target":"::ffff:192.0.2.10/128"}]}`)
			for _, scope := range []string{`", "ports":[443]`, `"`} {
				p := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"`+target+scope+`}]}`)
				_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, p)
				require.NoError(t, err)
				for i := range 2 {
					require.True(t, m.lastDrops.MatchesFlow(mapped, 443))
					require.False(t, m.lastDrops.MatchesFlow(native, 443))
					if i == 0 {
						require.NoError(t, m.ApplyStatic(ctx, p))
					}
				}
			}
			p := mustPolicy(t, `{"egress":[{"action":"allow","target":"`+target+`","ports":[443]}]}`)
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, p)
			require.NoError(t, err)
			require.False(t, m.lastDrops.MatchesFlow(mapped, 443))
			require.True(t, m.lastDrops.MatchesFlow(native, 443))
			require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[]}`)))
			require.False(t, m.lastDrops.MatchesFlow(mapped, 443), "the mapped keep previously admitted this port")
			require.True(t, m.lastDrops.MatchesFlow(native, 443), "the mapped keep never admitted native IPv4")
		})
	}
}
