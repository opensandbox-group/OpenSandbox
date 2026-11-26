// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

const (
	scopedLeaseV4           = "dyn_allow_tcp_v4"
	scopedLeaseV6           = "dyn_allow_tcp_v6"
	scopedDropV4            = "established_drop_tcp_v4"
	scopedDropV6            = "established_drop_tcp_v6"
	scopedEstablishedKeepV4 = "established_keep_tcp_v4"
	scopedEstablishedKeepV6 = "established_keep_tcp_v6"

	// Bound each kind of established tuple history independently, including
	// withdrawals retained across multiple opt-ins: one full port rule per
	// DNS address slot. Refuse overflow before replacing the current table.
	maxScopedHistoryEntries = maxResolvedDomains * maxDomainAddresses * 256
)

func scopedLeaseSet(addr netip.Addr) string {
	if addr.Is4() {
		return scopedLeaseV4
	}
	return scopedLeaseV6
}

func writeScopedAccepts(b *strings.Builder) {
	fmt.Fprintf(b, "add rule inet %s %s ip daddr . tcp dport @%s accept\n", tableName, chainName, scopedLeaseV4)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr . tcp dport @%s accept\n", tableName, chainName, scopedLeaseV6)
}

func writeScopedLeases(b *strings.Builder, leases map[netip.AddrPort]time.Time, now time.Time, replace bool) {
	for endpoint, deadline := range leases {
		if !deadline.After(now) {
			continue
		}
		addr, port := endpoint.Addr(), endpoint.Port()
		set := scopedLeaseSet(addr)
		if replace {
			fmt.Fprintf(b, "add element inet %s %s { %s . %d }\n", tableName, set, addr, port)
			fmt.Fprintf(b, "delete element inet %s %s { %s . %d }\n", tableName, set, addr, port)
		}
		seconds := (deadline.Sub(now) + time.Second - time.Nanosecond) / time.Second
		fmt.Fprintf(b, "add element inet %s %s { %s . %d timeout %ds }\n", tableName, set, addr, port, seconds)
	}
}

func (d *EstablishedDrops) addScopedDrop(key netip.AddrPort, deadline time.Time) {
	if d.scopedDrops == nil {
		d.scopedDrops = make(map[netip.AddrPort]time.Time)
	}
	d.scopedDrops[key] = deadline
}

func writeScopedDropElement(b *strings.Builder, verb string, key netip.AddrPort) {
	set := scopedDropV6
	if key.Addr().Is4() {
		set = scopedDropV4
	}
	fmt.Fprintf(b, "%s element inet %s %s { %s . %d }\n", verb, tableName, set, key.Addr(), key.Port())
}

// Each committed cut remains until an effective regrant or a rebuild after
// its deadline. The kernel set itself does not expire entries autonomously.
func writeScopedDrops(b *strings.Builder, drops map[netip.AddrPort]time.Time) {
	if len(drops) == 0 {
		return
	}
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr . inet_service; }\n", tableName, scopedDropV4)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr . inet_service; }\n", tableName, scopedDropV6)
	for key := range drops {
		writeScopedDropElement(b, "add", key)
	}
	fmt.Fprintf(b, "add rule inet %s %s ct state established,related ip daddr . tcp dport @%s drop\n", tableName, chainName, scopedDropV4)
	fmt.Fprintf(b, "add rule inet %s %s ct state established,related ip6 daddr . tcp dport @%s drop\n", tableName, chainName, scopedDropV6)
}

func (d EstablishedDrops) hasScopedEstablishedKeepSets(hasScopedLeases bool) bool {
	return d.hasLegacyLateDrop() && (hasScopedLeases || len(d.scopedEstablishedKeeps) != 0)
}

func writeScopedEstablishedKeepElement(b *strings.Builder, key netip.AddrPort) {
	set := scopedEstablishedKeepV6
	if key.Addr().Is4() {
		set = scopedEstablishedKeepV4
	}
	fmt.Fprintf(b, "add element inet %s %s { %s . %d }\n", tableName, set, key.Addr(), key.Port())
}

func writeScopedEstablishedKeeps(b *strings.Builder, keeps map[netip.AddrPort]struct{}) {
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr . inet_service; }\n", tableName, scopedEstablishedKeepV4)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr . inet_service; }\n", tableName, scopedEstablishedKeepV6)
	for key := range keeps {
		writeScopedEstablishedKeepElement(b, key)
	}
	fmt.Fprintf(b, "add rule inet %s %s ct state established,related ip daddr . tcp dport @%s accept\n", tableName, chainName, scopedEstablishedKeepV4)
	fmt.Fprintf(b, "add rule inet %s %s ct state established,related ip6 daddr . tcp dport @%s accept\n", tableName, chainName, scopedEstablishedKeepV6)
}

func scopedEstablishedKeepCapacity(count int) error {
	if count > maxScopedHistoryEntries {
		return fmt.Errorf("too many established scoped exceptions (limit %d)", maxScopedHistoryEntries)
	}
	return nil
}

func (d *EstablishedDrops) addScopedEstablishedKeep(key netip.AddrPort) {
	if d.scopedEstablishedKeeps == nil {
		d.scopedEstablishedKeeps = make(map[netip.AddrPort]struct{})
	}
	d.scopedEstablishedKeeps[key] = struct{}{}
}

func (d EstablishedDrops) needsScopedEstablishedKeep(coverage *scopedEstablishedKeepCoverage, key netip.AddrPort) bool {
	if _, known := d.scopedEstablishedKeeps[key]; known || key.Addr().IsLoopback() {
		return false
	}
	return d.matchesLegacyLateDrop(key.Addr(), int(key.Port())) && !coverage.contains(d, key)
}

// Only inherited keeps can replace DNS admission history. A raw static allow
// shadowed by a deny can disappear on the next ordinary apply without ever
// having contributed a keep. The invalid prefix is the shared port-only scope.
type scopedEstablishedKeepCoverage struct {
	scopes map[netip.Prefix][]policy.PortRange
	depths []int
}

var allEstablishedKeepPorts = []policy.PortRange{{Lo: 1, Hi: 65535}}

func (c *scopedEstablishedKeepCoverage) compile(d EstablishedDrops) {
	c.scopes = make(map[netip.Prefix][]policy.PortRange)
	for _, addr := range d.KeepAddrs {
		if addr.IsValid() {
			c.scopes[hostPrefix(addr)] = allEstablishedKeepPorts
		}
	}
	for _, prefix := range d.KeepPrefixes {
		if prefix.IsValid() {
			c.scopes[prefix.Masked()] = allEstablishedKeepPorts
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
		if len(spans) == 1 && spans[0] == allEstablishedKeepPorts[0] {
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
		if len(spans) < 2 {
			continue
		}
		slices.SortFunc(spans, func(a, b policy.PortRange) int { return a.Lo - b.Lo })
		merged := spans[:1]
		for _, span := range spans[1:] {
			last := &merged[len(merged)-1]
			if span.Lo <= last.Hi+1 {
				last.Hi = max(last.Hi, span.Hi)
			} else {
				merged = append(merged, span)
			}
		}
		c.scopes[scope] = merged
	}
	for depth, used := range depths {
		if used {
			c.depths = append(c.depths, depth)
		}
	}
}

func (c *scopedEstablishedKeepCoverage) contains(d EstablishedDrops, key netip.AddrPort) bool {
	if c.scopes == nil {
		c.compile(d)
	}
	port := int(key.Port())
	matches := func(spans []policy.PortRange) bool {
		i, _ := slices.BinarySearchFunc(spans, port, func(span policy.PortRange, p int) int { return span.Hi - p })
		return i < len(spans) && spans[i].Lo <= port
	}
	if matches(c.scopes[netip.Prefix{}]) {
		return true
	}
	addr := key.Addr()
	for _, depth := range c.depths {
		if depth > addr.BitLen() {
			break
		}
		if matches(c.scopes[netip.PrefixFrom(addr, depth).Masked()]) {
			return true
		}
	}
	return false
}

func (d *EstablishedDrops) reconcileScopedEstablishedKeeps(p *policy.NetworkPolicy, leases map[netip.AddrPort]time.Time) error {
	carried := d.scopedEstablishedKeeps
	d.scopedEstablishedKeeps = nil
	if len(carried)+len(leases) == 0 || !d.hasLegacyLateDrop() {
		return nil
	}
	var coverage scopedEstablishedKeepCoverage
	for key := range carried {
		if d.needsScopedEstablishedKeep(&coverage, key) {
			d.addScopedEstablishedKeep(key)
		}
	}
	var admission scopedReadmission
	for key := range leases {
		if _, keep := admission.changes(p, d, &coverage, key); !keep {
			continue
		}
		if err := scopedEstablishedKeepCapacity(len(d.scopedEstablishedKeeps) + 1); err != nil {
			return err
		}
		d.addScopedEstablishedKeep(key)
	}
	return nil
}

type scopedIPPolicy struct {
	allowIP, denyIP   []netip.Prefix
	allowTCP, denyTCP []policy.PortScopedRule
	defaultAllow      bool
}

func newScopedIPPolicy(p *policy.NetworkPolicy) scopedIPPolicy {
	parse := func(v4, v6 []string) []netip.Prefix {
		out := make([]netip.Prefix, 0, len(v4)+len(v6))
		for _, family := range [2][]string{v4, v6} {
			for _, target := range family {
				if prefix, err := parseAsPrefix(target); err == nil {
					out = append(out, prefix.Masked())
				}
			}
		}
		return out
	}
	allowV4, allowV6, denyV4, denyV6 := p.StaticIPSets()
	deny, allow := p.PortScopedRules()
	return scopedIPPolicy{parse(allowV4, allowV6), parse(denyV4, denyV6), allow, deny, p.DefaultAction == policy.ActionAllow}
}

func scopedIPRulesMatch(prefixes []netip.Prefix, rules []policy.PortScopedRule, key netip.AddrPort) bool {
	addr, port := key.Addr(), int(key.Port())
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	for _, rule := range rules {
		if !containsRawPort(rule.Ports, rule.Ranges, port) {
			continue
		}
		if rule.Target == "" {
			return true
		}
		// A literal retains its zone. Guard its nft family before the legacy
		// selector unmapped comparison so mapped IPv6 cannot match native IPv4.
		if rule.IsV6 == addr.Is6() && teardownSelectorFor(rule.Target).matches(addr) {
			return true
		}
	}
	return false
}

func (p scopedIPPolicy) denies(key netip.AddrPort) bool {
	return scopedIPRulesMatch(p.denyIP, p.denyTCP, key)
}

func (p scopedIPPolicy) allows(key netip.AddrPort, viaDNS bool) bool {
	return !p.denies(key) && (viaDNS || p.defaultAllow || scopedIPRulesMatch(p.allowIP, p.allowTCP, key))
}

type scopedReadmission struct {
	current scopedIPPolicy
	loaded  bool
}

func (r *scopedReadmission) rules(p *policy.NetworkPolicy) *scopedIPPolicy {
	if !r.loaded {
		r.current, r.loaded = newScopedIPPolicy(p), true
	}
	return &r.current
}

func (r *scopedReadmission) allows(p *policy.NetworkPolicy, d *EstablishedDrops, key netip.AddrPort) bool {
	if targetsMatch(d.DenyAddrs, d.DenyPrefixes, d.DenyPortDrops, key.Addr(), int(key.Port())) {
		return false
	}
	// A timed entry under an explicit deny (including the operator
	// floor) was not an effective admission and cannot retire a cut.
	return r.rules(p).allows(key, true)
}

// A live grant may retire an exact cut, shield a flow from a legacy drop, or
// do neither. Planning, snapshot estimates and acknowledged DNS updates use
// the same effective verdict; only the writer manages deadlines and capacity.
func (r *scopedReadmission) changes(p *policy.NetworkPolicy, d *EstablishedDrops, coverage *scopedEstablishedKeepCoverage, key netip.AddrPort) (cut, keep bool) {
	_, cut = d.scopedDrops[key]
	keep = d.hasLegacyLateDrop() && d.needsScopedEstablishedKeep(coverage, key)
	if (cut || keep) && r.allows(p, d, key) {
		return cut, keep
	}
	return false, false
}

func (d *EstablishedDrops) retireScopedDrops(p *policy.NetworkPolicy, scoped map[netip.AddrPort]time.Time, whole map[netip.Addr]time.Time) {
	if len(d.scopedDrops) == 0 || len(scoped)+len(whole) == 0 {
		return
	}
	var admission scopedReadmission
	for key := range d.scopedDrops {
		_, scopedLive := scoped[key]
		_, wholeLive := whole[key.Addr()]
		if (scopedLive || wholeLive) && admission.allows(p, d, key) {
			delete(d.scopedDrops, key)
		}
	}
	if len(d.scopedDrops) == 0 {
		d.scopedDrops = nil
	}
}

func (m *Manager) scopedDropsRegrantedByIP(deadlines map[netip.Addr]time.Time, now time.Time) []netip.AddrPort {
	var retired []netip.AddrPort
	var admission scopedReadmission
	for key := range m.lastDrops.scopedDrops {
		if deadlines[key.Addr()].After(now) && admission.allows(m.domainPolicy, &m.lastDrops, key) {
			retired = append(retired, key)
		}
	}
	return retired
}

func (d *EstablishedDrops) forgetScopedDrops(keys []netip.AddrPort) {
	for _, key := range keys {
		delete(d.scopedDrops, key)
	}
	if len(d.scopedDrops) == 0 {
		d.scopedDrops = nil
	}
}

// restoreScopedLeasesLocked repairs the scoped grants and withdrawals after
// an uncertain dynamic outcome, using only acknowledged state.
func (m *Manager) restoreScopedLeasesLocked(ctx context.Context, now time.Time) error {
	if !m.scopedDirty {
		return nil
	}
	var b strings.Builder
	if m.scopedLeases != nil {
		for _, set := range []string{scopedLeaseV4, scopedLeaseV6} {
			fmt.Fprintf(&b, "flush set inet %s %s\n", tableName, set)
		}
		writeScopedLeases(&b, m.scopedLeases, now, false)
	}
	if len(m.lastDrops.scopedDrops) != 0 {
		for _, set := range []string{scopedDropV4, scopedDropV6} {
			fmt.Fprintf(&b, "flush set inet %s %s\n", tableName, set)
		}
		for key := range m.lastDrops.scopedDrops {
			writeScopedDropElement(&b, "add", key)
		}
	}
	if m.lastDrops.hasScopedEstablishedKeepSets(m.scopedLeases != nil) {
		for _, set := range []string{scopedEstablishedKeepV4, scopedEstablishedKeepV6} {
			fmt.Fprintf(&b, "flush set inet %s %s\n", tableName, set)
		}
		for key := range m.lastDrops.scopedEstablishedKeeps {
			writeScopedEstablishedKeepElement(&b, key)
		}
	}
	if _, err := m.run(ctx, b.String()); err != nil {
		return err
	}
	m.scopedDirty = false
	return nil
}

func (m *Manager) addDomainDeadlinesLocked(ctx context.Context, permissions policy.DomainPermissions, deadlines map[netip.Addr]time.Time, now time.Time) error {
	if permissions.Unrestricted {
		return m.addResolvedDeadlinesLocked(ctx, deadlines, now)
	}
	var updates map[netip.AddrPort]time.Time
	var retired, kept []netip.AddrPort
	var admission scopedReadmission
	var coverage scopedEstablishedKeepCoverage
	for addr, deadline := range deadlines {
		for _, port := range permissions.AllowPorts {
			key := netip.AddrPortFrom(addr, uint16(port))
			if deadline.After(now) {
				cut, protect := admission.changes(m.domainPolicy, &m.lastDrops, &coverage, key)
				if cut {
					retired = append(retired, key)
				}
				if protect {
					kept = append(kept, key)
				}
			}
			if deadline.After(m.scopedLeases[key]) {
				if updates == nil {
					updates = make(map[netip.AddrPort]time.Time)
				}
				updates[key] = deadline
			}
		}
	}
	if err := scopedEstablishedKeepCapacity(len(m.lastDrops.scopedEstablishedKeeps) + len(kept)); err != nil {
		return err
	}
	if err := m.restoreScopedLeasesLocked(ctx, now); err != nil {
		return err
	}
	if len(updates)+len(retired)+len(kept) == 0 {
		return nil
	}
	var b strings.Builder
	writeScopedLeases(&b, updates, now, true)
	for _, key := range retired {
		writeScopedDropElement(&b, "delete", key)
	}
	for _, key := range kept {
		writeScopedEstablishedKeepElement(&b, key)
	}
	if _, err := m.run(ctx, b.String()); err != nil {
		m.scopedDirty = ApplyEffectOf(err) != ApplyUnchanged
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	maps.Copy(m.scopedLeases, updates)
	m.lastDrops.forgetScopedDrops(retired)
	for _, key := range kept {
		m.lastDrops.addScopedEstablishedKeep(key)
	}
	telemetry.RecordNftablesUpdate()
	return nil
}

type scopedRefresh struct {
	active  map[netip.AddrPort]struct{}
	renew   map[netip.AddrPort]time.Time
	credits []scopedRefreshCredit
}

type scopedRefreshCredit struct {
	owner *resolvedDomain
	key   netip.AddrPort
}

func (m *Manager) retainScopedLocked(plan *staticApplyPlan, current, retained *resolvedDomain, ports []int, now time.Time) {
	for addr, lease := range current.addresses {
		// Whole-IP grace must not authorize a newly scoped endpoint.
		if !lease.tcpUntil.IsZero() {
			plan.expiredCredits = append(plan.expiredCredits, dnsRefreshCredit{domain: retained, addr: addr})
		}
		for _, port := range ports {
			key := netip.AddrPortFrom(addr, uint16(port))
			activity := current.scopedActivity[key]
			expiry := lease.dnsUntil
			if activity.After(expiry) {
				expiry = activity
			}
			if !expiry.After(now) {
				continue
			}
			m.stageScopedLease(plan, retained, key, expiry, activity, now)
		}
	}
}

// A portless replacement can keep endpoint credit from the same DNS source,
// but only the recorded tuple and its existing absolute deadline.
func (m *Manager) retainScopedActivityLocked(plan *staticApplyPlan, current, retained *resolvedDomain, now time.Time) {
	for key, activity := range current.scopedActivity {
		if _, owns := current.addresses[key.Addr()]; owns && activity.After(now) {
			m.stageScopedLease(plan, retained, key, activity, activity, now)
		}
	}
}

func (m *Manager) stageScopedLease(plan *staticApplyPlan, retained *resolvedDomain, key netip.AddrPort, expiry, activity, now time.Time) {
	if plan.nextExpiry.IsZero() || expiry.Before(plan.nextExpiry) {
		plan.nextExpiry = expiry
	}
	if plan.scoped == nil {
		plan.scoped = make(map[netip.AddrPort]time.Time)
	}
	if expiry.After(plan.scoped[key]) {
		plan.scoped[key] = expiry
	}
	active := m.scopedCredited(key, activity)
	if active {
		if plan.scopedActive == nil {
			plan.scopedActive = make(map[netip.AddrPort]struct{})
		}
		plan.scopedActive[key] = struct{}{}
	}
	if active || activity.After(now) {
		if retained.scopedActivity == nil {
			retained.scopedActivity = make(map[netip.AddrPort]time.Time)
		}
		retained.scopedActivity[key] = activity
	}
}

func (m *Manager) scopedCredited(key netip.AddrPort, expiry time.Time) bool {
	_, wasActive := m.scopedActive[key]
	return wasActive && !expiry.IsZero() && expiry.Equal(m.tracker.previousActiveGrace)
}

func (m *Manager) planScopedRefreshLocked(connections []tcpConnection, now time.Time) scopedRefresh {
	var plan scopedRefresh
	if len(m.scopedLeases) == 0 {
		return plan
	}
	plan.active = make(map[netip.AddrPort]struct{})
	for _, c := range connections {
		key := netip.AddrPortFrom(c.remote.Unmap(), c.remotePort)
		if _, known := m.scopedLeases[key]; known && activeTCPState(c.state) {
			plan.active[key] = struct{}{}
		}
	}
	if len(plan.active)+len(m.scopedActive) > 0 {
		plan.renew = make(map[netip.AddrPort]time.Time)
		for key := range plan.active {
			plan.renew[key] = time.Time{}
		}
		for key := range m.scopedActive {
			plan.renew[key] = time.Time{}
		}
		grace := now.Add(dynSetTimeoutS * time.Second)
		for name, entry := range m.domains {
			permissions := m.domainPolicy.DomainPermissions(name)
			if len(permissions.AllowPorts) == 0 && (!permissions.Unrestricted || len(entry.scopedActivity) == 0) {
				continue
			}
			for key := range plan.renew {
				lease, owns := entry.addresses[key.Addr()]
				if !owns {
					continue
				}
				activity := entry.scopedActivity[key]
				live := activity.After(now)
				if permissions.Unrestricted {
					if !live {
						continue // neither DNS nor a sibling may create or revive inherited tuple credit
					}
				} else {
					if !slices.Contains(permissions.AllowPorts, int(key.Port())) {
						continue
					}
					live = live || lease.dnsUntil.After(now)
				}
				_, active := plan.active[key]
				if active && live || m.scopedCredited(key, activity) {
					plan.renew[key] = grace
					plan.credits = append(plan.credits, scopedRefreshCredit{entry, key})
				}
			}
		}
		for key, expiry := range plan.renew {
			if expiry.IsZero() {
				delete(plan.renew, key)
			}
		}
	}
	for key, expiry := range m.scopedLeases {
		if _, renewing := plan.renew[key]; !renewing {
			delete(plan.active, key)
			if !expiry.After(now) {
				delete(m.scopedLeases, key)
			}
		}
	}
	return plan
}

func (m *Manager) recordScopedRefreshLocked(plan scopedRefresh) {
	m.scopedActive = plan.active
	maps.Copy(m.scopedLeases, plan.renew)
	for _, credit := range plan.credits {
		if credit.owner.scopedActivity == nil {
			credit.owner.scopedActivity = make(map[netip.AddrPort]time.Time)
		}
		credit.owner.scopedActivity[credit.key] = plan.renew[credit.key]
	}
}
