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

package policy

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePolicy_EmptyOrNullDefaultsDeny(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"null",
		"{}\n",
	}
	for _, raw := range cases {
		p, err := ParsePolicy(raw)
		require.NoErrorf(t, err, "raw %q returned error", raw)
		require.NotNilf(t, p, "raw %q expected default deny policy, got nil", raw)
		require.Equalf(t, ActionDeny, p.DefaultAction, "raw %q expected defaultAction deny", raw)
		require.Equalf(t, ActionDeny, p.Evaluate("example.com."), "raw %q expected deny evaluation", raw)
	}
}

func TestParsePolicy_DefaultActionFallback(t *testing.T) {
	p, err := ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NotNil(t, p, "expected policy object, got nil")
	require.Equal(t, ActionDeny, p.DefaultAction, "expected defaultAction fallback to deny")
}

func TestParsePolicy_EmptyEgressDefaultsDeny(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":""}`)
	require.NoError(t, err)
	require.Equal(t, ActionDeny, p.DefaultAction, "expected default deny when defaultAction missing")
	require.Equal(t, ActionDeny, p.Evaluate("anything.com."), "expected evaluation deny for empty egress")
}

func TestParsePolicy_IPAndCIDRSupported(t *testing.T) {
	raw := `{
		"defaultAction":"deny",
		"egress":[
			{"action":"allow","target":"1.1.1.1"},
			{"action":"allow","target":"2.2.0.0/16"},
			{"action":"deny","target":"2001:db8::/32"},
			{"action":"deny","target":"2001:db8::1"}
		]
	}`
	p, err := ParsePolicy(raw)
	require.NoError(t, err)
	allowV4, allowV6, denyV4, denyV6 := p.StaticIPSets()
	require.Len(t, allowV4, 2, "allowV4 length mismatch")
	require.Equal(t, "1.1.1.1", allowV4[0])
	require.Equal(t, "2.2.0.0/16", allowV4[1])
	require.Len(t, denyV6, 2, "expected 2 denyV6 entries")
	require.Empty(t, allowV6, "allowV6 should be empty")
	require.Empty(t, denyV4, "denyV4 should be empty")
}

func TestParsePolicy_InvalidAction(t *testing.T) {
	_, err := ParsePolicy(`{"egress":[{"action":"foo","target":"example.com"}]}`)
	require.Error(t, err, "expected error for invalid action")
}

func TestParsePolicy_EmptyTargetError(t *testing.T) {
	_, err := ParsePolicy(`{"egress":[{"action":"allow","target":""}]}`)
	require.Error(t, err, "expected error for empty target")
}

func TestWithExtraAllowIPs(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	allowV4, allowV6, _, _ := p.StaticIPSets()
	require.Empty(t, allowV4, "domain-only policy should have no static allowV4 IPs")
	require.Empty(t, allowV6, "domain-only policy should have no static allowV6 IPs")

	ips := []netip.Addr{
		netip.MustParseAddr("192.168.65.7"),
		netip.MustParseAddr("2001:db8::1"),
	}
	merged := p.WithExtraAllowIPs(ips)
	require.NotSame(t, p, merged, "expected new policy instance")
	allowV4, allowV6, _, _ = merged.StaticIPSets()
	require.Len(t, allowV4, 1, "allowV4 length mismatch")
	require.Equal(t, "192.168.65.7", allowV4[0])
	require.Len(t, allowV6, 1, "allowV6 length mismatch")
	require.Equal(t, "2001:db8::1", allowV6[0])

	// nil/empty ips returns same policy
	require.Same(t, p, p.WithExtraAllowIPs(nil), "WithExtraAllowIPs(nil) should return same policy")
	require.Same(t, p, p.WithExtraAllowIPs([]netip.Addr{}), "WithExtraAllowIPs([]) should return same policy")
}

func TestEvaluate_CompiledIndexMatchesLinear(t *testing.T) {
	p, err := ParsePolicy(`{
		"defaultAction":"deny",
		"egress":[
			{"action":"allow","target":"*.example.com"},
			{"action":"deny","target":"api.example.com"},
			{"action":"allow","target":"*.internal.example.com"},
			{"action":"deny","target":"10.0.0.1"},
			{"action":"allow","target":"10.0.0.0/24"}
		]
	}`)
	require.NoError(t, err)
	require.NotNil(t, p.domainIndex, "parsed policy should build compiled domain index")

	queries := []string{
		"api.example.com.",
		"www.example.com.",
		"a.internal.example.com.",
		"internal.example.com.",
		"unknown.test.",
	}
	for _, q := range queries {
		got := p.Evaluate(q)
		want, matched := p.evaluateLinear(normalizeQueryForTest(q))
		if !matched {
			want = p.DefaultAction
		}
		require.Equalf(t, want, got, "compiled evaluate mismatch for query=%s", q)
	}
}

func TestEvaluate_ManualPolicyFallsBackToLinear(t *testing.T) {
	manual := &NetworkPolicy{
		DefaultAction: ActionDeny,
		Egress: []EgressRule{
			{Action: ActionAllow, Target: "*.example.com", targetKind: targetDomain},
			{Action: ActionDeny, Target: "api.example.com", targetKind: targetDomain},
		},
	}
	require.Nil(t, manual.domainIndex, "manual policy intentionally skips compile")
	require.Equal(t, ActionAllow, manual.Evaluate("api.example.com."))
	require.Equal(t, ActionAllow, manual.Evaluate("www.example.com."))
	require.Equal(t, ActionDeny, manual.Evaluate("unknown.example."))
}

func TestEvaluate_CompiledIndexKeepsFirstMatchPriority(t *testing.T) {
	p := &NetworkPolicy{
		DefaultAction: ActionDeny,
		Egress: []EgressRule{
			{Action: ActionAllow, Target: "*.example.com", targetKind: targetDomain},
			{Action: ActionDeny, Target: "api.example.com", targetKind: targetDomain},
		},
	}
	p = ensureDefaults(p)
	require.NotNil(t, p.domainIndex)
	require.Equal(t, ActionAllow, p.Evaluate("api.example.com."))
}

func normalizeQueryForTest(domain string) string {
	return strings.ToLower(strings.TrimSuffix(domain, "."))
}
