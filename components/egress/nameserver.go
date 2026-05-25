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

package main

import (
	"net/netip"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// AllowIPsForNft builds the extra allow-IP list for dns+nft: 127.0.0.1 (local DNS listen after REDIRECT),
// then up to ResolvNameserverCap non-loopback / non-0.0.0.0 nameservers from resolvPath.
func AllowIPsForNft(resolvPath string) []netip.Addr {
	raw, _ := dnsproxy.ResolvNameserverIPs(resolvPath)
	maxNsCount := constants.ResolvNameserverCap

	var validated []netip.Addr
	for _, ip := range raw {
		if len(validated) >= maxNsCount {
			break
		}
		if !isValidNameserverIP(ip) {
			continue
		}
		validated = append(validated, ip)
	}

	out := make([]netip.Addr, 0, 1+len(validated)) // 127.0.0.1 first: redirect target must be allowed
	out = append(out, netip.MustParseAddr("127.0.0.1"))
	out = append(out, validated...)

	if len(out) > 1 {
		log.Infof("[dns] whitelisting proxy listen + %d nameserver(s) for nft: %v", len(validated), formatIPs(out))
	} else {
		log.Infof("[dns] whitelisting proxy listen (127.0.0.1); no valid nameserver IPs from %s", resolvPath)
	}
	return out
}

func isValidNameserverIP(ip netip.Addr) bool {
	if ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() {
		return false
	}
	return true
}

func formatIPs(ips []netip.Addr) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

func allowIps() []netip.Addr {
	upstreams, err := dnsproxy.DiscoverUpstreams()
	if err != nil {
		log.Fatalf("failed to resolve DNS upstreams: %v", err)
	}
	allowIPs := AllowIPsForNft("/etc/resolv.conf")
	for _, addr := range dnsproxy.AllowIPsFromUpstreamAddrs(upstreams) {
		if !containsAddr(allowIPs, addr) {
			allowIPs = append(allowIPs, addr)
		}
	}

	// Exempt destinations: proxy dials them without SO_MARK; they must still be allowed by nft.
	for _, addr := range dnsproxy.ParseNameserverExemptList() {
		if !containsAddr(allowIPs, addr) {
			allowIPs = append(allowIPs, addr)
		}
	}
	return allowIPs
}
