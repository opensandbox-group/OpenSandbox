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
	"fmt"
	"net"
	"runtime"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/miekg/dns"
)

func BenchmarkExtractResolvedIPs(b *testing.B) {
	for _, tc := range []struct {
		name string
		rr   dns.RR
	}{
		{"IPv4", &dns.A{A: net.IPv4(192, 0, 2, 1)}},
		{"IPv6", &dns.AAAA{AAAA: net.ParseIP("2001:db8::1")}},
	} {
		for _, answers := range []int{1, 8} {
			b.Run(fmt.Sprintf("%s/%d", tc.name, answers), func(b *testing.B) {
				msg := &dns.Msg{Answer: make([]dns.RR, answers)}
				for i := range answers {
					msg.Answer[i] = tc.rr
				}
				var ips []nftables.ResolvedIP
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					ips = extractResolvedIPs(msg)
				}
				runtime.KeepAlive(ips)
			})
		}
	}
}
