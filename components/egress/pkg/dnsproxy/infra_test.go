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
	proxy.SetOnResolved(func(string, []nftables.ResolvedIP) { sandboxCalled = true })

	w := &fakeRespWriter{remote: addrFromIP("10.0.0.9")}
	q := new(dns.Msg)
	q.SetQuestion("proxy.example.com.", dns.TypeA)
	proxy.serveDNS(w, q)

	require.Len(t, w.msgs, 1)
	require.Equal(t, dns.RcodeSuccess, w.msgs[0].Rcode, "infra domain must resolve despite deny-all policy")
	require.NotEmpty(t, infraIPs, "infra callback must receive the resolved IPs")
	require.Equal(t, "10.9.8.7", infraIPs[0].Addr.String())
	require.False(t, sandboxCalled, "infra answers must not feed the sandbox dyn-allow callback")
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
