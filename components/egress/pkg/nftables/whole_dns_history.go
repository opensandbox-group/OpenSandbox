// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// The invalid prefix represents a targetless TCP port rule.
type scopedEstablishedKeepCoverage struct {
	scopes map[netip.Prefix]tcpSpans
	depths []int
}

func (c *scopedEstablishedKeepCoverage) compile(d EstablishedDrops) {
	if len(d.KeepAddrs)+len(d.KeepPrefixes)+len(d.KeepPorts) == 0 {
		return
	}
	c.scopes = make(map[netip.Prefix]tcpSpans)
	for _, addr := range d.KeepAddrs {
		if addr.IsValid() {
			c.scopes[hostPrefix(addr)] = allTCP
		}
	}
	for _, prefix := range d.KeepPrefixes {
		if prefix.IsValid() {
			c.scopes[prefix.Masked()] = allTCP
		}
	}
	for _, keep := range d.KeepPorts {
		if len(keep.Ports)+len(keep.Ranges) == 0 {
			continue // the renderer does not emit an empty scoped rule
		}
		scope := keep.Prefix.Masked()
		if keep.Addr.IsValid() {
			scope = hostPrefix(keep.Addr)
		}
		spans := c.scopes[scope]
		if spans.coversBounds(allTCP) {
			continue
		}
		for _, port := range keep.Ports {
			spans = append(spans, policy.PortRange{Lo: port, Hi: port})
		}
		c.scopes[scope] = append(spans, keep.Ranges...)
	}

	var depths [129]bool
	for scope, spans := range c.scopes {
		if scope.IsValid() {
			depths[scope.Bits()] = true
		}
		if len(spans) > 1 {
			c.scopes[scope] = mergeTCP(spans)
		}
	}
	for depth, used := range depths {
		if used {
			c.depths = append(c.depths, depth)
		}
	}
}

// spansAt unions only the matching ancestor scopes. Unrelated hosts and
// prefixes never participate in a DNS admission lookup.
func (c scopedEstablishedKeepCoverage) spansAt(addr netip.Addr) tcpSpans {
	ports := hostSpans{spans: slices.Clip(c.scopes[netip.Prefix{}])}
	if ports.spans.coversBounds(allTCP) {
		return allTCP
	}
	for _, depth := range c.depths {
		if depth > addr.BitLen() {
			break
		}
		if spans := c.scopes[netip.PrefixFrom(addr, depth).Masked()]; len(spans) != 0 {
			if spans.coversBounds(allTCP) {
				return allTCP
			}
			ports.add(spans)
		}
	}
	return ports.normalized()
}

// Derived exclusively from a confirmed guarded generation. Matching scopes
// are indexed once so dynamic answers do not scan unrelated policy rules.
type wholeDNSReadmission struct {
	denyWhole, denyTCP scopedEstablishedKeepCoverage
	keepWhole, keepTCP scopedEstablishedKeepCoverage
	lateWhole, lateTCP scopedEstablishedKeepCoverage
	spans              int
}

func newWholeDNSReadmission(p *policy.NetworkPolicy, d EstablishedDrops, opts Options) *wholeDNSReadmission {
	if !d.guardWholeDNS || p == nil {
		return nil
	}
	r := &wholeDNSReadmission{}
	r.compileDenies(p, d, opts)
	r.keepWhole.compile(EstablishedDrops{KeepAddrs: d.KeepAddrs, KeepPrefixes: d.KeepPrefixes})
	r.keepTCP.compile(EstablishedDrops{KeepPorts: d.KeepPorts})
	if !d.CatchAll {
		r.lateWhole.compile(EstablishedDrops{KeepAddrs: d.Addrs, KeepPrefixes: d.Prefixes})
		r.lateTCP.compile(EstablishedDrops{KeepPorts: d.PortDrops})
	}
	for _, keep := range d.wholeDNSKeeps {
		r.spans += keep.size()
	}
	return r
}

func (r *wholeDNSReadmission) compileDenies(p *policy.NetworkPolicy, d EstablishedDrops, opts Options) {
	_, _, denyV4, denyV6 := p.StaticIPSets()
	deniedPrefixes := slices.Clone(d.DenyPrefixes)
	for _, family := range [2][]string{denyV4, denyV6} {
		for _, target := range family {
			if prefix, err := parseAsPrefix(target); err == nil {
				deniedPrefixes = append(deniedPrefixes, prefix)
			}
		}
	}
	r.denyWhole.compile(EstablishedDrops{KeepAddrs: d.DenyAddrs, KeepPrefixes: deniedPrefixes})
	denied := slices.Clone(d.DenyPortDrops)
	portDeny, _ := p.PortScopedRules()
	for _, rule := range portDeny {
		target := EstablishedPortDrop{Ports: rule.Ports, Ranges: rule.Ranges}
		if rule.Target != "" {
			// Match the fresh rule's family and literal identity, not the
			// legacy withdrawal renderer's unmapped host representation.
			if addr, err := netip.ParseAddr(rule.Target); err == nil && addr.Zone() != "" {
				continue
			}
			prefix, err := parseAsPrefix(rule.Target)
			if err != nil || prefix.Addr().Is6() != rule.IsV6 {
				continue
			}
			target.Prefix = prefix
		}
		denied = append(denied, target)
	}
	if opts.BlockDoT {
		denied = append(denied, EstablishedPortDrop{Ports: []int{853}})
	}
	if opts.BlockDoH443 {
		if len(opts.DoHBlocklistV4)+len(opts.DoHBlocklistV6) == 0 {
			denied = append(denied, EstablishedPortDrop{Ports: []int{443}})
		}
		for _, family := range [2][]string{opts.DoHBlocklistV4, opts.DoHBlocklistV6} {
			for _, target := range family {
				if prefix, err := parseAsPrefix(target); err == nil {
					denied = append(denied, EstablishedPortDrop{Prefix: prefix, Ports: []int{443}})
				}
			}
		}
	}
	r.denyTCP.compile(EstablishedDrops{KeepPorts: denied})
}

// Only ports subject to a late drop and not already kept can need new history.
// Find that window before subtracting policy denies, which can be highly fragmented.
func (r *wholeDNSReadmission) uncovered(d EstablishedDrops, addr netip.Addr) wholeDNSEstablishedKeep {
	if addr.IsLoopback() || len(r.keepWhole.spansAt(addr)) != 0 {
		return wholeDNSEstablishedKeep{}
	}
	whole := d.CatchAll || len(r.lateWhole.spansAt(addr)) != 0
	ports := allTCP
	if !whole {
		ports = r.lateTCP.spansAt(addr)
	}
	prior := d.wholeDNSKeeps[addr]
	ports = ports.subtract(prior.ports)
	if len(ports) != 0 {
		ports = ports.subtract(r.keepTCP.spansAt(addr))
	}
	return wholeDNSEstablishedKeep{ports: ports, other: whole && !prior.other}
}

func (r *wholeDNSReadmission) admitted(addr netip.Addr, window wholeDNSEstablishedKeep) wholeDNSEstablishedKeep {
	if window.size() == 0 || len(r.denyWhole.spansAt(addr)) != 0 {
		return wholeDNSEstablishedKeep{}
	}
	if len(window.ports) != 0 {
		window.ports = window.ports.subtract(r.denyTCP.spansAt(addr))
	}
	return window
}

const maxWholeDNSHistoryEntries = maxResolvedDomains * maxDomainAddresses * 256

func checkWholeDNSSpanCount(spans int) error {
	if spans > maxWholeDNSHistoryEntries {
		return fmt.Errorf("too many whole DNS established exceptions (limit %d)", maxWholeDNSHistoryEntries)
	}
	return nil
}

type wholeDNSReadmissionPlan struct {
	added       map[netip.Addr]wholeDNSEstablishedKeep
	keeps       map[netip.Addr]wholeDNSEstablishedKeep
	hosts       int
	spans       int // Net size, including untouched hosts.
	stagedSpans int // Size of the replacement entries for hosts staged so far.
}

func (r *wholeDNSReadmission) plan(d EstablishedDrops, deadlines map[netip.Addr]time.Time, now time.Time, observedDomain bool) (wholeDNSReadmissionPlan, error) {
	plan := wholeDNSReadmissionPlan{hosts: len(d.wholeDNSKeeps), spans: r.spans}
	if !observedDomain {
		return plan, nil
	}
	for addr, deadline := range deadlines {
		if deadline.After(now) {
			added := r.admitted(addr, r.uncovered(d, addr))
			if err := plan.stage(d, addr, added); err != nil {
				return wholeDNSReadmissionPlan{}, err
			}
		}
	}
	if err := checkWholeDNSSpanCount(plan.spans); err != nil {
		return wholeDNSReadmissionPlan{}, err
	}
	return plan, nil
}

// The delta goes to nft; the merged history is published only after ACK.
func (plan *wholeDNSReadmissionPlan) stage(d EstablishedDrops, addr netip.Addr, added wholeDNSEstablishedKeep) error {
	if added.size() == 0 {
		return nil
	}
	prior, tracked := d.wholeDNSKeeps[addr]
	if !tracked {
		plan.hosts++
		if plan.hosts > maxResolvedDomains*maxDomainAddresses {
			return fmt.Errorf("too many whole DNS hosts with established exceptions (limit %d)", maxResolvedDomains*maxDomainAddresses)
		}
	}
	next := wholeDNSEstablishedKeep{ports: prior.ports.union(added.ports), other: prior.other || added.other}
	// Another host in this batch can coalesce old intervals and lower the
	// net total. Only the already-staged hosts are a lower bound mid-batch.
	size := next.size()
	plan.spans += size - prior.size()
	plan.stagedSpans += size
	if err := checkWholeDNSSpanCount(plan.stagedSpans); err != nil {
		return err
	}
	if plan.added == nil {
		plan.added = make(map[netip.Addr]wholeDNSEstablishedKeep)
		plan.keeps = make(map[netip.Addr]wholeDNSEstablishedKeep)
	}
	plan.added[addr], plan.keeps[addr] = added, next
	return nil
}
