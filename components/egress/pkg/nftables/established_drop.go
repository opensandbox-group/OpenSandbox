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

package nftables

import (
	"net/netip"
	"slices"
	"sort"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// defaultEstablishedCarryTTL is the Linux default for
// nf_conntrack_tcp_timeout_established (432000s), used when the sysctl cannot
// be read.
const defaultEstablishedCarryTTL = 5 * 24 * time.Hour

// EstablishedPortDrop is a destination:port teardown (or keep) target for
// TCP flows. Exactly one of Addr/Prefix is set for a scoped target; when both
// are unset the entry covers every destination (port-only rule). An explicit
// IPv4-mapped IPv6 target uses a /128 Prefix so rendering preserves its family.
type EstablishedPortDrop struct {
	Addr   netip.Addr
	Prefix netip.Prefix
	Ports  []int
	Ranges []policy.PortRange
}

// EstablishedDrops lists destinations whose established/related flows a
// policy change should tear down. The nft chain evaluates the buckets in this
// order, and MatchesFlow mirrors it:
//
//  1. infrastructure accepts (mark, loopback, DNS redirect, ::1, upstream proxy)
//  2. explicit denies and withdrawn-host ports still explicitly denied
//  3. DNS accepts (except TCP to guarded withdrawn hosts); then
//     withdrawn-host TCP ports denied by default
//  4. Keep*: accepted, then guarded host TCP drops, then raw static accepts
//  5. Addrs/Prefixes/PortDrops: allows the change removed — dropped
//  6. CatchAll ? blanket established drop : blanket established accept
type EstablishedDrops struct {
	DenyAddrs     []netip.Addr
	DenyPrefixes  []netip.Prefix
	DenyPortDrops []EstablishedPortDrop
	// Port-specific remnants of a whole-host withdrawal retain their
	// provenance even when an explicit deny turns into an implicit one.
	withdrawnHostDenyPorts portDenyUnion // Before DNS accepts.
	withdrawnHostPorts     portDenyUnion // After DNS, before static keeps.
	// A view of exact Addrs/Prefixes that still have a whole-host deny.
	// Their TCP withdrawals bypass DNS and raw static accepts, not keeps.
	guardedWithdrawals []netip.Addr

	Addrs     []netip.Addr
	Prefixes  []netip.Prefix
	PortDrops []EstablishedPortDrop
	CatchAll  bool

	KeepAddrs    []netip.Addr
	KeepPrefixes []netip.Prefix
	KeepPorts    []EstablishedPortDrop

	keepNonTCPAddrs []netip.Addr // Historical DNS accepts that never admitted TCP.
}

// Empty reports whether there is nothing to drop. Keep entries alone never
// change the ruleset.
func (d EstablishedDrops) Empty() bool {
	return len(d.DenyAddrs) == 0 && len(d.DenyPrefixes) == 0 && len(d.DenyPortDrops) == 0 &&
		len(d.withdrawnHostDenyPorts)+len(d.withdrawnHostPorts) == 0 &&
		len(d.Addrs) == 0 && len(d.Prefixes) == 0 && len(d.PortDrops) == 0 && !d.CatchAll
}

func (d EstablishedDrops) guardsWithdrawal(addr netip.Addr) bool {
	_, ok := slices.BinarySearchFunc(d.guardedWithdrawals, addr, netip.Addr.Compare)
	return ok
}

// Matches reports whether addr is covered by this drop set (port-agnostic).
// Port-scoped drops are ignored here; callers that know a dport should use
// MatchesFlow.
func (d EstablishedDrops) Matches(addr netip.Addr) bool {
	return d.MatchesFlow(addr, 0)
}

// MatchesFlow reports whether an ORIGINAL-direction TCP flow to addr[:dport]
// is torn down by the generated chain. dport 0 means "any/unknown port" and
// only matches portless address/prefix/catch-all targets. Loopback
// destinations are always accepted ahead of the teardown rules.
func (d EstablishedDrops) MatchesFlow(addr netip.Addr, dport int) bool {
	if !addr.IsValid() {
		return false
	}
	if addr.Unmap().IsLoopback() {
		return false
	}
	// Match destinations in the address family of the emitted rules.
	if targetsMatch(d.DenyAddrs, d.DenyPrefixes, d.DenyPortDrops, addr, dport) ||
		d.withdrawnHostDenyPorts.matchesHostPort(addr, dport) ||
		d.withdrawnHostPorts.matchesHostPort(addr, dport) {
		return true
	}
	if targetsMatch(d.KeepAddrs, d.KeepPrefixes, d.KeepPorts, addr, dport) {
		return false
	}
	if d.CatchAll {
		return true
	}
	return targetsMatch(d.Addrs, d.Prefixes, d.PortDrops, addr, dport)
}

// permitsHost evaluates the current deny/keep buckets, not older removals.
// A zero port evaluates only the portless policy.
func (d *EstablishedDrops) permitsHost(addr netip.Addr, port int, defaultAllow bool) bool {
	return !targetsMatch(d.DenyAddrs, d.DenyPrefixes, d.DenyPortDrops, addr, port) &&
		(defaultAllow || targetsMatch(d.KeepAddrs, d.KeepPrefixes, d.KeepPorts, addr, port))
}

func targetsMatch(addrs []netip.Addr, prefixes []netip.Prefix, ports []EstablishedPortDrop, addr netip.Addr, dport int) bool {
	for _, a := range addrs {
		if a.Unmap() == addr {
			return true
		}
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	if dport > 0 {
		for _, pd := range ports {
			if portDropCoversAddr(pd, addr) && portDropCoversPort(pd, dport) {
				return true
			}
		}
	}
	return false
}

func portDropCoversAddr(pd EstablishedPortDrop, addr netip.Addr) bool {
	if pd.Addr.IsValid() {
		return pd.Addr.Unmap() == addr
	}
	if pd.Prefix.IsValid() {
		return pd.Prefix.Contains(addr)
	}
	return true // port-only: every destination
}

func portDropCoversPort(pd EstablishedPortDrop, dport int) bool {
	for _, p := range pd.Ports {
		if p == dport {
			return true
		}
	}
	for _, r := range pd.Ranges {
		if r.Contains(dport) {
			return true
		}
	}
	return false
}

// SnapshotResolvedDomains returns DNS addresses still eligible for renewal.
func (m *Manager) SnapshotResolvedDomains() map[string][]netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	renewable, _ := m.snapshotResolvedDomainsLocked(time.Time{})
	return renewable
}

// A teardown snapshot uses all tracked history to identify revoked grants,
// but only live leases can keep a connection. A zero liveAt returns only
// the renewal view.
func (m *Manager) snapshotResolvedDomainsLocked(liveAt time.Time) (history, live map[string][]netip.Addr) {
	history = make(map[string][]netip.Addr, len(m.domains))
	teardown := !liveAt.IsZero()
	if teardown {
		live = make(map[string][]netip.Addr, len(m.domains))
	}
	for domain, entry := range m.domains {
		if entry == nil || len(entry.addresses) == 0 {
			continue
		}
		var addrs []netip.Addr
		for addr, lease := range entry.addresses {
			if teardown || lease.renewable {
				addrs = append(addrs, addr)
			}
			if teardown && lease.deadline().After(liveAt) {
				live[domain] = append(live[domain], addr)
			}
		}
		if len(addrs) == 0 {
			continue
		}
		if !teardown {
			sort.Slice(addrs, func(i, j int) bool { return addrs[i].Compare(addrs[j]) < 0 })
		}
		history[domain] = addrs
	}
	return history, live
}

// ComputeEstablishedDrops derives the teardown set for oldPol → newPol.
// priorDomains is the pre-apply DNS-learned map (from the manager snapshot).
//
// Deny* comes from newPol's explicit IP/CIDR/port deny rules. Addrs/Prefixes/
// PortDrops are removed allows (removed domains' learned IPs, removed IP/CIDR
// allows minus retained subnets, removed port-scoped allows). Keep* is always
// computed from what newPol still allows — static allows minus portless
// denies, surviving domain-lease IPs, and port-scoped allows (including
// port-only ones) — so a removed prefix can never cut a flow newPol permits.
func ComputeEstablishedDrops(oldPol, newPol *policy.NetworkPolicy, priorDomains map[string][]netip.Addr) EstablishedDrops {
	return computeEstablishedDrops(oldPol, newPol, priorDomains, priorDomains)
}

func computeEstablishedDrops(oldPol, newPol *policy.NetworkPolicy, priorDomains, liveDomains map[string][]netip.Addr) EstablishedDrops {
	if newPol == nil {
		newPol = policy.DefaultDenyPolicy()
	}

	var out EstablishedDrops
	// Explicit denies in newPol cut established flows even when the previous
	// policy was default-allow with no explicit allow entry. They are emitted
	// ahead of every keep so an overlapping allow cannot shield them.
	denyAddrs := newAddrSet()
	denyPrefixes := newPrefixSet()
	var allowPfx, denyPfx []netip.Prefix
	for _, r := range newPol.Egress {
		pfx := portlessTargetPrefix(r)
		if !pfx.IsValid() {
			continue
		}
		if r.Action == policy.ActionAllow {
			allowPfx = append(allowPfx, pfx)
			continue
		}
		if r.Action != policy.ActionDeny {
			continue
		}
		// Use the same address family when subtracting denies from keeps.
		denyPfx = append(denyPfx, pfx)
		if looksLikeIP(r) && !pfx.Addr().Is4In6() {
			denyAddrs.add(pfx.Addr())
		} else {
			denyPrefixes.add(pfx)
		}
	}
	out.DenyAddrs = denyAddrs.sorted()
	out.DenyPrefixes = denyPrefixes.sorted()
	portDeny, portAllow := newPol.PortScopedRules()
	for _, r := range portDeny {
		if pd, ok := portDropFromScoped(r); ok {
			out.DenyPortDrops = append(out.DenyPortDrops, pd)
		}
	}
	for _, r := range portAllow {
		if pd, ok := portDropFromScoped(r); ok {
			out.KeepPorts = append(out.KeepPorts, pd)
		}
	}
	defaultAllow := newPol.DefaultAction == policy.ActionAllow

	// IPs still covered by a surviving domain lease (and not explicitly denied).
	survivingDomainIPs := make(map[netip.Addr]struct{})
	for domain, ips := range liveDomains {
		if newPol.Evaluate(domain) != policy.ActionAllow {
			continue
		}
		for _, ip := range ips {
			if !ip.IsValid() {
				continue
			}
			ip = ip.Unmap()
			if targetsMatch(out.DenyAddrs, out.DenyPrefixes, nil, ip, 0) {
				continue
			}
			survivingDomainIPs[ip] = struct{}{}
		}
	}

	// Keeps: everything newPol still allows, deny-aware.
	keepAddrs := newAddrSet()
	keepPrefixes := newPrefixSet()
	for _, pfx := range allowPfx {
		for _, part := range prefixDiff(pfx, denyPfx) {
			if part.IsSingleIP() && !part.Addr().Is4In6() {
				keepAddrs.add(part.Addr())
			} else {
				keepPrefixes.add(part)
			}
		}
	}
	// During withdrawal planning this contains only static targets. Shared
	// DNS addresses are checked against survivingDomainIPs instead.
	out.KeepAddrs = keepAddrs.list
	out.KeepPrefixes = keepPrefixes.sorted()

	removedAddrs := newAddrSet()
	removedPrefixes := newPrefixSet()

	// Removed-domain candidates, excluding shared CDN IPs that a surviving
	// lease or an explicit IP/CIDR allow still permits.
	for domain, ips := range priorDomains {
		if newPol.Evaluate(domain) == policy.ActionAllow {
			continue
		}
		for _, ip := range ips {
			if !ip.IsValid() {
				continue
			}
			ip = ip.Unmap()
			if _, keep := survivingDomainIPs[ip]; keep {
				continue
			}
			if out.permitsHost(ip, 0, defaultAllow) {
				continue
			}
			removedAddrs.add(ip)
		}
	}

	if oldPol != nil {
		hasPortDeny := func(ip netip.Addr) bool {
			return slices.ContainsFunc(out.DenyPortDrops, func(pd EstablishedPortDrop) bool { return portDropCoversAddr(pd, ip) })
		}
		for _, r := range oldPol.Egress {
			if r.Action != policy.ActionAllow {
				continue
			}
			addr, _ := netip.ParseAddr(r.Target)
			var pfx netip.Prefix
			if addr.IsValid() {
				pfx = netip.PrefixFrom(addr, addr.BitLen())
			} else {
				pfx = parseRulePrefix(r).Masked()
			}
			if !pfx.IsValid() {
				// Domain grants have their own history. Removing a port-only
				// allow has no bounded destination and is left to conntrack.
				continue
			}
			ip := pfx.Addr()
			hasPorts := len(r.Ports) > 0 || len(r.PortRanges) > 0
			switch {
			case hasPorts && pfx.IsSingleIP():
				_, lease := survivingDomainIPs[ip]
				if lease && !hasPortDeny(ip) {
					continue
				}
				// Match the same typed targets we will emit for this host.
				ports, ranges := portsNoLongerAllowed(newPol, r.Ports, r.PortRanges, func(port int) bool {
					return out.permitsHost(ip, port, defaultAllow || lease)
				})
				if len(ports)+len(ranges) == 0 {
					continue
				}
				drop := EstablishedPortDrop{Prefix: pfx, Ports: ports, Ranges: ranges}
				if addr.IsValid() && !addr.Is4In6() {
					drop.Addr, drop.Prefix = addr, netip.Prefix{}
				}
				out.PortDrops = append(out.PortDrops, drop)
			case hasPorts:
				if portsFullyAllowedOnPrefix(newPol, pfx, r.Ports, r.PortRanges) {
					continue
				}
				out.PortDrops = append(out.PortDrops, EstablishedPortDrop{
					Prefix: pfx,
					Ports:  slices.Clone(r.Ports),
					Ranges: slices.Clone(r.PortRanges),
				})
			case pfx.IsSingleIP():
				if _, lease := survivingDomainIPs[ip]; lease {
					// Keep withdrawal history only if an explicit TCP deny
					// actually overrides this lease. The keep still shields
					// the other ports in the installed chain.
					if !hasPortDeny(ip) {
						continue
					}
				} else if out.permitsHost(ip, 0, defaultAllow) {
					continue
				}
				if addr.IsValid() && !addr.Is4In6() {
					removedAddrs.add(addr)
				} else {
					removedPrefixes.add(pfx)
				}
			default:
				for _, removed := range removedPrefixParts(newPol, pfx) {
					removedPrefixes.add(removed)
				}
			}
		}
	}

	out.Addrs = removedAddrs.sorted()
	out.Prefixes = removedPrefixes.sorted()
	for ip := range survivingDomainIPs {
		keepAddrs.add(ip)
	}
	out.KeepAddrs = keepAddrs.sorted()

	out.CatchAll = oldPol != nil &&
		oldPol.DefaultAction == policy.ActionAllow &&
		newPol.DefaultAction != policy.ActionAllow

	return out
}

// stillRelevant returns the part of a previously applied teardown set that
// must be re-emitted when p is applied without dropEstablished, so the next
// full ruleset rebuild does not silently reopen flows a dropEstablished commit
// closed. Destinations p allows again stop being dropped, and keeps accumulate
// (p's allows plus the currently learned domain IPs) so a destination that was
// allowed at any point after the commit is never torn down by a later plain
// apply the caller did not opt in to.
func (d EstablishedDrops) stillRelevant(p *policy.NetworkPolicy, learned []netip.Addr, retained map[netip.Addr]time.Time, hadAccept func(netip.Addr, bool) bool) EstablishedDrops {
	if d.Empty() {
		return EstablishedDrops{}
	}
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	var out EstablishedDrops

	// Deny bucket: only what p still explicitly denies (it precedes keeps).
	var curDeny []netip.Prefix
	for _, r := range p.Egress {
		if pfx := portlessDenyPrefix(r); pfx.IsValid() {
			curDeny = append(curDeny, pfx)
		}
	}
	denyAddrs := newAddrSet()
	for _, a := range d.DenyAddrs {
		if slices.ContainsFunc(curDeny, func(pfx netip.Prefix) bool { return pfx.Contains(a) }) {
			denyAddrs.add(a)
		}
	}
	denyPrefixes := newPrefixSet()
	for _, old := range d.DenyPrefixes {
		for _, cur := range curDeny {
			if !old.Overlaps(cur) {
				continue
			}
			if old.Bits() >= cur.Bits() {
				denyPrefixes.add(old)
			} else {
				denyPrefixes.add(cur)
			}
		}
	}
	portDeny, portAllow := p.PortScopedRules()
	out.DenyAddrs = denyAddrs.sorted()
	out.DenyPrefixes = denyPrefixes.sorted()
	defaultAllow := p.DefaultAction == policy.ActionAllow
	var fresh EstablishedDrops
	builtFresh := false
	policyTargets := func() *EstablishedDrops {
		if !builtFresh {
			fresh = ComputeEstablishedDrops(nil, p, nil)
			builtFresh = true
		}
		return &fresh
	}
	// Record exact withdrawals whose raw host drop may be removed or
	// overshadowed by accepts.
	var whole map[netip.Addr]hostWithdrawal

	mayShadowHost := len(portAllow)+len(learned)+len(retained)+len(d.KeepAddrs)+len(d.KeepPrefixes)+len(d.KeepPorts) != 0
	retainHost := func(addr netip.Addr) bool {
		current := policyTargets()
		keep := !current.permitsHost(addr, 0, defaultAllow)
		explicitDeny := keep && targetsMatch(current.DenyAddrs, current.DenyPrefixes, nil, addr, 0)
		if (keep && (mayShadowHost || explicitDeny)) || (!keep && len(portDeny) != 0) {
			if whole == nil {
				whole = make(map[netip.Addr]hostWithdrawal)
			}
			whole[addr] = hostWithdrawal{keepRule: keep, explicitDeny: explicitDeny}
		}
		return keep
	}

	// Removed bucket: drop entries p fully allows again; partial re-allows
	// are shielded by the recomputed keeps.
	removedAddrs := newAddrSet()
	for _, a := range d.Addrs {
		if retainHost(a) {
			removedAddrs.add(a)
		}
	}
	removedPrefixes := newPrefixSet()
	for _, pfx := range d.Prefixes {
		if pfx.IsSingleIP() {
			if retainHost(pfx.Addr()) {
				removedPrefixes.add(pfx)
			}
			continue
		}
		for _, part := range removedPrefixParts(p, pfx) {
			removedPrefixes.add(part)
		}
	}
	var scopedWithdrawals portDenyUnion
	for _, pd := range d.PortDrops {
		if target := portDenyTarget(pd); target.IsSingleIP() {
			if scopedWithdrawals == nil {
				scopedWithdrawals = make(portDenyUnion)
			}
			scopedWithdrawals.add(target, pd.Ports, pd.Ranges)
		} else if pd.Prefix.IsValid() {
			if !portsFullyAllowedOnPrefix(p, pd.Prefix, pd.Ports, pd.Ranges) {
				out.PortDrops = append(out.PortDrops, pd)
			}
		}
	}
	out.DenyPortDrops = carryPortDenies(d, portDeny, curDeny, &out)
	carryWithdrawals := len(whole)+len(scopedWithdrawals)+len(d.withdrawnHostDenyPorts)+len(d.withdrawnHostPorts) != 0
	out.Addrs = removedAddrs.sorted()
	out.Prefixes = removedPrefixes.sorted()
	out.CatchAll = d.CatchAll && p.DefaultAction != policy.ActionAllow

	if out.Empty() && !carryWithdrawals {
		return EstablishedDrops{}
	}

	// Keeps accumulate: previous keeps ∪ p's allows ∪ learned domain IPs.
	policyTargets()
	keepAddrs := newAddrSet()
	for _, a := range d.KeepAddrs {
		keepAddrs.add(a)
	}
	for _, a := range fresh.KeepAddrs {
		keepAddrs.add(a)
	}
	out.keepNonTCPAddrs = d.keepNonTCPAddrs
	var nonTCPKeeps *addrSet
	for _, a := range learned {
		if !d.guardsWithdrawal(a) {
			keepAddrs.add(a)
			continue
		}
		// The guarded DNS accept matched only non-TCP packets. Preserve
		// that exemption without promoting it to a whole-host keep.
		if _, kept := slices.BinarySearchFunc(d.keepNonTCPAddrs, a, netip.Addr.Compare); kept {
			continue
		}
		if nonTCPKeeps == nil {
			nonTCPKeeps = newAddrSet()
			for _, old := range d.keepNonTCPAddrs {
				nonTCPKeeps.add(old)
			}
		}
		nonTCPKeeps.add(a)
	}
	if nonTCPKeeps != nil {
		out.keepNonTCPAddrs = nonTCPKeeps.sorted()
	}
	keepPrefixes := newPrefixSet()
	for _, pfx := range d.KeepPrefixes {
		keepPrefixes.add(pfx)
	}
	for _, pfx := range fresh.KeepPrefixes {
		keepPrefixes.add(pfx)
	}
	out.KeepAddrs = keepAddrs.sorted()
	out.KeepPrefixes = keepPrefixes.sorted()
	out.KeepPorts = append([]EstablishedPortDrop(nil), d.KeepPorts...)
	for _, cur := range fresh.KeepPorts {
		dup := false
		for _, k := range out.KeepPorts {
			if samePortDrop(k, cur) {
				dup = true
				break
			}
		}
		if !dup {
			out.KeepPorts = append(out.KeepPorts, cur)
		}
	}
	if carryWithdrawals {
		out.withdrawnHostDenyPorts, out.withdrawnHostPorts = d.carryWithdrawnHostPorts(defaultAllow, &fresh, &out, whole, scopedWithdrawals, retained, hadAccept)
		if out.Empty() {
			return EstablishedDrops{}
		}
	}
	return out
}

// carryPortDenies intersects old and current explicit denies, retaining only
// TCP teardowns here. Whole-address intersections are carried separately.
func carryPortDenies(d EstablishedDrops, rules []policy.PortScopedRule, whole []netip.Prefix, retained *EstablishedDrops) []EstablishedPortDrop {
	if len(d.DenyAddrs)+len(d.DenyPrefixes)+len(d.DenyPortDrops) == 0 {
		return nil
	}
	// Union each side before intersecting destinations, so overlapping rules
	// for one destination do not multiply the work or the resulting rules.
	before := make(portDenyUnion)
	for _, old := range d.DenyPortDrops {
		target := portDenyTarget(old)
		if !retained.coversWholeDeny(target) {
			before.add(target, old.Ports, old.Ranges)
		}
	}
	after := make(portDenyUnion)
	for _, r := range rules {
		if cur, ok := parsePortDropTarget(r); ok {
			target := portDenyTarget(cur)
			if !retained.coversWholeDeny(target) {
				after.add(target, r.Ports, r.Ranges)
			}
		}
	}
	before.normalize()
	after.normalize()

	carried := make(portDenyUnion)
	// Factor out the global intersection before expanding per-target rules.
	// Both sides can borrow these spans; subtraction never mutates them.
	var global netip.Prefix
	if spans := intersectPortSpans(before[global].spans, after[global].spans); len(spans) != 0 {
		carried.add(global, nil, spans)
		before.subtractSpans(spans)
		after.subtractSpans(spans)
	}
	for old, ports := range before {
		for _, pfx := range whole {
			if target, ok := intersectPortDenyTargets(old, pfx); ok && !retained.coversWholeDeny(target) {
				carried.add(target, nil, ports.spans)
			}
		}
		for cur, denied := range after {
			if old == global && cur == global {
				continue // Already intersected above.
			}
			if target, ok := intersectPortDenyTargets(old, cur); ok && !retained.coversWholeDeny(target) {
				carried.add(target, nil, intersectPortSpans(ports.spans, denied.spans))
			}
		}
	}
	for cur, ports := range after {
		// Previously whole-address teardowns cover every port, but only
		// inside their original destination.
		for _, addr := range d.DenyAddrs {
			if target, ok := intersectPortDenyTargets(netip.PrefixFrom(addr, addr.BitLen()), cur); ok && !retained.coversWholeDeny(target) {
				carried.add(target, nil, ports.spans)
			}
		}
		for _, pfx := range d.DenyPrefixes {
			if target, ok := intersectPortDenyTargets(pfx, cur); ok && !retained.coversWholeDeny(target) {
				carried.add(target, nil, ports.spans)
			}
		}
	}
	carried.normalize()
	return carried.drops(nil)
}

type hostWithdrawal struct {
	keepRule     bool
	explicitDeny bool
}

func (d EstablishedDrops) carryWithdrawnHostPorts(defaultAllow bool, current, next *EstablishedDrops, whole map[netip.Addr]hostWithdrawal, scoped portDenyUnion, retained map[netip.Addr]time.Time, hadAccept func(netip.Addr, bool) bool) (explicit, implicit portDenyUnion) {
	denies := normalizedPortTargets(current.DenyPortDrops)
	early, late := make(portDenyUnion), make(portDenyUnion)
	oldKeeps := normalizedPortTargets(d.KeepPorts)
	var oldDenies portDenyUnion
	rawAccept := func(addr netip.Addr, includeStatic bool) bool {
		return hadAccept != nil && !d.guardsWithdrawal(addr) && hadAccept(addr, includeStatic)
	}
	wasHostKept := func(addr netip.Addr) bool {
		if targetsMatch(d.KeepAddrs, d.KeepPrefixes, nil, addr, 0) {
			return true
		}
		return rawAccept(addr, true)
	}
	addPreviouslyBlocked := func(out portDenyUnion, host netip.Prefix, candidate []policy.PortRange) {
		addr := host.Addr()
		// These exact-port withdrawals are already inputs to the next
		// verdict. Do not reconstruct their spans from the raw host drop.
		knownEarly := d.withdrawnHostDenyPorts[host].spans
		knownLate := d.withdrawnHostPorts[host].spans
		if len(knownLate) != 0 && rawAccept(addr, false) {
			knownLate = nil
		}
		addUncovered := func(spans []policy.PortRange) {
			spans, _ = subtractPortSpans(spans, knownEarly)
			spans, _ = subtractPortSpans(spans, knownLate)
			out.add(host, nil, spans)
		}
		if targetsMatch(d.DenyAddrs, d.DenyPrefixes, nil, addr, 0) {
			addUncovered(candidate)
			return
		}
		spans := candidate
		exempted := false
		if wasHostKept(addr) {
			spans, exempted = nil, true
		} else {
			for target, keep := range oldKeeps {
				if !target.IsValid() || target.Contains(addr) {
					var changed bool
					spans, changed = subtractPortSpans(spans, keep.spans)
					exempted = exempted || changed
				}
			}
		}
		addUncovered(spans)
		if !exempted {
			return
		}
		// Only the formerly exempted part needs rechecking. A higher-priority
		// drop made these accepts ineffective in the previous rule chain.
		masked, _ := subtractPortSpans(candidate, spans)
		if oldDenies == nil {
			oldDenies = normalizedPortTargets(d.DenyPortDrops)
		}
		restore := func(denied []policy.PortRange, moreScoped bool) {
			if portSpansCover(knownEarly, denied) || portSpansCover(knownLate, denied) {
				return
			}
			addUncovered(intersectPortSpans(masked, denied))
			if moreScoped {
				masked, _ = subtractPortSpans(masked, denied)
			}
		}
		// Normalized scoped targets exclude global coverage. Neither the
		// global nor the last matching scoped target needs a remainder.
		restore(oldDenies[netip.Prefix{}].spans, false)
		var pending []policy.PortRange
		for target, deny := range oldDenies {
			if target.Contains(addr) {
				if len(pending) != 0 {
					restore(pending, true)
				}
				pending = deny.spans
			}
		}
		restore(pending, false)
	}
	left := make(portDenyUnion)
	var carriedKeeps portDenyUnion
	var allPorts []policy.PortRange
	for addr, withdrawal := range whole {
		host := netip.PrefixFrom(addr, addr.BitLen())
		if !withdrawal.keepRule {
			for target, deny := range denies {
				if !target.IsValid() || target.Contains(addr) {
					addPreviouslyBlocked(early, host, deny.spans)
				}
			}
			continue
		}

		// The raw host drop covers the ports that no earlier accept can
		// shadow. Only the shadowed ports need a separate verdict.
		if next.coversWholeDeny(host) {
			continue
		}
		_, kept := slices.BinarySearchFunc(next.KeepAddrs, addr, netip.Addr.Compare)
		_, live := retained[addr]
		needsProjection := live || kept || withdrawal.explicitDeny || targetsMatch(nil, next.KeepPrefixes, nil, addr, 0)
		if withdrawal.explicitDeny && !kept && !wasHostKept(addr) {
			// Keep the scoped exemptions in rule form. Do not expand their
			// complement for every host, even if DNS currently grants it.
			next.guardedWithdrawals = append(next.guardedWithdrawals, addr)
			needsProjection = false
		}
		if needsProjection {
			if allPorts == nil {
				allPorts = []policy.PortRange{{Lo: 1, Hi: 65535}}
			}
			addPreviouslyBlocked(left, host, allPorts)
			continue
		}
		if carriedKeeps == nil {
			carriedKeeps = normalizedPortTargets(next.KeepPorts)
		}
		for target, allowed := range carriedKeeps {
			if !target.IsValid() || target.Contains(addr) {
				addPreviouslyBlocked(left, host, allowed.spans)
			}
		}
	}
	scoped.normalize()
	for host, group := range scoped {
		addPreviouslyBlocked(left, host, group.spans)
	}
	for i, previous := range [2]portDenyUnion{d.withdrawnHostDenyPorts, d.withdrawnHostPorts} {
		for host, group := range previous {
			addr := host.Addr()
			// Only the dynamic accept preceded a previously implicit drop.
			if i == 1 && rawAccept(addr, false) {
				continue
			}
			left.add(host, nil, group.spans)
		}
	}
	left.normalize()
	var currentKeeps portDenyUnion
	for host, group := range left {
		addr, spans := host.Addr(), group.spans
		if targetsMatch(current.DenyAddrs, current.DenyPrefixes, nil, addr, 0) {
			early.add(host, nil, spans)
			continue
		}
		for target, deny := range denies {
			if !target.IsValid() || target.Contains(addr) {
				early.add(host, nil, intersectPortSpans(spans, deny.spans))
				spans, _ = subtractPortSpans(spans, deny.spans)
			}
		}
		// Explicit denies still win. The lease reallows only the rest in
		// the new ruleset, so don't retain a drop that could revive at expiry.
		if _, learned := retained[addr]; learned || len(spans) == 0 || current.permitsHost(addr, 0, defaultAllow) {
			continue
		}
		if currentKeeps == nil {
			currentKeeps = normalizedPortTargets(current.KeepPorts)
		}
		for target, keep := range currentKeeps {
			if !target.IsValid() || target.Contains(addr) {
				spans, _ = subtractPortSpans(spans, keep.spans)
			}
		}
		// Implicit denial must not outrank a future live DNS reallow.
		late.add(host, nil, spans)
	}
	early.normalize()
	late.normalize()
	slices.SortFunc(next.guardedWithdrawals, netip.Addr.Compare)
	return early, late
}

func normalizedPortTargets(rules []EstablishedPortDrop) portDenyUnion {
	targets := make(portDenyUnion)
	for _, rule := range rules {
		targets.add(portDenyTarget(rule), rule.Ports, rule.Ranges)
	}
	targets.normalize()
	// Factor global coverage out of scoped contributors before expanding
	// them over matching hosts. Keep the original global contribution.
	var global netip.Prefix
	if floor := targets[global]; len(floor.spans) != 0 && len(targets) > 1 {
		delete(targets, global)
		targets.subtractSpans(floor.spans)
		targets[global] = floor
	}
	return targets
}

func (d EstablishedDrops) withdrawnHostDenyRules() []EstablishedPortDrop {
	if len(d.withdrawnHostDenyPorts) == 0 {
		return nil
	}
	for _, floor := range d.DenyPortDrops {
		if !portDenyTarget(floor).IsValid() {
			// Keep provenance even when the carried floor already drops all
			// these ports. Share its coverage only for this rendering.
			if len(floor.Ports) == 0 {
				return d.withdrawnHostDenyPorts.drops(floor.Ranges)
			}
			u := make(portDenyUnion, 1)
			u.add(netip.Prefix{}, floor.Ports, floor.Ranges)
			u.normalize()
			return d.withdrawnHostDenyPorts.drops(u[netip.Prefix{}].spans)
		}
	}
	return d.withdrawnHostDenyPorts.drops(nil)
}

func (u portDenyUnion) matchesHostPort(addr netip.Addr, port int) bool {
	return port > 0 && len(u) != 0 &&
		portSpansCover(u[netip.PrefixFrom(addr, addr.BitLen())].spans, []policy.PortRange{{Lo: port, Hi: port}})
}

// Only a whole-address teardown carried from the previous policy can
// subsume a TCP teardown, not a new address deny.
func (d *EstablishedDrops) coversWholeDeny(target netip.Prefix) bool {
	if !target.IsValid() {
		return false // a port-only rule spans both address families
	}
	if target.IsSingleIP() {
		if _, found := slices.BinarySearchFunc(d.DenyAddrs, target.Addr(), netip.Addr.Compare); found {
			return true
		}
	}
	return slices.ContainsFunc(d.DenyPrefixes, func(pfx netip.Prefix) bool {
		return pfx.Bits() <= target.Bits() && pfx.Contains(target.Addr())
	})
}

// A host and its full-length prefix share a key; the invalid prefix denotes
// a port-only rule. Native IPv4 and IPv4-mapped IPv6 have different keys.
func portDenyTarget(drop EstablishedPortDrop) netip.Prefix {
	if drop.Addr.IsValid() {
		addr := drop.Addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen())
	}
	return drop.Prefix
}

func intersectPortDenyTargets(a, b netip.Prefix) (netip.Prefix, bool) {
	switch {
	case !a.IsValid():
		return b, true
	case !b.IsValid():
		return a, true
	case !a.Overlaps(b):
		return netip.Prefix{}, false
	case a.Bits() >= b.Bits():
		return a, true
	default:
		return b, true
	}
}

type portDenyUnion map[netip.Prefix]struct {
	spans []policy.PortRange
	owned bool
}

func (u portDenyUnion) add(target netip.Prefix, ports []int, ranges []policy.PortRange) {
	if len(ports)+len(ranges) == 0 {
		return
	}
	group := u[target]
	switch {
	case len(group.spans) == 0 && len(ports) == 0:
		group.spans = ranges
	case !group.owned:
		spans := make([]policy.PortRange, 0, len(group.spans)+len(ports)+len(ranges))
		group.spans = append(append(spans, group.spans...), ranges...)
		group.owned = true
	default:
		group.spans = append(group.spans, ranges...)
	}
	for _, port := range ports {
		group.spans = append(group.spans, policy.PortRange{Lo: port, Hi: port})
	}
	u[target] = group
}

// Ports become singleton intervals, so overlap and coverage use one sorted,
// disjoint representation. An untouched normalized floor needs no sorting.
func (u portDenyUnion) normalize() {
	for target, group := range u {
		spans := group.spans
		for i := 1; i < len(spans); i++ {
			if spans[i].Lo > spans[i-1].Hi+1 {
				continue
			}
			if !group.owned {
				spans = slices.Clone(spans)
			}
			slices.SortFunc(spans, func(a, b policy.PortRange) int { return a.Lo - b.Lo })
			n := 0
			for _, span := range spans {
				if n != 0 && span.Lo <= spans[n-1].Hi+1 {
					spans[n-1].Hi = max(spans[n-1].Hi, span.Hi)
				} else {
					spans[n] = span
					n++
				}
			}
			group.spans, group.owned = spans[:n], true
			u[target] = group
			break
		}
	}
}

func intersectPortSpans(a, b []policy.PortRange) []policy.PortRange {
	if slices.Equal(a, b) {
		return a
	}
	// The inputs are normalized. Borrow a covered union rather than
	// copying its intervals when intersecting with a covering span.
	if len(a) == 1 && len(b) != 0 && a[0].Lo <= b[0].Lo && a[0].Hi >= b[len(b)-1].Hi {
		return b
	}
	if len(b) == 1 && len(a) != 0 && b[0].Lo <= a[0].Lo && b[0].Hi >= a[len(a)-1].Hi {
		return a
	}
	var out []policy.PortRange
	for len(a) != 0 && len(b) != 0 {
		if lo, hi := max(a[0].Lo, b[0].Lo), min(a[0].Hi, b[0].Hi); lo <= hi {
			out = append(out, policy.PortRange{Lo: lo, Hi: hi})
		}
		switch {
		case a[0].Hi < b[0].Hi:
			a = a[1:]
		case a[0].Hi > b[0].Hi:
			b = b[1:]
		default:
			a, b = a[1:], b[1:]
		}
	}
	return out
}

func (u portDenyUnion) subtractSpans(covered []policy.PortRange) {
	for target, group := range u {
		spans, changed := subtractPortSpans(group.spans, covered)
		if !changed {
			continue
		}
		if len(spans) == 0 {
			delete(u, target)
		} else {
			group.spans, group.owned = spans, true
			u[target] = group
		}
	}
}

// Both inputs are sorted and disjoint. Borrow the original slice when no
// span changes, and allocate only for uncovered output.
func subtractPortSpans(spans, covered []policy.PortRange) (out []policy.PortRange, changed bool) {
	for i, span := range spans {
		for len(covered) != 0 && covered[0].Hi < span.Lo {
			covered = covered[1:]
		}
		if len(covered) == 0 {
			if !changed {
				return spans, false
			}
			return append(out, spans[i:]...), true
		}
		if covered[0].Lo > span.Hi {
			if changed {
				out = append(out, span)
			}
			continue
		}
		if !changed {
			out = append(out, spans[:i]...)
			changed = true
		}
		lo := span.Lo
		for len(covered) != 0 && covered[0].Lo <= span.Hi {
			cut := covered[0]
			if cut.Lo > lo {
				out = append(out, policy.PortRange{Lo: lo, Hi: cut.Lo - 1})
			}
			lo = cut.Hi + 1
			if lo > span.Hi {
				break
			}
			covered = covered[1:]
		}
		if lo <= span.Hi {
			out = append(out, policy.PortRange{Lo: lo, Hi: span.Hi})
		}
	}
	if !changed {
		return spans, false
	}
	return out, true
}

func portSpansCover(outer, inner []policy.PortRange) bool {
	for _, span := range inner {
		for len(outer) != 0 && outer[0].Hi < span.Lo {
			outer = outer[1:]
		}
		if len(outer) == 0 || outer[0].Lo > span.Lo || outer[0].Hi < span.Hi {
			return false
		}
	}
	return true
}

// drops materializes sorted rules after normalization; covered global spans
// can be omitted without copying or changing the interval union.
func (u portDenyUnion) drops(covered []policy.PortRange) []EstablishedPortDrop {
	var out []EstablishedPortDrop
	for target, group := range u {
		if target.IsValid() && len(u) > 1 && u.coveredByAncestorPrefix(target, group.spans) {
			continue
		}
		spans, trimmed := subtractPortSpans(group.spans, covered)
		if len(spans) == 0 {
			continue
		}
		if out == nil {
			out = make([]EstablishedPortDrop, 0, len(u))
		}
		drop := EstablishedPortDrop{Prefix: target}
		if target.IsSingleIP() && !target.Addr().Is4In6() {
			drop.Addr, drop.Prefix = target.Addr(), netip.Prefix{}
		}
		singles := 0
		for _, span := range spans {
			if span.Lo == span.Hi {
				singles++
			}
		}
		if singles == 0 {
			drop.Ranges = spans
			if !trimmed && !group.owned {
				// A borrowed slice may belong to the incoming policy.
				drop.Ranges = slices.Clone(drop.Ranges)
			}
		} else {
			drop.Ports = make([]int, 0, singles)
			if singles < len(spans) {
				drop.Ranges = make([]policy.PortRange, 0, len(spans)-singles)
			}
			for _, span := range spans {
				if span.Lo == span.Hi {
					drop.Ports = append(drop.Ports, span.Lo)
				} else {
					drop.Ranges = append(drop.Ranges, span)
				}
			}
		}
		out = append(out, drop)
	}
	slices.SortFunc(out, func(a, b EstablishedPortDrop) int {
		x, y := portDenyTarget(a), portDenyTarget(b)
		if cmp := x.Addr().Compare(y.Addr()); cmp != 0 {
			return cmp
		}
		return x.Bits() - y.Bits()
	})
	return out
}

func (u portDenyUnion) coveredByAncestorPrefix(target netip.Prefix, spans []policy.PortRange) bool {
	for bits := target.Bits() - 1; bits >= 0; bits-- {
		pfx := netip.PrefixFrom(target.Addr(), bits).Masked()
		if portSpansCover(u[pfx].spans, spans) {
			return true
		}
	}
	return false
}

func portDropFromScoped(r policy.PortScopedRule) (EstablishedPortDrop, bool) {
	pd, ok := parsePortDropTarget(r)
	if !ok {
		return EstablishedPortDrop{}, false
	}
	pd.Ports = slices.Clone(r.Ports)
	pd.Ranges = slices.Clone(r.Ranges)
	return pd, true
}

func parsePortDropTarget(r policy.PortScopedRule) (EstablishedPortDrop, bool) {
	var pd EstablishedPortDrop
	if len(r.Ports)+len(r.Ranges) == 0 {
		return pd, false
	}
	if r.Target == "" {
		return pd, true
	}
	if ip, err := netip.ParseAddr(r.Target); err == nil {
		if ip.Is4In6() {
			pd.Prefix = netip.PrefixFrom(ip, ip.BitLen())
		} else {
			pd.Addr = ip
		}
		return pd, true
	}
	if pfx, err := netip.ParsePrefix(r.Target); err == nil {
		pd.Prefix = pfx.Masked()
		return pd, true
	}
	return pd, false
}

func samePortDrop(a, b EstablishedPortDrop) bool {
	if a.Addr != b.Addr || a.Prefix != b.Prefix || len(a.Ports) != len(b.Ports) || len(a.Ranges) != len(b.Ranges) {
		return false
	}
	for i := range a.Ports {
		if a.Ports[i] != b.Ports[i] {
			return false
		}
	}
	for i := range a.Ranges {
		if a.Ranges[i] != b.Ranges[i] {
			return false
		}
	}
	return true
}

// ipExplicitlyDenied reports whether a portless IP/CIDR deny rule covers ip.
func ipExplicitlyDenied(p *policy.NetworkPolicy, ip netip.Addr) bool {
	if p == nil || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, pfx := range portlessDenyPrefixes(p) {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

func portlessDenyPrefix(r policy.EgressRule) netip.Prefix {
	if r.Action != policy.ActionDeny {
		return netip.Prefix{}
	}
	return portlessTargetPrefix(r)
}

// Preserve the family written by StaticIPSets, including IPv4-mapped IPv6.
func portlessTargetPrefix(r policy.EgressRule) netip.Prefix {
	if len(r.Ports)+len(r.PortRanges) != 0 {
		return netip.Prefix{}
	}
	if addr, err := netip.ParseAddr(r.Target); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen())
	}
	return parseRulePrefix(r).Masked()
}

// portlessAllowPrefixes describes the raw static accepts, before removing
// the sections covered by denies when computing keep rules.
func portlessAllowPrefixes(p *policy.NetworkPolicy) []netip.Prefix {
	var allows []netip.Prefix
	for _, rule := range p.Egress {
		if rule.Action == policy.ActionAllow {
			if pfx := portlessTargetPrefix(rule); pfx.IsValid() {
				allows = append(allows, pfx)
			}
		}
	}
	return allows
}

// portlessDenyPrefixes retains the legacy host normalization used by
// removed-prefix planning. Explicit denies use portlessDenyPrefix.
func portlessDenyPrefixes(p *policy.NetworkPolicy) []netip.Prefix {
	if p == nil {
		return nil
	}
	var out []netip.Prefix
	for _, r := range p.Egress {
		if r.Action != policy.ActionDeny || len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			continue
		}
		if looksLikeIP(r) {
			if pfx := hostPrefix(parseRuleIP(r)); pfx.IsValid() {
				out = append(out, pfx)
			}
		} else if looksLikeCIDR(r) {
			if pfx := parseRulePrefix(r); pfx.IsValid() {
				out = append(out, pfx.Masked())
			}
		}
	}
	return out
}

func hostPrefix(ip netip.Addr) netip.Prefix {
	if !ip.IsValid() {
		return netip.Prefix{}
	}
	ip = ip.Unmap()
	p, err := ip.Prefix(ip.BitLen())
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

type addrSet struct {
	seen map[netip.Addr]struct{}
	list []netip.Addr
}

func newAddrSet() *addrSet { return &addrSet{seen: make(map[netip.Addr]struct{})} }

func (s *addrSet) add(a netip.Addr) {
	if !a.IsValid() {
		return
	}
	a = a.Unmap()
	if _, ok := s.seen[a]; ok {
		return
	}
	s.seen[a] = struct{}{}
	s.list = append(s.list, a)
}

func (s *addrSet) sorted() []netip.Addr {
	sort.Slice(s.list, func(i, j int) bool { return s.list[i].Compare(s.list[j]) < 0 })
	return s.list
}

type prefixSet struct {
	seen map[netip.Prefix]struct{}
	list []netip.Prefix
}

func newPrefixSet() *prefixSet { return &prefixSet{seen: make(map[netip.Prefix]struct{})} }

func (s *prefixSet) add(p netip.Prefix) {
	if !p.IsValid() {
		return
	}
	p = p.Masked()
	if _, ok := s.seen[p]; ok {
		return
	}
	s.seen[p] = struct{}{}
	s.list = append(s.list, p)
}

func (s *prefixSet) sorted() []netip.Prefix {
	sort.Slice(s.list, func(i, j int) bool { return s.list[i].String() < s.list[j].String() })
	return s.list
}

func looksLikeIP(r policy.EgressRule) bool {
	_, err := netip.ParseAddr(r.Target)
	return err == nil
}

func looksLikeCIDR(r policy.EgressRule) bool {
	_, err := netip.ParsePrefix(r.Target)
	return err == nil
}

func parseRuleIP(r policy.EgressRule) netip.Addr {
	ip, err := netip.ParseAddr(r.Target)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

func parseRulePrefix(r policy.EgressRule) netip.Prefix {
	pfx, err := netip.ParsePrefix(r.Target)
	if err != nil {
		return netip.Prefix{}
	}
	return pfx
}

// removedPrefixParts returns the portions of oldPfx that newPol no longer
// allows portlessly. When a retained allow covers only a subnet, the
// complementary prefixes are returned instead of the whole old range.
func removedPrefixParts(newPol *policy.NetworkPolicy, oldPfx netip.Prefix) []netip.Prefix {
	if !oldPfx.IsValid() {
		return nil
	}
	oldPfx = oldPfx.Masked()
	// Under a default-allow new policy only the explicit portless denies stop
	// traffic, and those already land in the Deny bucket; nothing else in the
	// old range belongs in the removed bucket.
	if newPol != nil && newPol.DefaultAction == policy.ActionAllow {
		return nil
	}
	if prefixFullyAllowedPortless(newPol, oldPfx) {
		return nil
	}
	keepers := overlappingPortlessAllows(newPol, oldPfx)
	if len(keepers) == 0 {
		return []netip.Prefix{oldPfx}
	}
	return prefixDiff(oldPfx, keepers)
}

func prefixFullyAllowedPortless(newPol *policy.NetworkPolicy, pfx netip.Prefix) bool {
	if newPol == nil || !pfx.IsValid() {
		return false
	}
	if newPol.EvaluateIP(pfx.Addr()) != policy.ActionAllow {
		return false
	}
	for _, deny := range portlessDenyPrefixes(newPol) {
		if deny.Overlaps(pfx) {
			return false
		}
	}
	last := lastAddr(pfx)
	if last.IsValid() && last != pfx.Addr() && newPol.EvaluateIP(last) != policy.ActionAllow {
		return false
	}
	return true
}

// overlappingPortlessAllows returns newPol's portless allows inside/overlapping
// oldPfx, minus newPol's portless denies (a deny inside a retained allow is not
// retained).
func overlappingPortlessAllows(newPol *policy.NetworkPolicy, oldPfx netip.Prefix) []netip.Prefix {
	if newPol == nil {
		return nil
	}
	denies := portlessDenyPrefixes(newPol)
	var allows []netip.Prefix
	for _, a := range rawOverlappingPortlessAllows(newPol, oldPfx) {
		allows = append(allows, prefixDiff(a, denies)...)
	}
	return allows
}

func rawOverlappingPortlessAllows(newPol *policy.NetworkPolicy, oldPfx netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range newPol.Egress {
		if r.Action != policy.ActionAllow || len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			continue
		}
		if looksLikeIP(r) {
			ip := parseRuleIP(r)
			if ip.IsValid() && oldPfx.Contains(ip) {
				bits := 32
				if ip.Is6() {
					bits = 128
				}
				p, err := ip.Prefix(bits)
				if err == nil {
					out = append(out, p)
				}
			}
			continue
		}
		if looksLikeCIDR(r) {
			pfx := parseRulePrefix(r).Masked()
			if pfx.IsValid() && prefixesOverlap(oldPfx, pfx) {
				out = append(out, pfx)
			}
		}
	}
	return out
}

func prefixesOverlap(a, b netip.Prefix) bool {
	if !a.IsValid() || !b.IsValid() {
		return false
	}
	return a.Overlaps(b)
}

// prefixDiff returns the parts of whole not covered by any keeper.
func prefixDiff(whole netip.Prefix, keepers []netip.Prefix) []netip.Prefix {
	remaining := []netip.Prefix{whole.Masked()}
	for _, k := range keepers {
		if !k.IsValid() {
			continue
		}
		k = k.Masked()
		var next []netip.Prefix
		for _, r := range remaining {
			next = append(next, excludePrefix(r, k)...)
		}
		remaining = next
		if len(remaining) == 0 {
			break
		}
	}
	return remaining
}

// excludePrefix returns the parts of whole that are not inside exclude.
func excludePrefix(whole, exclude netip.Prefix) []netip.Prefix {
	if !whole.IsValid() {
		return nil
	}
	whole = whole.Masked()
	if !exclude.IsValid() || !whole.Overlaps(exclude) {
		return []netip.Prefix{whole}
	}
	exclude = exclude.Masked()
	// exclude covers whole entirely.
	if exclude.Bits() <= whole.Bits() && exclude.Contains(whole.Addr()) {
		return nil
	}
	// whole is more specific than exclude and contained → fully excluded.
	if whole.Bits() >= exclude.Bits() && exclude.Contains(whole.Addr()) {
		return nil
	}
	// Bisect whole and recurse until exclude can be carved out.
	if whole.Bits() >= bitLen(whole) {
		if exclude.Contains(whole.Addr()) {
			return nil
		}
		return []netip.Prefix{whole}
	}
	left, right := splitPrefix(whole)
	var out []netip.Prefix
	out = append(out, excludePrefix(left, exclude)...)
	out = append(out, excludePrefix(right, exclude)...)
	return out
}

func bitLen(p netip.Prefix) int {
	if p.Addr().Is4() {
		return 32
	}
	return 128
}

func splitPrefix(p netip.Prefix) (netip.Prefix, netip.Prefix) {
	bits := p.Bits() + 1
	addr := p.Addr()
	left, err := addr.Prefix(bits)
	if err != nil {
		return p, netip.Prefix{}
	}
	left = left.Masked()
	// Flip the bit that distinguishes the two halves.
	rightAddr := setHostBit(addr, bits)
	right, err := rightAddr.Prefix(bits)
	if err != nil {
		return left, netip.Prefix{}
	}
	return left, right.Masked()
}

func setHostBit(addr netip.Addr, bitIndexAfterMSB int) netip.Addr {
	// bitIndexAfterMSB is the new prefix length (1..bitLen); the bit at
	// position (bitIndexAfterMSB-1) from the MSB is the one that splits the
	// parent prefix into left (0) and right (1).
	if addr.Is4() {
		var b [4]byte
		copy(b[:], addr.AsSlice())
		bit := bitIndexAfterMSB - 1 // 0..31
		b[bit/8] |= 1 << (7 - uint(bit%8))
		return netip.AddrFrom4(b)
	}
	var b [16]byte
	copy(b[:], addr.AsSlice())
	bit := bitIndexAfterMSB - 1 // 0..127
	b[bit/8] |= 1 << (7 - uint(bit%8))
	return netip.AddrFrom16(b)
}

// portsNoLongerAllowed selects the previously allowed ports denied by the
// supplied destination verdict.
func portsNoLongerAllowed(newPol *policy.NetworkPolicy, ports []int, ranges []policy.PortRange, allowed func(int) bool) ([]int, []policy.PortRange) {
	var outPorts []int
	for _, p := range ports {
		if !allowed(p) {
			outPorts = append(outPorts, p)
		}
	}
	var outRanges []policy.PortRange
	for _, r := range ranges {
		// Expand small ranges into the ports still denied; for large floor
		// ranges keep the whole range if any boundary port is denied.
		if r.Hi-r.Lo > 64 {
			if !tcpRangeAllowed(newPol, r, allowed) {
				outRanges = append(outRanges, r)
			}
			continue
		}
		for p := r.Lo; p <= r.Hi; p++ {
			if !allowed(p) {
				outPorts = append(outPorts, p)
			}
		}
	}
	return outPorts, outRanges
}

// tcpRangeAllowed samples a large range at its bounds and at every port-scoped
// rule boundary that falls inside it, which is where the verdict can change.
func tcpRangeAllowed(newPol *policy.NetworkPolicy, r policy.PortRange, allowed func(int) bool) bool {
	samples := []int{r.Lo, r.Hi}
	deny, allow := newPol.PortScopedRules()
	for _, rule := range append(append([]policy.PortScopedRule(nil), deny...), allow...) {
		for _, p := range rule.Ports {
			samples = append(samples, p, p-1, p+1)
		}
		for _, rg := range rule.Ranges {
			samples = append(samples, rg.Lo, rg.Hi, rg.Lo-1, rg.Hi+1)
		}
	}
	for _, p := range samples {
		if p < r.Lo || p > r.Hi {
			continue
		}
		if !allowed(p) {
			return false
		}
	}
	return true
}

func portsFullyAllowedOnPrefix(newPol *policy.NetworkPolicy, pfx netip.Prefix, ports []int, ranges []policy.PortRange) bool {
	// Sample the network and last address plus every rule target inside the
	// prefix (where the verdict can differ); if all still allow every listed
	// port, treat the port-scoped allow as retained for the whole prefix.
	pfx = pfx.Masked()
	samples := []netip.Addr{pfx.Addr()}
	if last := lastAddr(pfx); last.IsValid() && last != pfx.Addr() {
		samples = append(samples, last)
	}
	if newPol != nil {
		for _, r := range newPol.Egress {
			var t netip.Prefix
			if looksLikeIP(r) {
				t = hostPrefix(parseRuleIP(r))
			} else if looksLikeCIDR(r) {
				t = parseRulePrefix(r).Masked()
			}
			if t.IsValid() && t.Overlaps(pfx) {
				if pfx.Contains(t.Addr()) {
					samples = append(samples, t.Addr())
				}
				if l := lastAddr(t); pfx.Contains(l) {
					samples = append(samples, l)
				}
			}
		}
	}
	for _, addr := range samples {
		leftPorts, leftRanges := portsNoLongerAllowed(newPol, ports, ranges, func(port int) bool {
			return tcpPortAllowed(newPol, addr, port)
		})
		if len(leftPorts) > 0 || len(leftRanges) > 0 {
			return false
		}
	}
	return true
}

// tcpPortAllowed mirrors the egress chain for a new TCP dial to addr:port:
// port-scoped deny, plain deny set, port-scoped allow, plain allow set, then
// the chain default. A portless allow therefore never overrides a port-scoped
// deny (e.g. allow 1.2.3.4 + deny ports [8080] still denies :8080).
func tcpPortAllowed(newPol *policy.NetworkPolicy, addr netip.Addr, port int) bool {
	if newPol == nil || !addr.IsValid() || port < 1 || port > 65535 {
		return false
	}
	addr = addr.Unmap()
	deny, allow := newPol.PortScopedRules()
	for _, r := range deny {
		if portScopedMatches(r, addr, port) {
			return false
		}
	}
	if ipExplicitlyDenied(newPol, addr) {
		return false
	}
	for _, r := range allow {
		if portScopedMatches(r, addr, port) {
			return true
		}
	}
	return newPol.EvaluateIP(addr) == policy.ActionAllow
}

func portScopedMatches(r policy.PortScopedRule, addr netip.Addr, port int) bool {
	if r.Target != "" {
		if pfx, err := netip.ParsePrefix(r.Target); err == nil {
			if !pfx.Contains(addr) {
				return false
			}
		} else if ip, err := netip.ParseAddr(r.Target); err == nil {
			if ip.Unmap() != addr {
				return false
			}
		} else {
			return false
		}
	}
	for _, p := range r.Ports {
		if p == port {
			return true
		}
	}
	for _, rg := range r.Ranges {
		if rg.Contains(port) {
			return true
		}
	}
	return false
}

func lastAddr(pfx netip.Prefix) netip.Addr {
	if !pfx.IsValid() {
		return netip.Addr{}
	}
	addr := pfx.Addr()
	bits := pfx.Bits()
	if addr.Is4() {
		var b [4]byte
		copy(b[:], addr.AsSlice())
		hostBits := 32 - bits
		if hostBits <= 0 {
			return addr
		}
		mask := uint32((1 << hostBits) - 1)
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		v |= mask
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	var b [16]byte
	copy(b[:], addr.AsSlice())
	hostBits := 128 - bits
	for i := 15; hostBits > 0; i-- {
		take := hostBits
		if take > 8 {
			take = 8
		}
		b[i] |= byte((1 << take) - 1)
		hostBits -= take
	}
	return netip.AddrFrom16(b)
}
