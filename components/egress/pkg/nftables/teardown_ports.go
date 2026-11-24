// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"net/netip"
	"slices"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// Selectors are parsed once per evaluation. Normalize each matching rule's
// ports on first use, and never mutate the policy's slices.
type teardownSelector struct {
	literal netip.Addr
	prefix  netip.Prefix
}

func teardownSelectorFor(target string) teardownSelector {
	if ip, err := netip.ParseAddr(target); err == nil {
		ip = ip.Unmap()
		return teardownSelector{literal: ip, prefix: netip.PrefixFrom(ip, ip.BitLen())}
	}
	prefix, _ := netip.ParsePrefix(target)
	return teardownSelector{prefix: prefix.Masked()}
}

// An omitted (wildcard) target is handled by the caller. An invalid or
// nonmatching target must never become a wildcard when checking raw rules.
func (s teardownSelector) matches(addr netip.Addr) bool {
	if s.literal.IsValid() {
		return s.literal == addr
	}
	return s.prefix.Contains(addr)
}

type teardownPortRule struct {
	teardownSelector
	ports  []int
	ranges []policy.PortRange
	spans  tcpSpans
}

func compileScopedPorts(rules []policy.PortScopedRule) []teardownPortRule {
	out := make([]teardownPortRule, 0, len(rules))
	for _, r := range rules {
		s := teardownSelectorFor(r.Target)
		if r.Target == "" || s.prefix.IsValid() {
			out = append(out, teardownPortRule{teardownSelector: s, ports: r.Ports, ranges: r.Ranges})
		}
	}
	return out
}

type teardownPortFilter struct {
	policy            *policy.NetworkPolicy
	ips               portlessIPPolicy
	rawDeny, rawAllow []policy.PortScopedRule
	deny, allow       []teardownPortRule
	probed, compiled  bool
}

func newTeardownPortFilter(p *policy.NetworkPolicy) teardownPortFilter {
	deny, allow := p.PortScopedRules()
	return teardownPortFilter{policy: p, ips: compilePortlessIPPolicy(p), rawDeny: deny, rawAllow: allow}
}

func matchesSinglePort(rules []policy.PortScopedRule, addr netip.Addr, port int) bool {
	for _, r := range rules {
		if !slices.Contains(r.Ports, port) && !slices.ContainsFunc(r.Ranges, func(rg policy.PortRange) bool { return rg.Contains(port) }) {
			continue
		}
		if r.Target == "" || teardownSelectorFor(r.Target).matches(addr) {
			return true
		}
	}
	return false
}

// Evaluate only the requested window. Within it, static denies take
// precedence; an unmatched port takes the portless/default verdict.
func (f *teardownPortFilter) deniedWindow(addr netip.Addr, window tcpSpans) tcpSpans {
	if f.policy == nil || !addr.IsValid() || f.ips.denies(addr) {
		return window
	}
	addr = addr.Unmap()
	if !f.compiled {
		// A single withdrawal need not pay to compile any selector array
		// or sort any rule's ports. Switch to the reusable index if queried again.
		if !f.probed && len(window) == 1 && window[0].Lo == window[0].Hi {
			f.probed = true
			port := window[0].Lo
			if matchesSinglePort(f.rawDeny, addr, port) {
				return window
			}
			if f.ips.allows(addr) || matchesSinglePort(f.rawAllow, addr, port) {
				return nil
			}
			return window
		}
		f.deny, f.allow = compileScopedPorts(f.rawDeny), compileScopedPorts(f.rawAllow)
		f.compiled = true
	}
	matching := func(rules []teardownPortRule) tcpSpans {
		var group hostSpans
		for i := range rules {
			r := &rules[i]
			if r.prefix.IsValid() && !r.matches(addr) {
				continue
			}
			if r.spans == nil {
				r.spans = tcpRuleSpans(r.ports, r.ranges)
			}
			if r.spans.coversBounds(window) || slices.Equal(r.spans, window) {
				return window
			}
			if hit := r.spans.intersect(window); len(hit) != 0 {
				group.add(hit)
			}
		}
		return group.normalized()
	}
	block := matching(f.deny)
	if slices.Equal(block, window) || f.ips.allows(addr) {
		return block
	}
	return block.union(window.subtract(matching(f.allow)))
}
