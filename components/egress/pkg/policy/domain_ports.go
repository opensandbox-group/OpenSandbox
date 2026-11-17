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

import "strings"

// DomainPermissions separates DNS admission from the network permissions learned
// from its answer. Only Unrestricted may populate a whole-address allow set.
type DomainPermissions struct {
	Allowed      bool
	Unrestricted bool
	AllowPorts   []int
	DenyPorts    []int
}

// HasDomainPorts reports whether DNS answers need TCP endpoint enforcement.
func (p *NetworkPolicy) HasDomainPorts() bool {
	if p == nil {
		return false
	}
	if p.domainIndex != nil {
		return len(p.domainIndex.scoped) != 0
	}
	for _, r := range p.Egress {
		if r.targetKind == targetDomain && len(r.Ports) != 0 {
			return true
		}
	}
	return false
}

func (p *NetworkPolicy) matchPortlessDomain(domain string) (string, bool) {
	if p.domainIndex != nil {
		return p.domainIndex.match(domain)
	}
	return p.evaluateLinear(domain)
}

func (p *NetworkPolicy) scopedDomainRules() []EgressRule {
	if p.domainIndex != nil {
		return p.domainIndex.scoped
	}
	return p.Egress
}

// DomainPermissions preserves the ordered portless name gate, then combines
// matching TCP scopes. A scoped deny must not deny every other port at DNS.
func (p *NetworkPolicy) DomainPermissions(domain string) DomainPermissions {
	if p == nil {
		return DomainPermissions{}
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	action, matched := p.matchPortlessDomain(domain)
	nameDenied := matched && action != ActionAllow
	if !matched {
		action = p.DefaultAction
	}
	result := DomainPermissions{Unrestricted: !nameDenied && action == ActionAllow}
	result.Allowed = result.Unrestricted
	var allow, deny map[int]bool
	for _, r := range p.scopedDomainRules() {
		if r.targetKind != targetDomain || len(r.Ports) == 0 || !r.matchesDomain(domain) {
			continue
		}
		if r.Action == ActionDeny {
			if deny == nil {
				deny = make(map[int]bool)
			}
			for _, port := range r.Ports {
				if !deny[port] {
					deny[port] = true
					result.DenyPorts = append(result.DenyPorts, port)
				}
			}
		} else if !nameDenied {
			if allow == nil {
				allow = make(map[int]bool)
			}
			for _, port := range r.Ports {
				if !allow[port] {
					allow[port] = true
					result.AllowPorts = append(result.AllowPorts, port)
				}
			}
		}
	}
	// DNS has no destination-port dimension. Conflicting TCP denies are
	// enforced after resolution, not as a denial of every DNS record type.
	result.Allowed = !nameDenied && (result.Unrestricted || len(result.AllowPorts) != 0)
	return result
}
