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

//go:build linux

package nftables

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// TestNftEstablishedDropKernelRuleOrder loads a tightened policy with
// dropEstablished into a real kernel table and checks rule order in the
// loaded chain, which script-string assertions cannot prove.
func TestNftEstablishedDropKernelRuleOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	oldPol := mustPolicy(t, `{"defaultAction":"deny","egress":[
		{"action":"allow","target":"203.0.113.10/32"},
		{"action":"allow","target":"2001:db8::10/128"},
		{"action":"allow","target":"2001:db8::11"},
		{"action":"allow","target":"203.0.113.11"},
		{"action":"allow","target":"198.51.100.0/24"},
		{"action":"allow","target":"cdn.example.com"}]}`)
	newPol := mustPolicy(t, `{"defaultAction":"deny","egress":[
		{"action":"deny","target":"198.51.100.7"},
		{"action":"allow","target":"198.51.100.0/24"},
		{"action":"allow","target":"cdn.example.com"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, oldPol))
	require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.20"), TTL: time.Minute}}))

	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, oldPol, newPol)
	require.NoError(t, err)
	// The shared-CDN case: an allowed domain re-resolves to the denied IP.
	require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.7"), TTL: time.Minute}}))

	out, err := exec.CommandContext(ctx, "nft", "list", "chain", "inet", tableName, chainName).CombinedOutput()
	require.NoError(t, err, "%s", out)
	var rules []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "table ") || strings.HasPrefix(line, "chain ") ||
			strings.HasPrefix(line, "type ") || line == "}" {
			continue
		}
		rules = append(rules, line)
	}
	index := func(rule string) int {
		t.Helper()
		for i, r := range rules {
			if r == rule {
				return i
			}
		}
		require.Failf(t, "rule not loaded", "%q not in kernel chain:\n%s", rule, out)
		return -1
	}

	denyDrop := index("ip daddr 198.51.100.7 ct state established,related drop")
	removedDrop := index("ip daddr @removed_est_v4 ct state established,related drop")
	dynAccept := index("ip daddr @dyn_allow_v4 ct state established,related accept")
	staticAccept := index("ip daddr @allow_v4 ct state established,related accept")
	blanket := index("ct state established,related accept")

	require.Less(t, index("meta mark 0x00000001 accept"), denyDrop, "infra accepts stay ahead of teardown")
	require.Less(t, denyDrop, dynAccept, "explicit deny must precede DNS-learned established accept")
	require.Less(t, denyDrop, staticAccept)
	require.Less(t, dynAccept, removedDrop, "DNS-learned accept must be able to unpin a removed-allow drop")
	require.Less(t, staticAccept, removedDrop)
	require.Less(t, removedDrop, blanket, "teardown must precede the blanket established accept")

	loseAck := func(ctx context.Context, script string) ([]byte, error) {
		m.run = defaultRunner
		out, err := defaultRunner(ctx, script)
		if err == nil {
			err = &ApplyError{Effect: ApplyUnknown, Err: errors.New("lost lease acknowledgement")}
		}
		return out, err
	}
	t.Run("current deny before reclaim", func(t *testing.T) {
		blocked := netip.MustParseAddr("203.0.113.11")
		deny := mustPolicy(t, `{"egress":[{"action":"deny","target":"203.0.113.11/32"},{"action":"allow","target":"cdn.example.com"}]}`)
		require.NoError(t, m.ApplyStatic(ctx, deny))
		require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: blocked}}))
		require.True(t, m.lastDrops.Matches(blocked), "a new policy deny must not retire the previous teardown")
	})
	t.Run("lost ack without DNS retry", func(t *testing.T) {
		target := netip.MustParseAddr("203.0.113.10")
		target6 := netip.MustParseAddr("2001:db8::10")
		m.run = loseAck
		require.Error(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: target}, {Addr: target6}}))
		require.False(t, m.lastDrops.Matches(target), "reconcile committed teardown retirement without a DNS retry")
		require.False(t, m.lastDrops.Matches(target6), "the IPv6 host-prefix is retired as well")
		require.True(t, m.lastDrops.Matches(netip.MustParseAddr("2001:db8::11")), "unrelated IPv6 teardown survives")
		require.NoError(t, m.ApplyStatic(ctx, newPol))
		out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, removedEstV4).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.NotContains(t, string(out), target.String(), "ordinary rebuild must not revive the retired teardown")
		require.Contains(t, string(out), "203.0.113.11", "an unrelated teardown must survive readback")
		_, err = m.ApplyStaticComputingEstablishedDrops(ctx, newPol, mustPolicy(t,
			`{"egress":[{"action":"allow","target":"198.51.100.0/24"}]}`))
		require.NoError(t, err)
		require.True(t, m.lastDrops.Matches(target), "readback must preserve the newly learned source for revocation")
		require.True(t, m.lastDrops.Matches(target6))
	})
	t.Run("idle retry after readback failure", func(t *testing.T) {
		target := netip.MustParseAddr("203.0.113.11")
		m = NewManagerWithOptions(Options{})
		require.NoError(t, m.ApplyStatic(ctx, oldPol))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, oldPol, newPol)
		require.NoError(t, err)
		m.readExactDrops = func(context.Context) (map[netip.Addr]struct{}, error) {
			return nil, errors.New("readback temporarily unavailable")
		}
		m.run = loseAck
		require.Error(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: target}}))
		require.True(t, m.lastDrops.Matches(target), "a failed readback must not publish unknown state")
		require.Error(t, m.ApplyStatic(ctx, newPol), "reload cannot recreate stale teardown before reconciliation")
		m.readExactDrops = readExactDrops
		m.opts.ConnectionRefreshInterval = 10 * time.Millisecond
		m.tracker.listConnections = func(context.Context) ([]tcpConnection, error) {
			return nil, errors.New("TCP snapshot unavailable")
		}
		tickCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		m.StartConnectionRefresh(tickCtx)
		require.Eventually(t, func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return !m.lastDrops.Matches(target)
		}, 3*time.Second, 10*time.Millisecond)
	})

	for _, missingTable := range []bool{true, false} {
		name := "ordinary rebuild after two unknown writes"
		if missingTable {
			name = "ordinary rebuild after missing table"
		}
		t.Run(name, func(t *testing.T) {
			m = NewManagerWithOptions(Options{})
			require.NoError(t, m.ApplyStatic(ctx, oldPol))
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, oldPol, newPol)
			require.NoError(t, err)
			target := netip.MustParseAddr("203.0.113.10")
			ips := []ResolvedIP{{Addr: target}}
			if missingTable {
				require.NoError(t, exec.CommandContext(ctx, "nft", "delete", "table", "inet", tableName).Run())
			} else {
				m.readExactDrops = func(context.Context) (map[netip.Addr]struct{}, error) {
					return nil, errors.New("readback temporarily unavailable")
				}
				m.run = loseAck
			}
			require.Error(t, m.AddResolvedDomain(ctx, "cdn.example.com", ips))
			if !missingTable {
				m.run = loseAck
				_, err = m.ApplyStaticComputingEstablishedDrops(ctx, newPol, nil)
				require.Error(t, err)
			}
			m.readExactDrops = readExactDrops
			require.NoError(t, m.ApplyStatic(ctx, newPol))
			require.NotContains(t, readKernelLeaseExpiry(t, ctx), target.String(), "reload cannot renew the unconfirmed lease")
			require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.5")}}))
			_, err = m.ApplyStaticComputingEstablishedDrops(ctx, newPol, policy.DefaultDenyPolicy())
			require.NoError(t, err)
			require.True(t, m.lastDrops.Matches(target), "rebuild must retain possible grant history for teardown")
			require.NoError(t, m.ApplyStatic(ctx, newPol))
			require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example.com", ips))
		})
	}
	t.Run("regrant preserves only denied ports", func(t *testing.T) {
		m = NewManagerWithOptions(Options{})
		require.NoError(t, m.ApplyStatic(ctx, oldPol))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, oldPol, newPol)
		require.NoError(t, err)
		target := netip.MustParseAddr("203.0.113.10")
		p := mustPolicy(t, `{"egress":[{"action":"allow","target":"cdn.example.com"},{"action":"deny","target":"203.0.113.0/24","ports":[443]}]}`)
		require.NoError(t, m.ApplyStatic(ctx, p))
		require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example.com", []ResolvedIP{{Addr: target}}))
		require.True(t, m.lastDrops.MatchesFlow(target, 443))
		require.False(t, m.lastDrops.MatchesFlow(target, 80))
		p = mustPolicy(t, `{"egress":[{"action":"allow","target":"cdn.example.com"},{"action":"deny","ports":[443,80]}]}`)
		require.NoError(t, m.ApplyStatic(ctx, p))
		out, err := exec.CommandContext(ctx, "nft", "list", "chain", "inet", tableName, chainName).CombinedOutput()
		require.NoError(t, err, "%s", out)
		portDeny := strings.Index(string(out), "ip daddr 203.0.113.10 tcp dport 443 ct state established,related drop")
		dnsAccept := strings.Index(string(out), "ip daddr @dyn_allow_v4 ct state established,related accept")
		require.GreaterOrEqual(t, portDeny, 0)
		require.Greater(t, dnsAccept, portDeny, "the retained port deny must take precedence over the regrant")
		require.True(t, m.lastDrops.MatchesFlow(target, 443), "keep only the overlapping previous teardown")
		require.False(t, m.lastDrops.MatchesFlow(target, 80), "plain reload must not newly tear down another port")
	})
	t.Run("unknown write at domain quota cannot evict confirmed history", func(t *testing.T) {
		m = NewManagerWithOptions(Options{})
		now := time.Unix(1000, 0)
		m.tracker.now = func() time.Time { return now }
		wide := mustPolicy(t, `{"egress":[{"action":"allow","target":"203.0.113.10"},{"action":"allow","target":"*.example.com"}]}`)
		narrow := mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example.com"}]}`)
		require.NoError(t, m.ApplyStatic(ctx, wide))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, narrow)
		require.NoError(t, err)
		for i := range maxResolvedDomains {
			addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)})
			require.NoError(t, m.AddResolvedDomain(ctx, fmt.Sprintf("name-%d.example.com", i), []ResolvedIP{{Addr: addr}}))
		}
		now = now.Add(61 * time.Second)
		m.run = loseAck
		m.readExactDrops = func(context.Context) (map[netip.Addr]struct{}, error) {
			return nil, errors.New("readback temporarily unavailable")
		}
		attempted := netip.MustParseAddr("203.0.113.10")
		require.Error(t, m.AddResolvedDomain(ctx, "new.example.com", []ResolvedIP{{Addr: attempted}}))
		m.run = loseAck
		_, err = m.ApplyStaticComputingEstablishedDrops(ctx, narrow, narrow)
		require.Error(t, err)
		require.NoError(t, m.ApplyStatic(ctx, narrow))
		_, err = m.ApplyStaticComputingEstablishedDrops(ctx, narrow, policy.DefaultDenyPolicy())
		require.NoError(t, err)
		drops, err := readExactDrops(ctx)
		require.NoError(t, err)
		require.Len(t, drops, maxResolvedDomains+1)
		require.Contains(t, drops, attempted)
	})
	t.Run("unchanged port floor is carried once", func(t *testing.T) {
		m = NewManagerWithOptions(Options{})
		require.NoError(t, m.ApplyStatic(ctx, oldPol))
		floor := policy.NewPortFloorDenyRule([]policy.PortRange{{Lo: 5000, Hi: 6000}})
		p := policy.MergeAlwaysOverlay(newPol, []policy.EgressRule{floor}, nil)
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, oldPol, p)
		require.NoError(t, err)
		require.NoError(t, m.ApplyStatic(ctx, p))
		out, err := exec.CommandContext(ctx, "nft", "list", "chain", "inet", tableName, chainName).CombinedOutput()
		require.NoError(t, err, "%s", out)
		// One established rule plus the ordinary egress rule.
		require.Equal(t, 2, strings.Count(string(out), "tcp dport 5000-6000"))
		p = mustPolicy(t, `{"egress":[{"action":"allow","target":"cdn.example.com"},{"action":"deny","target":"203.0.113.0/24","ports":[5500]}]}`)
		p = policy.MergeAlwaysOverlay(p, []policy.EgressRule{floor}, nil)
		require.NoError(t, m.ApplyStatic(ctx, p))
		out, err = exec.CommandContext(ctx, "nft", "list", "chain", "inet", tableName, chainName).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.NotContains(t, string(out), "tcp dport 5500 ct state established,related drop",
			"the retained global established deny already covers the CIDR port")
	})
	t.Run("plain reload does not promote a kept port to teardown", func(t *testing.T) {
		m = NewManagerWithOptions(Options{})
		wide := mustPolicy(t, `{"egress":[{"action":"allow","target":"203.0.113.10"}]}`)
		keep := mustPolicy(t, `{"egress":[{"action":"allow","target":"203.0.113.10","ports":[443]}]}`)
		deny := mustPolicy(t, `{"egress":[{"action":"deny","target":"203.0.113.10","ports":[443]}]}`)
		require.NoError(t, m.ApplyStatic(ctx, wide))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, keep)
		require.NoError(t, err)
		require.NoError(t, m.ApplyStatic(ctx, deny))
		out, err := exec.CommandContext(ctx, "nft", "list", "chain", "inet", tableName, chainName).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.NotContains(t, string(out), "ip daddr 203.0.113.10 tcp dport 443 ct state established,related drop")
		require.Contains(t, string(out), "ip daddr 203.0.113.10 tcp dport 443 drop")
	})
}
