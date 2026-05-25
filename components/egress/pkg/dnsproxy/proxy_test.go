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

package dnsproxy

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

func TestProxyUpdatePolicy(t *testing.T) {
	proxy, err := New(nil, "127.0.0.1:15353", nil, nil)
	require.NoError(t, err, "init proxy")

	require.NotNil(t, proxy.CurrentPolicy(), "expected default deny policy (non-nil)")
	require.Equal(t, policy.ActionDeny, proxy.CurrentPolicy().Evaluate("example.com."), "expected default deny")

	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err, "parse policy")

	proxy.UpdatePolicy(pol)
	require.NotNil(t, proxy.CurrentPolicy(), "expected policy after update")
	require.Equal(t, policy.ActionAllow, proxy.CurrentPolicy().Evaluate("example.com."), "policy evaluation mismatch")

	proxy.UpdatePolicy(nil)
	require.NotNil(t, proxy.CurrentPolicy(), "expected default deny policy after clearing")
	require.Equal(t, policy.ActionDeny, proxy.CurrentPolicy().Evaluate("example.com."), "expected default deny after clearing")
}

func TestProxyAlwaysOverlayPrecedence(t *testing.T) {
	deny, err := policy.ParseValidatedEgressRule(policy.ActionDeny, "nope.test")
	require.NoError(t, err)
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"nope.test"}]}`)
	require.NoError(t, err)
	proxy, err := New(pol, "127.0.0.1:15353", []policy.EgressRule{deny}, nil)
	require.NoError(t, err)
	require.Equal(t, policy.ActionAllow, proxy.CurrentPolicy().Evaluate("nope.test."), "user policy without overlay")
	require.Equal(t, policy.ActionDeny, proxy.effectivePolicy.Evaluate("nope.test."), "effective policy includes always deny")
}

func TestExtractResolvedIPs(t *testing.T) {
	msg := new(dns.Msg)
	msg.Answer = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Ttl: 120}, A: net.ParseIP("1.2.3.4")},
		&dns.AAAA{Hdr: dns.RR_Header{Name: "example.com.", Ttl: 60}, AAAA: net.ParseIP("2001:db8::1")},
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Ttl: 90}, A: net.ParseIP("5.6.7.8")},
	}
	ips := extractResolvedIPs(msg)
	require.Len(t, ips, 3, "expected 3 IPs")
	// Order follows Answer; check first A and AAAA
	require.Equal(t, "1.2.3.4", ips[0].Addr.String(), "first IP mismatch")
	require.Equal(t, 120*time.Second, ips[0].TTL, "first IP TTL mismatch")
	require.Equal(t, "2001:db8::1", ips[1].Addr.String(), "second IP mismatch")
	require.Equal(t, 60*time.Second, ips[1].TTL, "second IP TTL mismatch")
	require.Equal(t, "5.6.7.8", ips[2].Addr.String(), "third IP mismatch")
	require.Equal(t, 90*time.Second, ips[2].TTL, "third IP TTL mismatch")
}

func TestExtractResolvedIPs_EmptyOrNil(t *testing.T) {
	require.Nil(t, extractResolvedIPs(nil), "nil msg: expected nil")
	msg := new(dns.Msg)
	require.Nil(t, extractResolvedIPs(msg), "empty answer: expected nil")
	msg.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: "x."}, Target: "y."}}
	require.Nil(t, extractResolvedIPs(msg), "CNAME only: expected nil")
}

func TestSetOnResolved(t *testing.T) {
	proxy, err := New(policy.DefaultDenyPolicy(), "", nil, nil)
	require.NoError(t, err)
	var called bool
	var capturedDomain string
	var capturedIPs []nftables.ResolvedIP
	proxy.SetOnResolved(func(domain string, ips []nftables.ResolvedIP) {
		called = true
		capturedDomain = domain
		capturedIPs = ips
	})
	require.NotNil(t, proxy.onResolved, "SetOnResolved did not set callback")
	proxy.SetOnResolved(nil)
	require.Nil(t, proxy.onResolved, "SetOnResolved(nil) did not clear callback")
	_ = called
	_ = capturedDomain
	_ = capturedIPs
}

func TestMaybeNotifyResolved_CallsCallbackWhenAOrAAAA(t *testing.T) {
	proxy, err := New(policy.DefaultDenyPolicy(), "", nil, nil)
	require.NoError(t, err)
	ch := make(chan struct {
		domain string
		ips    []nftables.ResolvedIP
	}, 1)
	proxy.SetOnResolved(func(domain string, ips []nftables.ResolvedIP) {
		ch <- struct {
			domain string
			ips    []nftables.ResolvedIP
		}{domain, ips}
	})

	msg := new(dns.Msg)
	msg.Answer = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Ttl: 120}, A: net.ParseIP("1.2.3.4")},
	}
	proxy.maybeNotifyResolved("example.com.", msg)

	select {
	case got := <-ch:
		require.Equal(t, "example.com.", got.domain, "domain mismatch")
		require.Len(t, got.ips, 1, "expected one resolved IP")
		require.Equal(t, "1.2.3.4", got.ips[0].Addr.String(), "resolved IP mismatch")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "callback was not invoked")
	}
}

func TestMaybeNotifyResolved_NoCallWhenOnResolvedNil(t *testing.T) {
	proxy, err := New(policy.DefaultDenyPolicy(), "", nil, nil)
	require.NoError(t, err)
	msg := new(dns.Msg)
	msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "x.", Ttl: 60}, A: net.ParseIP("10.0.0.1")}}
	proxy.maybeNotifyResolved("x.", msg)
	// No callback set; should not panic. No assertion needed.
}

func TestMaybeNotifyResolved_NoCallWhenNoAOrAAAA(t *testing.T) {
	proxy, err := New(policy.DefaultDenyPolicy(), "", nil, nil)
	require.NoError(t, err)
	ch := make(chan struct {
		domain string
		ips    []nftables.ResolvedIP
	}, 1)
	proxy.SetOnResolved(func(domain string, ips []nftables.ResolvedIP) {
		ch <- struct {
			domain string
			ips    []nftables.ResolvedIP
		}{domain, ips}
	})

	msg := new(dns.Msg)
	msg.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: "x."}, Target: "y."}}
	proxy.maybeNotifyResolved("x.", msg)

	select {
	case <-ch:
		require.FailNow(t, "callback should not be invoked when resp has no A/AAAA")
	case <-time.After(200 * time.Millisecond):
		// Expected: no callback
	}
}
