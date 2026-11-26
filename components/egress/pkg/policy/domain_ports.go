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

import (
	"errors"
	"slices"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
)

// DomainPermissions separates DNS admission from the network permissions learned
// from its answer. Only Unrestricted may populate a whole-address allow set.
type DomainPermissions struct {
	Allowed      bool
	Unrestricted bool
	AllowPorts   []int
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

// ValidateClassicDomainPorts bounds Classic port-scoped grants to TCP
// allow rules under a default-deny policy.
func (p *NetworkPolicy) ValidateClassicDomainPorts() error {
	if !p.HasDomainPorts() {
		return nil
	}
	if p.DefaultAction != ActionDeny {
		return errors.New("domain targets with TCP ports require a default-deny policy")
	}
	for _, rule := range p.scopedDomainRules() {
		if rule.targetKind != targetDomain || len(rule.Ports) == 0 {
			continue
		}
		if rule.Action != ActionAllow {
			return errors.New("only allow rules may combine domain targets with TCP ports")
		}
		_, err := hostselector.ParseCanonical(normalizeDomainPattern(rule.Target))
		if err != nil || strings.IndexFunc(rule.Target, func(r rune) bool { return r > 127 }) >= 0 {
			return errors.New("domain targets with TCP ports must be ASCII FQDNs or leftmost wildcards")
		}
	}
	return nil
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

// DomainPermissions applies the ordered portless name gate before combining
// matching scoped allows.
func (p *NetworkPolicy) DomainPermissions(domain string) DomainPermissions {
	return p.domainPermissions(domain, true)
}

// DNS admission needs only the first scoped allow after the portless decision.
// Grant installation needs all matching ports in declaration order.
func (p *NetworkPolicy) domainPermissions(domain string, collectPorts bool) DomainPermissions {
	if p == nil {
		return DomainPermissions{}
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	action, matched := p.matchPortlessDomain(domain)
	if matched {
		allowed := action == ActionAllow
		return DomainPermissions{Allowed: allowed, Unrestricted: allowed}
	}
	if p.DefaultAction == ActionAllow {
		return DomainPermissions{Allowed: true, Unrestricted: true}
	}
	if p.domainIndex != nil {
		return p.domainIndex.scopedPermissions(domain, collectPorts)
	}
	var ports domainPortUnion
	for _, r := range p.Egress {
		if r.targetKind != targetDomain || r.Action != ActionAllow || len(r.Ports) == 0 || !r.matchesDomain(domain) {
			continue
		}
		if !collectPorts {
			return DomainPermissions{Allowed: true}
		}
		ports.add(r.Ports)
	}
	return DomainPermissions{Allowed: len(ports.ports) != 0, AllowPorts: ports.ports}
}

type domainPortUnion struct {
	ports []int
	seen  map[int]struct{}
}

func (u *domainPortUnion) add(ports []int) {
	if u.ports == nil {
		u.ports = make([]int, 0, len(ports))
	}
	if u.seen == nil {
		const linearLimit = 8
		if len(u.ports)+len(ports) <= linearLimit {
			for _, port := range ports {
				if !slices.Contains(u.ports, port) {
					u.ports = append(u.ports, port)
				}
			}
			return
		}
		u.seen = make(map[int]struct{}, len(u.ports)+len(ports))
		for _, port := range u.ports {
			u.seen[port] = struct{}{}
		}
	}
	for _, port := range ports {
		if _, seen := u.seen[port]; !seen {
			u.seen[port] = struct{}{}
			u.ports = append(u.ports, port)
		}
	}
}
