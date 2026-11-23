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

package dnsproxy

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// startInfraUpstream runs a local fake upstream DNS that answers A records.
func startInfraUpstream(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	server := &dns.Server{
		PacketConn: conn,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			resp := new(dns.Msg)
			resp.SetReply(r)
			resp.Answer = []dns.RR{
				&dns.A{
					Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
					A:   net.ParseIP("10.9.8.7"),
				},
			}
			_ = w.WriteMsg(resp)
		}),
	}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	return conn.LocalAddr().String()
}

// A registered infra domain resolves under a deny-all sandbox policy and feeds
// its own callback — never the sandbox dyn-allow path (onResolved).
func TestServeDNSInfraDomainBypassesPolicy(t *testing.T) {
	t.Setenv(constants.EnvNameserverExempt, "127.0.0.1")
	resetNameserverExemptCache(t)

	upstream := startInfraUpstream(t)
	proxy := &Proxy{
		upstreams:               []string{upstream},
		activeUpstreams:         []string{upstream},
		upstreamExchangeTimeout: time.Second,
		effectivePolicy:         policy.DefaultDenyPolicy(),
		userPolicy:              policy.DefaultDenyPolicy(),
	}

	var infraIPs []nftables.ResolvedIP
	sandboxCalled := false
	proxy.SetInfraDomain("proxy.example.com", func(domain string, ips []nftables.ResolvedIP) {
		infraIPs = ips
	})
	proxy.SetOnResolved(func(string, []nftables.ResolvedIP) error {
		sandboxCalled = true
		return errors.New("workload grant installation must not run for infrastructure")
	})
	selectedCalled := false
	proxy.SetQueryPolicySelector(func(netip.Addr) (*QueryPolicy, string) {
		selectedCalled = true
		return &QueryPolicy{
			Policy: policy.DefaultDenyPolicy(),
			OnResolved: func(string, []nftables.ResolvedIP) error {
				sandboxCalled = true
				return errors.New("subject grant installation must not run for infrastructure")
			},
		}, ""
	})

	w := &fakeRespWriter{remote: addrFromIP("10.0.0.9")}
	q := new(dns.Msg)
	q.SetQuestion("proxy.example.com.", dns.TypeA)
	proxy.serveDNS(w, q)

	require.Len(t, w.msgs, 1)
	require.Equal(t, dns.RcodeSuccess, w.msgs[0].Rcode, "infra domain must resolve despite deny-all policy")
	require.Len(t, w.msgs[0].Answer, 1)
	require.Equal(t, "10.9.8.7", w.msgs[0].Answer[0].(*dns.A).A.String())
	require.NotEmpty(t, infraIPs, "infra callback must receive the resolved IPs")
	require.Equal(t, "10.9.8.7", infraIPs[0].Addr.String())
	require.False(t, sandboxCalled, "infra answers must not feed the sandbox dyn-allow callback")
	require.False(t, selectedCalled, "infrastructure resolution must not select a workload policy")
}

// Domains that are not registered as infra still follow the sandbox policy
// (deny-all → NXDOMAIN), and never touch the infra callback.
func TestServeDNSNonInfraStillDenied(t *testing.T) {
	upstream := startInfraUpstream(t)
	proxy := &Proxy{
		upstreams:               []string{upstream},
		activeUpstreams:         []string{upstream},
		upstreamExchangeTimeout: time.Second,
		effectivePolicy:         policy.DefaultDenyPolicy(),
		userPolicy:              policy.DefaultDenyPolicy(),
	}
	infraCalled := false
	proxy.SetInfraDomain("proxy.example.com", func(string, []nftables.ResolvedIP) { infraCalled = true })

	w := &fakeRespWriter{remote: addrFromIP("10.0.0.9")}
	q := new(dns.Msg)
	q.SetQuestion("other.example.org.", dns.TypeA)
	proxy.serveDNS(w, q)

	require.Len(t, w.msgs, 1)
	require.Equal(t, dns.RcodeNameError, w.msgs[0].Rcode)
	require.False(t, infraCalled)
}

func TestServeDNSInfraDomainWithNilCallbackBypassesPolicy(t *testing.T) {
	t.Setenv(constants.EnvNameserverExempt, "127.0.0.1")
	resetNameserverExemptCache(t)
	upstream := startInfraUpstream(t)
	proxy := &Proxy{
		upstreams:               []string{upstream},
		activeUpstreams:         []string{upstream},
		upstreamExchangeTimeout: time.Second,
		effectivePolicy:         policy.DefaultDenyPolicy(),
		userPolicy:              policy.DefaultDenyPolicy(),
	}
	proxy.SetInfraDomain("proxy.example.com", nil)
	sandboxCalled := false
	proxy.SetOnResolved(func(string, []nftables.ResolvedIP) error {
		sandboxCalled = true
		return errors.New("workload grant installation must not run for infrastructure")
	})

	w := &fakeRespWriter{remote: addrFromIP("10.0.0.9")}
	q := new(dns.Msg)
	q.SetQuestion("proxy.example.com.", dns.TypeA)
	proxy.serveDNS(w, q)

	require.Len(t, w.msgs, 1)
	require.Equal(t, dns.RcodeSuccess, w.msgs[0].Rcode)
	require.False(t, sandboxCalled)
}

func TestExpandInfraDomainNamesKubernetesClusterFirst(t *testing.T) {
	resolvPath := filepath.Join(t.TempDir(), "resolv.conf")
	err := os.WriteFile(resolvPath, []byte(
		"nameserver 10.96.0.10\n"+
			"search team-a.svc.cluster.local svc.cluster.local cluster.local\n"+
			"options ndots:5\n",
	), 0o600)
	require.NoError(t, err)

	require.Equal(t, []string{
		"gateway",
		"gateway.team-a.svc.cluster.local",
		"gateway.svc.cluster.local",
		"gateway.cluster.local",
	}, ExpandInfraDomainNames("gateway", resolvPath))

	require.Equal(t, []string{
		"gateway.infra",
		"gateway.infra.team-a.svc.cluster.local",
		"gateway.infra.svc.cluster.local",
		"gateway.infra.cluster.local",
	}, ExpandInfraDomainNames("gateway.infra", resolvPath))

	require.Equal(t, []string{
		"gateway.infra.svc",
		"gateway.infra.svc.team-a.svc.cluster.local",
		"gateway.infra.svc.svc.cluster.local",
		"gateway.infra.svc.cluster.local",
	}, ExpandInfraDomainNames("gateway.infra.svc", resolvPath))

	// Rooted FQDNs (trailing dot) are never search-expanded.
	require.Equal(t, []string{
		"gateway.example.com",
	}, ExpandInfraDomainNames("gateway.example.com.", resolvPath))

	// Missing resolv.conf falls back cleanly to the host itself.
	require.Equal(t, []string{"gateway"}, ExpandInfraDomainNames("gateway", filepath.Join(t.TempDir(), "missing")))
}

// Kubernetes ClusterFirst expands an in-namespace Service name ("gateway") to
// "gateway.<namespace>.svc.cluster.local". Registering all search-expanded
// candidates ensures the infra callback fires with the resolved ClusterIP
// while workload allow sets remain untouched.
func TestServeDNSSearchExpandedInfraDomainFiresCallback(t *testing.T) {
	t.Setenv(constants.EnvNameserverExempt, "127.0.0.1")
	resetNameserverExemptCache(t)

	resolvPath := filepath.Join(t.TempDir(), "resolv.conf")
	require.NoError(t, os.WriteFile(resolvPath, []byte(
		"nameserver 10.96.0.10\n"+
			"search team-a.svc.cluster.local svc.cluster.local cluster.local\n"+
			"options ndots:5\n",
	), 0o600))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	srv := &dns.Server{
		PacketConn: conn,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			resp := new(dns.Msg)
			resp.SetReply(r)
			q := r.Question[0]
			if q.Qtype == dns.TypeA && q.Name == "gateway.team-a.svc.cluster.local." {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
					A:   net.ParseIP("10.96.50.10"),
				})
			} else {
				resp.Rcode = dns.RcodeNameError
			}
			_ = w.WriteMsg(resp)
		}),
	}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	<-ready

	proxy := &Proxy{
		upstreams:               []string{conn.LocalAddr().String()},
		activeUpstreams:         []string{conn.LocalAddr().String()},
		upstreamExchangeTimeout: time.Second,
		effectivePolicy:         policy.DefaultDenyPolicy(),
		userPolicy:              policy.DefaultDenyPolicy(),
	}
	var gotDomain string
	var gotIPs []nftables.ResolvedIP
	for _, name := range ExpandInfraDomainNames("gateway", resolvPath) {
		proxy.SetInfraDomain(name, func(domain string, ips []nftables.ResolvedIP) {
			gotDomain = domain
			gotIPs = ips
		})
	}
	sandboxCalled := false
	proxy.SetOnResolved(func(string, []nftables.ResolvedIP) error {
		sandboxCalled = true
		return errors.New("sandbox callback must not run for infra service")
	})

	w := &fakeRespWriter{remote: addrFromIP("10.0.0.9")}
	q := new(dns.Msg)
	q.SetQuestion("gateway.team-a.svc.cluster.local.", dns.TypeA)
	proxy.serveDNS(w, q)

	require.Len(t, w.msgs, 1)
	require.Equal(t, dns.RcodeSuccess, w.msgs[0].Rcode)
	require.Equal(t, "gateway.team-a.svc.cluster.local.", gotDomain)
	require.Len(t, gotIPs, 1)
	require.Equal(t, netip.MustParseAddr("10.96.50.10"), gotIPs[0].Addr)
	require.False(t, sandboxCalled)
}

func TestHostsFileIPs(t *testing.T) {
	hostsPath := filepath.Join(t.TempDir(), "hosts")
	err := os.WriteFile(hostsPath, []byte(
		"# Kubernetes-managed hosts file.\n"+
			"127.0.0.1\tlocalhost\n"+
			"10.20.30.40\tgateway.private gateway-alias # hostAliases entry\n"+
			"fd00::40\tGATEWAY.PRIVATE.\n"+
			"10.20.30.40\tgateway.private\n"+ // duplicate
			"fe80::1%eth0\tgateway.private\n"+ // zoned address ignored
			"10.20.30.41\tother.private\n",
	), 0o600)
	require.NoError(t, err)

	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("10.20.30.40"),
		netip.MustParseAddr("fd00::40"),
	}, HostsFileIPs("gateway.private", hostsPath))
	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("10.20.30.40"),
		netip.MustParseAddr("fd00::40"),
	}, HostsFileIPs("gateway.private.", hostsPath))
	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("10.20.30.40"),
	}, HostsFileIPs("gateway-alias", hostsPath))
	require.Empty(t, HostsFileIPs("unlisted.private", hostsPath))
	require.Empty(t, HostsFileIPs("gateway.private", filepath.Join(t.TempDir(), "missing")))
}
