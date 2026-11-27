// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Run only as the peer subprocess; the client and its egress filter live in a
// different network namespace, so infrastructure and loopback accepts do not match.
func TestWithdrawalEchoPeer(t *testing.T) {
	ready := os.Getenv("OPENSANDBOX_WITHDRAWAL_PEER_READY")
	if ready == "" {
		return
	}
	for _, port := range strings.Split(os.Getenv("OPENSANDBOX_WITHDRAWAL_PEER_PORTS"), ",") {
		l, err := net.Listen("tcp4", net.JoinHostPort("", port))
		require.NoError(t, err)
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
			}
		}()
	}
	require.NoError(t, os.WriteFile(ready, nil, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	<-ctx.Done()
}

func startWithdrawalEchoPeer(t *testing.T, ctx context.Context, ports ...string) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	peer := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^TestWithdrawalEchoPeer$")
	peer.Env = append(os.Environ(), "OPENSANDBOX_WITHDRAWAL_PEER_READY="+ready, "OPENSANDBOX_WITHDRAWAL_PEER_PORTS="+strings.Join(ports, ","))
	peer.Stdout, peer.Stderr = os.Stdout, os.Stderr
	require.NoError(t, peer.Start())
	t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 4*time.Second, 10*time.Millisecond)
	pid := strconv.Itoa(peer.Process.Pid)
	for _, cmd := range [][]string{
		{"ip", "link", "add", "client0", "type", "veth", "peer", "name", "origin0", "netns", pid},
		{"ip", "addr", "add", "198.18.0.1/29", "dev", "client0"},
		{"ip", "link", "set", "client0", "up"},
		{"nsenter", "-t", pid, "-n", "ip", "addr", "add", "198.18.0.2/29", "dev", "origin0"},
		{"nsenter", "-t", pid, "-n", "ip", "addr", "add", "198.18.0.3/29", "dev", "origin0"},
		{"nsenter", "-t", pid, "-n", "ip", "link", "set", "origin0", "up"},
	} {
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		require.NoError(t, err, "%s: %s", cmd, out)
	}
}

func openWithdrawalStream(t *testing.T, host, port string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(host, port), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, withdrawalEcho(c, 1))
	return c
}

func withdrawalEcho(c net.Conn, mark byte) error {
	if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := c.Write([]byte{mark}); err != nil {
		return err
	}
	var reply [1]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return err
	}
	if reply[0] != mark {
		return fmt.Errorf("%s: got echo %d, want %d", c.RemoteAddr(), reply[0], mark)
	}
	return nil
}

// A timeout alone could be TCP backoff. On a fresh, already established stream,
// verify a data packet was attempted but none made it past the filter.
func requireWithdrawalDrop(t *testing.T, ctx context.Context, c net.Conn, mark byte) {
	t.Helper()
	dst := c.RemoteAddr().(*net.TCPAddr)
	match := fmt.Sprintf("ip daddr %s tcp sport %d tcp dport %d tcp flags & psh == psh", dst.IP, c.LocalAddr().(*net.TCPAddr).Port, dst.Port)
	script := fmt.Sprintf(`table inet withdrawal_probe {
		chain before { type filter hook output priority -1; policy accept; %s counter; }
		chain after  { type filter hook output priority 1; policy accept; %s counter; }
	}
`, match, match)
	run := func(script string) {
		t.Helper()
		out, err := defaultRunner(ctx, script)
		require.NoError(t, err, "%s", out)
	}
	run(script)
	defer run("delete table inet withdrawal_probe\n")
	require.ErrorIs(t, withdrawalEcho(c, mark), os.ErrDeadlineExceeded, "%s", dst)
	for _, hook := range []string{"before", "after"} {
		out, err := exec.CommandContext(ctx, "nft", "-n", "list", "chain", "inet", "withdrawal_probe", hook).CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.Contains(t, string(out), "counter packets ")
		require.Equal(t, hook == "after", strings.Contains(string(out), "counter packets 0 bytes"), "%s: %s", dst, out)
	}
}
