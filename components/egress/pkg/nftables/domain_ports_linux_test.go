// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Exercise the production manager and observe actual transmitted packets, not
// dial errors (a dropped SYN and a SYN sent to a dead link both time out).
func TestNftDomainPortsKernelVerdictsOverlapAndTightening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	setupDomainPortsLink(t, ctx)
	m := NewManagerWithOptions(Options{BlockDoT: true})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	old := mustPolicy(t, `{"egress":[
		{"action":"allow","target":"a.example","ports":[443,853,8443]},
		{"action":"allow","target":"b.example","ports":[443]},
		{"action":"deny","target":"a.example","ports":[9443]},
		{"action":"deny","target":"192.0.2.99"},
		{"action":"allow","target":"broad.example"},
		{"action":"allow","target":"blocked.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	addresses := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.77"), TTL: time.Minute}, {Addr: netip.MustParseAddr("2001:db8::77"), TTL: time.Minute}}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", addresses))
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.78"), TTL: time.Minute}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "broad.example", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.78"), TTL: time.Minute}}))
	short := []ResolvedIP{{Addr: addresses[0].Addr}, {Addr: addresses[1].Addr}}
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", short))
	require.NoError(t, m.AddResolvedDomain(ctx, "blocked.example", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.99")}}))
	probe := func(expected map[string]bool) {
		t.Helper()
		stop := sniffDomainPortsTCP(t, "domain0")
		for addr := range expected {
			conn, _ := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if conn != nil {
				_ = conn.Close()
			}
		}
		time.Sleep(100 * time.Millisecond)
		seen := stop()
		for addr, allowed := range expected {
			require.Equal(t, allowed, seen["tcp "+addr], "%s", addr)
		}
	}
	probe(map[string]bool{
		"192.0.2.77:443": true, "192.0.2.77:8443": true, "192.0.2.77:9443": false,
		"192.0.2.77:10443": false, "192.0.2.99:443": false,
		"192.0.2.77:853": false, "192.0.2.78:10443": true, "192.0.2.78:9443": false,
		"[2001:db8::77]:443": true, "[2001:db8::77]:9443": false,
	})
	// Advance the injected observation clock, then rebuild the real kernel
	// table. The short sibling is expired; it must not erase the long source
	// or re-clamp that source's remaining 59 seconds to another full lease.
	now = now.Add(61 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, old))
	out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, dynAllowPortV4Set).CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "192.0.2.77 . 443 timeout 59s")
	probe(map[string]bool{"192.0.2.77:443": true, "192.0.2.77:8443": true})
	// No new DNS lookup: retain :443 through a.example, remove :8443, and
	// materialize an explicit deny over both already-observed address families.
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[443]},{"action":"deny","target":"a.example","ports":[8443]}]}`)
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	probe(map[string]bool{"192.0.2.77:443": true, "192.0.2.77:8443": false, "[2001:db8::77]:443": true, "[2001:db8::77]:8443": false})
	out, err = exec.CommandContext(ctx, "nft", "list", "chain", "inet", tableName, chainName).CombinedOutput()
	require.NoError(t, err, "%s", out)
	deny := -1
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "ip daddr 192.0.2.77") && strings.Contains(line, "@dyn_deny_port_v4") && strings.Contains(line, "ct state established,related drop") {
			deny = strings.Index(string(out), line)
			break
		}
	}
	keep := strings.Index(string(out), "ip daddr . tcp dport @dyn_allow_port_v4 ct state established,related accept")
	require.GreaterOrEqual(t, deny, 0, "%s", out)
	require.Greater(t, keep, deny, "explicit scoped deny must precede learned endpoint established keeps")
}

func TestNftTimedPortElementKernelRenewalIsAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	apply := func(script string) {
		t.Helper()
		out, err := defaultRunner(ctx, script)
		require.NoError(t, err, "%s", out)
	}
	apply("add table inet endpoint_test\nadd set inet endpoint_test endpoints { type ipv4_addr . inet_service; flags timeout; }\n")
	endpoint := TCPEndpoint{Addr: netip.MustParseAddr("::ffff:192.0.2.10"), Port: 443}
	var b strings.Builder
	WriteTimedPortElement(&b, "endpoint_test", "endpoints", endpoint, time.Second)
	apply(b.String())
	b.Reset()
	WriteTimedPortElement(&b, "endpoint_test", "endpoints", endpoint, 20*time.Second)
	apply(b.String())
	b.Reset()
	WriteTimedPortElement(&b, "endpoint_test", "endpoints", endpoint, time.Second)
	_, err := defaultRunner(ctx, b.String()+"add element inet endpoint_test missing { 192.0.2.11 . 443 }\n")
	require.Error(t, err)
	time.Sleep(1500 * time.Millisecond)
	out, err := exec.CommandContext(ctx, "nft", "get", "element", "inet", "endpoint_test", "endpoints", "{ 192.0.2.10 . 443 }").CombinedOutput()
	require.NoError(t, err, "failed batch must not shorten the renewed endpoint: %s", out)
}

func TestNftDomainPortsKernelDisjointAddressOverflowTightening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	setupDomainPortsLink(t, ctx)
	var calls int
	var fail bool
	m := NewManagerWithRunner(func(ctx context.Context, script string) ([]byte, error) {
		calls++
		if fail {
			script += "add element inet opensandbox missing_set { 192.0.2.200 }\n"
		}
		return defaultRunner(ctx, script)
	})
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	tight := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"example.com","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	first, second := make([]ResolvedIP, maxDomainAddresses), make([]ResolvedIP, maxDomainAddresses)
	for i := range first {
		first[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}), TTL: 5 * time.Minute}
		second[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)}), TTL: 5 * time.Minute}
	}
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", first))
	before := calls
	require.ErrorContains(t, m.AddResolvedDomain(ctx, "example.com", second), "capacity")
	require.Equal(t, before, calls, "capacity rejection must not insert unowned kernel grants")
	list := func(set string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, set).CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	assertOriginalGrants := func() {
		t.Helper()
		out := list(dynAllowV4Set)
		for _, ip := range first {
			require.Contains(t, out, ip.Addr.String()+" expires")
		}
		require.NotContains(t, out, "198.51.100.")
		require.NotContains(t, list(dynDenyPortV4Set), ". 443")
	}
	assertOriginalGrants()
	fail = true
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.Error(t, err)
	assertOriginalGrants()
	stop := sniffDomainPortsTCP(t, "domain0")
	conn, _ := net.DialTimeout("tcp", "192.0.2.1:443", 200*time.Millisecond)
	if conn != nil {
		_ = conn.Close()
	}
	require.True(t, stop()["tcp 192.0.2.1:443"], "a failed policy transaction must leave the original packet verdict")
	fail = false
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	out := list(dynDenyPortV4Set)
	for _, ip := range first {
		require.Contains(t, out, ip.Addr.String()+" . 443 ", "every original grant must retain deny provenance")
	}
	require.NotContains(t, out, "198.51.100.")
	stop = sniffDomainPortsTCP(t, "domain0")
	for _, address := range []string{"192.0.2.1:443", "192.0.2.64:443", "192.0.2.1:8443", "192.0.2.64:8443"} {
		conn, _ := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
	}
	seen := stop()
	require.False(t, seen["tcp 192.0.2.1:443"])
	require.False(t, seen["tcp 192.0.2.64:443"])
	require.True(t, seen["tcp 192.0.2.1:8443"])
	require.True(t, seen["tcp 192.0.2.64:8443"])
}

// setupDomainPortsLink is called only after isolatedNftTest verifies a private
// network namespace. A dummy link lets the packet socket observe admitted SYNs.
func setupDomainPortsLink(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, args := range [][]string{
		{"link", "set", "lo", "up"},
		{"link", "add", "domain0", "type", "dummy"},
		{"addr", "add", "192.0.2.2/24", "dev", "domain0"},
		{"link", "set", "domain0", "up"},
		{"route", "add", "default", "dev", "domain0"},
		{"-6", "addr", "add", "2001:db8::2/64", "dev", "domain0", "nodad"},
		{"-6", "route", "add", "default", "dev", "domain0"},
	} {
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		require.NoError(t, err, "ip %v: %s", args, out)
	}
}

// sniffDomainPortsTCP observes transmitted TCP destinations, so a timeout on a
// dead link cannot be mistaken for a policy drop. No IPv6 extensions are sent.
func sniffDomainPortsTCP(t *testing.T, ifname string) func() map[string]bool {
	t.Helper()
	link, err := net.InterfaceByName(ifname)
	require.NoError(t, err)
	var protocol [2]byte
	binary.BigEndian.PutUint16(protocol[:], unix.ETH_P_ALL)
	nativeProtocol := binary.NativeEndian.Uint16(protocol[:])
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(nativeProtocol))
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: nativeProtocol, Ifindex: link.Index}))
	timeout := unix.NsecToTimeval((50 * time.Millisecond).Nanoseconds())
	require.NoError(t, unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout))
	seen := make(map[string]bool)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		var buf [2048]byte
		for {
			select {
			case <-done:
				return
			default:
			}
			n, _, err := unix.Recvfrom(fd, buf[:], 0)
			if err != nil || n < 20 {
				continue
			}
			var addr netip.Addr
			var next byte
			var offset int
			switch buf[0] >> 4 {
			case 4:
				addr, next, offset = netip.AddrFrom4([4]byte(buf[16:20])), buf[9], int(buf[0]&15)*4
			case 6:
				if n < 40 {
					continue
				}
				addr, next, offset = netip.AddrFrom16([16]byte(buf[24:40])), buf[6], 40
			default:
				continue
			}
			if next != 6 || n < offset+4 {
				continue
			}
			destination := netip.AddrPortFrom(addr, binary.BigEndian.Uint16(buf[offset+2:offset+4]))
			seen["tcp "+destination.String()] = true
		}
	}()
	return func() map[string]bool {
		close(done)
		<-stopped
		_ = unix.Close(fd)
		return seen
	}
}

func TestNftDomainPortsKernelDomainCapacityTightening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	setupDomainPortsLink(t, ctx)
	calls := 0
	m := NewManagerWithRunner(func(ctx context.Context, script string) ([]byte, error) {
		calls++
		return defaultRunner(ctx, script)
	})
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	tight := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"192.0.2.1.example","ports":[443]},{"action":"deny","target":"192.0.2.128.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	for i := range maxResolvedDomains {
		addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)})
		require.NoError(t, m.AddResolvedDomain(ctx, addr.String()+".example", []ResolvedIP{{Addr: addr, TTL: 5 * time.Minute}}))
	}
	before := calls
	require.ErrorContains(t, m.AddResolvedDomain(ctx, "overflow.example", []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.1"), TTL: 5 * time.Minute}}), "capacity")
	require.Equal(t, before, calls, "domain overflow must be rejected before the production nft runner")
	out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, dynAllowV4Set).CombinedOutput()
	require.NoError(t, err, "%s", out)
	for i := range maxResolvedDomains {
		addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)})
		require.Contains(t, string(out), addr.String()+" expires")
	}
	require.NotContains(t, string(out), "198.51.100.1")
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	out, err = exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, dynDenyPortV4Set).CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "192.0.2.1 . 443 ")
	require.Contains(t, string(out), "192.0.2.128 . 443 ")
	stop := sniffDomainPortsTCP(t, "domain0")
	for _, address := range []string{"192.0.2.1:443", "192.0.2.128:443", "192.0.2.1:8443", "192.0.2.128:8443"} {
		conn, _ := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
	}
	seen := stop()
	require.False(t, seen["tcp 192.0.2.1:443"])
	require.False(t, seen["tcp 192.0.2.128:443"])
	require.True(t, seen["tcp 192.0.2.1:8443"])
	require.True(t, seen["tcp 192.0.2.128:8443"])
}

func TestNftTimedChangesKernelExpiredRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	apply := func(script string) {
		t.Helper()
		out, err := defaultRunner(ctx, script)
		require.NoError(t, err, "%s", out)
	}
	apply("add table inet delta_test\n" +
		"add set inet delta_test ips4 { type ipv4_addr; flags timeout; }\n" +
		"add set inet delta_test ips6 { type ipv6_addr; flags timeout; }\n" +
		"add set inet delta_test ports4 { type ipv4_addr . inet_service; flags timeout; }\n" +
		"add set inet delta_test ports6 { type ipv6_addr . inet_service; flags timeout; }\n")
	now := time.Now()
	expired4, expired6 := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")
	live := netip.MustParseAddr("192.0.2.11")
	beforeIPs := map[netip.Addr]time.Time{expired4: now.Add(50 * time.Millisecond), expired6: now.Add(50 * time.Millisecond), live: now.Add(20 * time.Second)}
	beforePorts := make(map[TCPEndpoint]time.Time)
	for addr, deadline := range beforeIPs {
		beforePorts[TCPEndpoint{Addr: addr, Port: 443}] = deadline
	}
	var b strings.Builder
	WriteTimedIPChanges(&b, "delta_test", "ips4", "ips6", nil, beforeIPs, now)
	WriteTimedPortChanges(&b, "delta_test", "ports4", "ports6", nil, beforePorts, now)
	apply(b.String())
	b.Reset()
	WriteTimedIPChanges(&b, "delta_test", "ips4", "ips6", beforeIPs, beforeIPs, now)
	WriteTimedPortChanges(&b, "delta_test", "ports4", "ports6", beforePorts, beforePorts, now)
	require.Zero(t, b.Len(), "unchanged membership must not be rewritten")
	time.Sleep(100 * time.Millisecond)
	now = time.Now()
	afterIPs := map[netip.Addr]time.Time{live: now.Add(40 * time.Second)}
	afterPorts := map[TCPEndpoint]time.Time{{Addr: live, Port: 443}: now.Add(40 * time.Second)}
	WriteTimedIPChanges(&b, "delta_test", "ips4", "ips6", beforeIPs, afterIPs, now)
	WriteTimedPortChanges(&b, "delta_test", "ports4", "ports6", beforePorts, afterPorts, now)
	apply(b.String())
	apply(b.String()) // Removal remains safe when the element is already absent.
	for _, set := range []string{"ips4", "ips6", "ports4", "ports6"} {
		out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", "delta_test", set).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.NotContains(t, string(out), expired4.String())
		require.NotContains(t, string(out), expired6.String())
		if set == "ips4" || set == "ports4" {
			require.Contains(t, string(out), live.String())
			require.Contains(t, string(out), "timeout 40s")
		}
	}
}

func TestNftDomainPortsKernelUnknownDynamicRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	var unknown bool
	var scripts []string
	m := NewManagerWithRunner(func(ctx context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		out, err := defaultRunner(ctx, script)
		if err == nil && unknown {
			return out, errors.New("successful kernel apply result was lost")
		}
		return out, err
	})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]},{"action":"deny","target":"example.com","ports":[8443]}]}`)))
	original, ghost := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: original}}))
	unknown = true
	require.Error(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: ghost}}))
	require.True(t, m.domainLeasesDirty)
	require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: ghost, Port: 443})
	list := func(set string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, set).CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	require.Contains(t, list(dynAllowPortV4Set), ghost.String(), "the failed acknowledgement really left an uncommitted kernel element")
	unknown = false
	before := len(scripts)
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: original}}))
	require.False(t, m.domainLeasesDirty)
	require.Len(t, scripts, before+1)
	require.NotContains(t, scripts[before], "delete table")
	require.NotContains(t, scripts[before], "add rule")
	for _, set := range []string{dynAllowPortV4Set, dynDenyPortV4Set} {
		members := list(set)
		require.Contains(t, members, original.String())
		require.NotContains(t, members, ghost.String(), "full dynamic reconciliation must remove unacknowledged permits and denies")
	}
}

func TestNftDomainPortsKernelActivityRecoveryAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	for _, whole := range []bool{false, true} {
		var unknown bool
		m := NewManagerWithRunner(func(ctx context.Context, script string) ([]byte, error) {
			out, err := defaultRunner(ctx, script)
			if err == nil && unknown {
				return out, errors.New("activity acknowledgement lost")
			}
			return out, err
		})
		now := time.Unix(1000, 0)
		m.tracker.now = func() time.Time { return now }
		require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"broad.example"},{"action":"allow","target":"scoped.example","ports":[443]}]}`)))
		domain, set := "scoped.example", dynAllowPortV4Set
		if whole {
			domain, set = "broad.example", dynAllowV4Set
		}
		addr := netip.MustParseAddr("192.0.2.10")
		require.NoError(t, m.AddResolvedDomain(ctx, domain, []ResolvedIP{{Addr: addr}}))
		active := []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}
		now = now.Add(30 * time.Second)
		unknown = true
		require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
		now = now.Add(31 * time.Second)
		unknown = false
		require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
		out, err := exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, set).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.Contains(t, string(out), addr.String(), "reconciliation after the confirmed deadline must retain known active destination")
		require.Contains(t, string(out), "timeout 6m")
	}
}
