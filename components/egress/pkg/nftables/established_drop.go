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
// are unset the entry covers every destination (port-only rule).
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
//  2. explicitly denied destinations and carried explicit TCP withdrawals
//  3. DNS accepts, then carried implicit TCP withdrawals
//  4. Keep*: destinations the new policy still allows (static allows minus
//     portless denies, surviving domain-lease IPs, port-scoped allows) plus
//     the static allow sets — accepted
//  5. Addrs/Prefixes/PortDrops: allows the change removed — dropped
//  6. CatchAll ? blanket established drop : blanket established accept
type EstablishedDrops struct {
	DenyAddrs     []netip.Addr
	DenyPrefixes  []netip.Prefix
	DenyPortDrops []EstablishedPortDrop

	Addrs     []netip.Addr
	Prefixes  []netip.Prefix
	PortDrops []EstablishedPortDrop
	CatchAll  bool

	KeepAddrs    []netip.Addr
	KeepPrefixes []netip.Prefix
	KeepPorts    []EstablishedPortDrop

	exact        exactTCP
	exactCarried bool // Opt-in uses the original rules; plain reloads use the saved TCP verdicts.
}

// Empty reports whether there is nothing to drop. Keep entries alone never
// change the ruleset.
func (d EstablishedDrops) Empty() bool {
	return len(d.DenyAddrs) == 0 && len(d.DenyPrefixes) == 0 && len(d.DenyPortDrops) == 0 &&
		len(d.Addrs) == 0 && len(d.Prefixes) == 0 && len(d.PortDrops) == 0 && !d.CatchAll && !d.exact.hasDrops()
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
	if targetsMatch(d.DenyAddrs, d.DenyPrefixes, d.DenyPortDrops, addr, dport) {
		return true
	}
	if d.exactCarried && dport > 0 {
		if w, tracked := d.exact[addr]; tracked {
			return w.explicit.contains(dport) || w.implicit.contains(dport)
		}
	}
	if targetsMatch(d.KeepAddrs, d.KeepPrefixes, d.KeepPorts, addr, dport) {
		return false
	}
	if d.CatchAll {
		return true
	}
	return targetsMatch(d.Addrs, d.Prefixes, d.PortDrops, addr, dport)
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
			if ipExplicitlyDenied(newPol, ip) {
				continue
			}
			survivingDomainIPs[ip] = struct{}{}
		}
	}

	var out EstablishedDrops
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
			if portlessPolicyAllows(newPol, ip) {
				continue
			}
			removedAddrs.add(ip)
		}
	}

	if oldPol != nil {
		for _, r := range oldPol.Egress {
			if r.Action != policy.ActionAllow {
				continue
			}
			hasPorts := len(r.Ports) > 0 || len(r.PortRanges) > 0
			switch {
			case hasPorts && looksLikeIP(r):
				ip := tcpPrefix(r.Target).Addr()
				if !ip.IsValid() {
					continue
				}
				if _, keep := survivingDomainIPs[ip]; keep {
					continue
				}
				ports, ranges := portsNoLongerAllowed(newPol, ip, r.Ports, r.PortRanges)
				if len(ports) == 0 && len(ranges) == 0 {
					continue
				}
				out.PortDrops = append(out.PortDrops, exactPortDrop(ip, ports, ranges))
			case hasPorts && looksLikeCIDR(r):
				pfx := parseRulePrefix(r)
				if !pfx.IsValid() {
					continue
				}
				if portsFullyAllowedOnPrefix(newPol, pfx, r.Ports, r.PortRanges) {
					continue
				}
				out.PortDrops = append(out.PortDrops, EstablishedPortDrop{
					Prefix: pfx.Masked(),
					Ports:  append([]int(nil), r.Ports...),
					Ranges: append([]policy.PortRange(nil), r.PortRanges...),
				})
			case hasPorts:
				// Port-only allow removal: tearing down every destination on
				// those ports would need the full complement of newPol's allows.
				// Leave it to conntrack; callers can tighten with a port deny.
				continue
			case looksLikeIP(r):
				prefix := tcpPrefix(r.Target)
				ip := prefix.Addr()
				if !ip.IsValid() || portlessPolicyAllows(newPol, ip) {
					continue
				}
				if _, keep := survivingDomainIPs[ip]; keep {
					continue
				}
				if ip.Is4In6() {
					removedPrefixes.add(prefix)
				} else {
					removedAddrs.add(ip)
				}
			case looksLikeCIDR(r):
				for _, removed := range removedPrefixParts(newPol, parseRulePrefix(r)) {
					removedPrefixes.add(removed)
				}
			}
		}
	}

	// Explicit denies in newPol cut established flows even when the previous
	// policy was default-allow with no explicit allow entry. They are emitted
	// ahead of every keep so an overlapping allow cannot shield them.
	denyAddrs := newAddrSet()
	denyPrefixes := newPrefixSet()
	for _, r := range newPol.Egress {
		if r.Action != policy.ActionDeny || len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			continue
		}
		if prefix := tcpPrefix(r.Target); prefix.IsValid() {
			if looksLikeIP(r) && !prefix.Addr().Is4In6() {
				denyAddrs.add(prefix.Addr())
			} else {
				denyPrefixes.add(prefix)
			}
		}
	}
	portDeny, portAllow := newPol.PortScopedRules()
	for _, r := range portDeny {
		if pd, ok := portDropFromScoped(r); ok {
			out.DenyPortDrops = append(out.DenyPortDrops, pd)
		}
	}
	out.DenyAddrs = denyAddrs.sorted()
	out.DenyPrefixes = denyPrefixes.sorted()
	out.Addrs = removedAddrs.sorted()
	out.Prefixes = removedPrefixes.sorted()

	out.CatchAll = oldPol != nil &&
		oldPol.DefaultAction == policy.ActionAllow &&
		newPol.DefaultAction != policy.ActionAllow

	// Keeps: everything newPol still allows, deny-aware.
	keepAddrs := newAddrSet()
	for ip := range survivingDomainIPs {
		keepAddrs.add(ip)
	}
	keepPrefixes := newPrefixSet()
	denyPfx := portlessDenyPrefixes(newPol)
	for _, r := range newPol.Egress {
		if r.Action != policy.ActionAllow || len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			continue
		}
		pfx := tcpPrefix(r.Target)
		if !pfx.IsValid() {
			continue
		}
		for _, part := range prefixDiff(pfx, denyPfx) {
			if part.IsSingleIP() && !part.Addr().Is4In6() {
				keepAddrs.add(part.Addr())
			} else {
				keepPrefixes.add(part)
			}
		}
	}
	for _, r := range portAllow {
		if pd, ok := portDropFromScoped(r); ok {
			out.KeepPorts = append(out.KeepPorts, pd)
		}
	}
	out.KeepAddrs = keepAddrs.sorted()
	out.KeepPrefixes = keepPrefixes.sorted()
	return out
}

// stillRelevant returns the part of a previously applied teardown set that
// must be re-emitted when p is applied without dropEstablished, so the next
// full ruleset rebuild does not silently reopen flows a dropEstablished commit
// closed. Destinations p allows again stop being dropped, and keeps accumulate
// (p's allows plus the currently learned domain IPs) so a destination that was
// allowed at any point after the commit is never torn down by a later plain
// apply the caller did not opt in to.
func (d EstablishedDrops) stillRelevant(previous, p *policy.NetworkPolicy, learned []netip.Addr, priorDNS, leases map[netip.Addr]time.Time) EstablishedDrops {
	if d.Empty() {
		return EstablishedDrops{}
	}
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	allowTCP, denyTCP := tcpPolicy(p)
	var out EstablishedDrops

	// Deny bucket: only what p still explicitly denies (it precedes keeps).
	var curDeny []netip.Prefix
	for _, rule := range denyTCP {
		if rule.whole {
			curDeny = append(curDeny, rule.prefix)
		}
	}
	denyAddrs := newAddrSet()
	for _, a := range d.DenyAddrs {
		if slices.ContainsFunc(curDeny, func(prefix netip.Prefix) bool { return prefix.Contains(a) }) {
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
	_, portAllow := p.PortScopedRules()
	out.DenyAddrs = denyAddrs.sorted()
	out.DenyPrefixes = denyPrefixes.sorted()
	out.DenyPortDrops = d.carryExplicitTCP(denyTCP, out.DenyPrefixes)
	out.exact = d.carryExactTCP(previous, p, allowTCP, denyTCP, priorDNS, leases)
	out.exactCarried = len(out.exact) != 0
	out.DenyPortDrops = slices.DeleteFunc(out.DenyPortDrops, func(drop EstablishedPortDrop) bool {
		prefix := dropTCPPrefix(drop)
		_, tracked := out.exact[prefix.Addr()]
		return prefix.IsSingleIP() && tracked
	})

	// Removed bucket: drop entries p fully allows again; partial re-allows
	// are shielded by the recomputed keeps.
	removedAddrs := newAddrSet()
	for _, a := range d.Addrs {
		if !portlessPolicyAllows(p, a) {
			removedAddrs.add(a)
		}
	}
	removedPrefixes := newPrefixSet()
	for _, pfx := range d.Prefixes {
		for _, part := range removedPrefixParts(p, pfx) {
			removedPrefixes.add(part)
		}
	}
	for _, pd := range d.PortDrops {
		if prefix := dropTCPPrefix(pd); prefix.IsSingleIP() {
			if _, tracked := d.exact[prefix.Addr()]; tracked {
				continue
			}
		}
		switch {
		case pd.Addr.IsValid():
			ports, ranges := portsNoLongerAllowed(p, pd.Addr, pd.Ports, pd.Ranges)
			if len(ports) > 0 || len(ranges) > 0 {
				out.PortDrops = append(out.PortDrops, EstablishedPortDrop{Addr: pd.Addr, Ports: ports, Ranges: ranges})
			}
		case pd.Prefix.IsValid():
			if !portsFullyAllowedOnPrefix(p, pd.Prefix, pd.Ports, pd.Ranges) {
				out.PortDrops = append(out.PortDrops, pd)
			}
		}
	}
	out.Addrs = removedAddrs.sorted()
	out.Prefixes = removedPrefixes.sorted()
	out.CatchAll = d.CatchAll && p.DefaultAction != policy.ActionAllow

	if out.Empty() {
		return EstablishedDrops{}
	}

	// Keeps accumulate: previous keeps ∪ p's allows ∪ learned domain IPs.
	fresh := ComputeEstablishedDrops(nil, p, nil)
	keepAddrs := newAddrSet()
	for _, a := range d.KeepAddrs {
		keepAddrs.add(a)
	}
	for _, a := range fresh.KeepAddrs {
		keepAddrs.add(a)
	}
	for _, a := range learned {
		keepAddrs.add(a)
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
	for _, r := range portAllow {
		cur, ok := portDropFromScoped(r)
		if !ok {
			continue
		}
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
	return out
}

func portDropFromScoped(r policy.PortScopedRule) (EstablishedPortDrop, bool) {
	pd := EstablishedPortDrop{
		Ports:  append([]int(nil), r.Ports...),
		Ranges: append([]policy.PortRange(nil), r.Ranges...),
	}
	if len(pd.Ports) == 0 && len(pd.Ranges) == 0 {
		return EstablishedPortDrop{}, false
	}
	if r.Target == "" {
		return pd, true
	}
	prefix := tcpPrefix(r.Target)
	if !prefix.IsValid() {
		return EstablishedPortDrop{}, false
	}
	if _, err := netip.ParseAddr(r.Target); err == nil {
		return exactPortDrop(prefix.Addr(), pd.Ports, pd.Ranges), true
	}
	pd.Prefix = prefix
	return pd, true
}

func exactPortDrop(addr netip.Addr, ports []int, ranges []policy.PortRange) EstablishedPortDrop {
	drop := EstablishedPortDrop{Ports: ports, Ranges: ranges}
	if addr.Is4In6() {
		drop.Prefix = netip.PrefixFrom(addr, addr.BitLen())
	} else {
		drop.Addr = addr
	}
	return drop
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
	for _, pfx := range portlessDenyPrefixes(p) {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

// Match portless policy in the same address family as the nft sets.
func portlessPolicyAllows(p *policy.NetworkPolicy, addr netip.Addr) bool {
	if p == nil {
		return false
	}
	allowed := p.DefaultAction == policy.ActionAllow
	for _, r := range p.Egress {
		if len(r.Ports)+len(r.PortRanges) != 0 || !tcpPrefix(r.Target).Contains(addr) {
			continue
		}
		if r.Action == policy.ActionDeny {
			return false
		}
		allowed = allowed || r.Action == policy.ActionAllow
	}
	return allowed
}

// portlessDenyPrefixes returns newPol's portless IP/CIDR deny rules as prefixes.
func portlessDenyPrefixes(p *policy.NetworkPolicy) []netip.Prefix {
	if p == nil {
		return nil
	}
	var out []netip.Prefix
	for _, r := range p.Egress {
		if r.Action != policy.ActionDeny || len(r.Ports) > 0 || len(r.PortRanges) > 0 {
			continue
		}
		if pfx := tcpPrefix(r.Target); pfx.IsValid() {
			out = append(out, pfx)
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
	if !portlessPolicyAllows(newPol, pfx.Addr()) {
		return false
	}
	for _, deny := range portlessDenyPrefixes(newPol) {
		if deny.Overlaps(pfx) {
			return false
		}
	}
	last := lastAddr(pfx)
	if last.IsValid() && last != pfx.Addr() && !portlessPolicyAllows(newPol, last) {
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
		if pfx := tcpPrefix(r.Target); prefixesOverlap(oldPfx, pfx) {
			out = append(out, pfx)
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

// portsNoLongerAllowed returns the subset of ports/ranges that newPol no
// longer admits for new TCP dials to addr, evaluated in chain order (port
// deny, plain deny, port allow, plain allow, default).
func portsNoLongerAllowed(newPol *policy.NetworkPolicy, addr netip.Addr, ports []int, ranges []policy.PortRange) ([]int, []policy.PortRange) {
	var outPorts []int
	for _, p := range ports {
		if !tcpPortAllowed(newPol, addr, p) {
			outPorts = append(outPorts, p)
		}
	}
	var outRanges []policy.PortRange
	for _, r := range ranges {
		// Expand small ranges into the ports still denied; for large floor
		// ranges keep the whole range if any boundary port is denied.
		if r.Hi-r.Lo > 64 {
			if !tcpRangeAllowed(newPol, addr, r) {
				outRanges = append(outRanges, r)
			}
			continue
		}
		for p := r.Lo; p <= r.Hi; p++ {
			if !tcpPortAllowed(newPol, addr, p) {
				outPorts = append(outPorts, p)
			}
		}
	}
	return outPorts, outRanges
}

// tcpRangeAllowed samples a large range at its bounds and at every port-scoped
// rule boundary that falls inside it, which is where the verdict can change.
func tcpRangeAllowed(newPol *policy.NetworkPolicy, addr netip.Addr, r policy.PortRange) bool {
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
		if !tcpPortAllowed(newPol, addr, p) {
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
			t := tcpPrefix(r.Target)
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
		leftPorts, leftRanges := portsNoLongerAllowed(newPol, addr, ports, ranges)
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
	return portlessPolicyAllows(newPol, addr)
}

func portScopedMatches(r policy.PortScopedRule, addr netip.Addr, port int) bool {
	if r.Target != "" && !tcpPrefix(r.Target).Contains(addr) {
		return false
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
