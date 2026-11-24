// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"net/netip"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestApplyStatic_CarriesRemovedSingletonAsHost(t *testing.T) {
	native := netip.MustParseAddr("192.0.2.7")
	mapped := netip.MustParseAddr("::ffff:192.0.2.7")
	for _, pair := range [][2]netip.Addr{{native, mapped}, {mapped, native}} {
		host, other := pair[0], pair[1]
		for _, target := range []string{host.String(), netip.PrefixFrom(host, host.BitLen()).String()} {
			for _, tc := range []struct {
				name, oldScope, denyScope string
				whole                     bool
			}{
				{"whole", "", "", true},
				{"partial-port-reallow", `,"ports":[443,8443]`, `,"ports":[443]`, false},
			} {
				t.Run(target+"/"+tc.name, func(t *testing.T) {
					m, _ := recordingManager()
					ctx := t.Context()
					old := mustPolicy(t, `{"egress":[{"action":"allow","target":"`+target+`"`+tc.oldScope+`}]}`)
					_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, policy.DefaultDenyPolicy())
					require.NoError(t, err)
					require.True(t, m.lastDrops.MatchesFlow(host, 8443), "8443 was part of the withdrawal")
					require.False(t, m.lastDrops.MatchesFlow(other, 443), "the other family was not withdrawn")
					next := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"`+target+`"`+tc.denyScope+`}]}`)
					for range 2 {
						require.NoError(t, m.ApplyStatic(ctx, next))
						require.True(t, m.lastDrops.MatchesFlow(host, 443), "still explicitly denied")
						require.Equal(t, tc.whole, m.lastDrops.MatchesFlow(host, 8443), "only the portless deny still cuts 8443")
						require.Equal(t, tc.whole, m.lastDrops.Matches(host), "retain the portless withdrawal, not a TCP-only substitute")
						require.False(t, m.lastDrops.MatchesFlow(other, 443))
					}
				})
			}
		}
	}
}
