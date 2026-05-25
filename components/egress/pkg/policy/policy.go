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

package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"strings"
)

const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

type targetKind int

const (
	targetUnknown targetKind = iota
	targetDomain
	targetIP
	targetCIDR
)

// DefaultDenyPolicy is deny-by-default with an empty egress list.
func DefaultDenyPolicy() *NetworkPolicy {
	return &NetworkPolicy{
		DefaultAction: ActionDeny,
		domainIndex:   compileDomainIndex(nil),
	}
}

// NetworkPolicy: JSON defaultAction + egress; domain rules use first-match (see compiled index).
type NetworkPolicy struct {
	Egress        []EgressRule `json:"egress"`
	DefaultAction string       `json:"defaultAction"`

	domainIndex *compiledDomainIndex
}

type EgressRule struct {
	Action string `json:"action"`
	Target string `json:"target"`

	targetKind targetKind
	ip         netip.Addr
	prefix     netip.Prefix
}

// ParsePolicy unmarshals JSON; empty/null/{} → default deny. defaultAction defaults to deny if unset in JSON.
func ParsePolicy(raw string) (*NetworkPolicy, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return DefaultDenyPolicy(), nil
	}

	var p NetworkPolicy
	if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
		return nil, err
	}
	if err := normalizePolicy(&p); err != nil {
		return nil, err
	}
	return ensureDefaults(&p), nil
}

// Evaluate returns allow or deny for a query name (FQDN with or without trailing dot, lowercased).
func (p *NetworkPolicy) Evaluate(domain string) string {
	if p == nil {
		return ActionDeny
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))

	if p.domainIndex != nil {
		if action, ok := p.domainIndex.match(domain); ok {
			if action == "" {
				return ActionDeny
			}
			return action
		}
	} else {
		// Keep compatibility for policies built manually without ParsePolicy/ensureDefaults.
		if action, ok := p.evaluateLinear(domain); ok {
			return action
		}
	}
	if p.DefaultAction == "" {
		return ActionDeny
	}
	return p.DefaultAction
}

func (p *NetworkPolicy) evaluateLinear(domain string) (string, bool) {
	for _, r := range p.Egress {
		if r.targetKind != targetDomain {
			continue
		}
		if r.matchesDomain(domain) {
			if r.Action == "" {
				return ActionDeny, true
			}
			return r.Action, true
		}
	}
	return "", false
}

func ensureDefaults(p *NetworkPolicy) *NetworkPolicy {
	if p == nil {
		return DefaultDenyPolicy()
	}
	if p.DefaultAction == "" {
		p.DefaultAction = ActionDeny
	}
	p.domainIndex = compileDomainIndex(p.Egress)
	return p
}

func normalizePolicy(p *NetworkPolicy) error {
	p.DefaultAction = strings.ToLower(strings.TrimSpace(p.DefaultAction))
	if p.DefaultAction == "" {
		p.DefaultAction = ActionDeny
	}

	for i := range p.Egress {
		r := &p.Egress[i]
		r.Action = strings.ToLower(strings.TrimSpace(r.Action))
		if r.Action == "" {
			r.Action = ActionDeny
		}
		if r.Action != ActionAllow && r.Action != ActionDeny {
			return fmt.Errorf("unsupported action %q", r.Action)
		}

		r.Target = strings.TrimSpace(r.Target)
		if r.Target == "" {
			return fmt.Errorf("egress target cannot be empty")
		}
		if ip, err := netip.ParseAddr(r.Target); err == nil {
			r.targetKind = targetIP
			r.ip = ip
			continue
		}
		if prefix, err := netip.ParsePrefix(r.Target); err == nil {
			r.targetKind = targetCIDR
			r.prefix = prefix
			continue
		}
		r.targetKind = targetDomain
	}
	return nil
}

// WithExtraAllowIPs appends per-IP allow rules (e.g. resolv nameservers, explicit upstream) so client and
// proxy can reach the same address; does not change domain-mode egress rules.
func (p *NetworkPolicy) WithExtraAllowIPs(ips []netip.Addr) *NetworkPolicy {
	if p == nil || len(ips) == 0 {
		return p
	}
	out := *p
	n, m := len(p.Egress), len(ips)
	if m > math.MaxInt-n {
		panic("policy: egress rule slice capacity overflow")
	}
	out.Egress = make([]EgressRule, n, n+m)
	copy(out.Egress, p.Egress)
	for _, ip := range ips {
		out.Egress = append(out.Egress, EgressRule{
			Action:     ActionAllow,
			Target:     ip.String(),
			targetKind: targetIP,
			ip:         ip,
		})
	}
	return &out
}

// StaticIPSets buckets static IP/CIDR egress into allow/deny v4/v6 for nft element generation.
func (p *NetworkPolicy) StaticIPSets() (allowV4, allowV6, denyV4, denyV6 []string) {
	if p == nil {
		return
	}
	for _, r := range p.Egress {
		switch r.targetKind {
		case targetIP:
			addr := r.ip
			target := addr.String()
			if r.Action == ActionAllow {
				if addr.Is4() {
					allowV4 = append(allowV4, target)
				} else if addr.Is6() {
					allowV6 = append(allowV6, target)
				}
			} else {
				if addr.Is4() {
					denyV4 = append(denyV4, target)
				} else if addr.Is6() {
					denyV6 = append(denyV6, target)
				}
			}
		case targetCIDR:
			pfx := r.prefix
			target := pfx.String()
			if r.Action == ActionAllow {
				if pfx.Addr().Is4() {
					allowV4 = append(allowV4, target)
				} else if pfx.Addr().Is6() {
					allowV6 = append(allowV6, target)
				}
			} else {
				if pfx.Addr().Is4() {
					denyV4 = append(denyV4, target)
				} else if pfx.Addr().Is6() {
					denyV6 = append(denyV6, target)
				}
			}
		default:
			continue
		}
	}
	return
}

func (r *EgressRule) matchesDomain(domain string) bool {
	pattern := strings.ToLower(strings.TrimSpace(r.Target))
	domain = strings.ToLower(domain)

	if pattern == "" {
		return false
	}
	if pattern == domain {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		// "*.example.com" matches "a.example.com" but not "example.com"
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(domain, suffix) && domain != strings.TrimPrefix(pattern, "*.")
	}
	return false
}
