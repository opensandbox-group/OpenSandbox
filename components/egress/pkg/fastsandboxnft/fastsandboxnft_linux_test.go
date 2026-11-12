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

package fastsandboxnft

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func TestResolvedIPRenewalExtendsKernelExpiry(t *testing.T) {
	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		command := exec.Command("unshare", "--net", os.Args[0], "-test.run=^TestResolvedIPRenewalExtendsKernelExpiry$", "-test.v")
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
	a := NewApplier(nil)
	s := subject.FromSandboxUID("renewal")
	require.NoError(t, a.ApplyDenyFirst(ctx, s, testSlot("renewal", "10.0.0.5")))
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, a.ApplyPolicy(ctx, s, pol))
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}
	resolve := func(ttl time.Duration) {
		ips := make([]nftables.ResolvedIP, 0, len(addresses))
		for _, address := range addresses {
			ips = append(ips, nftables.ResolvedIP{Addr: address, TTL: ttl})
		}
		require.NoError(t, a.AddResolvedIPs(ctx, s, "example.com", ips))
	}
	resolve(0)                 // clamped to the 60s minimum
	resolve(300 * time.Second) // clamped to 360s

	expires := make(map[string]int)
	for _, family := range []string{"v4", "v6"} {
		snapshot, err := exec.CommandContext(ctx, "nft", "-j", "list", "set", "inet", TableName, dynSetName(s, family)).CombinedOutput()
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
		for _, entry := range ruleset.Nftables {
			if entry.Set != nil {
				for _, element := range entry.Set.Elements {
					expires[element.Element.Address] = element.Element.Expires
				}
			}
		}
	}
	for _, address := range addresses {
		require.Greater(t, expires[address.String()], 120, "renewal must extend kernel expiry for %s", address)
	}
}
