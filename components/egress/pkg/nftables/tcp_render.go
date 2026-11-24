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
)

type portRuleGroup struct {
	spans    tcpSpans
	prefixes []netip.Prefix
	verb     string
}

// Prefixes at the same depth are disjoint. Group equal port lists by content,
// without expanding CIDRs or assuming that equal lists share slice storage.
type portRuleKey struct {
	hash uint64
	bits int
	v4   bool
	verb string
}

type tcpRules struct {
	seed   maphash.Seed
	index  map[portRuleKey][]int
	groups []portRuleGroup
}

func (r *tcpRules) add(p netip.Prefix, spans tcpSpans, verb string) {
	if len(spans) == 0 {
		return
	}
	if r.index == nil {
		r.index = make(map[portRuleKey][]int)
		r.seed = maphash.MakeSeed()
	}
	var hash maphash.Hash
	hash.SetSeed(r.seed)
	var buf [4]byte
	for _, span := range spans {
		binary.LittleEndian.PutUint32(buf[:], uint32(span.Lo)|uint32(span.Hi)<<16)
		_, _ = hash.Write(buf[:])
	}
	k := portRuleKey{hash.Sum64(), p.Bits(), p.Addr().Is4(), verb}
	for _, i := range r.index[k] {
		if slices.Equal(r.groups[i].spans, spans) {
			r.groups[i].prefixes = append(r.groups[i].prefixes, p)
			return
		}
	}
	r.index[k] = append(r.index[k], len(r.groups))
	r.groups = append(r.groups, portRuleGroup{spans, []netip.Prefix{p}, verb})
}

func (r *tcpRules) write(b *strings.Builder, chain string) {
	for _, group := range r.groups {
		slices.SortFunc(group.prefixes, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	}
	slices.SortFunc(r.groups, func(a, b portRuleGroup) int {
		x, y := a.prefixes[0], b.prefixes[0]
		if x.Bits() != y.Bits() {
			return y.Bits() - x.Bits()
		}
		if c := x.Addr().Compare(y.Addr()); c != 0 {
			return c
		}
		return strings.Compare(a.verb, b.verb)
	})
	for i := 0; i < len(r.groups); i++ {
		if n := writeHostExceptions(b, chain, r.groups[i:]); n > 1 {
			i += n - 1
			continue
		}
		group := r.groups[i]
		fmt.Fprintf(b, "add rule inet %s %s ", tableName, chain)
		if first := group.prefixes[0]; first.IsValid() {
			fmt.Fprintf(b, "%s daddr ", addressFamily(first.Addr()))
			if len(group.prefixes) == 1 {
				b.WriteString(formatPrefixForNFT(first))
			} else {
				fmt.Fprintf(b, "{ %s", formatPrefixForNFT(first))
				for _, p := range group.prefixes[1:] {
					b.WriteString(", ")
					b.WriteString(formatPrefixForNFT(p))
				}
				b.WriteString(" }")
			}
			b.WriteByte(' ')
		}
		sep := "tcp dport { "
		for _, span := range group.spans {
			fmt.Fprintf(b, "%s%d", sep, span.Lo)
			sep = ", "
			if span.Lo != span.Hi {
				fmt.Fprintf(b, "-%d", span.Hi)
			}
		}
		fmt.Fprintf(b, " } ct state established,related %s\n", group.verb)
	}
}

func addressFamily(a netip.Addr) string {
	if a.Is4() {
		return "ip"
	}
	return "ip6"
}

// Batch exact hosts with the same port window minus isolated points. Limit
// holes per host: repeating the address for dense exclusions grows the set.
func writeHostExceptions(b *strings.Builder, chain string, groups []portRuleGroup) int {
	first, bounds := groups[0].prefixes[0].Addr(), groups[0].spans
	lo, hi := bounds[0].Lo, bounds[len(bounds)-1].Hi
	n := 0
hosts:
	for _, g := range groups {
		p, s := g.prefixes[0], g.spans
		if g.verb != "drop" || len(g.prefixes) != 1 || !p.IsSingleIP() || p.Addr().Is4() != first.Is4() ||
			len(s) < 2 || len(s) > 5 || s[0].Lo != lo || s[len(s)-1].Hi != hi {
			break
		}
		for i, next := range s[1:] {
			if next.Lo != s[i].Hi+2 {
				break hosts
			}
		}
		n++
	}
	if n < 2 {
		return n
	}
	family := addressFamily(first)
	fmt.Fprintf(b, "add rule inet %s %s %s daddr { %s", tableName, chain, family, first)
	for _, g := range groups[1:n] {
		b.WriteString(", ")
		b.WriteString(g.prefixes[0].Addr().String())
	}
	fmt.Fprintf(b, " } %s daddr . tcp dport != { ", family)
	sep := ""
	for _, g := range groups[:n] {
		addr := g.prefixes[0].Addr().String()
		for _, span := range g.spans[:len(g.spans)-1] {
			fmt.Fprintf(b, "%s%s . %d", sep, addr, span.Hi+1)
			sep = ", "
		}
	}
	fmt.Fprintf(b, " } tcp dport %d-%d ct state established,related drop\n", lo, hi)
	return n
}

// A port in the root early drop cannot reach the post-DNS rules unless a
// narrower early rule makes a hole. Subtract all such holes conservatively:
// the result is safe to ignore for every address, in either family.
func (m broadTCP) alwaysDropped() tcpSpans {
	floor := m.rules[netip.Prefix{}].block
	for _, delta := range m.rules {
		if len(floor) == 0 {
			break
		}
		floor = floor.subtract(delta.spare)
	}
	return floor
}

// Join neighboring port ranges if the entire gap was handled earlier.
// Never modify the plan: DNS admission still needs the original disjoint spans.
func bridgePortGaps(spans, handled tcpSpans) tcpSpans {
	if len(spans) < 2 || len(handled) == 0 {
		return spans
	}
	handled = handled.from(spans[0].Hi + 1)
	var out tcpSpans
	start := 0
	for i := 1; i < len(spans) && len(handled) != 0; i++ {
		lo, hi := spans[i-1].Hi+1, spans[i].Lo-1
		for len(handled) != 0 && handled[0].Hi < lo {
			handled = handled[1:]
		}
		if len(handled) != 0 && handled[0].Lo <= lo && hi <= handled[0].Hi {
			out = append(out, spans[start:i]...)
			out[len(out)-1].Hi = spans[i].Hi
			start = i + 1
		}
	}
	if out == nil {
		return spans
	}
	return append(out, spans[start:]...)
}

// A hole in an early drop must reach DNS and the late phase, not accept.
// A subchain is needed only when a more-specific prefix has such a hole.
func (m broadTCP) writePhase(b *strings.Builder, early bool, handled tcpSpans) {
	var rules tcpRules
	chain := chainName
	for p, delta := range m.rules {
		rules.add(p, bridgePortGaps(delta.block, handled), "drop")
		if hole := delta.spare; len(hole) != 0 {
			if early {
				chain = "carried_tcp_deny"
				rules.add(p, hole, "return")
			} else {
				rules.add(p, hole, "accept")
			}
		}
	}
	if chain != chainName {
		fmt.Fprintf(b, "add chain inet %s %s\nadd rule inet %s %s meta l4proto tcp ct state established,related jump %s\n", tableName, chain, tableName, chainName, chain)
	}
	rules.write(b, chain)
}
