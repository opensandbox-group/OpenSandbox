// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"math/bits"
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
	if target == "" {
		return teardownSelector{}
	}
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
	directPortsLeft   int
	compiled          bool
}

// Small deletions need not index the policy's port lists. Bound both the
// work on one host (so wide windows use set operations immediately) and the
// total raw scans over all hosts (so repeated misses eventually amortize an index).
const (
	maxDirectWindow  = 64 // one mask bit per requested port
	directPortBudget = 128
)

func newTeardownPortFilter(p *policy.NetworkPolicy) teardownPortFilter {
	deny, allow := p.PortScopedRules()
	return teardownPortFilter{policy: p, ips: compilePortlessIPPolicy(p), rawDeny: deny, rawAllow: allow, directPortsLeft: directPortBudget}
}

func containsRawPort(ports []int, ranges []policy.PortRange, port int) bool {
	return slices.Contains(ports, port) || slices.ContainsFunc(ranges, func(r policy.PortRange) bool { return r.Contains(port) })
}

func matchesSinglePort(rules []policy.PortScopedRule, addr netip.Addr, port int) bool {
	for _, r := range rules {
		if !containsRawPort(r.Ports, r.Ranges, port) {
			continue
		}
		if r.Target == "" || teardownSelectorFor(r.Target).matches(addr) {
			return true
		}
	}
	return false
}

// Each bit is an index into the small query, not a port number. Check the
// address once per rule and stop searching for points already matched.
func matchingPoints(rules []teardownPortRule, addr netip.Addr, ports []int, want uint64) uint64 {
	left := want
	for i := range rules {
		r := &rules[i]
		if r.prefix.IsValid() && !r.matches(addr) {
			continue
		}
		for todo := left; todo != 0; todo &= todo - 1 {
			j := bits.TrailingZeros64(todo)
			if containsRawPort(r.ports, r.ranges, ports[j]) {
				left &^= uint64(1) << j
			}
		}
		if left == 0 {
			break
		}
	}
	return want &^ left
}

func (f *teardownPortFilter) directPoints(window tcpSpans, buf []int) []int {
	if f.directPortsLeft == 0 {
		return nil
	}
	limit := min(cap(buf), f.directPortsLeft)
	for _, r := range window {
		if r.Lo < 1 || r.Hi > 65535 || r.Lo > r.Hi || r.Hi-r.Lo+1 > limit-len(buf) {
			f.directPortsLeft = 0
			return nil
		}
		for p := r.Lo; p <= r.Hi; p++ {
			buf = append(buf, p)
		}
	}
	f.directPortsLeft -= len(buf)
	return buf
}

func (f *teardownPortFilter) deniedPoints(addr netip.Addr, window tcpSpans, ports []int) tcpSpans {
	want := uint64(1)<<len(ports) - 1
	block := matchingPoints(f.deny, addr, ports, want)
	if block != want && !f.ips.allows(addr) {
		block = want &^ matchingPoints(f.allow, addr, ports, want&^block)
	}
	if block == want {
		return window
	}
	var out tcpSpans
	for ; block != 0; block &= block - 1 {
		p := ports[bits.TrailingZeros64(block)]
		if n := len(out); n != 0 && out[n-1].Hi+1 == p {
			out[n-1].Hi = p
		} else {
			out = append(out, policy.PortRange{Lo: p, Hi: p})
		}
	}
	return out
}

// Evaluate only the requested window. Within it, static denies take
// precedence; an unmatched port takes the portless/default verdict.
func (f *teardownPortFilter) deniedWindow(addr netip.Addr, window tcpSpans) tcpSpans {
	if f.policy == nil || !addr.IsValid() || f.ips.denies(addr) {
		return window
	}
	addr = addr.Unmap()
	var buf [maxDirectWindow]int
	first := f.directPortsLeft == directPortBudget
	points := f.directPoints(window, buf[:0])
	if !f.compiled {
		if first && len(points) == 1 {
			p := points[0]
			if matchesSinglePort(f.rawDeny, addr, p) {
				return window
			}
			if f.ips.allows(addr) || matchesSinglePort(f.rawAllow, addr, p) {
				return nil
			}
			return window
		}
		f.deny, f.allow = compileScopedPorts(f.rawDeny), compileScopedPorts(f.rawAllow)
		f.compiled = true
	}
	if len(points) != 0 {
		return f.deniedPoints(addr, window, points)
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
