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
	"net/netip"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// The infra exception must be scoped to (mitmproxy UID, proxy IP, proxy port)
// so the sandbox allow sets — which are IP-only — cannot be abused to reach
// the proxy directly and bounce CONNECT requests to denied destinations.
func TestBuildRulesetWithUpstreamProxyScopesAccept(t *testing.T) {
	rendered, err := buildRuleset(policy.DefaultDenyPolicy(), Options{
		BlockDoT: true,
		UpstreamProxy: &UpstreamProxyEndpoint{
			Port: 8443,
			UID:  10042,
			IPs:  []netip.Addr{netip.MustParseAddr("10.20.30.40")},
		},
	})
	require.NoError(t, err)

	expectContains(t, rendered, "add set inet opensandbox upstream_proxy_v4 { type ipv4_addr; timeout 360s; }")
	expectContains(t, rendered, "add set inet opensandbox upstream_proxy_v6 { type ipv6_addr; timeout 360s; }")
	expectContains(t, rendered, "add element inet opensandbox upstream_proxy_v4 { 10.20.30.40 }")
	expectContains(t, rendered,
		"add rule inet opensandbox egress ip daddr @upstream_proxy_v4 tcp dport 8443 meta skuid 10042 accept")
	expectContains(t, rendered,
		"add rule inet opensandbox egress ip6 daddr @upstream_proxy_v6 tcp dport 8443 meta skuid 10042 accept")
}

func TestBuildRulesetWithoutUpstreamProxyOmitsScopedSets(t *testing.T) {
	rendered, err := buildRuleset(policy.DefaultDenyPolicy(), Options{BlockDoT: true})
	require.NoError(t, err)
	require.NotContains(t, rendered, "upstream_proxy")
}

// DNS-learned proxy addresses land in the scoped sets with their (clamped)
// TTL — the same add/delete/add refresh pattern as the sandbox dynamic sets.
func TestAddUpstreamProxyIPsFeedsScopedSets(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunnerAndOptions(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	}, Options{
		UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128, UID: 10042},
	})

	err := m.AddUpstreamProxyIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("10.9.8.7"), TTL: 60 * time.Second},
		{Addr: netip.MustParseAddr("fd00::1"), TTL: 60 * time.Second},
	})
	require.NoError(t, err)
	require.Len(t, scripts, 1)
	expectContains(t, scripts[0], "add element inet opensandbox upstream_proxy_v4 { 10.9.8.7 timeout 120s }")
	expectContains(t, scripts[0], "add element inet opensandbox upstream_proxy_v6 { fd00::1 timeout 120s }")
	require.NotContains(t, scripts[0], "dyn_allow")
}

// Without a configured endpoint the callback must be a no-op — it must never
// write to the sandbox sets.
func TestAddUpstreamProxyIPsNoopWhenUnset(t *testing.T) {
	called := false
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		called = true
		return nil, nil
	})
	require.NoError(t, m.AddUpstreamProxyIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("10.9.8.7"), TTL: 60 * time.Second},
	}))
	require.False(t, called)
}
