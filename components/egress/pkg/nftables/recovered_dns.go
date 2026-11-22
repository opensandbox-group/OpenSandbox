/*
 * Copyright 2026 The OpenSandbox Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nftables

import (
	"net/netip"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// Check capacity before a grant can leave an uncertain outcome. While a grant
// is pending, no other writer can consume its reserved history budget.
func (m *Manager) canRecordRecoveredDNSLocked(domain string, addrs map[netip.Addr]time.Time) bool {
	if len(m.recoveredDNS) != 0 {
		now := m.now()
		for source, history := range m.recoveredDNS {
			for addr, expires := range history {
				if !expires.After(now) {
					delete(history, addr)
				}
			}
			if len(history) == 0 {
				delete(m.recoveredDNS, source)
			}
		}
	}
	history, exists := m.recoveredDNS[domain]
	if !exists && len(m.recoveredDNS) >= maxResolvedDomains {
		return false
	}
	count := len(history)
	for addr := range addrs {
		if _, recorded := history[addr]; !recorded {
			count++
		}
	}
	return count <= maxDomainAddresses
}

// A full acknowledged rebuild removes any unconfirmed lease from the kernel.
// Its earlier admission of established traffic is still possible. Do not
// replace an entry in the confirmed DNS ledger with that possible history.
func (m *Manager) recordRecoveredDNSLocked() {
	pending := m.pendingGrant
	if pending == nil || pending.source.entry == nil {
		return
	}
	if m.recoveredDNS == nil {
		m.recoveredDNS = make(map[string]map[netip.Addr]time.Time)
	}
	domain := pending.source.domain
	history := m.recoveredDNS[domain]
	if history == nil {
		history = make(map[netip.Addr]time.Time, len(pending.deadlines))
		m.recoveredDNS[domain] = history
	}
	expires := m.now().Add(establishedCarryTTL())
	for addr := range pending.deadlines {
		history[addr] = expires
	}
}

// Evidence used by an acknowledged whole-address revocation must not tear
// down a different connection after the address is subsequently reallowed.
func (m *Manager) consumeRecoveredDNSLocked(p *policy.NetworkPolicy, plan staticApplyPlan) {
	drops := plan.drops
	for domain, history := range m.recoveredDNS {
		if p.Evaluate(domain) == policy.ActionAllow {
			continue
		}
		for addr := range history {
			if !drops.Matches(addr) {
				continue
			}
			if !targetsMatch(drops.DenyAddrs, drops.DenyPrefixes, nil, addr, 0) {
				if _, renewed := plan.leases[addr]; renewed || retainedPort(drops, addr) {
					continue
				}
			}
			delete(history, addr)
		}
		if len(history) == 0 {
			delete(m.recoveredDNS, domain)
		}
	}
}

func retainedPort(d EstablishedDrops, addr netip.Addr) bool {
	for _, keep := range d.KeepPorts {
		if portDropCoversAddr(keep, addr) {
			part := exactPortDenyOutsideKeeps(addr, keep, d.DenyPortDrops)
			if len(part.Ports)+len(part.Ranges) != 0 {
				return true
			}
		}
	}
	return false
}
