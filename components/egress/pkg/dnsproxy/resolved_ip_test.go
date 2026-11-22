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
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func TestResolvedIPPreservesAddressRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name string
		rr   dns.RR
		want string
	}{
		{"four byte IPv4", &dns.A{A: net.IP{192, 0, 2, 1}}, "192.0.2.1"},
		{"sixteen byte IPv4", &dns.A{A: net.ParseIP("192.0.2.1")}, "192.0.2.1"},
		{"IPv6", &dns.AAAA{AAAA: net.ParseIP("2001:db8::1")}, "2001:db8::1"},
		{"mapped IPv4", &dns.AAAA{AAAA: net.ParseIP("::ffff:192.0.2.1")}, "192.0.2.1"},
		{"nil", &dns.A{}, ""},
		{"invalid length", &dns.AAAA{AAAA: net.IP{1, 2, 3}}, ""},
		{"CNAME", &dns.CNAME{Target: "elsewhere.example."}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.rr.Header().Ttl = 17
			ip, ok := resolvedIP(tc.rr)
			require.Equal(t, tc.want != "", ok)
			if ok {
				require.Equal(t, tc.want, ip.Addr.String())
				require.Equal(t, 17*time.Second, ip.TTL)
			}
		})
	}
}
