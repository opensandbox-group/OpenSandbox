// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

const (
	dynAllowPortV4Set = "dyn_allow_port_v4"
	dynAllowPortV6Set = "dyn_allow_port_v6"
	dynDenyPortV4Set  = "dyn_deny_port_v4"
	dynDenyPortV6Set  = "dyn_deny_port_v6"
)

// TCPEndpoint identifies one remote TCP destination. Addresses are unmapped
// before storing or rendering an endpoint.
type TCPEndpoint struct {
	Addr netip.Addr
	Port uint16
}

// WriteTimedPortElement replaces an endpoint's timeout atomically, including
// on kernels where adding an existing element does not renew its timeout.
func WriteTimedPortElement(b *strings.Builder, table, set string, endpoint TCPEndpoint, ttl time.Duration) {
	addr := endpoint.Addr.Unmap()
	fmt.Fprintf(b, "add element inet %s %s { %s . %d }\n", table, set, addr, endpoint.Port)
	fmt.Fprintf(b, "delete element inet %s %s { %s . %d }\n", table, set, addr, endpoint.Port)
	if ttl%time.Second == 0 {
		fmt.Fprintf(b, "add element inet %s %s { %s . %d timeout %ds }\n", table, set, addr, endpoint.Port, max(1, int(ttl/time.Second)))
	} else {
		fmt.Fprintf(b, "add element inet %s %s { %s . %d timeout %dms }\n", table, set, addr, endpoint.Port, max(1, int(ttl/time.Millisecond)))
	}
}

// WriteTimedIPChanges emits only membership or absolute-deadline changes.
// Add/delete in one transaction also removes a member already expired in-kernel.
func WriteTimedIPChanges(b *strings.Builder, table, v4, v6 string, before, after map[netip.Addr]time.Time, now time.Time) {
	changed := make([]netip.Addr, 0)
	for addr, deadline := range before {
		if next, exists := after[addr]; !exists || !deadline.Equal(next) {
			changed = append(changed, addr)
		}
	}
	for addr := range after {
		if _, exists := before[addr]; !exists && after[addr].After(now) {
			changed = append(changed, addr)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Compare(changed[j]) < 0 })
	for _, addr := range changed {
		set := v4
		if addr.Is6() {
			set = v6
		}
		if deadline := after[addr]; deadline.After(now) {
			WriteTimedElement(b, table, set, addr, deadline.Sub(now))
		} else {
			fmt.Fprintf(b, "add element inet %s %s { %s }\n", table, set, addr)
			fmt.Fprintf(b, "delete element inet %s %s { %s }\n", table, set, addr)
		}
	}
}

// WriteTimedPortChanges is the endpoint-scoped counterpart of WriteTimedIPChanges.
func WriteTimedPortChanges(b *strings.Builder, table, v4, v6 string, before, after map[TCPEndpoint]time.Time, now time.Time) {
	changed := make([]TCPEndpoint, 0)
	for endpoint, deadline := range before {
		if next, exists := after[endpoint]; !exists || !deadline.Equal(next) {
			changed = append(changed, endpoint)
		}
	}
	for endpoint := range after {
		if _, exists := before[endpoint]; !exists && after[endpoint].After(now) {
			changed = append(changed, endpoint)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if c := changed[i].Addr.Compare(changed[j].Addr); c != 0 {
			return c < 0
		}
		return changed[i].Port < changed[j].Port
	})
	for _, endpoint := range changed {
		set := v4
		if endpoint.Addr.Is6() {
			set = v6
		}
		if deadline := after[endpoint]; deadline.After(now) {
			WriteTimedPortElement(b, table, set, endpoint, deadline.Sub(now))
		} else {
			fmt.Fprintf(b, "add element inet %s %s { %s . %d }\n", table, set, endpoint.Addr.Unmap(), endpoint.Port)
			fmt.Fprintf(b, "delete element inet %s %s { %s . %d }\n", table, set, endpoint.Addr.Unmap(), endpoint.Port)
		}
	}
}

func cloneDomains(domains map[string]*resolvedDomain) map[string]*resolvedDomain {
	out := make(map[string]*resolvedDomain, len(domains))
	for domain, entry := range domains {
		out[domain] = cloneDomain(entry)
	}
	return out
}

func cloneDomain(entry *resolvedDomain) *resolvedDomain {
	if entry == nil {
		return nil
	}
	copy := *entry
	copy.addresses = maps.Clone(entry.addresses)
	copy.expires = maps.Clone(entry.expires)
	copy.activity = maps.Clone(entry.activity)
	return &copy
}

// domainCapacityVictimLocked reserves space without forgetting live DNS or TCP
// ownership. Callers remove the returned source only after a successful write.
func (m *Manager) domainCapacityVictimLocked(domain string, now time.Time) (string, error) {
	if m.domains[domain] != nil || len(m.domains) < maxResolvedDomains {
		return "", nil
	}
	var expired string
	for name, candidate := range m.domains {
		live := false
		for _, deadline := range candidate.expires {
			live = live || deadline.After(now)
		}
		for _, deadline := range candidate.activity {
			live = live || deadline.After(now)
		}
		if !live && (expired == "" || candidate.lastObserved.Before(m.domains[expired].lastObserved)) {
			expired = name
		}
	}
	if expired == "" {
		return "", fmt.Errorf("domain lease capacity reached (%d)", maxResolvedDomains)
	}
	return expired, nil
}

func (m *Manager) addScopedDomainLocked(ctx context.Context, domain string, ips []ResolvedIP) error {
	if err := m.reconcileDomainLeasesLocked(ctx); err != nil {
		return err
	}
	if len(ips) == 0 {
		return nil
	}
	now := m.tracker.now()
	expired, err := m.domainCapacityVictimLocked(domain, now)
	if err != nil {
		return err
	}
	domains := maps.Clone(m.domains)
	if expired != "" {
		delete(domains, expired)
	}
	entry := cloneDomain(domains[domain])
	if entry == nil {
		entry = &resolvedDomain{}
	}
	domains[domain] = entry
	if err := mergeDomainAddresses(entry, domain, ips, now); err != nil {
		return err
	}
	affected := make(map[netip.Addr]struct{}, len(entry.expires))
	for _, source := range []*resolvedDomain{m.domains[domain], m.domains[expired], entry} {
		if source != nil {
			for addr := range source.expires {
				affected[addr] = struct{}{}
			}
		}
	}
	if err := m.applyDomainLeaseChangesLocked(ctx, domains, affected, now); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	m.domains = domains
	telemetry.RecordNftablesUpdate()
	return nil
}

// mergeDomainAddresses stages bounded provenance before any kernel write.
// Its caller owns a clone, so capacity rejection cannot publish partial merges.
func mergeDomainAddresses(entry *resolvedDomain, domain string, ips []ResolvedIP, now time.Time) error {
	if entry.addresses == nil {
		entry.addresses = make(map[netip.Addr]struct{})
	}
	if entry.expires == nil {
		entry.expires = make(map[netip.Addr]time.Time)
	}
	// Retired, inactive observations no longer contribute a lease. Bound the
	// retained source map as well as the current revalidation address set.
	for addr, expiry := range entry.expires {
		if expiry.After(now) {
			continue
		}
		live := false
		for endpoint, deadline := range entry.activity {
			if endpoint.Addr == addr && deadline.After(now) {
				live = true
				break
			}
		}
		if !live {
			delete(entry.expires, addr)
			delete(entry.addresses, addr)
			for endpoint := range entry.activity {
				if endpoint.Addr == addr {
					delete(entry.activity, endpoint)
				}
			}
		}
	}
	for _, ip := range ips {
		addr := ip.Addr.Unmap()
		if !addr.IsValid() {
			continue
		}
		if _, exists := entry.expires[addr]; !exists && len(entry.expires) >= maxDomainAddresses {
			return fmt.Errorf("address lease capacity reached for domain %q (%d)", domain, maxDomainAddresses)
		}
		entry.addresses[addr] = struct{}{}
		expiry := now.Add(ClampTTL(ip.TTL))
		if expiry.After(entry.expires[addr]) {
			entry.expires[addr] = expiry
		}
	}
	entry.lastObserved = now
	entry.failures = 0
	entry.retryAt = time.Time{}
	return nil
}

// projectDomainLeases takes maximum absolute deadlines across independent
// sources, projecting only affected addresses on incremental updates.
func projectDomainLeases(p *policy.NetworkPolicy, domains map[string]*resolvedDomain, now time.Time, affected map[netip.Addr]struct{}) (map[netip.Addr]time.Time, map[TCPEndpoint]time.Time, map[TCPEndpoint]time.Time) {
	whole := make(map[netip.Addr]time.Time)
	allow := make(map[TCPEndpoint]time.Time)
	deny := make(map[TCPEndpoint]time.Time)
	for domain, entry := range domains {
		if affected != nil {
			relevant := false
			for addr := range entry.expires {
				if _, relevant = affected[addr]; relevant {
					break
				}
			}
			if !relevant {
				continue
			}
		}
		permissions := p.DomainPermissions(domain)
		for addr := range entry.expires {
			if affected != nil {
				if _, relevant := affected[addr]; !relevant {
					continue
				}
			}
			expiry := entry.expires[addr]
			for observed, activity := range entry.activity {
				if observed.Addr == addr && activity.After(expiry) {
					expiry = activity
				}
			}
			if permissions.Allowed && permissions.Unrestricted {
				if expiry.After(now) && expiry.After(whole[addr]) {
					whole[addr] = expiry
				}
			}
			for _, port := range permissions.AllowPorts {
				endpoint := TCPEndpoint{Addr: addr, Port: uint16(port)}
				deadline := entry.expires[addr]
				if activity := entry.activity[TCPEndpoint{Addr: addr}]; activity.After(deadline) {
					deadline = activity
				}
				if activity := entry.activity[endpoint]; activity.After(deadline) {
					deadline = activity
				}
				if deadline.After(now) && deadline.After(allow[endpoint]) {
					allow[endpoint] = deadline
				}
			}
			for _, port := range permissions.DenyPorts {
				endpoint := TCPEndpoint{Addr: addr, Port: uint16(port)}
				deadline := expiry
				if deadline.After(now) && deadline.After(deny[endpoint]) {
					deny[endpoint] = deadline
				}
			}
		}
	}
	return whole, allow, deny
}

// Full projection is reserved for policy replacement and table reconstruction.
func renderDomainLeases(p *policy.NetworkPolicy, domains map[string]*resolvedDomain, now time.Time) (string, map[netip.Addr]time.Time, map[TCPEndpoint]time.Time, map[TCPEndpoint]time.Time) {
	whole, allow, deny := projectDomainLeases(p, domains, now, nil)
	var b strings.Builder
	WriteTimedIPChanges(&b, tableName, dynAllowV4Set, dynAllowV6Set, nil, whole, now)
	WriteTimedPortChanges(&b, tableName, dynAllowPortV4Set, dynAllowPortV6Set, nil, allow, now)
	WriteTimedPortChanges(&b, tableName, dynDenyPortV4Set, dynDenyPortV6Set, nil, deny, now)
	return b.String(), whole, allow, deny
}

func (m *Manager) reconcileDomainLeasesLocked(ctx context.Context) error {
	if m.staticStateUncertain {
		return &ApplyError{Effect: ApplyUnchanged, Err: errStaticStateUncertain}
	}
	if !m.domainLeasesDirty {
		return nil
	}
	return m.syncDomainLeasesLocked(ctx, m.domains, m.tracker.now())
}

// syncDomainLeasesLocked commits staged activity together with reconciliation,
// so recovery cannot discard known endpoint identities before renewal planning.
func (m *Manager) syncDomainLeasesLocked(ctx context.Context, domains map[string]*resolvedDomain, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	leases, whole, allow, deny := renderDomainLeases(m.domainPolicy, domains, now)
	var b strings.Builder
	for _, set := range []string{dynAllowV4Set, dynAllowV6Set, dynAllowPortV4Set, dynAllowPortV6Set, dynDenyPortV4Set, dynDenyPortV6Set} {
		fmt.Fprintf(&b, "flush set inet %s %s\n", tableName, set)
	}
	b.WriteString(leases)
	if _, err := m.run(ctx, b.String()); err != nil {
		if m.opts.QuiesceOnApplyFailure && ApplyEffectOf(err) != ApplyUnchanged {
			m.quiesced.Store(true)
		}
		return err
	}
	m.tracker.dynamicIPs = whole
	m.tracker.dynamicEndpoints = allow
	m.tracker.dynamicDenies = deny
	m.domainLeasesDirty = false
	telemetry.RecordNftablesUpdate()
	return nil
}

func (m *Manager) applyDomainLeaseChangesLocked(ctx context.Context, domains map[string]*resolvedDomain, affected map[netip.Addr]struct{}, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	if m.domainLeasesDirty {
		return m.syncDomainLeasesLocked(ctx, domains, now)
	}
	if len(affected) == 0 {
		return nil
	}
	whole, allow, deny := projectDomainLeases(m.domainPolicy, domains, now, affected)
	beforeIPs := make(map[netip.Addr]time.Time, len(affected))
	beforeAllow := make(map[TCPEndpoint]time.Time)
	beforeDeny := make(map[TCPEndpoint]time.Time)
	for addr := range affected {
		if deadline, exists := m.tracker.dynamicIPs[addr]; exists {
			beforeIPs[addr] = deadline
		}
	}
	for endpoint, deadline := range m.tracker.dynamicEndpoints {
		if _, relevant := affected[endpoint.Addr]; relevant {
			beforeAllow[endpoint] = deadline
		}
	}
	for endpoint, deadline := range m.tracker.dynamicDenies {
		if _, relevant := affected[endpoint.Addr]; relevant {
			beforeDeny[endpoint] = deadline
		}
	}
	var b strings.Builder
	WriteTimedIPChanges(&b, tableName, dynAllowV4Set, dynAllowV6Set, beforeIPs, whole, now)
	WriteTimedPortChanges(&b, tableName, dynAllowPortV4Set, dynAllowPortV6Set, beforeAllow, allow, now)
	WriteTimedPortChanges(&b, tableName, dynDenyPortV4Set, dynDenyPortV6Set, beforeDeny, deny, now)
	if b.Len() != 0 {
		if _, err := m.run(ctx, b.String()); err != nil {
			if ApplyEffectOf(err) != ApplyUnchanged {
				m.domainLeasesDirty = true
				if m.opts.QuiesceOnApplyFailure {
					m.quiesced.Store(true)
				}
			}
			return err
		}
	}
	for addr := range beforeIPs {
		delete(m.tracker.dynamicIPs, addr)
	}
	for endpoint := range beforeAllow {
		delete(m.tracker.dynamicEndpoints, endpoint)
	}
	for endpoint := range beforeDeny {
		delete(m.tracker.dynamicDenies, endpoint)
	}
	maps.Copy(m.tracker.dynamicIPs, whole)
	maps.Copy(m.tracker.dynamicEndpoints, allow)
	maps.Copy(m.tracker.dynamicDenies, deny)
	return nil
}

func (m *Manager) refreshScopedConnectionsLocked(ctx context.Context, connections []tcpConnection) error {
	now := m.tracker.now()
	active := make(map[TCPEndpoint]struct{})
	for _, connection := range connections {
		if activeTCPState(connection.state) && connection.remote.IsValid() {
			active[TCPEndpoint{Addr: connection.remote.Unmap(), Port: connection.remotePort}] = struct{}{}
		}
	}
	domains := maps.Clone(m.domains)
	affected := make(map[netip.Addr]struct{})
	// Remove expired installed snapshots even if kernel GC already ran.
	for addr, deadline := range m.tracker.dynamicIPs {
		if !deadline.After(now) {
			affected[addr] = struct{}{}
		}
	}
	for endpoint, deadline := range m.tracker.dynamicEndpoints {
		if !deadline.After(now) {
			affected[endpoint.Addr] = struct{}{}
		}
	}
	for endpoint, deadline := range m.tracker.dynamicDenies {
		if !deadline.After(now) {
			affected[endpoint.Addr] = struct{}{}
		}
	}
	current := make(map[TCPEndpoint]struct{})
	for domain, entry := range domains {
		permissions := m.domainPolicy.DomainPermissions(domain)
		if !permissions.Allowed {
			continue
		}
		changed := false
		recordActivity := func(endpoint TCPEndpoint) {
			deadline := now.Add(dynSetTimeoutS * time.Second)
			if entry.activity[endpoint].Equal(deadline) {
				return
			}
			if !changed {
				copy := *entry
				copy.activity = maps.Clone(entry.activity)
				if copy.activity == nil {
					copy.activity = make(map[TCPEndpoint]time.Time)
				}
				entry = &copy
				domains[domain] = entry
				changed = true
			}
			entry.activity[endpoint] = deadline
			affected[endpoint.Addr] = struct{}{}
		}
		for addr := range entry.expires {
			if permissions.Unrestricted {
				endpoint := TCPEndpoint{Addr: addr}
				_, known := m.tracker.dynamicIPs[addr]
				if !known {
					continue
				}
				isActive := false
				for remote := range active {
					if remote.Addr == addr {
						isActive = true
						break
					}
				}
				_, wasActive := m.tracker.previousActiveEndpoints[endpoint]
				if isActive || wasActive {
					recordActivity(endpoint)
				}
				if isActive {
					current[endpoint] = struct{}{}
				}
			}
			for _, port := range permissions.AllowPorts {
				endpoint := TCPEndpoint{Addr: addr, Port: uint16(port)}
				if _, known := m.tracker.dynamicEndpoints[endpoint]; !known {
					continue
				}
				_, isActive := active[endpoint]
				_, wasActive := m.tracker.previousActiveEndpoints[endpoint]
				if isActive || wasActive {
					recordActivity(endpoint)
				}
				if isActive {
					current[endpoint] = struct{}{}
				}
			}
		}
	}
	if err := m.applyDomainLeaseChangesLocked(ctx, domains, affected, now); err != nil {
		return err
	}
	m.domains = domains
	m.tracker.previousActiveEndpoints = current
	telemetry.RecordNftablesUpdate()
	return nil
}

// Carry keeps remain endpoint-scoped; a sibling name on the same address is
// not evidence that every port on that address remains authorized.
func (d EstablishedDrops) withDomainCarry(previous EstablishedDrops, p *policy.NetworkPolicy, domains map[string][]netip.Addr) EstablishedDrops {
	fresh := ComputeEstablishedDrops(nil, p, domains)
	for _, current := range fresh.DomainPortDenies {
		if !current.Addr.IsValid() {
			continue
		}
		for _, port := range current.Ports {
			if targetsMatch(nil, nil, previous.DomainPortDenies, current.Addr, port) {
				d.DomainPortDenies = append(d.DomainPortDenies, EstablishedPortDrop{Addr: current.Addr, Ports: []int{port}})
			}
		}
	}
	if !d.Empty() {
		d.KeepAddrs = append(d.KeepAddrs, fresh.KeepAddrs...)
		d.KeepPorts = append(d.KeepPorts, fresh.KeepPorts...)
	}
	return d
}
