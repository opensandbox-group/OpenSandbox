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
		{"action":"allow","target":"203.0.113.10"},
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
	removedDrop := index("ip daddr 203.0.113.10 ct state established,related drop")
	dynAccept := index("ip daddr @dyn_allow_v4 ct state established,related accept")
	staticAccept := index("ip daddr @allow_v4 ct state established,related accept")
	blanket := index("ct state established,related accept")

	require.Less(t, index("meta mark 0x00000001 accept"), denyDrop, "infra accepts stay ahead of teardown")
	require.Less(t, denyDrop, dynAccept, "explicit deny must precede DNS-learned established accept")
	require.Less(t, denyDrop, staticAccept)
	require.Less(t, dynAccept, removedDrop, "DNS-learned accept must be able to unpin a removed-allow drop")
	require.Less(t, staticAccept, removedDrop)
	require.Less(t, removedDrop, blanket, "teardown must precede the blanket established accept")
}

// Plain reload must neither lift an existing TCP block nor add a new one.
// Check both streams immediately after DNS installation, without a reload.
func TestNftFirstDNSGrantAfterPlainReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	const readyEnv = "OPENSANDBOX_NFT_DNS_ECHO_READY"
	if ready := os.Getenv(readyEnv); ready != "" {
		for _, port := range []string{"2443", "28443"} {
			l, err := net.Listen("tcp4", ":"+port)
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
		<-ctx.Done()
		return
	}
	if !isolatedNftTest(t, ctx) {
		return
	}
	ready := filepath.Join(t.TempDir(), "ready")
	peer := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^TestNftFirstDNSGrantAfterPlainReload$")
	peer.Env = append(os.Environ(), readyEnv+"="+ready)
	require.NoError(t, peer.Start())
	t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 4*time.Second, 10*time.Millisecond)
	pid := strconv.Itoa(peer.Process.Pid)
	for _, cmd := range [][]string{
		{"ip", "link", "add", "client0", "type", "veth", "peer", "name", "origin0", "netns", pid},
		{"ip", "addr", "add", "198.18.0.1/30", "dev", "client0"},
		{"ip", "link", "set", "client0", "up"},
		{"nsenter", "-t", pid, "-n", "ip", "addr", "add", "198.18.0.2/30", "dev", "origin0"},
		{"nsenter", "-t", pid, "-n", "ip", "link", "set", "origin0", "up"},
	} {
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		require.NoError(t, err, "%s: %s", cmd, out)
	}
	m := NewManagerWithOptions(Options{})
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	connect := func(port string) net.Conn {
		c, err := net.DialTimeout("tcp4", "198.18.0.2:"+port, time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	cut, spared := connect("28443"), connect("2443")
	echo := func(c net.Conn, mark byte) error {
		if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		if _, err := c.Write([]byte{mark}); err != nil {
			return err
		}
		var b [1]byte
		for b[0] != mark {
			if _, err := io.ReadFull(c, b[:]); err != nil {
				return err
			}
		}
		return nil
	}
	require.NoError(t, echo(cut, 1))
	require.NoError(t, echo(spared, 1))
	const domain = `{"action":"allow","target":"cdn.example"}`
	first := mustPolicy(t, `{"egress":[`+domain+`,{"action":"allow","ports":[2443]}]}`)
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, first)
	require.NoError(t, err)
	require.ErrorIs(t, echo(cut, 2), os.ErrDeadlineExceeded)
	require.NoError(t, echo(spared, 2))
	next := mustPolicy(t, `{"egress":[`+domain+`,{"action":"deny","target":"198.18.0.0/24","ports":[2443,28443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, next))
	require.NoError(t, m.AddResolvedDomain(ctx, "cdn.example", []ResolvedIP{{Addr: netip.MustParseAddr("198.18.0.2"), TTL: time.Minute}}))
	require.ErrorIs(t, echo(cut, 3), os.ErrDeadlineExceeded)
	require.NoError(t, echo(spared, 3))
}
