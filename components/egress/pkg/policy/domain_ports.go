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
	var result DomainPermissions
	var allow map[int]bool
	for _, r := range p.scopedDomainRules() {
		if r.targetKind != targetDomain || r.Action != ActionAllow || len(r.Ports) == 0 || !r.matchesDomain(domain) {
			continue
		}
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
	result.Allowed = len(result.AllowPorts) != 0
	return result
}
