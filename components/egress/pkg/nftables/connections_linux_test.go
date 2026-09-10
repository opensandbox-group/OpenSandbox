// Copyright 2026 Alibaba Group Holding Ltd.
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
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadTCPConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tcp")
	contents := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1234 01010101:01BB 01 00000000:00000000 00:00000000 00000000  1000 0 1\n" +
		"   1: 0100007F:1235 02020202:01BB 06 00000000:00000000 00:00000000 00000000  1000 0 2\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	connections, err := readTCPConnections(context.Background(), path, false)
	require.NoError(t, err)
	require.Equal(t, []tcpConnection{{remote: mustAddr("1.1.1.1"), state: "ESTABLISHED"}}, connections)
}

func TestDecodeProcAddressIPv6(t *testing.T) {
	addr, err := decodeProcAddress("B80D0120000000000000000001000000:01BB", true)
	require.NoError(t, err)
	require.Equal(t, "2001:db8::1", addr.String())
}

func mustAddr(value string) netip.Addr {
	return netip.MustParseAddr(value)
}

func TestDynamicElementRenewal(t *testing.T) {
	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		command := exec.Command("unshare", "--net", os.Args[0], "-test.run=^TestDynamicElementRenewal$", "-test.v")
		command.Env = append(os.Environ(), "OPENSANDBOX_NFT_TEST=netns")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("%s", output)
		return
	case "netns":
	default:
		t.Skip("set OPENSANDBOX_NFT_TEST=1 to run with nft and permission to create a network namespace")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	apply := func(script string) {
		output, err := defaultRunner(ctx, script)
		require.NoError(t, err, "%s", output)
	}
	apply("add table inet renewal_test\n" +
		"add set inet renewal_test dyn_allow_v4 { type ipv4_addr; timeout 360s; }\n" +
		"add set inet renewal_test dyn_allow_v6 { type ipv6_addr; timeout 360s; }\n")
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}
	for _, address := range addresses {
		apply(buildResolvedIPElementsScript("renewal_test", []ResolvedIP{{Addr: address, TTL: 2 * time.Second}}))
	}
	for _, address := range addresses {
		apply(buildResolvedIPElementsScript("renewal_test", []ResolvedIP{{Addr: address, TTL: 20 * time.Second}}))
	}
	expired := netip.MustParseAddr("192.0.2.2")
	apply(buildResolvedIPElementsScript("renewal_test", []ResolvedIP{{Addr: expired, TTL: time.Second}}))
	time.Sleep(2500 * time.Millisecond)
	apply(buildResolvedIPElementsScript("renewal_test", []ResolvedIP{{Addr: expired, TTL: 20 * time.Second}}))
	_, err := defaultRunner(ctx, buildResolvedIPElementsScript("renewal_test", []ResolvedIP{{Addr: addresses[0], TTL: time.Second}})+
		"add element inet renewal_test missing_set { 192.0.2.3 }\n")
	require.Error(t, err)

	snapshot, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "renewal_test").CombinedOutput()
	require.NoError(t, err, "%s", snapshot)
	var ruleset struct {
		Nftables []struct {
			Set *struct {
				Elements []struct {
					Element struct {
						Address string `json:"val"`
						Expires int    `json:"expires"`
					} `json:"elem"`
				} `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal(snapshot, &ruleset))
	expires := make(map[string]int)
	for _, entry := range ruleset.Nftables {
		if entry.Set != nil {
			for _, element := range entry.Set.Elements {
				expires[element.Element.Address] = element.Element.Expires
			}
		}
	}
	for _, address := range append(addresses, expired) {
		require.Greater(t, expires[address.String()], 10, "renewal must extend kernel expiry for %s: %s", address, snapshot)
	}
}
