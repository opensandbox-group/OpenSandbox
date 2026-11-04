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

package iptables

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const gatewayDNSKernelTestName = "TestGatewayDNSRedirectKernelPrivateNS"

// TestGatewayDNSRedirectKernelPrivateNS reads the real nft table after each
// desired-set replacement. The parent only starts a child in a fresh network
// namespace; no nft command runs until the child proves it is not in either
// the captured parent namespace or /proc/1's namespace.
func TestGatewayDNSRedirectKernelPrivateNS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		parent, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^"+gatewayDNSKernelTestName+"$", "-test.v")
		cmd.Env = append(os.Environ(), "OPENSANDBOX_NFT_TEST=netns", "OPENSANDBOX_NFT_TEST_PARENT_NETNS="+parent)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		require.Contains(t, string(output), "--- PASS: "+gatewayDNSKernelTestName, "%s", output)
		t.Logf("%s", output)
		return
	case "netns":
		parent := os.Getenv("OPENSANDBOX_NFT_TEST_PARENT_NETNS")
		require.NotEmpty(t, parent, "the parent namespace identity must be captured by the opt-in wrapper")
		current, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		actualParent, err := os.Readlink("/proc/" + strconv.Itoa(os.Getppid()) + "/ns/net")
		require.NoError(t, err)
		require.Equal(t, parent, actualParent, "the captured namespace must belong to the actual process parent")
		pidOne, err := os.Readlink("/proc/1/ns/net")
		require.NoError(t, err)
		require.NotEqual(t, parent, current, "the child must not mutate the captured parent namespace")
		require.NotEqual(t, pidOne, current, "the child must not mutate /proc/1's namespace")
		t.Logf("netns guard passed: uid=%d child=%s captured-parent=%s actual-parent=%s proc1=%s", os.Getuid(), current, parent, actualParent, pidOne)
	default:
		t.Skip("kernel DNS validation not executed: set OPENSANDBOX_NFT_TEST=1 with namespace/nft permissions")
	}

	nftPath, err := exec.LookPath("nft")
	require.NoError(t, err)
	version, err := exec.CommandContext(ctx, nftPath, "--version").CombinedOutput()
	require.NoError(t, err, "%s", version)
	t.Logf("nft version: %s", strings.TrimSpace(string(version)))

	assertGatewayDNSTableMissing(t, ctx)
	t.Cleanup(func() {
		if err := ReplaceGatewayDNSRedirects(context.Background(), nil, 15353); err != nil {
			t.Errorf("remove gateway DNS test table during cleanup: %v", err)
		}
	})

	gw1 := netip.MustParseAddr("10.0.0.1")
	gw1PrefixNeighbor := netip.MustParseAddr("10.0.0.10")
	gw3 := netip.MustParseAddr("10.0.0.20")
	ipv6 := netip.MustParseAddr("fd00::1")
	mappedGW1 := netip.MustParseAddr("::ffff:10.0.0.1")

	// The first real install begins with no table and exercises the production
	// missing-table retry, not just rule parsing in a fake runner.
	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), []netip.Addr{gw1}, 15353))
	assertGatewayDNSKernelRules(t, ctx, []netip.Addr{gw1})

	wantShared := []netip.Addr{gw1, gw1PrefixNeighbor, ipv6, gw1PrefixNeighbor, mappedGW1}
	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), wantShared, 15353))
	assertGatewayDNSKernelRules(t, ctx, wantShared)

	remaining := []netip.Addr{gw1PrefixNeighbor, ipv6}
	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), remaining, 15353))
	assertGatewayDNSKernelRules(t, ctx, remaining)

	rebound := []netip.Addr{gw1PrefixNeighbor, gw3, ipv6}
	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), rebound, 15353))
	assertGatewayDNSKernelRules(t, ctx, rebound)
	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), rebound, 15353), "replay of the same complete set")
	assertGatewayDNSKernelRules(t, ctx, rebound)

	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), nil, 15353))
	assertGatewayDNSTableMissing(t, ctx)
	require.NoError(t, ReplaceGatewayDNSRedirects(context.Background(), nil, 15353), "empty desired set is idempotent when the table is absent")
	assertGatewayDNSTableMissing(t, ctx)
}

func assertGatewayDNSKernelRules(t *testing.T, ctx context.Context, gateways []netip.Addr) {
	t.Helper()
	actual, output, err := readGatewayDNSKernelRules(ctx)
	require.NoError(t, err, "%s", output)
	want := make(map[string]int, len(gateways)*2)
	unique := make(map[netip.Addr]struct{}, len(gateways))
	for _, gateway := range gateways {
		unique[gateway.Unmap()] = struct{}{}
	}
	for gateway := range unique {
		family := "ip6"
		if gateway.Is4() {
			family = "ip"
		}
		for _, protocol := range []string{"udp", "tcp"} {
			rule := fmt.Sprintf("%s daddr %s %s dport 53 redirect to :15353", family, gateway, protocol)
			want[rule] = 1
		}
	}
	require.Equal(t, want, actual, "actual nft table:\n%s", output)
	t.Logf("actual nft table:\n%s", output)
}

func assertGatewayDNSTableMissing(t *testing.T, ctx context.Context) {
	t.Helper()
	output, err := exec.CommandContext(ctx, "nft", "-n", "list", "table", "inet", gatewayDNSNftTable).CombinedOutput()
	require.Error(t, err, "the DNS table should be absent; output: %s", output)
	require.Contains(t, strings.ToLower(string(output)), "no such file or directory", "absence must be ENOENT, not a permission or command failure")
	require.Contains(t, string(output), gatewayDNSNftTable, "ENOENT output must identify the requested DNS table")
}

func readGatewayDNSKernelRules(ctx context.Context) (map[string]int, []byte, error) {
	output, err := exec.CommandContext(ctx, "nft", "-n", "list", "table", "inet", gatewayDNSNftTable).CombinedOutput()
	if err != nil {
		return nil, output, fmt.Errorf("list gateway DNS nft table: %w", err)
	}

	rules := make(map[string]int)
	inGatewayChain := false
	chains := 0
	chainHeader := make([]string, 0, 2)
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "chain ") && strings.HasSuffix(line, " {") {
			chains++
			inGatewayChain = line == "chain gw {"
			continue
		}
		if !inGatewayChain || line == "" {
			continue
		}
		if line == "}" {
			inGatewayChain = false
			continue
		}
		if strings.HasPrefix(line, "type ") || strings.HasPrefix(line, "policy ") {
			for _, statement := range strings.Split(line, ";") {
				if statement = strings.TrimSpace(statement); statement != "" {
					chainHeader = append(chainHeader, statement)
				}
			}
			continue
		}
		rules[line]++
	}
	if chains != 1 {
		return nil, output, fmt.Errorf("expected exactly one gateway DNS chain, got %d", chains)
	}
	if !equalGatewayDNSChainHeader(chainHeader) {
		return nil, output, fmt.Errorf("unexpected gateway DNS chain header: %q", chainHeader)
	}
	return rules, output, nil
}

func equalGatewayDNSChainHeader(header []string) bool {
	if len(header) != 2 || header[1] != "policy accept" {
		return false
	}
	return header[0] == "type nat hook prerouting priority dstnat" ||
		header[0] == "type nat hook prerouting priority -100"
}
