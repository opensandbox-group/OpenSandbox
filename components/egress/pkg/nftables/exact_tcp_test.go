// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyStatic_KeepsExactWithdrawalsAcrossBroadCoverage(t *testing.T) {
	const host = `{"action":"allow","target":"192.0.2.10"}`
	const floor = `{"action":"deny","ports":[443]}`
	const covered = `{"action":"deny","target":"192.0.2.0/24","ports":[443]}`
	const exact = `{"action":"deny","target":"192.0.2.10","ports":[443]}`
	const exactOther = `{"action":"deny","target":"192.0.2.10","ports":[8443]}`
	addr := netip.MustParseAddr("192.0.2.10")
	for _, tt := range []struct{ name, first, reload string }{
		{"redundant exact scope", floor, covered + "," + exact},
		{"host added on opt-in", host + "," + floor, ""},
		{"only implicit withdrawal", floor, exactOther},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m, _ := recordingManager()
			old := mustPolicy(t, `{"defaultAction":"allow"}`)
			first := mustPolicy(t, `{"defaultAction":"allow","egress":[`+tt.first+`]}`)
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, first)
			require.NoError(t, err)
			require.True(t, m.lastDrops.MatchesFlow(addr, 443))
			for _, rules := range []string{tt.reload, ""} {
				require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[`+rules+`]}`)))
				require.True(t, m.lastDrops.MatchesFlow(addr, 443), "previously denied port is still denied")
				require.False(t, m.lastDrops.MatchesFlow(addr, 8443), "ordinary reload must not cut a previously admitted port")
			}
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

func TestApplyStatic_ElidesExactPortsAlreadyRenderedByAncestor(t *testing.T) {
	for _, ancestor := range []string{``, `"target":"192.0.2.0/24",`} {
		ctx := context.Background()
		m, scripts := recordingManager()
		old := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"allow","target":"192.0.2.10"},{"action":"allow","target":"192.0.2.11"}]}`)
		p := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny",`+ancestor+`"ports":[9000,9001]},{"action":"deny","target":"192.0.2.10","ports":[443]},{"action":"deny","target":"192.0.2.11","ports":[444]}]}`)
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, p)
		require.NoError(t, err)
		require.NoError(t, m.ApplyStatic(ctx, p))
		script := (*scripts)[len(*scripts)-1]
		beforeDNS := script[:ruleIndex(t, script, "ip daddr @dyn_allow_v4 ct state established,related accept")]
		require.Equal(t, 1, strings.Count(beforeDNS, "9000-9001"), "ancestor already renders the common ports")
		require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[]}`)))
		for _, host := range []string{"192.0.2.10", "192.0.2.11"} {
			require.True(t, m.lastDrops.MatchesFlow(netip.MustParseAddr(host), 9000), "render deduplication must not erase history")
		}
	}
}
