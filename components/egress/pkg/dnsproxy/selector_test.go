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
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// fakeRespWriter is a minimal dns.ResponseWriter for serveDNS tests.
type fakeRespWriter struct {
	remote net.Addr
	msgs   []*dns.Msg
}

func (w *fakeRespWriter) RemoteAddr() net.Addr { return w.remote }
func (w *fakeRespWriter) Close() error         { return nil }
func (w *fakeRespWriter) WriteMsg(m *dns.Msg) error {
	w.msgs = append(w.msgs, m)
	return nil
}

func (w *fakeRespWriter) Write([]byte) (int, error) { return 0, nil }
func (w *fakeRespWriter) TsigStatus() error         { return nil }
func (w *fakeRespWriter) TsigTimersOnly(bool)       {}
func (w *fakeRespWriter) Hijack()                   {}
func (w *fakeRespWriter) LocalAddr() net.Addr       { return w.remote }

func addrFromIP(ip string) net.Addr {
	a, err := net.ResolveUDPAddr("udp", ip+":12345")
	if err != nil {
		panic(err)
	}
	return a
}

// startUpstream runs a local DNS server that answers A records.
func startUpstream(t *testing.T) string {
	t.Helper()
	t.Setenv(constants.EnvNameserverExempt, "127.0.0.1")
	resetNameserverExemptCache(t)
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	server := &dns.Server{
		PacketConn: conn,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			resp := new(dns.Msg)
			resp.SetReply(r)
			resp.Answer = []dns.RR{
				&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("1.2.3.4")},
			}
			_ = w.WriteMsg(resp)
		}),
	}
	started := make(chan struct{})
	server.NotifyStartedFunc = func() { close(started) }
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	<-started
	return conn.LocalAddr().String()
}

func selectorProxy(t *testing.T) *Proxy {
	upstream := startUpstream(t)
	return &Proxy{
		upstreams:               []string{upstream},
		activeUpstreams:         []string{upstream},
		upstreamExchangeTimeout: time.Second,
		effectivePolicy:         policy.DefaultDenyPolicy(),
		userPolicy:              policy.DefaultDenyPolicy(),
	}
}

func TestQueryPolicySelectorDispatch(t *testing.T) {
	proxy := selectorProxy(t)
	allowPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)

	proxy.SetQueryPolicySelector(func(remote netip.Addr) *QueryPolicy {
		switch remote.String() {
		case "10.0.0.5":
			return &QueryPolicy{Policy: allowPol}
		case "10.0.0.6":
			return &QueryPolicy{Policy: policy.DefaultDenyPolicy()}
		default:
			return nil
		}
	})

	// subject A: allow example.com -> NOERROR
	w := &fakeRespWriter{remote: addrFromIP("10.0.0.5")}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	proxy.serveDNS(w, q)
	require.Len(t, w.msgs, 1)
	require.Equal(t, dns.RcodeSuccess, w.msgs[0].Rcode)

	// subject B: default deny -> NXDOMAIN
	w = &fakeRespWriter{remote: addrFromIP("10.0.0.6")}
	proxy.serveDNS(w, q)
	require.Equal(t, dns.RcodeNameError, w.msgs[0].Rcode)

	// unknown source: selector returns nil -> fail closed NXDOMAIN
	w = &fakeRespWriter{remote: addrFromIP("10.0.0.99")}
	proxy.serveDNS(w, q)
	require.Equal(t, dns.RcodeNameError, w.msgs[0].Rcode)
}

func TestQueryPolicySelectorPerQueryOnResolved(t *testing.T) {
	proxy := selectorProxy(t)
	allowPol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)

	resolved := make(chan struct {
		domain string
		ips    []nftables.ResolvedIP
	}, 1)
	proxy.SetQueryPolicySelector(func(remote netip.Addr) *QueryPolicy {
		return &QueryPolicy{
			Policy: allowPol,
			OnResolved: func(domain string, ips []nftables.ResolvedIP) {
				resolved <- struct {
					domain string
					ips    []nftables.ResolvedIP
				}{domain, ips}
			},
		}
	})

	w := &fakeRespWriter{remote: addrFromIP("10.0.0.5")}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	proxy.serveDNS(w, q)

	select {
	case got := <-resolved:
		require.Equal(t, "example.com.", got.domain)
		require.Len(t, got.ips, 1)
		require.Equal(t, "1.2.3.4", got.ips[0].Addr.String())
	case <-time.After(2 * time.Second):
		require.FailNow(t, "per-query onResolved was not invoked")
	}
}
