// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
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
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[2443,28443]},{"action":"allow","target":"b.example","ports":[2443]}]}`)
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"b.example","ports":[2443]},{"action":"deny","target":"a.example","ports":[28443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("198.18.0.2"), TTL: time.Minute}}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", ips))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", ips))
	connect := func(port string) net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", "198.18.0.2:"+port, time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	echo := func(conn net.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return err
		}
		if _, err := conn.Write([]byte{'x'}); err != nil {
			return err
		}
		var reply [1]byte
		_, err := io.ReadFull(conn, reply[:])
		if err == nil {
			require.Equal(t, byte('x'), reply[0])
		}
		return err
	}
	kept, removed := connect("2443"), connect("28443")
	require.NoError(t, echo(kept))
	require.NoError(t, echo(removed))
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.NoError(t, echo(kept), "the overlapping :2443 grant must survive")
	require.Error(t, echo(removed), "the :28443 flow must stop despite the sibling name on the same IP")
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(kept), "plain rebuild must preserve the surviving endpoint")
	require.Error(t, echo(removed), "plain rebuild must carry the committed scoped deny")
	require.NoError(t, echo(connect("2443")), "no client DNS relookup is required after narrowing")
	conn, err := net.DialTimeout("tcp", "198.18.0.2:28443", 300*time.Millisecond)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err, "the denied endpoint must also reject a new connection")
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

func TestNftDomainPortsKernelDenyExpiryAllowsNewEstablishedFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startDomainPortsTCPPeer(t, ctx)
	m := NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	tight := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"example.com","ports":[28443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: netip.MustParseAddr("198.18.0.2")}}))
	connect := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", "198.18.0.2:28443", time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	echo := func(conn net.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return err
		}
		if _, err := conn.Write([]byte{'x'}); err != nil {
			return err
		}
		var reply [1]byte
		_, err := io.ReadFull(conn, reply[:])
		if err == nil {
			require.Equal(t, byte('x'), reply[0])
		}
		return err
	}
	before := connect()
	require.NoError(t, echo(before))
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.Error(t, echo(before), "the opt-in deny must stop the existing flow while its lease is live")
	require.NoError(t, before.Close())
	m.applyDomainRefresh(ctx, "example.com", m.domains["example.com"], nil)
	// Leave one actual second on the reconstructed lease, then let the kernel
	// expire it without another manager write. DNS no longer confirms the IP.
	now = now.Add(59 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	listDeny := func() ([]byte, error) {
		return exec.CommandContext(ctx, "nft", "list", "set", "inet", tableName, dynDenyPortV4Set).CombinedOutput()
	}
	out, err := listDeny()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "198.18.0.2", "the reconstructed deny lease must first exist")
	require.Eventually(t, func() bool {
		out, err := listDeny()
		return err == nil && !strings.Contains(string(out), "198.18.0.2")
	}, 4*time.Second, 20*time.Millisecond)
	require.NoError(t, echo(connect()), "a new SYN and its subsequent established packets must both pass after deny expiry")
	now = now.Add(2 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NoError(t, echo(connect()), "a later plain apply cannot resurrect an expired DNS-derived established deny")
}
