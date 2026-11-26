// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

func TestNftDomainPortOwnerTransitions(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startDomainPortsTCPPeer(t, ctx)
	parse := func(raw string) *policy.NetworkPolicy {
		p, err := policy.ParseClassicPolicy(raw)
		require.NoError(t, err)
		return p
	}
	const host = "198.18.0.2"
	addr := netip.MustParseAddr(host)
	ips := []ResolvedIP{{Addr: addr, TTL: time.Second}}
	scoped := parse(`{"egress":[{"action":"allow","target":"api.example","ports":[2443]}]}`)
	blocked := func(t *testing.T, port string) {
		t.Helper()
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 300*time.Millisecond)
		if c != nil {
			c.Close()
		}
		var timeout net.Error
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.Timeout(), "expected a filtered SYN, got %v", err)
	}
	for _, tc := range []struct{ name, rules string }{
		{"replace-with-portless", `{"egress":[{"action":"allow","target":"api.example"}]}`},
		{"add-portless", `{"egress":[{"action":"allow","target":"api.example"},{"action":"allow","target":"api.example","ports":[2443]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManagerWithOptions(Options{})
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			connect, echo := domainPortStreams(t)
			require.NoError(t, m.ApplyStatic(ctx, scoped))
			require.NoError(t, m.AddResolvedDomain(ctx, "api.example", ips))
			c := connect("2443")
			require.NoError(t, echo(c))
			require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, remotePort: 2443, state: "ESTABLISHED"}}, m))
			require.NoError(t, c.Close())
			require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
			now = now.Add(330 * time.Second) // DNS expired; 30 seconds of reconnect grace remain.
			wide := parse(tc.rules)
			for range 2 {
				require.NoError(t, m.ApplyStatic(ctx, wide))
				require.Regexp(t, `\. 2443 timeout 30s\b`, checkScopedElement(t, ctx, host, "2443", true))
				require.NotContains(t, readKernelLeaseExpiry(t, ctx), host)
			}
			require.NoError(t, echo(connect("2443")))
			blocked(t, "28443")
			now = now.Add(31 * time.Second)
			require.NoError(t, m.ApplyStatic(ctx, wide))
			blocked(t, "2443")
		})
	}
	t.Run("remove-whole-keep-scoped", func(t *testing.T) {
		m := NewManagerWithOptions(Options{})
		now := time.Unix(1000, 0)
		m.tracker.now = func() time.Time { return now }
		both := parse(`{"egress":[{"action":"allow","target":"whole.example"},{"action":"allow","target":"api.example","ports":[2443]}]}`)
		connect, echo := domainPortStreams(t)
		require.NoError(t, m.ApplyStatic(ctx, both))
		for _, name := range []string{"whole.example", "api.example"} {
			require.NoError(t, m.AddResolvedDomain(ctx, name, ips))
		}
		kept, cut, cutAfterReload := connect("2443"), connect("28443"), connect("28443")
		for _, c := range []net.Conn{kept, cut, cutAfterReload} {
			require.NoError(t, echo(c))
		}
		now = now.Add(time.Minute)
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, both, scoped)
		require.NoError(t, err)
		requireDomainPortWithdrawal(t, ctx, cut, echo)
		now = now.Add(2 * time.Second)
		require.Eventually(t, func() bool {
			out, err := exec.CommandContext(ctx, "nft", "get", "element", "inet", tableName, scopedLeaseV4, "{", host, ".", "2443", "}").CombinedOutput()
			return err != nil && strings.Contains(string(out), "No such file or directory")
		}, 3*time.Second, 30*time.Millisecond)
		require.NoError(t, echo(kept), "expiry stops new dials, not the retained stream")
		blocked(t, "2443")
		require.NoError(t, m.ApplyStatic(ctx, scoped))
		require.NoError(t, echo(kept))
		requireDomainPortWithdrawal(t, ctx, cutAfterReload, echo)
		blocked(t, "2443")
	})
	t.Run("transient-static-allow-is-not-a-carried-keep", func(t *testing.T) {
		m := NewManagerWithOptions(Options{})
		now := time.Unix(1000, 0)
		m.tracker.now = func() time.Time { return now }
		wide := parse(`{"defaultAction":"allow"}`)
		require.NoError(t, m.ApplyStatic(ctx, wide))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, scoped)
		require.NoError(t, err)
		require.NoError(t, m.AddResolvedDomain(ctx, "api.example", ips))
		connect, echo := domainPortStreams(t)
		kept, control := connect("2443"), connect("2443")
		require.NoError(t, echo(kept))
		require.NoError(t, echo(control))
		now = now.Add(2 * time.Minute)
		transient := parse(`{"egress":[{"action":"allow","target":"198.18.0.0/24"},{"action":"deny","target":"198.18.0.2"}]}`)
		empty := policy.DefaultDenyPolicy()
		require.NoError(t, m.ApplyStatic(ctx, transient))
		require.NoError(t, echo(kept))
		require.NoError(t, m.ApplyStatic(ctx, empty))
		require.NoError(t, echo(kept), "neither replacement opted in to a new withdrawal")
		blocked(t, "2443")
		_, err = m.ApplyStaticComputingEstablishedDrops(ctx, empty, parse(`{"egress":[{"action":"deny","target":"198.18.0.2","ports":[2443]}]}`))
		require.NoError(t, err)
		requireDomainPortWithdrawal(t, ctx, control, echo)
	})
	t.Run("history-only-where-a-drop-survives", func(t *testing.T) {
		m := NewManagerWithOptions(Options{})
		wide := parse(`{"defaultAction":"allow"}`)
		const common = `{"action":"allow","ports":[2443]},{"action":"allow","target":"api.example","ports":[2443,28443]},{"action":"allow","target":"other.example","ports":[2443]}`
		tight := parse(`{"egress":[` + common + `]}`)
		require.NoError(t, m.ApplyStatic(ctx, wide))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, tight)
		require.NoError(t, err)
		answers := []ResolvedIP{{Addr: addr, TTL: time.Minute}, {Addr: netip.MustParseAddr("198.18.0.3"), TTL: time.Minute}}
		require.NoError(t, m.AddResolvedDomain(ctx, "api.example", answers))
		for _, ip := range answers {
			checkNftTuple(t, ctx, scopedEstablishedKeepV4, ip.Addr.String(), "2443", false)
			checkNftTuple(t, ctx, scopedEstablishedKeepV4, ip.Addr.String(), "28443", true)
		}
		require.NoError(t, m.AddResolvedDomain(ctx, "other.example", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.4"), TTL: time.Minute}}))
		checkScopedElement(t, ctx, "192.0.2.4", "2443", true)
		checkNftTuple(t, ctx, scopedEstablishedKeepV4, "192.0.2.4", "2443", false)
		require.NoError(t, m.ApplyStatic(ctx, parse(`{"egress":[`+common+`,{"action":"allow","target":"198.18.0.2/32","ports":[28443]}]}`)))
		checkNftTuple(t, ctx, scopedEstablishedKeepV4, host, "28443", false)
		checkNftTuple(t, ctx, scopedEstablishedKeepV4, "198.18.0.3", "28443", true)
	})
}
