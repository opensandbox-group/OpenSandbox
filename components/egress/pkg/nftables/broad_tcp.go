// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// A sparse longest-prefix map for TCP connections whose host was not named.
// Children override their parent, including with an empty set. The invalid
// prefix is the common root; /0 rules still belong to their own family.
type broadTCP map[netip.Prefix]tcpSpans

func parentScope(p netip.Prefix) netip.Prefix {
	if p.Bits() <= 0 {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(p.Addr(), p.Bits()-1).Masked()
}

func (m broadTCP) lookup(p netip.Prefix) tcpSpans {
	if len(m) == 0 {
		return nil
	}
	for p.IsValid() {
		if v, ok := m[p]; ok {
			return v
		}
		p = parentScope(p)
	}
	return m[netip.Prefix{}]
}

func (m broadTCP) at(a netip.Addr) tcpSpans { return m.lookup(netip.PrefixFrom(a, a.BitLen())) }

// Once newly configured wide allows and denies coincide, the legacy union
// of keeps no longer expresses what was actually reopened. From then on,
// carry blocked = priorBlocked ∩ currentlyDenied for unnamed destinations.
// Exact subjects, dynamic permits and non-TCP verdicts remain independent.
func (d EstablishedDrops) carryBroadTCP(previous, next *policy.NetworkPolicy, allows, denies tcpTargets, exact exactTCP) broadTCP {
	if d.broad == nil {
		wide := func(t tcpTarget) bool { return !t.prefix.IsSingleIP() }
		if !(d.CatchAll || len(d.Prefixes)+len(d.PortDrops) != 0) || !slices.ContainsFunc(denies, wide) ||
			!(next.DefaultAction == policy.ActionAllow || slices.ContainsFunc(allows, wide)) {
			return nil
		}
	}
	const (
		early = iota
		late
		keep
		allow
		deny
	)
	type inputs [5]hostSpans
	byScope := make(map[netip.Prefix]inputs)
	root := netip.Prefix{}
	add := func(k int, p netip.Prefix, spans tcpSpans) {
		if p.IsSingleIP() {
			if _, ok := exact[p.Addr()]; ok {
				return
			}
		}
		s := byScope[p]
		s[k].add(spans)
		byScope[p] = s
	}
	addRules := func(k int, rules tcpTargets) {
		for _, r := range rules {
			add(k, r.prefix, r.spans)
		}
	}
	if d.broad == nil {
		// Reconstruct the previous installed verdict, not the new allows.
		// Old singleton address buckets are already owned by the exact plan.
		addRules(early, compiledTCP(d.DenyPortDrops))
		addRules(late, compiledTCP(d.PortDrops))
		addRules(keep, compiledTCP(d.KeepPorts))
		for _, p := range d.DenyPrefixes {
			add(early, p, allTCP)
		}
		for _, p := range d.Prefixes {
			add(late, p, allTCP)
		}
		for _, p := range d.KeepPrefixes {
			add(keep, p, allTCP)
		}
		for _, p := range compilePortlessIPPolicy(previous).allow {
			add(keep, p, allTCP)
		}
		if d.CatchAll {
			add(late, root, allTCP)
		}
	} else {
		for p := range d.broad {
			byScope[p] = inputs{}
		}
	}
	addRules(allow, allows)
	addRules(deny, denies)
	keys := make([]netip.Prefix, 0, len(byScope))
	for p := range byScope {
		keys = append(keys, p)
	}
	slices.SortFunc(keys, func(a, b netip.Prefix) int { return a.Bits() - b.Bits() })
	out := make(broadTCP)
	for _, p := range keys {
		s := byScope[p]
		for i := range s {
			s[i].normalized()
		}
		if p.IsValid() {
			for parent := parentScope(p); ; parent = parentScope(parent) {
				if inherited, ok := byScope[parent]; ok {
					for i := range s {
						s[i].spans = inherited[i].spans.union(s[i].spans)
					}
					break
				}
				if !parent.IsValid() {
					break
				}
			}
		}
		byScope[p] = s
		blocked := d.broad.lookup(p)
		if d.broad == nil {
			blocked = s[early].spans.union(s[late].spans.subtract(s[keep].spans))
		}
		left := blocked.intersect(s[deny].spans)
		if next.DefaultAction != policy.ActionAllow {
			left = left.union(blocked.subtract(s[allow].spans))
		}
		if !slices.Equal(left, out.lookup(parentScope(p))) {
			out[p] = left
		}
	}
	return out
}

// Emit only the differences from the containing prefix, most specific first.
// A shared port floor need not be repeated at every more-specific scope.
func (m broadTCP) write(b *strings.Builder) {
	keys := make([]netip.Prefix, 0, len(m))
	for p := range m {
		keys = append(keys, p)
	}
	slices.SortFunc(keys, func(a, b netip.Prefix) int {
		if a.Bits() != b.Bits() {
			return b.Bits() - a.Bits()
		}
		return a.Addr().Compare(b.Addr())
	})
	for _, p := range keys {
		var parent tcpSpans
		if p.IsValid() {
			parent = m.lookup(parentScope(p))
		}
		writeEstablishedPortRule(b, EstablishedPortDrop{Prefix: p, Ranges: m[p].subtract(parent)}, "drop")
		writeEstablishedPortRule(b, EstablishedPortDrop{Prefix: p, Ranges: parent.subtract(m[p])}, "accept")
	}
	fmt.Fprintf(b, "add rule inet %s %s meta l4proto tcp ct state established,related accept\n", tableName, chainName)
}
