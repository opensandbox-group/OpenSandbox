// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestApplyStatic_NamedHostFollowsItsEffectiveTCPVerdict(t *testing.T) {
	const whole = `{"action":"allow","target":"192.0.2.10"}`
	const scoped = `{"action":"allow","target":"192.0.2.10/32","ports":[8443]}`
	const deny443 = `{"action":"deny","target":"192.0.2.0/24","ports":[443]}`
	const denyCIDR = `{"action":"deny","target":"192.0.2.0/24"}`
	const allow8443 = `{"action":"allow","ports":[8443]}`
	addr := netip.MustParseAddr("192.0.2.10")
	ctx := context.Background()
	for _, named := range []struct{ name, rule string }{{"whole", whole}, {"scoped", scoped}} {
		for _, stage := range []string{"old", "opt-in", "plain"} {
			for _, block := range []struct{ name, action, rule string }{
				{"scoped-deny", "allow", deny443}, {"whole-deny", "allow", denyCIDR}, {"catch-all", "deny", allow8443},
			} {
				t.Run(named.name+"/"+stage+"/"+block.name, func(t *testing.T) {
					makePolicy := func(action, when string, rules ...string) *policy.NetworkPolicy {
						if when == stage {
							rules = append(rules, named.rule)
						}
						return mustPolicy(t, `{"defaultAction":"`+action+`","egress":[`+strings.Join(rules, ",")+`]}`)
					}
					m, _ := recordingManager()
					_, err := m.ApplyStaticComputingEstablishedDrops(ctx, makePolicy("allow", "old"), makePolicy(block.action, "opt-in", block.rule))
					require.NoError(t, err)
					// Only a whole-address allow present at opt-in can spare 443 from the catch-all.
					blocked := !(block.name == "catch-all" && stage == "opt-in" && named.rule == whole)
					require.Equal(t, blocked, m.lastDrops.MatchesFlow(addr, 443))
					require.Equal(t, block.name == "whole-deny", m.lastDrops.MatchesFlow(addr, 8443))

					// Remove the original source of teardown, then the replacement rule too.
					// The first reload permits 8443; the second permits no new connections.
					for _, next := range []*policy.NetworkPolicy{makePolicy("allow", "plain", deny443), policy.DefaultDenyPolicy()} {
						require.NoError(t, m.ApplyStatic(ctx, next))
						require.Equal(t, blocked, m.lastDrops.MatchesFlow(addr, 443), "retain only the previously blocked port")
						require.False(t, m.lastDrops.MatchesFlow(addr, 8443), "plain reload must not cut an admitted port")
					}
					require.NoError(t, m.ApplyStatic(ctx, makePolicy("allow", "")))
					require.NoError(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
					require.False(t, m.lastDrops.MatchesFlow(addr, 443), "a genuine reallow retires the old block")
				})
			}
		}
	}
}

func TestApplyStatic_LaterHostAllowsSpareOnlyTheirUnion(t *testing.T) {
	ctx := context.Background()
	addr := netip.MustParseAddr("192.0.2.10")
	m, _ := recordingManager()
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	first := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","ports":[80,81,82,83,84]}]}`)
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, first)
	require.NoError(t, err)
	partial := mustPolicy(t, `{"egress":[{"action":"allow","target":"192.0.2.10","ports":[82,84]},{"action":"allow","target":"192.0.2.10/32","ports":[80,82]}]}`)
	for _, p := range []*policy.NetworkPolicy{partial, policy.DefaultDenyPolicy()} {
		require.NoError(t, m.ApplyStatic(ctx, p))
		for port := 80; port <= 84; port++ {
			require.Equal(t, port == 81 || port == 83, m.lastDrops.MatchesFlow(addr, port), "port %d", port)
		}
	}
}

func BenchmarkCaptureExactTCPRemovedDNS(b *testing.B) {
	p := policy.DefaultDenyPolicy()
	for _, count := range []int{1024, 4096, 8192} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			ips := make([]netip.Addr, count)
			for i := range ips {
				ips[i] = netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)})
			}
			history := map[string][]netip.Addr{"removed.example": ips}
			base := ComputeEstablishedDrops(nil, p, history)
			b.ReportAllocs()
			for b.Loop() {
				d := base
				d.captureExactTCP(nil, p, nil)
			}
		})
	}
}

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
		{"scoped host added on opt-in", `{"action":"allow","target":"192.0.2.10","ports":[443,8443]},` + floor, ""},
		{"scoped singleton CIDR added on opt-in", `{"action":"allow","target":"192.0.2.10/32","ports":[443,8443]},` + floor, ""},
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
