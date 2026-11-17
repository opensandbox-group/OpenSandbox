// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDomainPortsScopedAnswersAndDenyOrder(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunnerAndOptions(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	}, Options{})
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443,8443]},{"action":"deny","target":"example.com","ports":[8443]}]}`)
	ctx := context.Background()
	require.NoError(t, m.ApplyStatic(ctx, p))
	require.NoError(t, m.AddResolvedDomain(ctx, "EXAMPLE.COM.", []ResolvedIP{{Addr: netip.MustParseAddr("::ffff:192.0.2.10"), TTL: time.Minute}, {Addr: netip.MustParseAddr("2001:db8::10"), TTL: time.Minute}}))
	require.Empty(t, m.tracker.dynamicIPs)
	require.Contains(t, scripts[1], "dyn_allow_port_v4 { 192.0.2.10 . 443 timeout 120s }")
	require.Contains(t, scripts[1], "dyn_allow_port_v6 { 2001:db8::10 . 443 timeout 120s }")
	require.Contains(t, scripts[1], "dyn_deny_port_v4 { 192.0.2.10 . 8443 timeout 120s }")
	require.NotContains(t, scripts[1], "dyn_allow_v4 {")
	rules := scripts[0]
	deny := strings.Index(rules, "ip daddr . tcp dport @dyn_deny_port_v4 drop")
	plainDeny := strings.Index(rules, "ip daddr @deny_v4 drop")
	allow := strings.Index(rules, "ip daddr . tcp dport @dyn_allow_port_v4 accept")
	require.GreaterOrEqual(t, deny, 0)
	require.Less(t, deny, allow)
	require.Less(t, plainDeny, allow)
}

func TestDomainPortsOverlappingSourcesKeepAbsoluteDeadlines(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) { rendered = script; return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[443]},{"action":"allow","target":"b.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	addr := netip.MustParseAddr("192.0.2.10")
	endpoint := TCPEndpoint{Addr: addr, Port: 443}
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: addr, TTL: 5 * time.Minute}}))
	deadline := m.tracker.dynamicEndpoints[endpoint]
	now = now.Add(30 * time.Second)
	before := rendered
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: addr, TTL: time.Second}}))
	require.Equal(t, deadline, m.tracker.dynamicEndpoints[endpoint])
	require.Equal(t, before, rendered, "a shorter sibling must not rewrite the shared endpoint")
	p = mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	require.Equal(t, deadline, m.tracker.dynamicEndpoints[endpoint])
	require.Contains(t, rendered, "192.0.2.10 . 443 timeout 330s")
	require.Contains(t, m.domains, "a.example")
	require.NotContains(t, m.domains, "b.example")
}

func TestDomainPortsReplacementNarrowsLegacyAndAddsObservedDeny(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) { rendered = script; return nil, nil })
	ctx := context.Background()
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	addr := netip.MustParseAddr("192.0.2.10")
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr, TTL: time.Minute}}))
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]},{"action":"deny","target":"example.com","ports":[8443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.Empty(t, m.tracker.dynamicIPs)
	require.Contains(t, rendered, "dyn_allow_port_v4 { 192.0.2.10 . 443 timeout")
	require.Contains(t, rendered, "dyn_deny_port_v4 { 192.0.2.10 . 8443 timeout")
	require.NotContains(t, rendered, "add element inet opensandbox dyn_allow_v4")
	// A late foreground DNS answer is a fact, not an old whole-IP grant.
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.11")}}))
	require.Empty(t, m.tracker.dynamicIPs)
	require.Contains(t, rendered, "192.0.2.11 . 443 timeout")
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"deny"}`)))
	require.Error(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
}

func TestDomainPortsDefaultAllowObservationsSupportLaterDeny(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) { rendered = script; return nil, nil })
	ctx := context.Background()
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"allow"}`)))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.10")}}))
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"example.com","ports":[443]}]}`)))
	require.Contains(t, rendered, "dyn_deny_port_v4 { 192.0.2.10 . 443 timeout")
}

func TestDomainPortsFailedWritesDoNotPublishLeases(t *testing.T) {
	ctx := context.Background()
	m := NewManagerWithRunnerAndOptions(func(context.Context, string) ([]byte, error) { return nil, nil }, Options{QuiesceOnApplyFailure: true})
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.10")}}
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", ips))
	entry := m.domains["example.com"]
	endpoint := TCPEndpoint{Addr: ips[0].Addr, Port: 443}
	deadline := m.tracker.dynamicEndpoints[endpoint]
	m.run = func(context.Context, string) ([]byte, error) { return nil, errors.New("result lost after submission") }
	require.Error(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.11")}}))
	require.Same(t, entry, m.domains["example.com"])
	require.Equal(t, deadline, m.tracker.dynamicEndpoints[endpoint])
	require.Len(t, m.tracker.dynamicEndpoints, 1)
	require.True(t, m.isQuiesced(), "unknown effects must not be treated as unchanged")
}

func TestDomainPortsActivityIsEndpointScopedAndGraceIsBounded(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443,8443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	addr := netip.MustParseAddr("192.0.2.10")
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr, TTL: time.Second}}))
	other := m.tracker.dynamicEndpoints[TCPEndpoint{Addr: addr, Port: 8443}]
	now = now.Add(30 * time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}, m))
	require.Equal(t, other, m.tracker.dynamicEndpoints[TCPEndpoint{Addr: addr, Port: 8443}])
	require.Equal(t, now.Add(360*time.Second), m.tracker.dynamicEndpoints[TCPEndpoint{Addr: addr, Port: 443}])
	now = now.Add(60 * time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
	grace := m.tracker.dynamicEndpoints[TCPEndpoint{Addr: addr, Port: 443}]
	require.Equal(t, now.Add(360*time.Second), grace)
	require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: addr, Port: 8443})
	now = now.Add(time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
	require.Equal(t, grace, m.tracker.dynamicEndpoints[TCPEndpoint{Addr: addr, Port: 443}])
}

func TestDomainPortsEstablishedDropsRetainOnlyAuthorizedEndpoints(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.10")
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[443,8443]},{"action":"allow","target":"b.example","ports":[443]}]}`)
	newPol := mustPolicy(t, `{"egress":[{"action":"allow","target":"b.example","ports":[443]},{"action":"deny","target":"b.example","ports":[9443]}]}`)
	drops := ComputeEstablishedDrops(old, newPol, map[string][]netip.Addr{"a.example": {addr}, "b.example": {addr}})
	require.False(t, drops.MatchesFlow(addr, 443), "overlapping endpoint remains authorized")
	require.True(t, drops.MatchesFlow(addr, 8443), "a sibling name is not a whole-IP keep")
	require.True(t, drops.MatchesFlow(addr, 9443), "explicit scoped deny precedes keeps")
	legacy := mustPolicy(t, `{"egress":[{"action":"allow","target":"b.example"}]}`)
	drops = ComputeEstablishedDrops(legacy, newPol, map[string][]netip.Addr{"b.example": {addr}})
	require.False(t, drops.MatchesFlow(addr, 443))
	require.True(t, drops.MatchesFlow(addr, 8443))
}

func TestDomainPortsEstablishedDenySurvivesPlainApply(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com"}]}`)
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]},{"action":"deny","target":"example.com","ports":[8443]}]}`)
	addr := netip.MustParseAddr("192.0.2.10")
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.True(t, m.lastDrops.MatchesFlow(addr, 8443))
	require.False(t, m.lastDrops.MatchesFlow(addr, 443))
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.True(t, m.lastDrops.MatchesFlow(addr, 8443))
	require.False(t, m.lastDrops.MatchesFlow(addr, 443))
}

func TestDomainPortsInflightRefreshCannotRestoreOldPermissions(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443,8443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.10")}}
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", ips))
	entry := m.domains["example.com"]
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]}]}`)))
	m.applyDomainRefresh(ctx, "example.com", entry, ips)
	require.Len(t, m.tracker.dynamicEndpoints, 1)
	require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: ips[0].Addr, Port: 8443})
}

func TestDomainPortsFailedActivityDoesNotPublishGrace(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]}]}`)))
	addr := netip.MustParseAddr("192.0.2.10")
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
	endpoint := TCPEndpoint{Addr: addr, Port: 443}
	deadline := m.tracker.dynamicEndpoints[endpoint]
	now = now.Add(30 * time.Second)
	m.run = func(context.Context, string) ([]byte, error) { return nil, errors.New("nft renewal failed") }
	require.Error(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}, m))
	require.Equal(t, deadline, m.tracker.dynamicEndpoints[endpoint])
	require.Empty(t, m.tracker.previousActiveEndpoints)
	require.Empty(t, m.domains["example.com"].activity)
}

func TestDomainPortsDenyWithoutDNSAdmissionUsesObservedAddress(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) { rendered = script; return nil, nil })
	ctx := context.Background()
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com"}]}`)))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.10")}}))
	p := mustPolicy(t, `{"egress":[{"action":"deny","target":"example.com","ports":[443]}]}`)
	require.False(t, p.DomainPermissions("example.com").Allowed)
	require.NoError(t, m.ApplyStatic(ctx, p))
	require.Contains(t, rendered, "dyn_deny_port_v4 { 192.0.2.10 . 443 timeout")
	require.NotContains(t, rendered, "add element inet opensandbox dyn_allow_v4")
	require.NotContains(t, rendered, "add element inet opensandbox dyn_allow_port_v4")
}

func TestDomainPortsRefreshDoesNotRenewRetiredSourceOrShortenSibling(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	p := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[443]},{"action":"allow","target":"b.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, p))
	first, retired := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: first, TTL: time.Minute}, {Addr: retired, TTL: time.Minute}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: retired, TTL: 5 * time.Minute}}))
	siblingDeadline := m.tracker.dynamicEndpoints[TCPEndpoint{Addr: retired, Port: 443}]
	now = now.Add(time.Minute)
	m.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
		if !m.mu.TryLock() {
			return nil, errors.New("external DNS ran under manager lock")
		}
		m.mu.Unlock()
		return []ResolvedIP{{Addr: first, TTL: time.Minute}}, nil
	})
	require.NotContains(t, m.domains["a.example"].addresses, retired)
	require.Equal(t, siblingDeadline, m.tracker.dynamicEndpoints[TCPEndpoint{Addr: retired, Port: 443}])
	now = now.Add(61 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example","ports":[443]}]}`)))
	require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: retired, Port: 443})
	require.Contains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: first, Port: 443})
}

func TestDomainPortsLegacyNegativeDNSRetainsTighteningProvenance(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	addr := netip.MustParseAddr("192.0.2.10")
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}, m))
	now = now.Add(300 * time.Second)
	m.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) { return nil, nil })
	require.Empty(t, m.domains["example.com"].addresses)
	tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]}]}`)
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.Contains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: addr, Port: 443})
	require.False(t, m.lastDrops.MatchesFlow(addr, 443))
	require.True(t, m.lastDrops.MatchesFlow(addr, 8443))
}

func TestDomainPortsCapacityDoesNotEraseLiveSources(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[443]}]}`)))
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	for i := range maxResolvedDomains {
		addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})
		require.NoError(t, m.AddResolvedDomain(ctx, addr.String()+".example", []ResolvedIP{{Addr: addr}}))
	}
	require.Error(t, m.AddResolvedDomain(ctx, "overflow.example", []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.10")}}))
	require.Len(t, m.domains, maxResolvedDomains)
	require.Len(t, m.tracker.dynamicEndpoints, maxResolvedDomains)
	now = now.Add(61 * time.Second)
	require.NoError(t, m.AddResolvedDomain(ctx, "overflow.example", []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.10")}}))
	require.Contains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: netip.MustParseAddr("198.51.100.10"), Port: 443})
}

func TestDomainPortsEstablishedDeniesUseLiveLeases(t *testing.T) {
	var rendered string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) { rendered = script; return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	tight := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"example.com","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	addr := netip.MustParseAddr("192.0.2.10")
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
	require.NoError(t, m.ApplyStatic(ctx, tight))
	require.NotContains(t, rendered, "@dyn_deny_port_v4 ct state established,related drop", "ordinary apply must not opt in to teardown")
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.Empty(t, m.lastDrops.DenyPortDrops, "DNS-derived denies must not become unconditional carried targets")
	require.Len(t, m.lastDrops.DomainPortDenies, 1)
	require.Contains(t, rendered, "ip daddr 192.0.2.10 tcp dport { 443 } ip daddr . tcp dport @dyn_deny_port_v4 ct state established,related drop")
	more := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"example.com","ports":[443,8443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, more))
	require.Len(t, m.lastDrops.DomainPortDenies, 1)
	require.Equal(t, []int{443}, m.lastDrops.DomainPortDenies[0].Ports, "a plain update cannot opt a different endpoint into teardown")
	m.applyDomainRefresh(ctx, "example.com", m.domains["example.com"], nil)
	now = now.Add(61 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, more))
	require.Empty(t, m.lastDrops.DenyPortDrops)
	require.NotContains(t, rendered, "add element inet opensandbox dyn_deny_port_v4", "expired DNS facts cannot reinstall a deny lease")
}

func TestDomainPortsFullAddressRepeatPreservesSourceDeadlines(t *testing.T) {
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com"}]}`)))
	ips := make([]ResolvedIP, maxDomainAddresses)
	for i := range ips {
		ips[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), TTL: 5 * time.Minute}
	}
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", ips))
	deadline := now.Add(360 * time.Second)
	now = now.Add(30 * time.Second)
	for i := range ips {
		ips[i].TTL = 0
	}
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", ips))
	for _, ip := range ips {
		require.Equal(t, deadline, m.domains["example.com"].expires[ip.Addr])
		require.Equal(t, deadline, m.tracker.dynamicIPs[ip.Addr])
	}
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]}]}`)))
	require.Len(t, m.tracker.dynamicEndpoints, maxDomainAddresses)
	for _, ip := range ips {
		require.Equal(t, deadline, m.tracker.dynamicEndpoints[TCPEndpoint{Addr: ip.Addr, Port: 443}])
	}
}

func TestDomainPortsLegacyDisjointAddressOverflowIsAtomic(t *testing.T) {
	for _, defaultAllow := range []bool{false, true} {
		name := "portless_allow"
		initial := `{"egress":[{"action":"allow","target":"example.com"}]}`
		tightened := `{"egress":[{"action":"allow","target":"example.com","ports":[8443]},{"action":"deny","target":"example.com","ports":[443]}]}`
		if defaultAllow {
			name = "default_allow"
			initial = `{"defaultAction":"allow"}`
			tightened = `{"defaultAction":"allow","egress":[{"action":"deny","target":"example.com","ports":[443]}]}`
		}
		t.Run(name, func(t *testing.T) {
			var calls int
			var rendered string
			var writeErr error
			m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
				calls++
				rendered = script
				return nil, writeErr
			})
			ctx := context.Background()
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			old := mustPolicy(t, initial)
			tight := mustPolicy(t, tightened)
			require.NoError(t, m.ApplyStatic(ctx, old))
			first, second := make([]ResolvedIP, maxDomainAddresses), make([]ResolvedIP, maxDomainAddresses)
			for i := range first {
				first[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}), TTL: 5 * time.Minute}
				second[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)}), TTL: 5 * time.Minute}
			}
			require.NoError(t, m.AddResolvedDomain(ctx, "example.com", first))
			entry, before, deadline := m.domains["example.com"], calls, now.Add(360*time.Second)
			now = now.Add(30 * time.Second)
			require.ErrorContains(t, m.AddResolvedDomain(ctx, "example.com", second), "capacity")
			require.Equal(t, before, calls, "overflow must be rejected before any kernel write")
			require.Same(t, entry, m.domains["example.com"])
			writeErr = errors.New("rejected")
			require.Error(t, m.AddResolvedDomain(ctx, "example.com", first))
			require.Same(t, entry, m.domains["example.com"], "a failed renewal cannot publish longer source deadlines")
			_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
			require.Error(t, err)
			require.Same(t, old, m.domainPolicy)
			require.Same(t, entry, m.domains["example.com"])
			for i := range first {
				require.Equal(t, deadline, entry.expires[first[i].Addr])
				require.Equal(t, deadline, m.tracker.dynamicIPs[first[i].Addr])
				require.NotContains(t, entry.expires, second[i].Addr)
				require.NotContains(t, m.tracker.dynamicIPs, second[i].Addr)
			}
			writeErr = nil
			_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
			require.NoError(t, err)
			for i := range first {
				require.Contains(t, rendered, "dyn_deny_port_v4 { "+first[i].Addr.String()+" . 443 timeout 330s }")
				require.NotContains(t, rendered, second[i].Addr.String()+" .")
				require.True(t, m.lastDrops.MatchesFlow(first[i].Addr, 443))
				require.False(t, m.lastDrops.MatchesFlow(first[i].Addr, 8443))
				if !defaultAllow {
					require.Equal(t, deadline, m.tracker.dynamicEndpoints[TCPEndpoint{Addr: first[i].Addr, Port: 8443}])
				}
			}
			if !defaultAllow {
				require.Empty(t, m.tracker.dynamicIPs, "narrowing cannot preserve a whole-address grant")
			}
		})
	}
}

func TestDomainPortsLegacyDomainCapacityPreservesTighteningProvenance(t *testing.T) {
	var calls int
	var rendered string
	var writeErr error
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		calls++
		rendered = script
		return nil, writeErr
	})
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	old := mustPolicy(t, `{"defaultAction":"allow"}`)
	tight := mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"192.0.2.1.example","ports":[443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	entries := make(map[string]*resolvedDomain)
	for i := range maxResolvedDomains {
		addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)})
		domain := addr.String() + ".example"
		require.NoError(t, m.AddResolvedDomain(ctx, domain, []ResolvedIP{{Addr: addr, TTL: 5 * time.Minute}}))
		entries[domain] = m.domains[domain]
	}
	before, deadline := calls, now.Add(360*time.Second)
	now = now.Add(30 * time.Second)
	overflow := netip.MustParseAddr("198.51.100.1")
	require.ErrorContains(t, m.AddResolvedDomain(ctx, "overflow.example", []ResolvedIP{{Addr: overflow, TTL: 5 * time.Minute}}), "capacity")
	require.Equal(t, before, calls, "the 129th domain must not install an unowned kernel lease")
	require.Len(t, m.domains, maxResolvedDomains)
	require.Len(t, m.tracker.dynamicIPs, maxResolvedDomains)
	require.NotContains(t, m.domains, "overflow.example")
	require.NotContains(t, m.tracker.dynamicIPs, overflow)
	writeErr = errors.New("rejected")
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.Error(t, err)
	require.Same(t, old, m.domainPolicy)
	for domain, entry := range entries {
		require.Same(t, entry, m.domains[domain])
		for addr := range entry.expires {
			require.Equal(t, deadline, entry.expires[addr])
			require.Equal(t, deadline, m.tracker.dynamicIPs[addr])
		}
	}
	writeErr = nil
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, old, tight)
	require.NoError(t, err)
	require.Len(t, m.domains, maxResolvedDomains)
	require.Contains(t, rendered, "dyn_deny_port_v4 { 192.0.2.1 . 443 timeout 330s }")
	require.True(t, m.lastDrops.MatchesFlow(netip.MustParseAddr("192.0.2.1"), 443))
	require.False(t, m.lastDrops.MatchesFlow(netip.MustParseAddr("192.0.2.1"), 8443))
}

func TestDomainPortsLegacyDomainCapacityReusesOnlyExpiredSources(t *testing.T) {
	var calls int
	var writeErr error
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) {
		calls++
		return nil, writeErr
	})
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"allow"}`)))
	entries := make(map[string]*resolvedDomain)
	var active []tcpConnection
	for i := range maxResolvedDomains {
		addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)})
		domain := addr.String() + ".example"
		require.NoError(t, m.AddResolvedDomain(ctx, domain, []ResolvedIP{{Addr: addr, TTL: 5 * time.Minute}}))
		entries[domain] = m.domains[domain]
		active = append(active, tcpConnection{remote: addr, remotePort: 443, state: "ESTABLISHED"})
	}
	now = now.Add(30 * time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
	now = now.Add(331 * time.Second)
	overflow := []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.1"), TTL: 5 * time.Minute}}
	before := calls
	require.ErrorContains(t, m.AddResolvedDomain(ctx, "overflow.example", overflow), "capacity")
	require.Equal(t, before, calls, "expired DNS deadlines do not release live TCP grace ownership")
	for domain, entry := range entries {
		require.Same(t, entry, m.domains[domain])
	}
	now = now.Add(30 * time.Second)
	writeErr = errors.New("rejected")
	require.Error(t, m.AddResolvedDomain(ctx, "overflow.example", overflow))
	require.NotContains(t, m.domains, "overflow.example")
	require.NotContains(t, m.tracker.dynamicIPs, overflow[0].Addr)
	for domain, entry := range entries {
		require.Same(t, entry, m.domains[domain], "failed insertion cannot evict even expired provenance")
	}
	writeErr = nil
	require.NoError(t, m.AddResolvedDomain(ctx, "overflow.example", overflow))
	require.Len(t, m.domains, maxResolvedDomains)
	require.Contains(t, m.domains, "overflow.example")
	require.Equal(t, now.Add(360*time.Second), m.tracker.dynamicIPs[overflow[0].Addr])
	retained := 0
	for domain, entry := range entries {
		if current := m.domains[domain]; current != nil {
			require.Same(t, entry, current)
			retained++
		}
	}
	require.Equal(t, maxResolvedDomains-1, retained, "exactly one genuinely expired source can be reclaimed")
}

func TestDomainPortsDeltaRenewalAndSharedSourceRemoval(t *testing.T) {
	var scripts []string
	m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	})
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[443,8443]},{"action":"deny","target":"*.example","ports":[9443]}]}`)))
	shared, other := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: shared}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: shared, TTL: 5 * time.Minute}}))
	before := len(scripts)
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: shared}}))
	require.Len(t, scripts, before, "unchanged effective membership must not invoke nft")
	deadline := m.tracker.dynamicEndpoints[TCPEndpoint{Addr: shared, Port: 443}]
	now = now.Add(61 * time.Second)
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: other}}))
	require.NotContains(t, m.domains["a.example"].expires, shared, "the expired source has retired")
	require.Equal(t, deadline, m.tracker.dynamicEndpoints[TCPEndpoint{Addr: shared, Port: 443}])
	require.NotContains(t, scripts[len(scripts)-1], shared.String(), "retiring one source must not rewrite its live sibling")
	now = now.Add(time.Second)
	require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: other, TTL: 5 * time.Minute}}))
	renewal := scripts[len(scripts)-1]
	require.Equal(t, 9, strings.Count(renewal, "\n"), "two allows and one deny are the only changed elements")
	require.NotContains(t, renewal, "flush")
	require.NotContains(t, renewal, shared.String())
	require.Equal(t, now.Add(360*time.Second), m.tracker.dynamicDenies[TCPEndpoint{Addr: other, Port: 9443}])
	before = len(scripts)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
	require.Len(t, scripts, before, "idle refresh must not rewrite live membership")
	now = now.Add(time.Second)
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: other, remotePort: 443, state: "ESTABLISHED"}}, m))
	activity := scripts[len(scripts)-1]
	require.NotContains(t, activity, ". 8443", "activity on one port must not renew its sibling")
	require.NotContains(t, activity, shared.String())
}

func TestDomainPortsDeltaFailureRecovery(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "unknown"}[unknown], func(t *testing.T) {
			var scripts []string
			var writeErr error
			m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
				scripts = append(scripts, script)
				return nil, writeErr
			})
			ctx := context.Background()
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			p := mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[443]},{"action":"deny","target":"*.example","ports":[8443]}]}`)
			require.NoError(t, m.ApplyStatic(ctx, p))
			addr, failed := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
			require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: addr}}))
			entry := m.domains["a.example"]
			writeErr = &ApplyError{Effect: ApplyUnchanged, Err: errors.New("not submitted")}
			if unknown {
				writeErr = errors.New("result lost after submission")
			}
			require.Error(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: failed}}))
			require.Same(t, entry, m.domains["a.example"])
			require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: failed, Port: 443})
			require.Equal(t, unknown, m.domainLeasesDirty)
			// A second failed recovery must retain the dirty marker and never
			// publish the failed source, even if that retry was not submitted.
			if unknown {
				writeErr = &ApplyError{Effect: ApplyUnchanged, Err: errors.New("not submitted")}
				require.Error(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: addr}}))
				require.True(t, m.domainLeasesDirty)
			}
			writeErr = nil
			before := len(scripts)
			require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: addr}}))
			require.False(t, m.domainLeasesDirty)
			if unknown {
				require.Len(t, scripts, before+1, "a no-op still requires full reconciliation after unknown apply")
				require.Equal(t, 6, strings.Count(scripts[before], "flush set"))
				require.NotContains(t, scripts[before], "delete table")
				require.NotContains(t, scripts[before], "add rule")
				require.NotContains(t, scripts[before], failed.String())
				require.Len(t, m.tracker.dynamicDenies, 1)
			} else {
				require.Len(t, scripts, before, "trusted unchanged failure must retain a usable installed snapshot")
			}
		})
	}
}

func TestTimedChangesSkipUnchangedAndShortenDeadlines(t *testing.T) {
	now := time.Unix(1000, 0)
	unchanged, changed, expired := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::11"), netip.MustParseAddr("192.0.2.12")
	before := map[netip.Addr]time.Time{unchanged: now.Add(time.Minute), changed: now.Add(time.Minute), expired: now.Add(-time.Second)}
	after := map[netip.Addr]time.Time{unchanged: before[unchanged], changed: now.Add(10 * time.Second)}
	var b strings.Builder
	WriteTimedIPChanges(&b, "test", "v4", "v6", before, after, now)
	require.NotContains(t, b.String(), unchanged.String())
	require.Contains(t, b.String(), "v6 { 2001:db8::11 timeout 10s }")
	require.Contains(t, b.String(), "add element inet test v4 { 192.0.2.12 }\ndelete element inet test v4 { 192.0.2.12 }\n")
	require.Equal(t, 5, strings.Count(b.String(), "\n"))
	oldPorts, newPorts := make(map[TCPEndpoint]time.Time), make(map[TCPEndpoint]time.Time)
	for addr, deadline := range before {
		oldPorts[TCPEndpoint{Addr: addr, Port: 443}] = deadline
	}
	for addr, deadline := range after {
		newPorts[TCPEndpoint{Addr: addr, Port: 443}] = deadline
	}
	b.Reset()
	WriteTimedPortChanges(&b, "test", "v4", "v6", oldPorts, newPorts, now)
	require.NotContains(t, b.String(), unchanged.String())
	require.Contains(t, b.String(), "v6 { 2001:db8::11 . 443 timeout 10s }")
	require.Contains(t, b.String(), "add element inet test v4 { 192.0.2.12 . 443 }\ndelete element inet test v4 { 192.0.2.12 . 443 }\n")
	require.Equal(t, 5, strings.Count(b.String(), "\n"))
}

func TestDomainPortsDeltaChangedWorkBound(t *testing.T) {
	var script string
	m := NewManagerWithRunner(func(_ context.Context, value string) ([]byte, error) {
		script = value
		return nil, nil
	})
	ctx := context.Background()
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[443,444,445,446,447,448,449,450]}]}`)))
	var refresh []ResolvedIP
	var domain string
	for source := range 64 {
		ips := make([]ResolvedIP, 16)
		for i := range ips {
			ips[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{10, byte(source), 0, byte(i + 1)}), TTL: 5 * time.Minute}
		}
		domain = ips[0].Addr.String() + ".example"
		require.NoError(t, m.AddResolvedDomain(ctx, domain, ips))
		refresh = ips
	}
	now = now.Add(time.Second)
	require.NoError(t, m.AddResolvedDomain(ctx, domain, refresh))
	require.Len(t, m.tracker.dynamicEndpoints, 64*16*8)
	require.Equal(t, 16*8*3, strings.Count(script, "\n"), "refresh work must scale with changed endpoints, not all cached names")
	require.Less(t, len(script), 40_000)
	require.NotContains(t, script, "flush")
}

func TestDomainPortsUnrelatedUpdatesPreserveInflightRevalidation(t *testing.T) {
	for _, activity := range []bool{false, true} {
		t.Run(map[bool]string{false: "dns", true: "activity"}[activity], func(t *testing.T) {
			m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
			ctx := context.Background()
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[443]}]}`)))
			first, other := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
			require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: first}}))
			require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: other}}))
			// A resolver worker retains the source snapshot while outside
			// the manager lock. Traffic for another source must not make
			// that still-current answer look stale when it returns.
			inflight := m.domains["a.example"]
			now = now.Add(time.Second)
			if activity {
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: other, remotePort: 443, state: "ESTABLISHED"}}, m))
			} else {
				require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: other}}))
			}
			now = now.Add(time.Second)
			m.applyDomainRefresh(ctx, "a.example", inflight, []ResolvedIP{{Addr: first, TTL: 5 * time.Minute}})
			require.Equal(t, now.Add(360*time.Second), m.tracker.dynamicEndpoints[TCPEndpoint{Addr: first, Port: 443}], "the unrelated update must not discard the revalidation answer")
			require.Equal(t, now.Add(360*time.Second), m.domains["a.example"].expires[first])
		})
	}
}

func TestDomainPortsUnknownStaticApplyRequiresNormalPolicyRecovery(t *testing.T) {
	for _, quiesce := range []bool{false, true} {
		t.Run(map[bool]string{false: "recoverable", true: "quiesced"}[quiesce], func(t *testing.T) {
			var scripts []string
			var writeErr error
			m := NewManagerWithRunnerAndOptions(func(_ context.Context, script string) ([]byte, error) {
				scripts = append(scripts, script)
				return nil, writeErr
			}, Options{QuiesceOnApplyFailure: quiesce})
			ctx := context.Background()
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			old := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443,8443]}]}`)
			tight := mustPolicy(t, `{"egress":[{"action":"allow","target":"example.com","ports":[443]}]}`)
			require.NoError(t, m.ApplyStatic(ctx, old))
			addr := netip.MustParseAddr("192.0.2.10")
			require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: addr}}))
			entry := m.domains["example.com"]
			writeErr = errors.New("unknown static replacement result")
			require.Error(t, m.ApplyStatic(ctx, tight))
			require.True(t, m.staticStateUncertain)
			writeErr = nil
			before := len(scripts)
			unobserved := netip.MustParseAddr("192.0.2.11")
			require.Error(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: unobserved}}))
			require.Error(t, m.AddResolvedIPs(ctx, []ResolvedIP{{Addr: unobserved}}))
			require.Error(t, m.tracker.refreshActiveConnections(ctx, []tcpConnection{{remote: addr, remotePort: 8443, state: "ESTABLISHED"}}, m))
			now = now.Add(time.Second)
			m.applyDomainRefresh(ctx, "example.com", entry, []ResolvedIP{{Addr: addr, TTL: 5 * time.Minute}})
			require.Len(t, scripts, before, "DNS and activity must not rebuild static rules or grant elements under unknown static state")
			require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: unobserved, Port: 443})
			require.Equal(t, time.Unix(1060, 0), entry.expires[addr], "blocked background revalidation must not publish source deadlines")
			if quiesce {
				require.ErrorIs(t, m.ApplyStatic(ctx, tight), ErrQuiesced)
				require.Len(t, scripts, before, "normal policy apply cannot bypass permanent quiescence")
				return
			}
			require.NoError(t, m.ApplyStatic(ctx, tight))
			require.False(t, m.staticStateUncertain)
			require.Contains(t, scripts[before], "delete table inet opensandbox")
			require.NoError(t, m.AddResolvedDomain(ctx, "example.com", []ResolvedIP{{Addr: unobserved}}))
			require.Contains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: unobserved, Port: 443})
			require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: unobserved, Port: 8443})
		})
	}
}

func TestDomainPortsActivityRecoveryAfterInstalledDeadline(t *testing.T) {
	for _, whole := range []bool{false, true} {
		for _, closed := range []bool{false, true} {
			name := map[bool]string{false: "endpoint", true: "whole_ip"}[whole] + "/" + map[bool]string{false: "active", true: "final_grace"}[closed]
			t.Run(name, func(t *testing.T) {
				var writeErr error
				var scripts []string
				m := NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
					scripts = append(scripts, script)
					return nil, writeErr
				})
				ctx := context.Background()
				now := time.Unix(1000, 0)
				m.tracker.now = func() time.Time { return now }
				require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"broad.example"},{"action":"allow","target":"scoped.example","ports":[443,8443]}]}`)))
				domain := "scoped.example"
				if whole {
					domain = "broad.example"
				}
				addr := netip.MustParseAddr("192.0.2.10")
				endpoint := TCPEndpoint{Addr: addr, Port: 443}
				require.NoError(t, m.AddResolvedDomain(ctx, domain, []ResolvedIP{{Addr: addr}}))
				installedDeadline := func() time.Time {
					if whole {
						return m.tracker.dynamicIPs[addr]
					}
					return m.tracker.dynamicEndpoints[endpoint]
				}
				active := []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}
				now = now.Add(30 * time.Second)
				if closed {
					require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
					active = nil
					now = now.Add(30 * time.Second)
				}
				lastConfirmed := installedDeadline()
				writeErr = errors.New("renewal result unknown")
				require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
				require.True(t, m.domainLeasesDirty)
				require.Equal(t, lastConfirmed, installedDeadline(), "failed activity must not publish its proposed deadline")
				// Retry after the last confirmed lease expires, including a
				// second unknown failure during the recovery itself.
				now = lastConfirmed.Add(time.Second)
				require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
				require.Equal(t, lastConfirmed, installedDeadline(), "failed recovery must retain known identity without a false installed deadline")
				writeErr = nil
				now = now.Add(time.Second)
				before := len(scripts)
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
				require.False(t, m.domainLeasesDirty)
				require.Equal(t, now.Add(360*time.Second), installedDeadline(), "known activity or final grace must survive recovery after expiry")
				require.Len(t, scripts, before+1, "reconciliation and renewal must commit atomically")
				require.Equal(t, 6, strings.Count(scripts[before], "flush set"))
				require.NotContains(t, scripts[before], "delete table")
				if !whole {
					require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: addr, Port: 8443}, "activity must not revive a sibling port")
				}
				if closed {
					deadline := installedDeadline()
					now = now.Add(time.Second)
					require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
					require.Equal(t, deadline, installedDeadline(), "final grace must remain bounded without renewed activity")
				}
			})
		}
	}
}

func TestDomainPortsDNSRecoveryPreservesExpiredActivityIdentities(t *testing.T) {
	for _, whole := range []bool{false, true} {
		for _, closed := range []bool{false, true} {
			name := map[bool]string{false: "endpoint", true: "whole_ip"}[whole] + "/" + map[bool]string{false: "active", true: "final_grace"}[closed]
			t.Run(name, func(t *testing.T) {
				var writeErr error
				m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, writeErr })
				ctx := context.Background()
				now := time.Unix(1000, 0)
				m.tracker.now = func() time.Time { return now }
				require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"broad.example"},{"action":"allow","target":"*.scoped.example","ports":[443,8443]}]}`)))
				domain := "a.scoped.example"
				if whole {
					domain = "broad.example"
				}
				addr := netip.MustParseAddr("192.0.2.10")
				endpoint := TCPEndpoint{Addr: addr, Port: 443}
				require.NoError(t, m.AddResolvedDomain(ctx, domain, []ResolvedIP{{Addr: addr}}))
				installedDeadline := func() time.Time {
					if whole {
						return m.tracker.dynamicIPs[addr]
					}
					return m.tracker.dynamicEndpoints[endpoint]
				}
				active := []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}
				now = now.Add(30 * time.Second)
				if closed {
					require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
					active = nil
					now = now.Add(30 * time.Second)
				}
				sourceDeadline := m.domains[domain].expires[addr]
				lastConfirmed := installedDeadline()
				writeErr = errors.New("activity result unknown")
				require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
				now = lastConfirmed.Add(time.Second)
				other := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.11")}}
				require.Error(t, m.AddResolvedDomain(ctx, "b.scoped.example", other), "a failed DNS-triggered reconciliation cannot publish a new snapshot")
				require.Equal(t, lastConfirmed, installedDeadline())
				writeErr = nil
				require.NoError(t, m.AddResolvedDomain(ctx, "b.scoped.example", other))
				require.True(t, installedDeadline().IsZero(), "the expired member is not installed after DNS reconciliation")
				require.Equal(t, sourceDeadline, m.domains[domain].expires[addr], "DNS recovery must not extend the original source deadline")
				writeErr = errors.New("activity retry result unknown")
				require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
				require.True(t, installedDeadline().IsZero(), "a failed retry cannot publish proposed activity")
				writeErr = nil
				now = now.Add(time.Second)
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
				require.Equal(t, now.Add(360*time.Second), installedDeadline(), "unrelated DNS must not erase known activity or final-grace identity")
				if !whole {
					require.NotContains(t, m.tracker.dynamicEndpoints, TCPEndpoint{Addr: addr, Port: 8443}, "recovery cannot revive an inactive sibling port")
				}
				if !closed {
					now = now.Add(time.Second)
					require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
				}
				deadline := installedDeadline()
				now = now.Add(time.Second)
				require.NoError(t, m.tracker.refreshActiveConnections(ctx, nil, m))
				require.Equal(t, deadline, installedDeadline(), "final grace must remain bounded without renewed activity")
			})
		}
	}
}

func TestDomainPortsDNSRecoveryRequiresRetainedSourceAndPolicy(t *testing.T) {
	for _, retire := range []string{"source", "policy"} {
		t.Run(retire, func(t *testing.T) {
			var writeErr error
			m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, writeErr })
			ctx := context.Background()
			now := time.Unix(1000, 0)
			m.tracker.now = func() time.Time { return now }
			require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[443]}]}`)))
			addr, other := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")
			endpoint := TCPEndpoint{Addr: addr, Port: 443}
			require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: addr}}))
			active := []tcpConnection{{remote: addr, remotePort: 443, state: "ESTABLISHED"}}
			now = now.Add(30 * time.Second)
			writeErr = errors.New("activity result unknown")
			require.Error(t, m.tracker.refreshActiveConnections(ctx, active, m))
			now = now.Add(31 * time.Second)
			writeErr = nil
			require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: other}}))
			require.Contains(t, m.tracker.renewalEndpoints, endpoint)
			if retire == "source" {
				require.NoError(t, m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: other}}))
				require.NotContains(t, m.domains["a.example"].expires, addr)
				// A further unknown DNS write followed by full recovery must
				// prune the retired identity rather than accumulating old sources.
				writeErr = errors.New("DNS result unknown")
				require.Error(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: other, TTL: 5 * time.Minute}}))
				writeErr = nil
				require.NoError(t, m.AddResolvedDomain(ctx, "b.example", []ResolvedIP{{Addr: other}}))
			} else {
				require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"egress":[{"action":"allow","target":"*.example","ports":[8443]}]}`)))
			}
			require.NotContains(t, m.tracker.renewalEndpoints, endpoint)
			require.NoError(t, m.tracker.refreshActiveConnections(ctx, active, m))
			require.NotContains(t, m.tracker.dynamicEndpoints, endpoint, "retired ownership cannot authorize renewal")
			require.Empty(t, m.tracker.renewalEndpoints, "a successful activity scan retires pending identities")
		})
	}
}
