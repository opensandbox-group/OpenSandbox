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

package nftables

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestApplyStatic_BuildsRuleset_DefaultDeny(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})

	p, err := policy.ParsePolicy(`{
		"defaultAction":"deny",
		"egress":[
			{"action":"allow","target":"1.1.1.1"},
			{"action":"allow","target":"2.2.0.0/16"},
			{"action":"deny","target":"2001:db8::/32"}
		]
	}`)
	require.NoError(t, err, "unexpected parse error")

	require.NoError(t, m.ApplyStatic(context.Background(), p), "ApplyStatic returned error")

	expectContains(t, rendered, "add chain inet opensandbox egress { type filter hook output priority 0; policy drop; }")
	expectContains(t, rendered, "add rule inet opensandbox egress ct state established,related accept")
	expectContains(t, rendered, "add rule inet opensandbox egress meta mark 0x1 accept")
	expectContains(t, rendered, "add rule inet opensandbox egress oifname \"lo\" accept")
	expectContains(t, rendered, "add rule inet opensandbox egress tcp dport 853 drop")
	expectContains(t, rendered, "add rule inet opensandbox egress udp dport 853 drop")
	expectContains(t, rendered, "add set inet opensandbox dyn_allow_v4 { type ipv4_addr; timeout 360s; }")
	expectContains(t, rendered, "add set inet opensandbox dyn_allow_v6 { type ipv6_addr; timeout 360s; }")
	expectContains(t, rendered, "add element inet opensandbox allow_v4 { 1.1.1.1, 2.2.0.0/16 }")
	expectContains(t, rendered, "add element inet opensandbox deny_v6 { 2001:db8::/32 }")
	expectContains(t, rendered, "add rule inet opensandbox egress ip daddr @dyn_allow_v4 accept")
	expectContains(t, rendered, "add rule inet opensandbox egress ip6 daddr @dyn_allow_v6 accept")
	expectContains(t, rendered, "add rule inet opensandbox egress counter drop")
}

func TestApplyStatic_DefaultAllowUsesAcceptPolicy(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})

	p, err := policy.ParsePolicy(`{
		"defaultAction":"allow",
		"egress":[{"action":"deny","target":"10.0.0.0/8"}]
	}`)
	require.NoError(t, err, "unexpected parse error")

	require.NoError(t, m.ApplyStatic(context.Background(), p), "ApplyStatic returned error")

	expectContains(t, rendered, "policy accept;")
	expectContains(t, rendered, "add rule inet opensandbox egress tcp dport 853 drop")
	require.NotContains(t, rendered, "counter drop", "did not expect drop counter when defaultAction is allow:\n%s", rendered)
	expectContains(t, rendered, "add element inet opensandbox deny_v4 { 10.0.0.0/8 }")
}

func expectContains(t *testing.T, s, substr string) {
	t.Helper()
	require.Contains(t, s, substr, "expected rendered ruleset to contain %q\nrendered:\n%s", substr, s)
}

func TestApplyStatic_RetryWhenTableMissing(t *testing.T) {
	var calls int
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		calls++
		scripts = append(scripts, script)
		if calls == 1 {
			return nil, fmt.Errorf("nft apply failed: exit status 1 (output: /dev/stdin:1:19-29: Error: No such file or directory; did you mean table ‘opensandbox’ in family inet?\ndelete table inet opensandbox\n                  ^^^^^^^^^^^)")
		}
		return nil, nil
	})

	p, _ := policy.ParsePolicy(`{"egress":[]}`)
	require.NoError(t, m.ApplyStatic(context.Background(), p), "expected retry to succeed")
	require.Equal(t, 2, calls, "expected 2 calls (fail then retry)")
	require.GreaterOrEqual(t, len(scripts), 2, "expected second attempt script to be recorded")
	require.NotContains(t, scripts[1], "delete table inet opensandbox", "expected second attempt to drop delete-table line")
}

func TestApplyStatic_DoHBlocklist(t *testing.T) {
	var rendered string
	opts := Options{
		BlockDoT:       true,
		BlockDoH443:    true,
		DoHBlocklistV4: []string{"9.9.9.9"},
		DoHBlocklistV6: []string{"2001:db8::/32"},
	}
	m := NewManagerWithRunnerAndOptions(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	}, opts)

	p, _ := policy.ParsePolicy(`{"defaultAction":"allow","egress":[]}`)
	require.NoError(t, m.ApplyStatic(context.Background(), p), "ApplyStatic returned error")

	expectContains(t, rendered, "add set inet opensandbox doh_block_v4 { type ipv4_addr; flags interval; }")
	expectContains(t, rendered, "add element inet opensandbox doh_block_v4 { 9.9.9.9 }")
	expectContains(t, rendered, "add rule inet opensandbox egress ip daddr @doh_block_v4 tcp dport 443 drop")
	expectContains(t, rendered, "add rule inet opensandbox egress ip6 daddr @doh_block_v6 tcp dport 443 drop")
}

func TestAddResolvedIPs_BuildsDynamicElements(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})
	ips := []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: 120 * time.Second},
		{Addr: netip.MustParseAddr("2001:db8::1"), TTL: 60 * time.Second},
	}
	require.NoError(t, m.AddResolvedIPs(context.Background(), ips), "AddResolvedIPs returned error")
	expectContains(t, rendered, "add element inet opensandbox dyn_allow_v4 { 1.1.1.1 timeout 180s }")
	expectContains(t, rendered, "add element inet opensandbox dyn_allow_v6 { 2001:db8::1 timeout 120s }")
}

func TestAddResolvedIPs_ClampsTTL(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})
	ips := []ResolvedIP{
		{Addr: netip.MustParseAddr("10.0.0.1"), TTL: 10 * time.Second},
		{Addr: netip.MustParseAddr("10.0.0.2"), TTL: 9999 * time.Second},
	}
	require.NoError(t, m.AddResolvedIPs(context.Background(), ips), "AddResolvedIPs returned error")
	expectContains(t, rendered, "10.0.0.1 timeout 70s")
	expectContains(t, rendered, "10.0.0.2 timeout 360s")
}

func TestAddResolvedIPs_EmptyNoOp(t *testing.T) {
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		require.FailNow(t, "runner should not be called for empty ips")
		return nil, nil
	})
	require.NoError(t, m.AddResolvedIPs(context.Background(), nil), "AddResolvedIPs returned error")
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{}), "AddResolvedIPs returned error")
}

func TestApplyStatic_NormalizesOverlappingAllow(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})
	p, err := policy.ParsePolicy(`{
		"defaultAction":"deny",
		"egress":[
			{"action":"allow","target":"100.64.0.0/10"},
			{"action":"allow","target":"100.100.2.136"}
		]
	}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(context.Background(), p))
	expectContains(t, rendered, "add element inet opensandbox allow_v4 { 100.64.0.0/10 }")
}
