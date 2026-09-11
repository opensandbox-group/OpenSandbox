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
	"strings"
	"sync"
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
	expectContains(t, rendered, "add rule inet opensandbox egress drop")
}

func TestApplyStatic_DefaultDenyFallbackRuleUsesPlainDrop(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})

	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(context.Background(), p))

	expectContains(t, rendered, "add chain inet opensandbox egress { type filter hook output priority 0; policy drop; }")
	expectContains(t, rendered, "add rule inet opensandbox egress drop")
	require.NotContains(t, rendered, "counter drop", "counter expression is not supported in the QA pod netns")
}

func TestApplyStatic_AllowsRedirectedDNSBeforeAlwaysDeny(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		rendered = script
		return nil, nil
	})

	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)
	denyLoopback, err := policy.ParseValidatedEgressRule(policy.ActionDeny, "127.0.0.0/8")
	require.NoError(t, err)
	merged := policy.MergeAlwaysOverlay(p, []policy.EgressRule{denyLoopback}, nil)
	policyWithDNS := merged.WithExtraAllowIPs([]netip.Addr{netip.MustParseAddr("127.0.0.1")})

	require.NoError(t, m.ApplyStatic(context.Background(), policyWithDNS))

	denyRule := "add rule inet opensandbox egress ip daddr @deny_v4 drop"
	denyRuleIndex := strings.Index(rendered, denyRule)
	require.NotEqual(t, -1, denyRuleIndex, "expected rendered ruleset to contain %q", denyRule)
	for _, dnsRule := range []string{
		"add rule inet opensandbox egress ip daddr 127.0.0.1 udp dport 15353 accept",
		"add rule inet opensandbox egress ip daddr 127.0.0.1 tcp dport 15353 accept",
	} {
		dnsRuleIndex := strings.Index(rendered, dnsRule)
		require.NotEqual(t, -1, dnsRuleIndex, "expected rendered ruleset to contain %q", dnsRule)
		require.Less(t, dnsRuleIndex, denyRuleIndex, "expected %q before %q", dnsRule, denyRule)
	}
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
	require.NotContains(t, rendered, " egress drop", "did not expect final drop rule when defaultAction is allow:\n%s", rendered)
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

func TestDomainRefresh_OnlyRenewsObservedAndConfirmedIPs(t *testing.T) {
	var scripts []string
	manager := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	allowed, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, manager.ApplyStatic(ctx, allowed))
	now := time.Unix(1_000, 0)
	manager.tracker.now = func() time.Time { return now }
	first := ResolvedIP{Addr: netip.MustParseAddr("192.0.2.1"), TTL: time.Minute}
	rotated := ResolvedIP{Addr: netip.MustParseAddr("192.0.2.2"), TTL: time.Minute}
	newAddress := ResolvedIP{Addr: netip.MustParseAddr("192.0.2.3"), TTL: time.Minute}
	ipv6 := ResolvedIP{Addr: netip.MustParseAddr("2001:db8::1"), TTL: time.Minute}
	require.NoError(t, manager.AddResolvedDomain(ctx, "EXAMPLE.COM.", []ResolvedIP{first, rotated}))
	require.NoError(t, manager.AddResolvedDomain(ctx, "example.com", []ResolvedIP{ipv6}))
	now = now.Add(domainRefreshLead)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		return []ResolvedIP{first, ipv6, newAddress}, nil
	})
	require.Len(t, scripts, 4)
	require.Contains(t, scripts[3], "192.0.2.1 timeout 120s")
	require.Contains(t, scripts[3], "2001:db8::1 timeout 120s")
	require.NotContains(t, scripts[3], rotated.Addr.String())
	require.NotContains(t, scripts[3], newAddress.Addr.String())

	now = now.Add(domainRefreshLead)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		return nil, fmt.Errorf("upstream unavailable")
	})
	require.Len(t, scripts, 4, "failed DNS must not extend a lease")
	require.Len(t, manager.domains, 1, "transient errors may be retried")
	now = now.Add(time.Minute)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		return []ResolvedIP{rotated, newAddress}, nil
	})
	require.Len(t, scripts, 4, "rotated or unobserved IPs must not be authorized")
	require.Empty(t, manager.domains)

	require.NoError(t, manager.AddResolvedDomain(ctx, "example.com", []ResolvedIP{first}))
	now = now.Add(domainRefreshLead)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) { return nil, nil })
	require.Len(t, scripts, 5, "negative answers must not extend a lease")
	require.Empty(t, manager.domains)
}

func TestDomainRefresh_PreservesLongerTCPGracePeriod(t *testing.T) {
	var scripts []string
	manager := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	allowed, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, manager.ApplyStatic(ctx, allowed))
	now := time.Unix(1_000, 0)
	manager.tracker.now = func() time.Time { return now }
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.1")}}
	require.NoError(t, manager.AddResolvedDomain(ctx, "example.com", ips))
	require.NoError(t, manager.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: ips[0].Addr, state: "ESTABLISHED"}}, manager))
	require.NoError(t, manager.tracker.refreshActiveConnections(ctx, nil, manager))
	lookups := 0
	lookup := func(context.Context, string) ([]ResolvedIP, error) {
		lookups++
		return nil, nil
	}
	manager.refreshDomains(ctx, lookup)
	require.Zero(t, lookups, "a fresh TCP lease must not trigger a DNS query")
	require.Len(t, scripts, 4, "background DNS must not shorten the final TCP lease")
	require.Contains(t, scripts[3], "192.0.2.1 timeout 360s")
	require.Len(t, manager.domains, 1)
	now = now.Add(dynSetTimeoutS*time.Second - domainRefreshLead)
	manager.refreshDomains(ctx, lookup)
	require.Equal(t, 1, lookups)
	require.Empty(t, manager.domains)
	require.Len(t, scripts, 4, "negative DNS stops domain refresh without revoking TCP grace")
}

func TestDomainRefresh_PolicyReplacementDiscardsInflightResult(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoke=%t", revoke), func(t *testing.T) {
			var scripts []string
			manager := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
				scripts = append(scripts, script)
				return nil, nil
			})
			allowed, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			require.NoError(t, manager.ApplyStatic(ctx, allowed))
			now := time.Unix(1_000, 0)
			manager.tracker.now = func() time.Time { return now }
			ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.1"), TTL: time.Minute}}
			require.NoError(t, manager.AddResolvedDomain(ctx, "example.com", ips))
			now = now.Add(domainRefreshLead)
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				manager.refreshDomains(ctx, func(ctx context.Context, _ string) ([]ResolvedIP, error) {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return ips, nil
				})
			}()
			<-started
			if revoke {
				allowed = policy.DefaultDenyPolicy()
			}
			require.NoError(t, manager.ApplyStatic(ctx, allowed))
			close(release)
			<-done
			require.Len(t, scripts, 3, "old lookup must not repopulate the new ruleset")
			require.Empty(t, manager.domains)
			if revoke {
				require.Error(t, manager.AddResolvedDomain(ctx, "example.com", ips))
				require.Len(t, scripts, 3, "late foreground callback must respect revocation")
			}
		})
	}
}

func TestAddResolvedDomain_BoundsTrackingAndRequiresSuccessfulWrite(t *testing.T) {
	var writeErr error
	manager := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, writeErr })
	allowed, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, manager.ApplyStatic(ctx, allowed))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.1"), TTL: time.Minute}}
	writeErr = fmt.Errorf("nft unavailable")
	require.Error(t, manager.AddResolvedDomain(ctx, "failed.example.com", ips))
	require.Empty(t, manager.domains)
	writeErr = nil
	for index := 0; index < maxResolvedDomains; index++ {
		require.NoError(t, manager.AddResolvedDomain(ctx, fmt.Sprintf("%d.example.com", index), ips))
	}
	require.NoError(t, manager.AddResolvedDomain(ctx, "0.example.com", ips))
	require.NoError(t, manager.AddResolvedDomain(ctx, "new.example.com", ips))
	require.Len(t, manager.domains, maxResolvedDomains)
	require.Contains(t, manager.domains, "0.example.com")
	require.NotContains(t, manager.domains, "1.example.com")
	for index := 0; index < maxDomainAddresses+1; index++ {
		ips = append(ips, ResolvedIP{Addr: netip.AddrFrom4([4]byte{198, 51, 100, byte(index)}), TTL: time.Minute})
	}
	require.NoError(t, manager.AddResolvedDomain(ctx, "new.example.com", ips))
	require.Len(t, manager.domains["new.example.com"].addresses, maxDomainAddresses)
	writeErr = fmt.Errorf("table does not exist")
	require.NoError(t, manager.RemoveEnforcement(ctx))
	require.Empty(t, manager.domains)
	require.Error(t, manager.AddResolvedDomain(ctx, "new.example.com", ips))
	writeErr = nil
	require.NoError(t, manager.ApplyStatic(ctx, allowed))
	require.Empty(t, manager.domains)
	allowAll, err := policy.ParsePolicy(`{"defaultAction":"allow"}`)
	require.NoError(t, err)
	require.NoError(t, manager.ApplyStatic(ctx, allowAll))
	require.NoError(t, manager.AddResolvedDomain(ctx, "new.example.com", ips))
	require.Empty(t, manager.domains, "default-allow needs no background authorization")
}

func TestDomainRefresh_BoundsWorkersAndStopsOnCancellation(t *testing.T) {
	manager := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	allowed, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, manager.ApplyStatic(ctx, allowed))
	for index := 0; index < maxResolvedDomains; index++ {
		require.NoError(t, manager.AddResolvedDomain(ctx, fmt.Sprintf("%d.example.com", index), []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.1")}}))
	}
	started, done := make(chan struct{}, maxResolvedDomains), make(chan struct{})
	go func() {
		defer close(done)
		manager.refreshDomains(ctx, func(ctx context.Context, _ string) ([]ResolvedIP, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	for range domainRefreshWorkers {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("lookup worker did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("too many lookup workers")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh did not stop after cancellation")
	}
	require.Empty(t, started, "cancelled batch must not start queued lookups")
}

func TestDomainRefresh_SkipsFreshLeasesAndBacksOffFailures(t *testing.T) {
	manager := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	allowed, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, manager.ApplyStatic(ctx, allowed))
	now := time.Unix(1_000, 0)
	manager.tracker.now = func() time.Time { return now }
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.1"), TTL: 5 * time.Minute}}
	require.NoError(t, manager.AddResolvedDomain(ctx, "example.com", ips))

	lookups := 0
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		lookups++
		return nil, fmt.Errorf("upstream unavailable")
	})
	require.Zero(t, lookups, "a domain whose lease is not near expiry must not be queried")

	now = now.Add(clampTTL(ips[0].TTL) - domainRefreshLead)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		lookups++
		return nil, fmt.Errorf("upstream unavailable")
	})
	require.Equal(t, 1, lookups, "a domain near lease expiry must be queried")
	entry := manager.domains["example.com"]
	require.NotNil(t, entry)
	require.Equal(t, 1, entry.failures)
	require.Equal(t, now.Add(domainRefreshInterval), entry.retryAt, "the first failure must retry at the base interval")

	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		lookups++
		return nil, fmt.Errorf("upstream unavailable")
	})
	require.Equal(t, 1, lookups, "a backed-off domain must not be re-queried")

	now = now.Add(time.Minute)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		lookups++
		return ips, nil
	})
	require.Equal(t, 2, lookups, "backoff expiry must re-query")
	entry = manager.domains["example.com"]
	require.NotNil(t, entry)
	require.Zero(t, entry.failures)
	require.True(t, entry.retryAt.IsZero(), "success must clear the backoff")

	// Little lease left: the retry delay must be clamped so one retry stays
	// possible before the earliest lease expires (less one lookup timeout).
	now = now.Add(clampTTL(ips[0].TTL) - 30*time.Second)
	manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		lookups++
		return nil, fmt.Errorf("upstream unavailable")
	})
	require.Equal(t, 3, lookups)
	entry = manager.domains["example.com"]
	require.Equal(t, 1, entry.failures)
	require.Equal(t, now.Add(25*time.Second), entry.retryAt, "retry must stay possible before the lease lapses")
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

func TestRefreshActiveConnections_RenewsKnownActiveIP(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute},
	}))

	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
		{remote: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"},
		{remote: netip.MustParseAddr("2.2.2.2"), state: "ESTABLISHED"},
		{remote: netip.MustParseAddr("1.1.1.1"), state: "TIME_WAIT"},
	}, m))

	require.Len(t, scripts, 2)
	require.Equal(t, "add element inet opensandbox dyn_allow_v4 { 1.1.1.1 }\n"+
		"delete element inet opensandbox dyn_allow_v4 { 1.1.1.1 }\n"+
		"add element inet opensandbox dyn_allow_v4 { 1.1.1.1 timeout 360s }\n", scripts[1])
}

func TestRefreshActiveConnections_RenewsOnceAfterConnectionCloses(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("2001:db8::1"), TTL: time.Minute},
	}))
	active := []tcpConnection{
		{remote: netip.MustParseAddr("2001:db8::1"), state: "ESTABLISHED"},
	}
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), active, m))
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), nil, m))
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), nil, m))

	require.Len(t, scripts, 3)
	require.Equal(t, "add element inet opensandbox dyn_allow_v6 { 2001:db8::1 }\n"+
		"delete element inet opensandbox dyn_allow_v6 { 2001:db8::1 }\n"+
		"add element inet opensandbox dyn_allow_v6 { 2001:db8::1 timeout 360s }\n", scripts[1])
	require.Equal(t, scripts[1], scripts[2])
}

func TestRefreshActiveConnections_DoesNotExtendFailedRenewal(t *testing.T) {
	now := time.Unix(1_000, 0)
	manager := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	manager.tracker.now = func() time.Time { return now }
	address := netip.MustParseAddr("192.0.2.1")
	require.NoError(t, manager.AddResolvedIPs(context.Background(), []ResolvedIP{{Addr: address, TTL: time.Minute}}))
	expiresAt := manager.tracker.dynamicIPs[address]
	now = now.Add(30 * time.Second)
	manager.run = func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("nft failed") }
	require.Error(t, manager.tracker.refreshActiveConnections(context.Background(), []tcpConnection{{remote: address, state: "ESTABLISHED"}}, manager))
	require.Equal(t, expiresAt, manager.tracker.dynamicIPs[address])
	require.Empty(t, manager.tracker.previousActiveIPs)
}

func TestApplyStatic_ClearsTrackedDynamicIPs(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute},
	}))
	require.NoError(t, m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()))
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
		{remote: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"},
	}, m))

	require.Len(t, scripts, 2)
}

func TestAddResolvedIPs_DoesNotTrackFailedInsert(t *testing.T) {
	m := NewManagerWithRunner(func(_ context.Context, _ string) ([]byte, error) {
		return nil, fmt.Errorf("nft failed")
	})
	require.Error(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute},
	}))

	m.run = func(_ context.Context, _ string) ([]byte, error) {
		require.FailNow(t, "failed insert must not become refresh eligible")
		return nil, nil
	}
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
		{remote: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"},
	}, m))
}

func TestRefreshActiveConnections_ForgetsExpiredInactiveIP(t *testing.T) {
	now := time.Unix(1_000, 0)
	m := NewManagerWithRunner(func(_ context.Context, _ string) ([]byte, error) {
		return nil, nil
	})
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: 10 * time.Second},
	}))
	now = now.Add(71 * time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), nil, m))

	require.Empty(t, m.tracker.dynamicIPs)
}

func TestRefreshActiveConnections_RenewsExpiredActiveIP(t *testing.T) {
	now := time.Unix(1_000, 0)
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: 10 * time.Second},
	}))
	now = now.Add(71 * time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{
		{remote: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"},
	}, m))

	require.Len(t, scripts, 2)
	require.Equal(t, now.Add(6*time.Minute), m.tracker.dynamicIPs[netip.MustParseAddr("1.1.1.1")])
}

func TestStartConnectionRefresh_StopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{}, 1)
	m := NewManagerWithRunnerAndOptions(func(_ context.Context, _ string) ([]byte, error) {
		return nil, nil
	}, Options{ConnectionRefreshInterval: time.Millisecond})
	m.tracker.listConnections = func(context.Context) ([]tcpConnection, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return nil, nil
	}
	m.StartConnectionRefresh(ctx)
	select {
	case <-called:
	case <-time.After(time.Second):
		require.FailNow(t, "refresh worker did not run")
	}
	cancel()
	time.Sleep(10 * time.Millisecond)
	for len(called) > 0 {
		<-called
	}
	time.Sleep(10 * time.Millisecond)
	require.Empty(t, called)
}

func TestStartConnectionRefresh_PollErrorClearsPriorActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var scripts []string
	var polls int
	done := make(chan struct{})
	m := NewManagerWithRunnerAndOptions(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	}, Options{ConnectionRefreshInterval: time.Millisecond})
	m.tracker.listConnections = func(context.Context) ([]tcpConnection, error) {
		polls++
		switch polls {
		case 1:
			return []tcpConnection{{remote: netip.MustParseAddr("1.1.1.1"), state: "ESTABLISHED"}}, nil
		case 2:
			return nil, fmt.Errorf("proc unavailable")
		default:
			select {
			case <-done:
			default:
				close(done)
			}
			return nil, nil
		}
	}
	require.NoError(t, m.AddResolvedIPs(context.Background(), []ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute},
	}))
	m.StartConnectionRefresh(ctx)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "refresh worker did not complete poll sequence")
	}
	cancel()
	time.Sleep(10 * time.Millisecond)

	// Initial insert plus the active renewal. A stale final renewal must not be
	// emitted after the observation gap.
	require.Len(t, scripts, 2)
}

func TestNewManager_DefaultsConnectionRefreshInterval(t *testing.T) {
	m := NewManagerWithOptions(Options{})
	require.Equal(t, 30*time.Second, m.opts.ConnectionRefreshInterval)
}

func TestManagerSerializesConcurrentTrackerUpdates(t *testing.T) {
	m := NewManagerWithRunner(func(_ context.Context, _ string) ([]byte, error) {
		return nil, nil
	})
	addr := netip.MustParseAddr("1.1.1.1")
	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 20 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			errs <- m.AddResolvedIPs(context.Background(), []ResolvedIP{{Addr: addr, TTL: time.Minute}})
		}()
		go func() {
			defer wg.Done()
			errs <- m.tracker.refreshActiveConnections(context.Background(), []tcpConnection{{remote: addr, state: "ESTABLISHED"}}, m)
		}()
		go func() {
			defer wg.Done()
			errs <- m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
