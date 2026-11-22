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

// startUpstream runs a local DNS server that answers A and AAAA records.
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
			switch r.Question[0].Qtype {
			case dns.TypeAAAA:
				resp.Answer = []dns.RR{
					&dns.AAAA{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}, AAAA: net.ParseIP("2001:db8::1234")},
				}
			default:
				resp.Answer = []dns.RR{
					&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("1.2.3.4")},
				}
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

	proxy.SetQueryPolicySelector(func(remote netip.Addr) (*QueryPolicy, string) {
		switch remote.String() {
		case "10.0.0.5":
			return &QueryPolicy{Policy: allowPol}, ""
		case "10.0.0.6":
			return &QueryPolicy{Policy: policy.DefaultDenyPolicy()}, ""
		default:
			return nil, "unknown source"
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

	installed := make(map[string][]nftables.ResolvedIP)
	fallbackCalls := 0
	proxy.SetOnResolved(func(string, []nftables.ResolvedIP) error {
		fallbackCalls++
		return errors.New("proxy-wide installation failed")
	})
	proxy.SetQueryPolicySelector(func(remote netip.Addr) (*QueryPolicy, string) {
		subject := remote.String()
		if subject == "10.0.0.7" {
			return &QueryPolicy{Policy: allowPol}, ""
		}
		return &QueryPolicy{
			Policy: allowPol,
			OnResolved: func(domain string, ips []nftables.ResolvedIP) error {
				require.Equal(t, "example.com.", domain)
				require.Len(t, ips, 1)
				require.Equal(t, "1.2.3.4", ips[0].Addr.String())
				if subject == "10.0.0.6" {
					return errors.New("subject installation failed")
				}
				installed[subject] = append(installed[subject], ips...)
				return nil
			},
		}, ""
	})

	for _, tc := range []struct {
		subject string
		rcode   int
	}{
		{"10.0.0.5", dns.RcodeSuccess},
		{"10.0.0.6", dns.RcodeServerFailure},
		{"10.0.0.5", dns.RcodeSuccess},
		{"10.0.0.7", dns.RcodeServerFailure},
	} {
		w := &fakeRespWriter{remote: addrFromIP(tc.subject)}
		q := new(dns.Msg)
		q.SetQuestion("example.com.", dns.TypeA)
		proxy.serveDNS(w, q)
		require.Len(t, w.msgs, 1)
		require.Equal(t, tc.rcode, w.msgs[0].Rcode, "subject %s", tc.subject)
		if tc.rcode == dns.RcodeSuccess {
			require.Len(t, w.msgs[0].Answer, 1)
			require.Equal(t, "1.2.3.4", w.msgs[0].Answer[0].(*dns.A).A.String())
		} else {
			require.Empty(t, w.msgs[0].Answer)
		}
	}
	require.Len(t, installed["10.0.0.5"], 2)
	require.Empty(t, installed["10.0.0.6"], "failed subject must not inherit another subject's grants")
	require.Empty(t, installed["10.0.0.7"])
	require.Equal(t, 1, fallbackCalls, "selected callbacks must not fall back to proxy-wide installation")
}
