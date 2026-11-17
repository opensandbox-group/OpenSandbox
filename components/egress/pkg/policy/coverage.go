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
	"slices"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
)

// AllowsEntireHostSelector conservatively proves that every hostname in a
// canonical credential selector is allowed by the ordered domain policy. It is
// an internal candidate-validation prerequisite, not a change to Evaluate.
// A wildcard proof requires a covering allow (or default allow) and no earlier
// unshadowed overlapping deny. Several narrower selectors are not treated as
// exhaustive coverage, even at the maximum DNS length; this may reject a safe
// candidate but cannot authorize a denied member of its selector.
func (p *NetworkPolicy) AllowsEntireHostSelector(host string) bool {
	if p == nil {
		return false
	}
	if _, err := hostselector.ParseCanonical(host); err != nil {
		return false
	}
	if !strings.HasPrefix(host, "*.") {
		return p.evaluateUnscopedDomain(host) == ActionAllow
	}
	var allowed []string
	for _, rule := range p.Egress {
		if rule.targetKind != targetDomain || len(rule.Ports) != 0 {
			continue
		}
		// Match compileDomainIndex exactly. In particular, a trailing root dot in
		// a rule is not removed, and Unicode is not converted to an ASCII A-label.
		target := strings.ToLower(strings.TrimSpace(rule.Target))
		overlap, ok := selectorIntersection(host, target)
		if !ok {
			continue
		}
		if rule.Action == ActionAllow {
			if selectorContains(target, host) {
				return true
			}
			allowed = append(allowed, target)
			continue
		}
		shadowed := false
		for _, earlier := range allowed {
			if selectorContains(earlier, overlap) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			return false
		}
	}
	return p.DefaultAction == ActionAllow
}

// AllowsEntireHostSelectorPort proves coverage at one TCP port without treating
// permission to resolve a name as permission to use every credential scheme.
func (p *NetworkPolicy) AllowsEntireHostSelectorPort(host string, port int) bool {
	if p == nil || port < 1 || port > 65535 {
		return false
	}
	if _, err := hostselector.ParseCanonical(host); err != nil {
		return false
	}
	if !p.HasDomainPorts() {
		return p.AllowsEntireHostSelector(host)
	}
	if !strings.HasPrefix(host, "*.") {
		permission := p.DomainPermissions(host)
		return permission.Allowed && !slices.Contains(permission.DenyPorts, port) &&
			(permission.Unrestricted || slices.Contains(permission.AllowPorts, port))
	}
	// A different fallback proves only that the legacy name gate does not
	// reject any member; it grants no network permission by itself.
	gate := *p
	gate.DefaultAction = ActionAllow
	if !gate.AllowsEntireHostSelector(host) {
		return false
	}
	covered := p.AllowsEntireHostSelector(host)
	for _, rule := range p.scopedDomainRules() {
		if rule.targetKind != targetDomain || !slices.Contains(rule.Ports, port) {
			continue
		}
		target := strings.ToLower(strings.TrimSpace(rule.Target))
		if _, overlaps := selectorIntersection(host, target); !overlaps {
			continue
		}
		if rule.Action == ActionDeny {
			return false
		}
		if selectorContains(target, host) {
			covered = true
		}
	}
	return covered
}

// Domain selector sets are nested or disjoint. These helpers intentionally
// also support policy suffixes such as *.com, unlike credential selectors.
func selectorContains(outer, inner string) bool {
	if outer == inner {
		return true
	}
	if !strings.HasPrefix(outer, "*.") {
		return false
	}
	suffix := strings.TrimPrefix(outer, "*")
	if strings.HasPrefix(inner, "*.") {
		inner = strings.TrimPrefix(inner, "*.")
	}
	return strings.HasSuffix(inner, suffix)
}

func selectorIntersection(a, b string) (string, bool) {
	if selectorContains(a, b) {
		return b, true
	}
	if selectorContains(b, a) {
		return a, true
	}
	return "", false
}
