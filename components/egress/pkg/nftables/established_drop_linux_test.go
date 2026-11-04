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
	"net/netip"
	"os/exec"
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
