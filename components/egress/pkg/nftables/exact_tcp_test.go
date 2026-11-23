// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestApplyStatic_DNSOnlyHostKeepsEffectiveTeardown(t *testing.T) {
	const domain = `{"action":"allow","target":"cdn.example"}`
	const allowCIDR = `{"action":"allow","target":"192.0.2.0/24"}`
	const denyCIDR = `{"action":"deny","target":"192.0.2.0/24"}`
	const deny443 = `{"action":"deny","ports":[443]}`
	addr := netip.MustParseAddr("192.0.2.10")
	for _, tt := range []struct {
		name, rules string
		lateDNS     bool
		cutSibling  bool
	}{
		{"retained DNS under CIDR deny", domain + "," + denyCIDR, false, true},
		{"revoked DNS under CIDR allow", allowCIDR + "," + deny443, false, false},
		{"DNS learned after opt-in under CIDR deny", domain + "," + denyCIDR, true, true},
		{"revoked DNS is not a permit", deny443, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			m, _ := recordingManager()
			old := mustPolicy(t, `{"egress":[`+domain+`]}`)
			require.NoError(t, m.ApplyStatic(ctx, old))
			grant := func() {
				t.Helper()
				require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
			}
			if !tt.lateDNS {
				grant()
			}
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, mustPolicy(t, `{"egress":[`+tt.rules+`]}`))
			require.NoError(t, err)
			if tt.lateDNS {
				grant()
			}
			for i := 0; i < 3; i++ {
				if i != 0 {
					require.NoError(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
				}
				require.True(t, m.lastDrops.MatchesFlow(addr, 443), "port never reallowed, stage %d", i)
				require.Equal(t, tt.cutSibling, m.lastDrops.MatchesFlow(addr, 8443), "sibling, stage %d", i)
			}
			require.NoError(t, m.ApplyStatic(ctx, old))
			grant()
			require.NoError(t, m.ApplyStatic(ctx, old))
			require.False(t, m.lastDrops.MatchesFlow(addr, 443), "a fresh live grant reallows the stream")
		})
	}
}

func TestApplyStatic_PreviousDNSSubjectIsNotANewGrant(t *testing.T) {
	const domain = `{"action":"allow","target":"cdn.example"}`
	addr := netip.MustParseAddr("192.0.2.10")
	for _, action := range []string{"allow", "deny"} {
		t.Run(action, func(t *testing.T) {
			m, _ := recordingManager()
			ctx := context.Background()
			old := mustPolicy(t, `{"defaultAction":"`+action+`","egress":[`+domain+`]}`)
			require.NoError(t, m.ApplyStatic(ctx, old))
			require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
			first := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"192.0.2.0/24"}]}`)
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, first)
			require.NoError(t, err)
			require.True(t, m.lastDrops.MatchesFlow(addr, 443))
			require.NoError(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
			require.True(t, m.lastDrops.MatchesFlow(addr, 443), "old dynamic address identifies the withdrawn host")
			require.Empty(t, m.tracker.dynamicIPs, "old dynamic address is not a live permit")
		})
	}
}

func TestApplyStatic_DNSCleanupDoesNotForgetMaskedGrant(t *testing.T) {
	const domain = `{"action":"allow","target":"cdn.example"}`
	addr := netip.MustParseAddr("192.0.2.10")
	for _, poll := range []bool{false, true} {
		t.Run(strconv.FormatBool(poll), func(t *testing.T) {
			m, _ := recordingManager()
			now := time.Now()
			m.tracker.now = func() time.Time { return now }
			ctx := context.Background()
			old := mustPolicy(t, `{"egress":[`+domain+`,{"action":"allow","ports":[443,8443]}]}`)
			first := mustPolicy(t, `{"egress":[`+domain+`,{"action":"deny","target":"192.0.2.0/24","ports":[443]}]}`)
			require.NoError(t, m.ApplyStatic(ctx, old))
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, first)
			require.NoError(t, err)
			require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
			now = now.Add(2 * time.Minute)
			if poll {
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
			}
			for range 2 {
				require.NoError(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
				require.True(t, m.lastDrops.MatchesFlow(addr, 443), "poll must not retire TCP history")
				require.False(t, m.lastDrops.MatchesFlow(addr, 8443), "sibling was previously admitted")
			}
		})
	}
}

func TestApplyStatic_PollCannotTurnPastDNSAdmissionIntoTeardown(t *testing.T) {
	const domain = `{"action":"allow","target":"cdn.example"}`
	addr := netip.MustParseAddr("192.0.2.10")
	ctx := context.Background()
	for _, tt := range []struct {
		name         string
		poll, reopen bool
	}{{"before poll", false, false}, {"after poll", true, false}, {"last cut cleared after poll", true, true}} {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := recordingManager()
			now := time.Now()
			m.tracker.now = func() time.Time { return now }
			old := mustPolicy(t, `{"defaultAction":"allow"}`)
			first := mustPolicy(t, `{"egress":[`+domain+`]}`)
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, first)
			require.NoError(t, err)
			require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[`+domain+`,{"action":"deny","ports":[443]}]}`)))
			require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
			now = now.Add(2 * time.Minute)
			if tt.poll {
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
			}
			next := policy.DefaultDenyPolicy()
			if tt.reopen {
				next = mustPolicy(t, `{"egress":[{"action":"allow","ports":[443]}]}`)
			}
			require.NoError(t, m.ApplyStatic(ctx, next))
			require.Equal(t, !tt.reopen, m.lastDrops.MatchesFlow(addr, 443))
			require.False(t, m.lastDrops.MatchesFlow(addr, 8443), "the same confirmed grant actually admitted this port")
		})
	}
}

// Measure the full ruleset built on the next plain reload, not the preceding
// opt-in snapshot. The address history is prepared outside the timed loop.
func BenchmarkBuildRulesetRemovedDNS(b *testing.B) {
	for _, count := range []int{1024, 4096, 8192} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			p, history, base := removedDNSBenchmarkPlan(count)
			base.captureExactTCP(nil, p, history, nil)
			carried := base.stillRelevant(p, p, nil, knownDNS{}, nil)
			var size int
			b.ReportAllocs()
			for b.Loop() {
				script, err := buildRuleset(p, Options{}, carried)
				if err != nil {
					b.Fatal(err)
				}
				size = len(script)
			}
			b.ReportMetric(float64(size), "script-B")
		})
	}
}

func removedDNSBenchmarkPlan(count int) (*policy.NetworkPolicy, knownDNS, EstablishedDrops) {
	p := policy.DefaultDenyPolicy()
	ips := make([]netip.Addr, count)
	for i := range ips {
		ips[i] = netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)})
	}
	history := map[string][]netip.Addr{"removed.example": ips}
	ledger := &resolvedDomain{addresses: make(map[netip.Addr]domainLease, count)}
	for _, ip := range ips {
		ledger.addresses[ip] = domainLease{}
	}
	return p, knownDNS{domains: map[string]*resolvedDomain{"removed.example": ledger}}, ComputeEstablishedDrops(nil, p, history)
}

func BenchmarkCaptureExactTCPRemovedDNS(b *testing.B) {
	for _, count := range []int{1024, 4096, 8192} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			p, history, base := removedDNSBenchmarkPlan(count)
			b.ReportAllocs()
			for b.Loop() {
				d := base
				d.captureExactTCP(nil, p, history, nil)
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
