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
	for _, group := range r.groups {
		fmt.Fprintf(b, "add rule inet %s %s ", tableName, chain)
		if first := group.prefixes[0]; first.IsValid() {
			family := "ip6"
			if first.Addr().Is4() {
				family = "ip"
			}
			fmt.Fprintf(b, "%s daddr ", family)
			if len(group.prefixes) > 1 {
				b.WriteString("{ ")
			}
			for i, p := range group.prefixes {
				if i != 0 {
					b.WriteString(", ")
				}
				if p.IsSingleIP() {
					b.WriteString(p.Addr().String())
				} else {
					b.WriteString(p.String())
				}
			}
			if len(group.prefixes) > 1 {
				b.WriteString(" }")
			}
			b.WriteByte(' ')
		}
		b.WriteString("tcp dport { ")
		for i, span := range group.spans {
			if i != 0 {
				b.WriteString(", ")
			}
			fmt.Fprint(b, span.Lo)
			if span.Lo != span.Hi {
				fmt.Fprintf(b, "-%d", span.Hi)
			}
		}
		fmt.Fprintf(b, " } ct state established,related %s\n", group.verb)
	}
}

// A hole in an early drop must reach DNS and the late phase, not accept.
// A subchain is needed only when a more-specific prefix has such a hole.
func (m broadTCP) writePhase(b *strings.Builder, early bool) {
	var rules tcpRules
	chain := chainName
	for p, delta := range m.rules {
		rules.add(p, delta.block, "drop")
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
