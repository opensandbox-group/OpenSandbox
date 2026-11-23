// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// Each entry owns the effective TCP withdrawal for an exact address. An empty
// entry still exempts that host from the legacy address/prefix teardown, whose
// late rules are retained for non-TCP and for destinations outside this plan.
type exactTCP map[netip.Addr]tcpWithdrawal

type tcpWithdrawal struct{ explicit, implicit tcpSpans }

type tcpSpans []policy.PortRange // sorted, disjoint, immutable once published

var allTCP = tcpSpans{{Lo: 1, Hi: 65535}}

func tcpRuleSpans(ports []int, ranges []policy.PortRange) tcpSpans {
	if len(ports) == 0 && len(ranges) == 0 {
		return allTCP
	}
	// Already normalized range-only rules (including the operator floor) can be borrowed.
	if len(ports) == 0 {
		for i := 1; i < len(ranges); i++ {
			if ranges[i].Lo <= ranges[i-1].Hi+1 {
				return mergeTCP(slices.Clone(ranges))
			}
		}
		return ranges
	}
	out := make(tcpSpans, 0, len(ports)+len(ranges))
	out = append(out, ranges...)
	for _, port := range ports {
		out = append(out, policy.PortRange{Lo: port, Hi: port})
	}
	return mergeTCP(out)
}

func mergeTCP(spans tcpSpans) tcpSpans {
	slices.SortFunc(spans, func(a, b policy.PortRange) int { return a.Lo - b.Lo })
	out := spans[:0]
	for _, r := range spans {
		if n := len(out); n != 0 && r.Lo <= out[n-1].Hi+1 {
			out[n-1].Hi = max(out[n-1].Hi, r.Hi)
		} else {
			out = append(out, r)
		}
	}
	return out
}

func (a tcpSpans) union(b tcpSpans) tcpSpans {
	switch {
	case len(a) == 0 || b.coversBounds(a):
		return b
	case len(b) == 0 || a.coversBounds(b) || slices.Equal(a, b):
		return a
	}
	out := make(tcpSpans, len(a)+len(b))
	copy(out, a)
	copy(out[len(a):], b)
	return mergeTCP(out)
}

func (a tcpSpans) coversBounds(b tcpSpans) bool {
	return len(a) == 1 && len(b) != 0 && a[0].Lo <= b[0].Lo && a[0].Hi >= b[len(b)-1].Hi
}

func (a tcpSpans) intersect(b tcpSpans) tcpSpans {
	if a.coversBounds(b) {
		return b
	}
	if b.coversBounds(a) || slices.Equal(a, b) {
		return a
	}
	var out tcpSpans
	for i, j := 0, 0; i < len(a) && j < len(b); {
		x, y := a[i], b[j]
		if lo, hi := max(x.Lo, y.Lo), min(x.Hi, y.Hi); lo <= hi {
			out = append(out, policy.PortRange{Lo: lo, Hi: hi})
		}
		if x.Hi <= y.Hi {
			i++
		} else {
			j++
		}
	}
	return out
}

func (a tcpSpans) subtract(b tcpSpans) tcpSpans {
	if len(a) == 0 || len(b) == 0 {
		return a
	}
	if b.coversBounds(a) || slices.Equal(a, b) {
		return nil
	}
	var out tcpSpans
	j, changed := 0, false
	for i, r := range a {
		for j < len(b) && b[j].Hi < r.Lo {
			j++
		}
		if j == len(b) || b[j].Lo > r.Hi {
			if changed {
				out = append(out, r)
			}
			continue
		}
		if !changed {
			out, changed = append(out, a[:i]...), true
		}
		for j < len(b) && b[j].Lo <= r.Hi {
			if b[j].Lo > r.Lo {
				out = append(out, policy.PortRange{Lo: r.Lo, Hi: b[j].Lo - 1})
			}
			r.Lo = b[j].Hi + 1
			if r.Lo > r.Hi {
				break
			}
			j++
		}
		if r.Lo <= r.Hi {
			out = append(out, r)
		}
	}
	if !changed {
		return a
	}
	return out
}

func (a tcpSpans) contains(port int) bool {
	i, _ := slices.BinarySearchFunc(a, port, func(r policy.PortRange, p int) int { return r.Hi - p })
	return i < len(a) && a[i].Lo <= port
}

type tcpTarget struct {
	prefix netip.Prefix // invalid means a port-only rule
	spans  tcpSpans
	whole  bool
}

type tcpTargets []tcpTarget

func (ts tcpTargets) at(addr netip.Addr) tcpSpans {
	var spans tcpSpans
	for _, t := range ts {
		if !t.prefix.IsValid() || t.prefix.Contains(addr) {
			spans = spans.union(t.spans)
		}
	}
	return spans
}

func tcpPrefix(target string) netip.Prefix {
	if a, err := netip.ParseAddr(target); err == nil {
		return netip.PrefixFrom(a, a.BitLen())
	}
	p, _ := netip.ParsePrefix(target)
	return p.Masked()
}

func tcpPolicy(p *policy.NetworkPolicy) (allow, deny tcpTargets) {
	if p == nil {
		return
	}
	for _, r := range p.Egress {
		prefix := tcpPrefix(r.Target)
		whole := len(r.Ports)+len(r.PortRanges) == 0
		if !prefix.IsValid() && (r.Target != "" || whole) {
			continue
		}
		t := tcpTarget{prefix: prefix, spans: tcpRuleSpans(r.Ports, r.PortRanges), whole: whole}
		if r.Action == policy.ActionAllow {
			allow = append(allow, t)
		} else {
			deny = append(deny, t)
		}
	}
	return
}

func dropTCPPrefix(d EstablishedPortDrop) netip.Prefix {
	if d.Addr.IsValid() {
		return hostPrefix(d.Addr)
	}
	return d.Prefix
}

func compiledTCP(ports []EstablishedPortDrop) tcpTargets {
	out := make(tcpTargets, 0, len(ports))
	for _, d := range ports {
		out = append(out, tcpTarget{prefix: dropTCPPrefix(d), spans: tcpRuleSpans(d.Ports, d.Ranges)})
	}
	return out
}

// Capture the verdict of the opt-in rules actually installed, including scoped
// denies in front of a whole-address allow or of a live DNS grant.
func (d *EstablishedDrops) captureExactTCP(old, p *policy.NetworkPolicy, leases map[netip.Addr]time.Time) {
	if d.Empty() {
		return
	}
	subjects := d.exactSubjects(old, p)
	if len(subjects) == 0 {
		return
	}
	snapshot := d.snapshotTCP(p)
	hosts := make(exactTCP, len(d.Addrs))
	for a := range subjects {
		_, admitted := leases[a]
		if w, affected := snapshot(a, admitted); affected {
			hosts[a] = w
		}
	}
	d.exact = hosts
}

// Discover subjects, not verdicts. A named host can be denied by any bucket
// (including CatchAll). Keep derived singleton targets and DNS addresses;
// never expand a multi-address prefix to discover hosts.
func (d EstablishedDrops) exactSubjects(old, next *policy.NetworkPolicy) map[netip.Addr]struct{} {
	hosts := make(map[netip.Addr]struct{}, len(d.Addrs))
	for _, as := range [][]netip.Addr{d.Addrs, d.DenyAddrs, d.KeepAddrs} {
		for _, a := range as {
			hosts[a] = struct{}{}
		}
	}
	for _, ps := range [][]netip.Prefix{d.Prefixes, d.DenyPrefixes, d.KeepPrefixes} {
		for _, pr := range ps {
			if pr.IsSingleIP() {
				hosts[pr.Addr()] = struct{}{}
			}
		}
	}
	for _, ps := range [][]EstablishedPortDrop{d.PortDrops, d.DenyPortDrops, d.KeepPorts} {
		for _, rule := range ps {
			if pr := dropTCPPrefix(rule); pr.IsSingleIP() {
				hosts[pr.Addr()] = struct{}{}
			}
		}
	}
	for _, p := range []*policy.NetworkPolicy{old, next} {
		if p == nil {
			continue
		}
		for _, r := range p.Egress {
			if pr := tcpPrefix(r.Target); pr.IsSingleIP() {
				hosts[pr.Addr()] = struct{}{}
			}
		}
	}
	return hosts
}

// Index exact targets once per plan. Only multi-address and global
// rules require a containment check for each host.
type snapshotTargets struct {
	exact map[netip.Addr]hostSpans
	wide  tcpTargets
}

type hostSpans struct {
	spans tcpSpans
	merge bool
}

func (s *snapshotTargets) add(p netip.Prefix, spans tcpSpans) {
	if !p.IsSingleIP() {
		s.wide = append(s.wide, tcpTarget{prefix: p, spans: spans})
		return
	}
	if s.exact == nil {
		s.exact = make(map[netip.Addr]hostSpans)
	}
	a := p.Addr() // /128 preserves mapped IPv6, unlike an Addr bucket.
	group := s.exact[a]
	switch {
	case len(group.spans) == 0 || spans.coversBounds(allTCP):
		// Borrow the first rule. Clipping ensures that appending a second
		// rule produces a private buffer before it is ever normalized.
		group = hostSpans{spans: slices.Clip(spans)}
	case !group.merge && group.spans.coversBounds(spans):
		return
	default:
		group.spans = append(group.spans, spans...)
		group.merge = true
	}
	s.exact[a] = group
}

func (ts tcpTargets) indexed() snapshotTargets {
	var s snapshotTargets
	for _, t := range ts {
		s.add(t.prefix, t.spans)
	}
	return s
}

func indexSnapshotTargets(addrs []netip.Addr, ports []EstablishedPortDrop, prefixSets ...[]netip.Prefix) snapshotTargets {
	var s snapshotTargets
	if len(addrs) != 0 {
		s.exact = make(map[netip.Addr]hostSpans, len(addrs))
		for _, a := range addrs {
			s.exact[a.Unmap()] = hostSpans{spans: allTCP}
		}
	}
	for _, prefixes := range prefixSets {
		for _, p := range prefixes {
			if p.IsValid() {
				s.add(p, allTCP)
			}
		}
	}
	for _, rule := range ports {
		s.add(dropTCPPrefix(rule), tcpRuleSpans(rule.Ports, rule.Ranges))
	}
	return s
}

func (s snapshotTargets) at(a netip.Addr) tcpSpans {
	group := s.exact[a]
	if group.merge {
		group.spans = mergeTCP(group.spans)
		group.merge = false
		s.exact[a] = group
	}
	exact := group.spans
	if len(s.wide) == 0 || exact.coversBounds(allTCP) {
		return exact
	}
	return exact.union(s.wide.at(a))
}

// The bool distinguishes an unaffected host from a masked legacy drop: the
// latter may need an empty TCP withdrawal to preserve its exemption later.
func (d EstablishedDrops) snapshotTCP(p *policy.NetworkPolicy) func(netip.Addr, bool) (tcpWithdrawal, bool) {
	deny := indexSnapshotTargets(d.DenyAddrs, d.DenyPortDrops, d.DenyPrefixes)
	removed := indexSnapshotTargets(d.Addrs, d.PortDrops, d.Prefixes)
	var keep snapshotTargets
	if d.CatchAll || len(d.Addrs)+len(d.Prefixes)+len(d.PortDrops) != 0 {
		keep = indexSnapshotTargets(d.KeepAddrs, d.KeepPorts, d.KeepPrefixes, compilePortlessIPPolicy(p).allow)
	}
	return func(a netip.Addr, admitted bool) (tcpWithdrawal, bool) {
		early := deny.at(a)
		late := allTCP
		if !d.CatchAll {
			late = removed.at(a)
			if len(late) == 0 {
				return tcpWithdrawal{explicit: early}, len(early) != 0
			}
		}
		if admitted {
			late = nil
		} else {
			late = late.subtract(keep.at(a)).subtract(early)
		}
		return tcpWithdrawal{early, late}, true
	}
}

func (d EstablishedDrops) carryExactTCP(previous, p *policy.NetworkPolicy, allow, deny tcpTargets, priorDNS, leases map[netip.Addr]time.Time) exactTCP {
	allows, denies := allow.indexed(), deny.indexed()
	reclassify := func(a netip.Addr, prev tcpWithdrawal) tcpWithdrawal {
		blocked := prev.explicit.union(prev.implicit)
		early := blocked.intersect(denies.at(a))
		var late tcpSpans
		if _, live := leases[a]; !live && p.DefaultAction != policy.ActionAllow {
			late = blocked.subtract(early).subtract(allows.at(a))
		}
		return tcpWithdrawal{early, late}
	}
	out := make(exactTCP, len(d.exact))
	for a, prev := range d.exact {
		out[a] = reclassify(a, prev)
	}
	// A host first named on a plain reload also inherits the prior effective
	// verdict, even when its new rule is an allow beneath a broader deny.
	var snapshot func(netip.Addr, bool) (tcpWithdrawal, bool)
	for _, targets := range [2]snapshotTargets{allows, denies} {
		for a := range targets.exact {
			if _, tracked := out[a]; tracked {
				continue
			}
			if snapshot == nil {
				snapshot = d.snapshotTCP(previous)
			}
			_, admitted := priorDNS[a]
			prev, affected := snapshot(a, admitted)
			if !affected {
				continue
			}
			next := reclassify(a, prev)
			if len(next.explicit)+len(next.implicit) != 0 {
				out[a] = next
			}
		}
	}
	return out
}

func (d exactTCP) hasDrops() bool {
	for _, host := range d {
		if len(host.explicit)+len(host.implicit) != 0 {
			return true
		}
	}
	return false
}

// Wide explicit denies remain in the legacy buckets. Intersect only current
// explicit scope here; exact withdrawals use the saved per-port verdict.
func (d EstablishedDrops) carryExplicitTCP(current tcpTargets, retainedWhole []netip.Prefix) []EstablishedPortDrop {
	prior := compiledTCP(d.DenyPortDrops)
	for _, prefix := range d.DenyPrefixes {
		prior = append(prior, tcpTarget{prefix: prefix, spans: allTCP, whole: true})
	}
	kept := make(map[netip.Prefix]tcpSpans)
	for _, r := range prior {
		if _, tracked := d.exact[r.prefix.Addr()]; r.prefix.IsSingleIP() && tracked {
			continue
		}
		for _, cur := range current {
			if r.whole && cur.whole || r.prefix.IsValid() && cur.prefix.IsValid() && !r.prefix.Overlaps(cur.prefix) {
				continue
			}
			prefix := r.prefix
			if cur.prefix.Bits() > prefix.Bits() {
				prefix = cur.prefix
			}
			if _, tracked := d.exact[prefix.Addr()]; prefix.IsSingleIP() && tracked {
				continue
			}
			kept[prefix] = kept[prefix].union(r.spans.intersect(cur.spans))
		}
	}
	var out []EstablishedPortDrop
	for prefix, spans := range kept {
		if prefix.IsValid() {
			if slices.ContainsFunc(retainedWhole, func(p netip.Prefix) bool { return p.Bits() <= prefix.Bits() && p.Contains(prefix.Addr()) }) {
				continue
			}
			spans = spans.subtract(kept[netip.Prefix{}])
			for bits := prefix.Bits() - 1; bits >= 0 && len(spans) != 0; bits-- {
				spans = spans.subtract(kept[netip.PrefixFrom(prefix.Addr(), bits).Masked()])
			}
		}
		if len(spans) != 0 {
			out = append(out, EstablishedPortDrop{Prefix: prefix, Ranges: spans})
		}
	}
	slices.SortFunc(out, func(a, b EstablishedPortDrop) int {
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c
		}
		return a.Prefix.Bits() - b.Prefix.Bits()
	})
	return out
}

// A tracked host needs a TCP remainder accept only if a legacy late drop
// could still catch it. Track the host even when this rule is unnecessary.
func (d EstablishedDrops) needsTCPRemainder(addr netip.Addr) bool {
	if targetsMatch(d.KeepAddrs, d.KeepPrefixes, nil, addr, 0) {
		return false
	}
	return d.CatchAll || targetsMatch(d.Addrs, d.Prefixes, nil, addr, 0) ||
		slices.ContainsFunc(d.PortDrops, func(drop EstablishedPortDrop) bool { return portDropCoversAddr(drop, addr) })
}

func (d EstablishedDrops) writeExactTCP(b *strings.Builder, explicit bool, covered tcpTargets) {
	if len(d.exact) == 0 {
		return
	}
	// Hash normalized content (not the slice identity); verify equality before
	// grouping. A hash collision can cost a comparison but cannot widen scope.
	type key struct {
		sum uint64
		v4  bool
	}
	type group struct {
		spans tcpSpans
		addrs []netip.Addr
	}
	groups := make(map[key][]int)
	var rules []group
	var tracked [2][]netip.Addr
	seed := maphash.MakeSeed()
	for a, host := range d.exact {
		spans := host.implicit
		if explicit {
			spans = host.explicit
			// Suppress only rules already emitted at this priority. The
			// saved per-host withdrawal must outlive their coverage.
			for _, rule := range covered {
				if !rule.prefix.IsValid() || rule.prefix.Contains(a) {
					spans = spans.subtract(rule.spans)
				}
			}
		} else if d.needsTCPRemainder(a) {
			if a.Is4() {
				tracked[0] = append(tracked[0], a)
			} else {
				tracked[1] = append(tracked[1], a)
			}
		}
		if len(spans) == 0 {
			continue
		}
		var h maphash.Hash
		h.SetSeed(seed)
		var bytes [4]byte
		for _, r := range spans {
			binary.LittleEndian.PutUint32(bytes[:], uint32(r.Lo)|uint32(r.Hi)<<16)
			_, _ = h.Write(bytes[:])
		}
		k := key{h.Sum64(), a.Is4()}
		i := len(rules)
		for _, candidate := range groups[k] {
			if slices.Equal(rules[candidate].spans, spans) {
				i = candidate
				break
			}
		}
		if i == len(rules) {
			rules = append(rules, group{spans: spans})
			groups[k] = append(groups[k], i)
		}
		rules[i].addrs = append(rules[i].addrs, a)
	}
	for _, r := range rules {
		slices.SortFunc(r.addrs, netip.Addr.Compare)
	}
	slices.SortFunc(rules, func(a, b group) int { return a.addrs[0].Compare(b.addrs[0]) })
	for _, r := range rules {
		writeExactAddrMatch(b, r.addrs)
		b.WriteString(" tcp dport { ")
		for i, span := range r.spans {
			if i != 0 {
				b.WriteString(", ")
			}
			fmt.Fprint(b, span.Lo)
			if span.Lo != span.Hi {
				fmt.Fprintf(b, "-%d", span.Hi)
			}
		}
		b.WriteString(" } ct state established,related drop\n")
	}
	for _, addrs := range tracked {
		if len(addrs) == 0 {
			continue
		}
		slices.SortFunc(addrs, netip.Addr.Compare)
		writeExactAddrMatch(b, addrs)
		b.WriteString(" meta l4proto tcp ct state established,related accept\n")
	}
}

func writeExactAddrMatch(b *strings.Builder, addrs []netip.Addr) {
	family := "ip6"
	if addrs[0].Is4() {
		family = "ip"
	}
	fmt.Fprintf(b, "add rule inet %s %s %s daddr ", tableName, chainName, family)
	if len(addrs) == 1 {
		b.WriteString(addrs[0].String())
		return
	}
	b.WriteString("{ ")
	for i, addr := range addrs {
		if i != 0 {
			b.WriteString(", ")
		}
		b.WriteString(addr.String())
	}
	b.WriteString(" }")
}
