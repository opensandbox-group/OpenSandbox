// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
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

func TestSinglePortWithdrawalInEitherHostOrder(t *testing.T) {
	a, b := "192.0.2.7", "192.0.2.8"
	next := mustPolicy(t, `{"egress":[{"action":"allow","target":"192.0.2.0/24","ports":[443,444]},{"action":"deny","target":"192.0.2.7","ports":[8443,443]}]}`)
	next.Egress = append(next.Egress, policy.NewPortFloorDenyRule([]policy.PortRange{{Lo: 444, Hi: 445}}))
	for _, port := range []int{443, 444} {
		for _, order := range [][2]string{{a, b}, {b, a}} {
			old := mustPolicy(t, fmt.Sprintf(`{"egress":[{"action":"allow","target":"%s","ports":[%d]},{"action":"allow","target":"%s","ports":[%d]}]}`, order[0], port, order[1], port))
			d := ComputeEstablishedDrops(old, next, nil)
			withdrawn := map[string][]int{}
			for _, pd := range d.PortDrops {
				withdrawn[pd.Addr.String()] = pd.Ports
			}
			want := map[string][]int{a: {port}}
			if port == 444 {
				want[b] = []int{port}
			}
			require.Equal(t, want, withdrawn, "host order %v, port %d", order, port)
		}
	}
}

func TestRemovedPortsInPartiallyAdmittedWindows(t *testing.T) {
	const hostRule = `{"action":"allow","target":"192.0.2.7","ports":[65535,80,81,443,444,445,446,65534]}`
	host := netip.MustParseAddr("192.0.2.7")
	next := mustPolicy(t, `{"egress":[{"action":"allow","target":"192.0.2.0/24","ports":[443,445,65534]}]}`)
	for _, priorHosts := range []int{0, 128} {
		var rules []string
		for i := range priorHosts {
			// These earlier queries must not change the decision for .7.
			rules = append(rules, fmt.Sprintf(`{"action":"allow","target":"198.18.0.%d","ports":[443]}`, i+1))
		}
		old := mustPolicy(t, `{"egress":[`+strings.Join(append(rules, hostRule), ",")+`]}`)
		d := ComputeEstablishedDrops(old, next, nil)
		var got []int
		for _, pd := range d.PortDrops {
			if pd.Addr == host {
				got = pd.Ports
			}
		}
		require.Equal(t, []int{65535, 80, 81, 444, 446}, got, "after %d earlier host queries", priorHosts)
	}
}

func benchmarkPortPolicy(b *testing.B, action string, rules []policy.EgressRule) *policy.NetworkPolicy {
	b.Helper()
	data, err := json.Marshal(policy.NetworkPolicy{DefaultAction: action, Egress: rules})
	require.NoError(b, err)
	p, err := policy.ParsePolicy(string(data))
	require.NoError(b, err)
	return p
}

// Remove host A's allow, retaining all unrelated denies. In keep-B only A
// is removed, but the evaluator must also decide whether B still has access.
func BenchmarkComputeEstablishedDropsSingleHostDelete(b *testing.B) {
	for _, tc := range []struct {
		name         string
		rules, width int
		shuffle      bool
		portsA       []int
		portB        int
		keepB        bool
	}{
		{"many-short", 256, 1, false, []int{443}, 0, false},
		{"long-sorted", 64, 128, false, []int{443}, 0, false},
		{"long-unsorted", 64, 128, true, []int{443}, 0, false},
		{"long-unsorted/two-ports", 64, 128, true, []int{80, 443}, 0, false},
		{"long-unsorted/two-hosts", 64, 128, true, []int{443}, 8443, false},
		{"long-unsorted/keep-B", 64, 128, true, []int{443}, 8443, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			deny := make([]policy.EgressRule, tc.rules)
			for i := range deny {
				ports := make([]int, tc.width)
				for j := range ports {
					idx := j
					if tc.shuffle {
						idx = (37 * j) % tc.width
					}
					ports[j] = 20000 + 2*idx + i
				}
				deny[i] = policy.EgressRule{Action: policy.ActionDeny, Ports: ports}
			}
			host := netip.MustParseAddr("192.0.2.7")
			hostB := netip.MustParseAddr("192.0.2.8")
			allow := policy.EgressRule{Action: policy.ActionAllow, Target: host.String(), Ports: tc.portsA}
			oldRules := []policy.EgressRule{allow}
			nextRules := deny
			if tc.portB != 0 {
				other := policy.EgressRule{Action: policy.ActionAllow, Target: hostB.String(), Ports: []int{tc.portB}}
				oldRules = append(oldRules, other)
				if tc.keepB {
					nextRules = append(nextRules, other)
				}
			}
			old := benchmarkPortPolicy(b, policy.ActionDeny, append(oldRules, deny...))
			next := benchmarkPortPolicy(b, policy.ActionDeny, nextRules)
			require.Equal(b, deny[0].Ports, next.Egress[0].Ports)
			d := ComputeEstablishedDrops(old, next, nil)
			for _, port := range tc.portsA {
				require.True(b, d.MatchesFlow(host, port))
			}
			if tc.portB != 0 {
				require.Equal(b, !tc.keepB, d.MatchesFlow(hostB, tc.portB))
			}
			require.False(b, d.MatchesFlow(host, 8443))
			b.ReportAllocs()
			for b.Loop() {
				ComputeEstablishedDrops(old, next, nil)
			}
		})
	}
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
			old := benchmarkPortPolicy(b, policy.ActionAllow, oldRules)
			next := benchmarkPortPolicy(b, policy.ActionAllow, deny)
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
