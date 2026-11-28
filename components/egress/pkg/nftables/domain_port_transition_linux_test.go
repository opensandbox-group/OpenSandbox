// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
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
	startWithdrawalEchoPeer(t, ctx, "2443", "28443", "32443", "udp:2443")
	const host = "198.18.0.2"
	addr := netip.MustParseAddr(host)
	ips := []ResolvedIP{{Addr: addr, TTL: time.Second}}
	scoped := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"api.example","ports":[2443]}]}`)
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
			wide := mustClassicPolicy(t, tc.rules)
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
	for _, tc := range []struct{ name, whole string }{
		{"remove-whole-keep-scoped", "whole.example"},
		{"narrow-same-domain", "api.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManagerWithOptions(Options{})
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			epoch := now
			m.now = func() time.Time { return epoch }
			firstDeadline := epoch.Add(establishedCarryTTL())
			both := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"`+tc.whole+`"},{"action":"allow","target":"api.example","ports":[2443]}]}`)
			connect, echo := domainPortStreams(t)
			require.NoError(t, m.ApplyStatic(ctx, both))
			require.NoError(t, m.AddResolvedDomain(ctx, "api.example", ips))
			if tc.whole != "api.example" {
				require.NoError(t, m.AddResolvedDomain(ctx, tc.whole, ips))
			}
			kept, cut, cutAfterReload := connect("2443"), connect("28443"), connect("28443")
			cutAfterOptIn := connect("28443")
			older := connect("32443")
			for _, c := range []net.Conn{kept, cut, cutAfterReload, cutAfterOptIn} {
				require.NoError(t, echo(c))
			}
			if tc.whole == "api.example" {
				both = mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"api.example"},{"action":"deny","target":"198.18.0.2","ports":[28443]}]}`)
				require.NoError(t, m.ApplyStatic(ctx, both))
				require.NoError(t, echo(cut), "a plain deny affects fresh dials, not the already-admitted stream")
			}
			now = now.Add(time.Minute)
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, both, scoped)
			require.NoError(t, err)
			requireWithdrawalDrop(t, ctx, cut, 255)
			now = now.Add(2 * time.Second)
			require.Eventually(t, func() bool {
				out, err := exec.CommandContext(ctx, "nft", "get", "element", "inet", tableName, scopedLeaseV4, "{", host, ".", "2443", "}").CombinedOutput()
				return err != nil && strings.Contains(string(out), "No such file or directory")
			}, 3*time.Second, 30*time.Millisecond)
			require.NoError(t, echo(kept), "expiry stops new dials, not the retained stream")
			blocked(t, "2443")
			require.NoError(t, m.ApplyStatic(ctx, scoped))
			require.NoError(t, echo(kept))
			requireWithdrawalDrop(t, ctx, cutAfterReload, 255)
			blocked(t, "2443")
			unrelated := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"api.example","ports":[2443]},{"action":"allow","target":"later.example","ports":[28443]}]}`)
			epoch = epoch.Add(establishedCarryTTL() / 2)
			_, err = m.ApplyStaticComputingEstablishedDrops(ctx, scoped, unrelated)
			require.NoError(t, err)
			require.NoError(t, echo(kept), "unrelated opt-in must not revoke the retained port")
			requireWithdrawalDrop(t, ctx, cutAfterOptIn, 255)
			blocked(t, "2443")
			if tc.whole == "whole.example" {
				hostRule := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"198.18.0.2"},{"action":"deny","target":"198.18.0.2","ports":[28443,32443]},{"action":"allow","target":"api.example","ports":[2443]}]}`)
				require.NoError(t, m.ApplyStatic(ctx, hostRule))
				require.NoError(t, m.AddResolvedDomain(ctx, "api.example", ips))
				udp, err := net.Dial("udp4", net.JoinHostPort(host, "2443"))
				require.NoError(t, err)
				defer udp.Close()
				require.NoError(t, withdrawalEcho(udp, 10))
				_, err = m.ApplyStaticComputingEstablishedDrops(ctx, hostRule, scoped)
				require.NoError(t, err)
				_, err = m.ApplyStaticComputingEstablishedDrops(ctx, scoped, unrelated)
				require.NoError(t, err)
				out, err := defaultRunner(ctx, "get element inet "+tableName+" "+scopedHostOtherV4+" { "+host+" }\n")
				require.NoError(t, err, "%s", out)
				err = withdrawalEcho(udp, 11)
				require.True(t, errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrDeadlineExceeded), "expected filtered UDP, got %v", err)
				require.NoError(t, echo(kept), "a UDP withdrawal is not a TCP 2443 withdrawal")
			}
			require.NoError(t, m.AddResolvedDomain(ctx, "later.example", ips))
			reopened := connect("28443")
			now = now.Add(2 * time.Minute)
			require.NoError(t, m.ApplyStatic(ctx, unrelated))
			require.NoError(t, echo(reopened), "expiry of a genuine regrant must not restore its old cut")
			blocked(t, "28443")
			_, err = m.ApplyStaticComputingEstablishedDrops(ctx, unrelated, scoped)
			require.NoError(t, err)
			epoch = firstDeadline
			require.NoError(t, m.ApplyStatic(ctx, scoped))
			require.NoError(t, echo(older), "the unrelated opt-in must not extend the first host withdrawal")
			requireWithdrawalDrop(t, ctx, reopened, 255) // a later withdrawal has its own deadline
		})
	}
	t.Run("transient-static-allow-is-not-a-carried-keep", func(t *testing.T) {
		m := NewManagerWithOptions(Options{})
		now := time.Unix(1000, 0)
		m.tracker.now = func() time.Time { return now }
		wide := mustClassicPolicy(t, `{"defaultAction":"allow"}`)
		require.NoError(t, m.ApplyStatic(ctx, wide))
		_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, scoped)
		require.NoError(t, err)
		require.NoError(t, m.AddResolvedDomain(ctx, "api.example", ips))
		connect, echo := domainPortStreams(t)
		kept, control := connect("2443"), connect("2443")
		require.NoError(t, echo(kept))
		require.NoError(t, echo(control))
		now = now.Add(2 * time.Minute)
		transient := mustClassicPolicy(t, `{"egress":[{"action":"allow","target":"198.18.0.0/24"},{"action":"deny","target":"198.18.0.2"}]}`)
		empty := policy.DefaultDenyPolicy()
		require.NoError(t, m.ApplyStatic(ctx, transient))
		require.NoError(t, echo(kept))
		require.NoError(t, m.ApplyStatic(ctx, empty))
		require.NoError(t, echo(kept), "neither replacement opted in to a new withdrawal")
		blocked(t, "2443")
		_, err = m.ApplyStaticComputingEstablishedDrops(ctx, empty, mustClassicPolicy(t, `{"egress":[{"action":"deny","target":"198.18.0.2","ports":[2443]}]}`))
		require.NoError(t, err)
		requireWithdrawalDrop(t, ctx, control, 255)
	})
	t.Run("history-only-where-a-drop-survives", func(t *testing.T) {
		m := NewManagerWithOptions(Options{})
		wide := mustClassicPolicy(t, `{"defaultAction":"allow"}`)
		const common = `{"action":"allow","ports":[2443]},{"action":"allow","target":"api.example","ports":[2443,28443]},{"action":"allow","target":"other.example","ports":[2443]}`
		tight := mustClassicPolicy(t, `{"egress":[`+common+`]}`)
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
		require.NoError(t, m.ApplyStatic(ctx, mustClassicPolicy(t, `{"egress":[`+common+`,{"action":"allow","target":"198.18.0.2/32","ports":[28443]}]}`)))
		checkNftTuple(t, ctx, scopedEstablishedKeepV4, host, "28443", false)
		checkNftTuple(t, ctx, scopedEstablishedKeepV4, "198.18.0.3", "28443", true)
	})
}
