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

package policy

import "net/netip"

// AllowsProxyDestination evaluates a TCP destination without resolving it.
// Domain admission follows DNS policy; IP/port denies take precedence over
// packet-layer allows, as in nftables. An applicable IP deny rejects unresolved
// hostnames conservatively: only the upstream proxy knows their real addresses.
func (p *NetworkPolicy) AllowsProxyDestination(host string, port int) bool {
	if p == nil || port < 1 || port > 65535 {
		return false
	}
	ip, err := netip.ParseAddr(host)
	literal := err == nil
	ip = ip.Unmap()
	allowed := p.DefaultAction == ActionAllow
	for _, r := range p.Egress {
		if r.targetKind == targetDomain {
			continue
		}
		if len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			matchesPort := false
			for _, candidate := range r.Ports {
				matchesPort = matchesPort || candidate == port
			}
			for _, interval := range r.PortRanges {
				matchesPort = matchesPort || interval.Contains(port)
			}
			if !matchesPort {
				continue
			}
		}
		matches := r.targetKind == targetPortOnly
		if r.targetKind == targetIP || r.targetKind == targetCIDR {
			if !literal {
				if r.Action != ActionAllow {
					return false
				}
				continue
			}
			if r.targetKind == targetIP {
				matches = r.ip.Unmap() == ip
			} else {
				matches = r.prefix.Contains(ip) || ip.Is4() && r.prefix.Contains(netip.AddrFrom16(ip.As16()))
			}
		}
		if matches {
			if r.Action != ActionAllow {
				return false
			}
			allowed = true
		}
	}
	if !literal {
		return p.Evaluate(host) == ActionAllow
	}
	return allowed
}
