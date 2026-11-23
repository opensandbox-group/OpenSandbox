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
	hosts := make(exactTCP)
	for _, as := range [][]netip.Addr{d.Addrs, d.DenyAddrs} {
		for _, a := range as {
			hosts[a] = tcpWithdrawal{}
		}
	}
	for _, ps := range [][]netip.Prefix{d.Prefixes, d.DenyPrefixes} {
		for _, pr := range ps {
			if pr.IsSingleIP() {
				hosts[pr.Addr()] = tcpWithdrawal{}
			}
		}
	}
	for _, ps := range [][]EstablishedPortDrop{d.PortDrops, d.DenyPortDrops} {
		for _, rule := range ps {
			if pr := dropTCPPrefix(rule); pr.IsSingleIP() {
				hosts[pr.Addr()] = tcpWithdrawal{}
			}
		}
	}
	if old != nil && (len(d.DenyPortDrops) != 0 || d.CatchAll) {
		for _, r := range old.Egress {
			if pr := tcpPrefix(r.Target); r.Action == policy.ActionAllow && pr.IsSingleIP() {
				hosts[pr.Addr()] = tcpWithdrawal{}
			}
		}
	}
	if len(hosts) == 0 {
		return
	}
	snapshot := d.snapshotTCP(p)
	for a := range hosts {
		_, admitted := leases[a]
		hosts[a] = snapshot(a, admitted)
	}
	d.exact = hosts
}

func (d EstablishedDrops) snapshotTCP(p *policy.NetworkPolicy) func(netip.Addr, bool) tcpWithdrawal {
	deny, keep, removed := compiledTCP(d.DenyPortDrops), compiledTCP(d.KeepPorts), compiledTCP(d.PortDrops)
	static, _ := tcpPolicy(p)
	return func(a netip.Addr, admitted bool) tcpWithdrawal {
		early := deny.at(a)
		if targetsMatch(d.DenyAddrs, d.DenyPrefixes, nil, a, 0) {
			early = allTCP
		}
		late := removed.at(a)
		if d.CatchAll || targetsMatch(d.Addrs, d.Prefixes, nil, a, 0) {
			late = allTCP
		}
		if admitted || targetsMatch(d.KeepAddrs, d.KeepPrefixes, nil, a, 0) {
			late = nil
		} else {
			late = late.subtract(keep.at(a)).subtract(static.at(a)).subtract(early)
		}
		return tcpWithdrawal{early, late}
	}
}

func (d EstablishedDrops) carryExactTCP(previous, p *policy.NetworkPolicy, allow, deny, wide tcpTargets, priorDNS, leases map[netip.Addr]time.Time) exactTCP {
	reclassify := func(a netip.Addr, prev tcpWithdrawal) tcpWithdrawal {
		blocked := prev.explicit.union(prev.implicit)
		early := blocked.intersect(deny.at(a))
		var late tcpSpans
		if _, live := leases[a]; !live && p.DefaultAction != policy.ActionAllow {
			late = blocked.subtract(early).subtract(allow.at(a))
		}
		return tcpWithdrawal{early, late}
	}
	out := make(exactTCP, len(d.exact))
	for a, prev := range d.exact {
		out[a] = reclassify(a, prev)
	}
	// A former broad deny can become an exact withdrawal on this reload.
	// Snapshot those hosts too, unless an older broad rule still covers them.
	var snapshot func(netip.Addr, bool) tcpWithdrawal
	for _, cur := range deny {
		if !cur.prefix.IsSingleIP() {
			continue
		}
		a := cur.prefix.Addr()
		if _, tracked := out[a]; tracked {
			continue
		}
		if snapshot == nil {
			snapshot = d.snapshotTCP(previous)
		}
		_, admitted := priorDNS[a]
		next := reclassify(a, snapshot(a, admitted))
		if len(next.explicit) == 0 || len(next.implicit) == 0 && len(next.explicit.subtract(wide.at(a))) == 0 {
			continue
		}
		out[a] = next
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

func writeExactTCP(b *strings.Builder, d exactTCP, explicit bool) {
	if len(d) == 0 {
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
	for a, host := range d {
		spans := host.implicit
		if explicit {
			spans = host.explicit
		} else if a.Is4() {
			tracked[0] = append(tracked[0], a)
		} else {
			tracked[1] = append(tracked[1], a)
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
