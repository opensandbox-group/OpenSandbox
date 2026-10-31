// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nftables

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestComputeEstablishedDrops_RemovedDomainUsesLearnedIPs(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, map[string][]netip.Addr{
		"deb.debian.org": {netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("2001:db8::10")},
	})
	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("203.0.113.10"),
		netip.MustParseAddr("2001:db8::10"),
	}, drops.Addrs)
	require.Empty(t, drops.Prefixes)
	require.False(t, drops.CatchAll)
}

func TestComputeEstablishedDrops_KeepsSurvivingDomainLease(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"},{"action":"allow","target":"pypi.org"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org"}]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, map[string][]netip.Addr{
		"deb.debian.org": {netip.MustParseAddr("203.0.113.10")},
		"pypi.org":       {netip.MustParseAddr("198.51.100.2")},
	})
	require.Equal(t, []netip.Addr{netip.MustParseAddr("203.0.113.10")}, drops.Addrs)
}

func TestComputeEstablishedDrops_SharedCDNIPKept(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"cdn-a.example"},{"action":"allow","target":"cdn-b.example"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"cdn-b.example"}]}`)
	require.NoError(t, err)

	shared := netip.MustParseAddr("203.0.113.50")
	drops := ComputeEstablishedDrops(oldPol, newPol, map[string][]netip.Addr{
		"cdn-a.example": {shared},
		"cdn-b.example": {shared},
	})
	require.Empty(t, drops.Addrs, "shared CDN IP must survive with the remaining domain lease")
}

func TestComputeEstablishedDrops_RemovedDomainSkipsExplicitIPAllow(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"cdn.example"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"203.0.113.50"}]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, map[string][]netip.Addr{
		"cdn.example": {netip.MustParseAddr("203.0.113.50")},
	})
	require.Empty(t, drops.Addrs)
}

func TestComputeEstablishedDrops_RemovedStaticIPAndCIDR(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"},{"action":"allow","target":"203.0.113.0/24"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, drops.Addrs)
	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, drops.Prefixes)
}

func TestComputeEstablishedDrops_CIDRTightenKeepsRetainedSubnet(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"203.0.113.0/24"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"203.0.113.0/25"}]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Empty(t, drops.Addrs)
	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("203.0.113.128/25")}, drops.Prefixes)
	require.False(t, drops.Matches(netip.MustParseAddr("203.0.113.10")))
	require.True(t, drops.Matches(netip.MustParseAddr("203.0.113.200")))
}

func TestComputeEstablishedDrops_NewDenyCutsDefaultAllow(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"deny","target":"9.9.9.9"}]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("9.9.9.9")}, drops.DenyAddrs)
	require.Empty(t, drops.Addrs)
	require.False(t, drops.CatchAll)
	require.True(t, drops.Matches(netip.MustParseAddr("9.9.9.9")))
}

func TestComputeEstablishedDrops_AllowToDenyCatchAll(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"allow","egress":[]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.True(t, drops.CatchAll)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, drops.KeepAddrs)
	require.False(t, drops.Empty())
	require.False(t, drops.Matches(netip.MustParseAddr("1.1.1.1")))
	require.True(t, drops.Matches(netip.MustParseAddr("8.8.8.8")))
}

func TestComputeEstablishedDrops_RemovedPortScopedAllow(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1","ports":[443]}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Empty(t, drops.Addrs)
	require.Len(t, drops.PortDrops, 1)
	require.Equal(t, netip.MustParseAddr("1.1.1.1"), drops.PortDrops[0].Addr)
	require.Equal(t, []int{443}, drops.PortDrops[0].Ports)
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("1.1.1.1"), 443))
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("1.1.1.1"), 80))
}

func TestComputeEstablishedDrops_PortScopedAllowRetained(t *testing.T) {
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1","ports":[443,80]}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1","ports":[443]}]}`)
	require.NoError(t, err)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Len(t, drops.PortDrops, 1)
	require.Equal(t, []int{80}, drops.PortDrops[0].Ports)
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("1.1.1.1"), 443))
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("1.1.1.1"), 80))
}

func TestApplyStaticDropEstablished_EmitsDropsBeforeEstablishedAccept(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	matched, err := m.ApplyStaticDropEstablished(context.Background(), p, EstablishedDrops{
		Addrs:    []netip.Addr{netip.MustParseAddr("203.0.113.10")},
		Prefixes: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
	})
	require.NoError(t, err)
	require.Equal(t, 0, matched) // no /proc conntrack in unit test
	require.Len(t, scripts, 1)

	script := scripts[0]
	dynAllow := strings.Index(script, "ip daddr @dyn_allow_v4 ct state established,related accept")
	dropIP := strings.Index(script, "ip daddr 203.0.113.10 ct state established,related drop")
	dropCIDR := strings.Index(script, "ip daddr 198.51.100.0/24 ct state established,related drop")
	established := strings.Index(script, "ct state established,related accept\n")
	// Find the blanket accept (last established accept without daddr).
	blanket := strings.LastIndex(script, "add rule inet opensandbox egress ct state established,related accept\n")
	require.GreaterOrEqual(t, dynAllow, 0, script)
	require.GreaterOrEqual(t, dropIP, 0, script)
	require.GreaterOrEqual(t, dropCIDR, 0, script)
	require.GreaterOrEqual(t, established, 0, script)
	require.GreaterOrEqual(t, blanket, 0, script)
	require.Less(t, dynAllow, dropIP, "dyn allow must bypass teardown drops")
	require.Less(t, dropIP, blanket)
	require.Less(t, dropCIDR, blanket)
}

func TestApplyStaticDropEstablished_CatchAllEmitsDrop(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`)
	require.NoError(t, err)

	_, err = m.ApplyStaticDropEstablished(context.Background(), p, EstablishedDrops{
		CatchAll:  true,
		KeepAddrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	})
	require.NoError(t, err)
	script := scripts[0]
	require.Contains(t, script, "ip daddr 1.1.1.1 ct state established,related accept")
	require.Contains(t, script, "add rule inet opensandbox egress ct state established,related drop\n")
	require.NotContains(t, script, "add rule inet opensandbox egress ct state established,related accept\n")
}

func TestApplyStaticDropEstablished_PortDropRule(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	_, err = m.ApplyStaticDropEstablished(context.Background(), p, EstablishedDrops{
		PortDrops: []EstablishedPortDrop{{
			Addr:  netip.MustParseAddr("1.1.1.1"),
			Ports: []int{443},
		}},
	})
	require.NoError(t, err)
	require.Contains(t, scripts[0], "ip daddr 1.1.1.1 tcp dport { 443 } ct state established,related drop")
}

func TestApplyStaticComputingEstablishedDrops_AtomicWithDomainMap(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	oldPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"deb.debian.org"}]}`)
	require.NoError(t, err)
	newPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)

	// Seed domain map as if DNS already learned the IP under the old policy.
	require.NoError(t, m.ApplyStatic(context.Background(), oldPol))
	m.mu.Lock()
	m.domains["deb.debian.org"] = &resolvedDomain{
		addresses: map[netip.Addr]struct{}{netip.MustParseAddr("203.0.113.10"): {}},
	}
	m.mu.Unlock()

	matched, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	require.Equal(t, 0, matched)
	require.GreaterOrEqual(t, len(scripts), 2)
	require.Contains(t, scripts[len(scripts)-1], "ip daddr 203.0.113.10 ct state established,related drop")
}

func TestCountMatchingConntrackFlowsData(t *testing.T) {
	data := []byte(`
ipv4     2 tcp      6 431999 ESTABLISHED src=10.0.0.2 dst=203.0.113.10 sport=12345 dport=80 src=203.0.113.10 dst=10.0.0.2 sport=80 dport=12345 [ASSURED] mark=0 zone=0 use=2
ipv4     2 tcp      6 431999 ESTABLISHED src=10.0.0.2 dst=1.1.1.1 sport=12346 dport=443 src=1.1.1.1 dst=10.0.0.2 sport=443 dport=12346 [ASSURED] mark=0 zone=0 use=2
ipv4     2 udp      17 30 src=10.0.0.2 dst=203.0.113.10 sport=53 dport=53 [UNREPLIED] src=203.0.113.10 dst=10.0.0.2 sport=53 dport=53 mark=0 zone=0 use=2
`)
	n := countMatchingConntrackFlowsData(data, EstablishedDrops{
		Addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")},
	})
	require.Equal(t, 1, n)

	n = countMatchingConntrackFlowsData(data, EstablishedDrops{
		PortDrops: []EstablishedPortDrop{{
			Addr:  netip.MustParseAddr("1.1.1.1"),
			Ports: []int{443},
		}},
	})
	require.Equal(t, 1, n)

	n = countMatchingConntrackFlowsData(data, EstablishedDrops{
		PortDrops: []EstablishedPortDrop{{
			Addr:  netip.MustParseAddr("1.1.1.1"),
			Ports: []int{80},
		}},
	})
	require.Equal(t, 0, n)
}

func TestPrefixDiff_Basic(t *testing.T) {
	got := prefixDiff(netip.MustParsePrefix("203.0.113.0/24"), []netip.Prefix{netip.MustParsePrefix("203.0.113.0/25")})
	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("203.0.113.128/25")}, got)
}

func mustPolicy(t *testing.T, raw string) *policy.NetworkPolicy {
	t.Helper()
	p, err := policy.ParsePolicy(raw)
	require.NoError(t, err)
	return p
}

func recordingManager() (*Manager, *[]string) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	return m, &scripts
}

func ruleIndex(t *testing.T, script, rule string) int {
	t.Helper()
	i := strings.Index(script, "add rule inet opensandbox egress "+rule+"\n")
	require.GreaterOrEqual(t, i, 0, "missing rule %q in:\n%s", rule, script)
	return i
}

// DNS replies from the :15353 redirect, loopback TCP and marked proxy upstream
// flows are ct established; they must be accepted before any teardown drop.
func TestBuildRuleset_InfraAcceptsPrecedeEstablishedTeardown(t *testing.T) {
	m, scripts := recordingManager()
	oldPol := mustPolicy(t, `{"defaultAction":"allow","egress":[]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"},{"action":"deny","target":"9.9.9.9"}]}`)
	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	script := (*scripts)[len(*scripts)-1]

	firstDrop := ruleIndex(t, script, "ip daddr 9.9.9.9 ct state established,related drop")
	blanketDrop := ruleIndex(t, script, "ct state established,related drop")
	for _, infra := range []string{
		"meta mark 0x1 accept",
		`oifname "lo" accept`,
		"ip daddr 127.0.0.1 udp dport 15353 accept",
		"ip daddr 127.0.0.1 tcp dport 15353 accept",
		"ip6 daddr ::1 accept",
	} {
		i := ruleIndex(t, script, infra)
		require.Less(t, i, firstDrop, "%q must precede teardown drops", infra)
		require.Less(t, i, blanketDrop, "%q must precede the catch-all drop", infra)
	}

	// Conntrack accounting mirrors the chain: loopback and marked flows are
	// never counted as torn down.
	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("127.0.0.1"), 15353))
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("::1"), 15353))
	data := []byte(`
ipv4     2 tcp      6 431999 ESTABLISHED src=10.0.0.2 dst=8.8.8.8 sport=1 dport=443 src=8.8.8.8 dst=10.0.0.2 sport=443 dport=1 [ASSURED] mark=1 zone=0 use=2
ipv4     2 tcp      6 431999 ESTABLISHED src=127.0.0.1 dst=127.0.0.1 sport=2 dport=15353 src=127.0.0.1 dst=127.0.0.1 sport=15353 dport=2 [ASSURED] mark=0 zone=0 use=2
ipv4     2 tcp      6 431999 ESTABLISHED src=10.0.0.2 dst=8.8.4.4 sport=3 dport=443 src=8.8.4.4 dst=10.0.0.2 sport=443 dport=3 [ASSURED] mark=0 zone=0 use=2
`)
	require.Equal(t, 1, countMatchingConntrackFlowsData(data, drops))
}

func TestComputeEstablishedDrops_AllowDenyOverlapTearsDownDeny(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.0/24"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.0/24"},{"action":"deny","target":"1.2.3.4"}]}`)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, drops.DenyAddrs)
	require.True(t, drops.Matches(netip.MustParseAddr("1.2.3.4")))
	require.False(t, drops.Matches(netip.MustParseAddr("1.2.3.5")))
	for _, keep := range drops.KeepPrefixes {
		require.False(t, keep.Contains(netip.MustParseAddr("1.2.3.4")), "keep %s must exclude the new deny", keep)
	}
	require.NotContains(t, drops.KeepAddrs, netip.MustParseAddr("1.2.3.4"))

	m, scripts := recordingManager()
	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	script := (*scripts)[len(*scripts)-1]
	deny := ruleIndex(t, script, "ip daddr 1.2.3.4 ct state established,related drop")
	require.Less(t, ruleIndex(t, script, "ip daddr @dyn_allow_v4 ct state established,related accept"), deny)
	require.Less(t, deny, ruleIndex(t, script, "ip daddr @allow_v4 ct state established,related accept"),
		"deny teardown must precede the static allow established accept")
}

func TestComputeEstablishedDrops_CatchAllKeepsAreDenyAware(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"allow","egress":[]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.0/24"},{"action":"deny","target":"1.2.3.4"}]}`)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.True(t, drops.CatchAll)
	require.True(t, drops.Matches(netip.MustParseAddr("1.2.3.4")))
	require.False(t, drops.Matches(netip.MustParseAddr("1.2.3.200")))
	require.True(t, drops.Matches(netip.MustParseAddr("8.8.8.8")))
	for _, keep := range drops.KeepPrefixes {
		require.False(t, keep.Contains(netip.MustParseAddr("1.2.3.4")))
	}
}

// old {allow 10.0.0.0/8, allow example.com} → new {allow example.com}: the
// prefix drop must not cut example.com's lease IP inside 10/8.
func TestComputeEstablishedDrops_SurvivingDomainLeaseInsideRemovedPrefix(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"10.0.0.0/8"},{"action":"allow","target":"example.com"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	lease := netip.MustParseAddr("10.1.2.3")

	drops := ComputeEstablishedDrops(oldPol, newPol, map[string][]netip.Addr{"example.com": {lease}})
	require.False(t, drops.CatchAll)
	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, drops.Prefixes)
	require.Contains(t, drops.KeepAddrs, lease)
	require.False(t, drops.Matches(lease))
	require.True(t, drops.Matches(netip.MustParseAddr("10.9.9.9")))

	m, scripts := recordingManager()
	require.NoError(t, m.ApplyStatic(context.Background(), oldPol))
	m.mu.Lock()
	m.domains["example.com"] = &resolvedDomain{addresses: map[netip.Addr]struct{}{lease: {}}}
	m.mu.Unlock()
	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	script := (*scripts)[len(*scripts)-1]
	require.Less(t,
		ruleIndex(t, script, "ip daddr 10.1.2.3 ct state established,related accept"),
		ruleIndex(t, script, "ip daddr 10.0.0.0/8 ct state established,related drop"))
}

func TestComputeEstablishedDrops_SurvivingDomainLeaseInsideRemovedPortPrefix(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"10.0.0.0/8","ports":[443]},{"action":"allow","target":"example.com"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	lease := netip.MustParseAddr("10.1.2.3")

	drops := ComputeEstablishedDrops(oldPol, newPol, map[string][]netip.Addr{"example.com": {lease}})
	require.Len(t, drops.PortDrops, 1)
	require.False(t, drops.MatchesFlow(lease, 443))
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("10.9.9.9"), 443))
}

func TestComputeEstablishedDrops_CatchAllKeepsPortOnlyAllow(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"allow","egress":[]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","ports":[443]}]}`)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.True(t, drops.CatchAll)
	require.Len(t, drops.KeepPorts, 1)
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("8.8.8.8"), 443))
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("8.8.8.8"), 80))

	m, scripts := recordingManager()
	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	script := (*scripts)[len(*scripts)-1]
	require.Less(t,
		ruleIndex(t, script, "tcp dport { 443 } ct state established,related accept"),
		ruleIndex(t, script, "ct state established,related drop"))
}

func TestComputeEstablishedDrops_PortlessToPortScopedAllowKeepsPort(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4","ports":[443]}]}`)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("1.2.3.4")}, drops.Addrs)
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("1.2.3.4"), 443))
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("1.2.3.4"), 80))
}

// {allow 1.2.3.4, deny ports [8080]}: new dials to :8080 are denied by the
// port-scoped deny ahead of the plain allow, so established :8080 must go too.
func TestTCPPortAllowed_PortScopedDenyBeatsPortlessAllow(t *testing.T) {
	pol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"},{"action":"deny","ports":[8080]}]}`)
	addr := netip.MustParseAddr("1.2.3.4")
	require.False(t, tcpPortAllowed(pol, addr, 8080))
	require.True(t, tcpPortAllowed(pol, addr, 443))

	ports, ranges := portsNoLongerAllowed(pol, addr, []int{443, 8080}, nil)
	require.Equal(t, []int{8080}, ports)
	require.Empty(t, ranges)

	scoped := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"},{"action":"deny","target":"1.2.3.4","ports":[8080]}]}`)
	require.False(t, tcpPortAllowed(scoped, addr, 8080))

	ranged := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"},{"action":"deny","ports":[5000]}]}`)
	_, ranges = portsNoLongerAllowed(ranged, addr, nil, []policy.PortRange{{Lo: 1000, Hi: 9000}})
	require.Len(t, ranges, 1, "a deny inside a large range must not be missed")
}

func TestComputeEstablishedDrops_PortDenyOnAllowedIP(t *testing.T) {
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"},{"action":"deny","ports":[8080]}]}`)

	drops := ComputeEstablishedDrops(oldPol, newPol, nil)
	require.Len(t, drops.DenyPortDrops, 1)
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("1.2.3.4"), 8080))
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("1.2.3.4"), 443))

	m, scripts := recordingManager()
	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	script := (*scripts)[len(*scripts)-1]
	require.Less(t,
		ruleIndex(t, script, "tcp dport { 8080 } ct state established,related drop"),
		ruleIndex(t, script, "ip daddr @allow_v4 ct state established,related accept"))

	// Old port-scoped allow on the same IP: only the newly denied port drops.
	oldPorts := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4","ports":[443,8080]}]}`)
	drops = ComputeEstablishedDrops(oldPorts, newPol, nil)
	require.True(t, drops.MatchesFlow(netip.MustParseAddr("1.2.3.4"), 8080))
	require.False(t, drops.MatchesFlow(netip.MustParseAddr("1.2.3.4"), 443))
}

// A later full ApplyStatic recreates the table; the still-relevant drops must
// be re-emitted, and dropped again once the destination is re-allowed.
func TestApplyStatic_ReemitsStillRelevantDrops(t *testing.T) {
	m, scripts := recordingManager()
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"203.0.113.10"},{"action":"allow","target":"example.com"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	dropRule := "add rule inet opensandbox egress ip daddr 203.0.113.10 ct state established,related drop\n"

	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	require.Contains(t, (*scripts)[len(*scripts)-1], dropRule)

	// Plain reapply (e.g. always-rule reload) with a lease learned meanwhile.
	lease := netip.MustParseAddr("198.51.100.7")
	m.mu.Lock()
	m.domains["example.com"] = &resolvedDomain{addresses: map[netip.Addr]struct{}{lease: {}}}
	m.mu.Unlock()
	require.NoError(t, m.ApplyStatic(context.Background(), newPol))
	script := (*scripts)[len(*scripts)-1]
	require.Contains(t, script, dropRule, "plain ApplyStatic must keep the teardown")
	require.Contains(t, script, "ip daddr 198.51.100.7 ct state established,related accept",
		"learned lease IPs are kept so the dyn set reset cannot blackhole them")

	// Re-allowing the destination retires the drop.
	require.NoError(t, m.ApplyStatic(context.Background(), oldPol))
	require.NotContains(t, (*scripts)[len(*scripts)-1], dropRule)
	m.mu.Lock()
	require.True(t, m.lastDrops.Empty())
	m.mu.Unlock()
}

func TestApplyStatic_ReemitsCatchAllUntilDefaultAllow(t *testing.T) {
	m, scripts := recordingManager()
	oldPol := mustPolicy(t, `{"defaultAction":"allow","egress":[]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`)
	blanketDrop := "add rule inet opensandbox egress ct state established,related drop\n"

	_, err := m.ApplyStaticComputingEstablishedDrops(context.Background(), oldPol, newPol)
	require.NoError(t, err)
	require.Contains(t, (*scripts)[len(*scripts)-1], blanketDrop)

	// A destination allowed after the commit stays kept even once removed by
	// a plain apply the caller did not opt in to.
	widened := mustPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"},{"action":"allow","target":"8.8.8.8"}]}`)
	require.NoError(t, m.ApplyStatic(context.Background(), widened))
	require.NoError(t, m.ApplyStatic(context.Background(), newPol))
	script := (*scripts)[len(*scripts)-1]
	require.Contains(t, script, blanketDrop)
	require.Contains(t, script, "ip daddr 8.8.8.8 ct state established,related accept")

	require.NoError(t, m.ApplyStatic(context.Background(), oldPol))
	require.NotContains(t, (*scripts)[len(*scripts)-1], blanketDrop)
}
