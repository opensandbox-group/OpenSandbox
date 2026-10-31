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

// EvaluateIP returns allow or deny for a literal destination address using
// portless IP/CIDR egress rules and the policy default. Deny rules win over
// allow rules regardless of declaration order, matching the sidecar nftables
// chain (plain deny set before plain allow set). Port-scoped rules are ignored:
// established-flow teardown has no reliable port when only the address is known
// from a DNS-learned lease, and callers that need port precision should tighten
// the policy with an address/CIDR rule instead.
//
// Domain-learned destinations are not visible here; callers that tracked
// DNS-resolved addresses must consult that snapshot separately.
func (p *NetworkPolicy) EvaluateIP(addr netip.Addr) string {
	if p == nil || !addr.IsValid() {
		return ActionDeny
	}
	addr = addr.Unmap()

	denied := false
	allowed := false
	for _, r := range p.Egress {
		if len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			continue
		}
		if !r.matchesAddr(addr) {
			continue
		}
		if r.Action == ActionDeny {
			denied = true
			break
		}
		if r.Action == ActionAllow {
			allowed = true
		}
	}
	if denied {
		return ActionDeny
	}
	if allowed {
		return ActionAllow
	}
	if p.DefaultAction == "" {
		return ActionDeny
	}
	return p.DefaultAction
}

func (r *EgressRule) matchesAddr(addr netip.Addr) bool {
	switch r.targetKind {
	case targetIP:
		return r.ip.IsValid() && r.ip.Unmap() == addr
	case targetCIDR:
		return r.prefix.IsValid() && r.prefix.Contains(addr)
	default:
		return false
	}
}
