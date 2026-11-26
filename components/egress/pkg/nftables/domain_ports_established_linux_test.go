// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// The peer has its own network namespace so these are genuine remote TCP
// connections; loopback and the infrastructure exceptions remain untouched.
func TestNftDomainPortsKernelEstablishedTightening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startDomainPortsTCPPeer(t, ctx)
	m := NewManagerWithOptions(Options{})
	old, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"allow","target":"b.example","ports":[2443]}]}`)
	require.NoError(t, err)
	tight, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"b.example","ports":[2443]}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, old))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("198.18.0.2"), TTL: time.Minute}}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", ips))
	connect, echo := domainPortStreams(t)
	removeTimedGrant := func(port string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "nft", "delete", "element", "inet", tableName, scopedLeaseV4,
			"{", "198.18.0.2", ".", port, "}").CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	kept, removed := connect("2443"), connect("28443")
	removedOnReload := connect("28443")
	require.NoError(t, echo(kept))
	require.NoError(t, echo(removed))
	require.NoError(t, echo(removedOnReload))
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.NoError(t, echo(kept), "the overlapping :2443 grant must survive")
	requireDomainPortWithdrawal(t, ctx, removed, echo)
	removeTimedGrant("2443")
	require.NoError(t, echo(kept), "expiry of a tuple retained by the opt-in must not expose a stale cut")
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(kept), "plain rebuild must preserve the surviving endpoint")
	requireDomainPortWithdrawal(t, ctx, removedOnReload, echo)
	require.NoError(t, echo(connect("2443")), "no client DNS relookup is required after narrowing")
	assertNewConnectionBlocked := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp", "198.18.0.2:28443", 300*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
		require.Error(t, err, "the revoked endpoint must reject a new connection")
	}
	assertNewConnectionBlocked()

	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	fresh := connect("28443")
	require.NoError(t, echo(fresh))
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(fresh), "a plain revoke must not cut a connection established after re-allow")
	assertNewConnectionBlocked()

	m = NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	active := connect("28443")
	require.NoError(t, echo(active))
	now = now.Add(121 * time.Second)
	next, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"allow","target":"b.example","ports":[2443]},{"action":"allow","target":"unrelated.example","ports":[443]}]}`)
	require.NoError(t, err)
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, next)
	require.NoError(t, err)
	require.NoError(t, echo(active), "an unchanged port rule is not a revocation even when the DNS lease expires")

	m = NewManagerWithOptions(Options{})
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	sibling, siblingAfterRegrant, siblingAfterReload := connect("2443"), connect("2443"), connect("2443")
	for _, c := range []net.Conn{sibling, siblingAfterRegrant, siblingAfterReload} {
		require.NoError(t, echo(c))
	}
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	requireDomainPortWithdrawal(t, ctx, sibling, echo)
	regrant, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"a.example","ports":[28443]},{"action":"allow","target":"b.example","ports":[2443]}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, regrant))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	reopened := connect("28443")
	require.NoError(t, echo(reopened))
	removeTimedGrant("28443")
	require.NoError(t, echo(reopened), "expiry of a re-learned tuple must not reactivate a stale teardown against new flows")
	requireDomainPortWithdrawal(t, ctx, siblingAfterRegrant, echo)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(reopened), "a plain rebuild must not recreate a retired tuple cut")
	requireDomainPortWithdrawal(t, ctx, siblingAfterReload, echo)
	assertNewConnectionBlocked()

	// A plain replacement's deny must still win when a timed grant is
	// written for a tuple whose established cut is already installed.
	m = NewManagerWithOptions(Options{})
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	scopedDeniedStream, wholeDeniedStream := connect("28443"), connect("28443")
	require.NoError(t, echo(scopedDeniedStream))
	require.NoError(t, echo(wholeDeniedStream))
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	scopedDenied, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"deny","target":"198.18.0.2","ports":[28443]}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, scopedDenied))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	require.NoError(t, echo(connect("2443")), "the scoped DNS grant admits the allowed sibling")
	requireDomainPortWithdrawal(t, ctx, scopedDeniedStream, echo)

	wholeDenied, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"c.example"},{"action":"deny","target":"198.18.0.2","ports":[28443]}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, wholeDenied))
	require.NoError(t, m.AddResolvedDomain(ctx, "c.example", ips))
	require.NoError(t, echo(connect("2443")), "the whole-IP DNS grant admits the allowed sibling")
	requireDomainPortWithdrawal(t, ctx, wholeDeniedStream, echo)

	whole, err := policy.ParseClassicPolicy(`{"egress":[{"action":"allow","target":"d.example"},{"action":"allow","target":"control.example"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, whole))
	require.NoError(t, m.AddResolvedDomain(ctx, "control.example", []ResolvedIP{
		{Addr: netip.MustParseAddr("192.0.2.99"), TTL: time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::99"), TTL: time.Minute},
	}))
	run := m.run
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		out, err := run(ctx, script)
		require.NoError(t, err, "%s", out)
		return nil, &ApplyError{Effect: ApplyUnknown, Err: context.DeadlineExceeded}
	}
	mixed := append([]ResolvedIP{{Addr: netip.MustParseAddr("2001:db8::2"), TTL: time.Minute}}, ips...)
	require.Error(t, m.AddResolvedDomain(ctx, "d.example", mixed))
	m.run = run
	require.ErrorIs(t, m.AddResolvedDomain(ctx, "d.example", ips), errStaticStateUncertain)
	require.NoError(t, m.ApplyStatic(ctx, whole))
	restored := readKernelAddressSets(t, ctx, dynAllowV4Set, dynAllowV6Set)
	require.ElementsMatch(t, []string{"192.0.2.99"}, restored[dynAllowV4Set])
	require.ElementsMatch(t, []string{"2001:db8::99"}, restored[dynAllowV6Set])
	out, err := run(ctx, "get element inet "+tableName+" "+scopedDropV4+" { 198.18.0.2 . 28443 }\n")
	require.NoError(t, err, "%s", out)
	// Use a new DNS source so the cut must retire on this acknowledgement,
	// not from a lease staged by the preceding static transaction.
	require.NoError(t, m.AddResolvedDomain(ctx, "d.example", ips))
	wholeReopened := connect("28443")
	require.NoError(t, echo(wholeReopened))
	out, err = exec.CommandContext(ctx, "nft", "delete", "element", "inet", tableName, dynAllowV4Set,
		"{", "198.18.0.2", "}").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.NoError(t, echo(wholeReopened), "expiry of a whole-IP regrant must not expose the old exact cut")
	assertNewConnectionBlocked()
}

func domainPortStreams(t *testing.T) (func(string) net.Conn, func(net.Conn) error) {
	t.Helper()
	connect := func(port string) net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", "198.18.0.2:"+port, time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	var marker byte
	echo := func(conn net.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return err
		}
		marker++
		if _, err := conn.Write([]byte{marker}); err != nil {
			return err
		}
		var reply [1]byte
		_, err := io.ReadFull(conn, reply[:])
		if err == nil {
			require.Equal(t, marker, reply[0])
		}
		return err
	}
	return connect, echo
}

// Adapt the established withdrawal probe: a timeout alone could be TCP
// backoff. Use a fresh stream for each probe and watch both sides of egress.
func requireDomainPortWithdrawal(t *testing.T, ctx context.Context, c net.Conn, echo func(net.Conn) error) {
	t.Helper()
	dst := c.RemoteAddr().(*net.TCPAddr)
	match := fmt.Sprintf("ip daddr %s tcp sport %d tcp dport %d tcp flags & psh == psh", dst.IP, c.LocalAddr().(*net.TCPAddr).Port, dst.Port)
	run := func(script string) {
		t.Helper()
		out, err := defaultRunner(ctx, script)
		require.NoError(t, err, "%s", out)
	}
	run(fmt.Sprintf(`table inet withdrawal_probe {
		chain before { type filter hook output priority -1; policy accept; %s counter; }
		chain after  { type filter hook output priority 1; policy accept; %s counter; }
	}
`, match, match))
	defer run("delete table inet withdrawal_probe\n")
	require.ErrorIs(t, echo(c), os.ErrDeadlineExceeded)
	for _, hook := range []string{"before", "after"} {
		out, err := exec.CommandContext(ctx, "nft", "-n", "list", "chain", "inet", "withdrawal_probe", hook).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.Contains(t, string(out), "counter packets ")
		require.Equal(t, hook == "after", strings.Contains(string(out), "counter packets 0 bytes"), "%s: %s", dst, out)
	}
}

func TestNftDomainPortsTCPPeer(t *testing.T) {
	ready := os.Getenv("OPENSANDBOX_DOMAIN_PORT_PEER")
	if ready == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	require.NoError(t, os.WriteFile(ready+"-namespace", []byte("ready"), 0o600))
	require.Eventually(t, func() bool {
		return exec.CommandContext(ctx, "ip", "link", "show", "grantpeer").Run() == nil
	}, 5*time.Second, 10*time.Millisecond)
	for _, args := range [][]string{
		{"link", "set", "lo", "up"},
		{"addr", "add", "198.18.0.2/30", "dev", "grantpeer"},
		{"link", "set", "grantpeer", "up"},
	} {
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		require.NoError(t, err, "ip %v: %s", args, out)
	}
	for _, endpoint := range []string{"198.18.0.2:2443", "198.18.0.2:28443"} {
		listener, err := net.Listen("tcp", endpoint)
		require.NoError(t, err)
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
			}
		}()
	}
	require.NoError(t, os.WriteFile(ready, []byte("ready"), 0o600))
	<-ctx.Done()
}

func startDomainPortsTCPPeer(t *testing.T, ctx context.Context) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "peer-ready")
	peer := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^TestNftDomainPortsTCPPeer$")
	peer.Env = append(os.Environ(), "OPENSANDBOX_DOMAIN_PORT_PEER="+ready)
	require.NoError(t, peer.Start())
	t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ready + "-namespace"); return err == nil }, 5*time.Second, 10*time.Millisecond)
	for _, args := range [][]string{
		{"link", "set", "lo", "up"},
		{"link", "add", "grant0", "type", "veth", "peer", "name", "grantpeer"},
		{"link", "set", "grantpeer", "netns", strconv.Itoa(peer.Process.Pid)},
		{"addr", "add", "198.18.0.1/30", "dev", "grant0"},
		{"link", "set", "grant0", "up"},
	} {
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		require.NoError(t, err, "ip %v: %s", args, out)
	}
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 5*time.Second, 10*time.Millisecond)
}
