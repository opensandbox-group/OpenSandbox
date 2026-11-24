// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"net/netip"
	"slices"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// Each prefix stores only ports that change relative to its parent. A
// single-host exception never copies the entire network's port list.
type portDelta struct {
	block, spare tcpSpans
	netPorts     int // Cardinality change without expanding the inherited list.
}

func (r portDelta) apply(prior, window tcpSpans) tcpSpans {
	return prior.subtract(r.spare).union(r.block.intersect(window))
}

type broadTCP struct {
	rules  map[netip.Prefix]portDelta
	depths []int // sorted prefix lengths actually present; -1 is the common root
}

func parentScope(p netip.Prefix) netip.Prefix {
	if p.Bits() <= 0 {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(p.Addr(), p.Bits()-1).Masked()
}

func scopeAt(a netip.Addr, depth int) netip.Prefix {
	if depth < 0 {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(a, depth).Masked()
}

func (m broadTCP) contains(a netip.Addr, port int) bool {
	for i := len(m.depths) - 1; i >= 0; i-- {
		if depth := m.depths[i]; depth <= a.BitLen() {
			r := m.rules[scopeAt(a, depth)]
			if r.block.contains(port) {
				return true
			}
			if r.spare.contains(port) {
				return false
			}
		}
	}
	return false
}

func (m *broadTCP) record(p netip.Prefix, ports, parent tcpSpans) {
	if slices.Equal(ports, parent) {
		return
	}
	if m.rules == nil {
		m.rules = make(map[netip.Prefix]portDelta)
	}
	change := portDelta{block: ports.subtract(parent), spare: parent.subtract(ports)}
	for _, span := range change.block {
		change.netPorts += span.Hi - span.Lo + 1
	}
	for _, span := range change.spare {
		change.netPorts -= span.Hi - span.Lo + 1
	}
	m.rules[p] = change
	if i, present := slices.BinarySearch(m.depths, p.Bits()); !present {
		m.depths = slices.Insert(m.depths, i, p.Bits())
	}
}

func (m broadTCP) anyAt(a netip.Addr) bool {
	n := 0
	for _, depth := range m.depths {
		if depth > a.BitLen() {
			break
		}
		n += m.rules[scopeAt(a, depth)].netPorts
	}
	return n != 0
}

func (m broadTCP) at(a netip.Addr) tcpSpans {
	var ports tcpSpans
	for _, depth := range m.depths {
		if depth > a.BitLen() {
			break
		}
		if r, ok := m.rules[scopeAt(a, depth)]; ok {
			ports = r.apply(ports, allTCP)
		}
	}
	return ports
}

// The two disjoint port sets are evaluated before and after a DNS grant.
// Together they are the full static TCP verdict, including exact targets.
type tcpPlan struct {
	early, late broadTCP
}

const maxDNSExceptions = maxResolvedDomains * maxDomainAddresses

// Preserve genuine earlier admissions while the static baseline still cuts
// those ports. An ACK (or a lease staged in the same static transaction)
// also admits everything that DNS precedes. Empty means no history or slot.
func (p tcpPlan) exception(a netip.Addr, previous tcpSpans, live bool) tcpSpans {
	// A full exemption can be shared. Subsequent plain reloads can only
	// shrink the baseline, and an explicit opt-in starts a new plan.
	if full := previous.coversBounds(allTCP); live || full {
		if early := p.early.anyAt(a); full || !early {
			if early || p.late.anyAt(a) {
				return allTCP
			}
			return nil
		}
	}
	late := p.late.at(a)
	if live {
		if len(previous) == 0 {
			return late
		}
		return late.union(previous.intersect(p.early.at(a)))
	}
	if len(previous) == 0 {
		return nil
	}
	return previous.intersect(p.early.at(a).union(late))
}

// A non-nil previous plan means plain reload. There is deliberately no
// special case for the presence of a *new* deny: default deny counts too.
func (d EstablishedDrops) planTCP(p *policy.NetworkPolicy, previous *tcpPlan) tcpPlan {
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
		s := byScope[p]
		s[k].add(spans)
		byScope[p] = s
	}
	addRules := func(k int, rules tcpTargets) {
		for _, r := range rules {
			add(k, r.prefix, r.spans)
		}
	}
	if previous == nil {
		// Match the rules in the opt-in transaction, including raw static accepts.
		addRules(early, compiledTCP(d.DenyPortDrops))
		addRules(late, compiledTCP(d.PortDrops))
		addRules(keep, compiledTCP(d.KeepPorts))
		for k, addrs := range [3][]netip.Addr{d.DenyAddrs, d.Addrs, d.KeepAddrs} {
			for _, a := range addrs {
				add(k, hostPrefix(a), allTCP)
			}
		}
		for _, p := range d.DenyPrefixes {
			add(early, p, allTCP)
		}
		for _, p := range d.Prefixes {
			add(late, p, allTCP)
		}
		for _, p := range d.KeepPrefixes {
			add(keep, p, allTCP)
		}
		for _, p := range compilePortlessIPPolicy(p).allow {
			add(keep, p, allTCP)
		}
		if d.CatchAll {
			add(late, root, allTCP)
		}
	} else {
		if len(previous.early.rules)+len(previous.late.rules) == 0 {
			return tcpPlan{}
		}
		allows, denies := tcpPolicy(p)
		if p != nil && p.DefaultAction == policy.ActionAllow && len(denies) == 0 {
			return tcpPlan{}
		}
		for p := range previous.early.rules {
			byScope[p] = inputs{}
		}
		for p := range previous.late.rules {
			byScope[p] = inputs{}
		}
		addRules(allow, allows)
		addRules(deny, denies)
	}
	keys := make([]netip.Prefix, 0, len(byScope))
	for p := range byScope {
		keys = append(keys, p)
	}
	// Address order visits parents before children and keeps each subtree
	// contiguous. Only ports mentioned at this node can change its verdict.
	slices.SortFunc(keys, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	var out tcpPlan
	var path []netip.Prefix
	defaultAllow := p != nil && p.DefaultAction == policy.ActionAllow
	for _, p := range keys {
		for len(path) > 0 {
			parent := path[len(path)-1]
			if !parent.IsValid() || parent.Contains(p.Addr()) {
				break
			}
			path = path[:len(path)-1]
		}
		local := byScope[p]
		var window tcpSpans
		for i := range local {
			window = window.union(local[i].normalized())
		}
		byScope[p] = local
		var oldEarly, oldLate portDelta
		if previous != nil {
			oldEarly, oldLate = previous.early.rules[p], previous.late.rules[p]
			for _, r := range [2]portDelta{oldEarly, oldLate} {
				window = window.union(r.block).union(r.spare)
			}
		}
		var parentEarly, parentLate, priorEarly, priorLate tcpSpans
		for _, ancestor := range path {
			for i, r := range byScope[ancestor] {
				local[i].spans = local[i].spans.union(r.spans.intersect(window))
			}
			parentEarly = out.early.rules[ancestor].apply(parentEarly, window)
			parentLate = out.late.rules[ancestor].apply(parentLate, window)
			if previous != nil {
				priorEarly = previous.early.rules[ancestor].apply(priorEarly, window)
				priorLate = previous.late.rules[ancestor].apply(priorLate, window)
			}
		}
		var beforeDNS, afterDNS tcpSpans
		if previous == nil {
			beforeDNS = local[early].spans
			afterDNS = local[late].spans.subtract(local[keep].spans).subtract(beforeDNS)
		} else {
			blocked := oldEarly.apply(priorEarly, window).union(oldLate.apply(priorLate, window))
			beforeDNS = blocked.intersect(local[deny].spans)
			if !defaultAllow {
				afterDNS = blocked.subtract(local[allow].spans).subtract(beforeDNS)
			}
		}
		out.early.record(p, beforeDNS, parentEarly)
		out.late.record(p, afterDNS, parentLate)
		if !p.IsSingleIP() {
			path = append(path, p)
		}
	}
	return out
}

func (d *EstablishedDrops) captureTCP(p *policy.NetworkPolicy, leases map[netip.Addr]time.Time) {
	if d.Empty() {
		return
	}
	plan := d.planTCP(p, nil)
	d.tcp = &plan
	d.retainDNS(nil, leases)
}

func (d *EstablishedDrops) retainDNS(previous map[netip.Addr]tcpSpans, leases map[netip.Addr]time.Time) {
	store := func(a netip.Addr, ports tcpSpans, live bool) {
		if keep := d.tcp.exception(a, ports, live); len(keep) != 0 {
			if d.dnsKeeps == nil {
				d.dnsKeeps = make(map[netip.Addr]tcpSpans)
			}
			d.dnsKeeps[a] = keep
		}
	}
	for a, ports := range previous {
		_, live := leases[a]
		store(a, ports, live)
	}
	for a := range leases {
		if _, processed := previous[a]; !processed {
			if len(d.dnsKeeps) > maxDNSExceptions {
				return // The caller rejects this plan before any kernel write.
			}
			store(a, nil, true)
		}
	}
}
