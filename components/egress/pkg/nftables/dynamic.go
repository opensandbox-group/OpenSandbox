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
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/internal/safego"
)

const (
	maxResolvedDomains    = 128
	maxDomainAddresses    = 64
	domainRefreshWorkers  = 4
	domainRefreshInterval = 30 * time.Second
	domainLookupTimeout   = 5 * time.Second
	domainRefreshBudget   = 20 * time.Second
	// domainRefreshLead is how far ahead of lease expiry a domain becomes
	// due for revalidation: two ticks absorb the jittered interval (up to
	// 1.5x) plus lookup latency while still leaving a spare tick before the
	// lease actually lapses. Domains with longer-lived leases are not
	// queried at all, which keeps revalidation volume proportional to the
	// number of leases about to expire instead of the number of tracked
	// domains (issue #1805).
	domainRefreshLead = 2 * domainRefreshInterval
	// domainBackoffMax caps the exponential retry delay for a domain whose
	// revalidation keeps failing; domainBackoffMaxStep bounds the shift so
	// the delay calculation cannot overflow.
	domainBackoffMax     = 10 * time.Minute
	domainBackoffMaxStep = 5
)

type resolvedDomain struct {
	addresses   map[netip.Addr]domainLease
	lastAttempt time.Time
	failures    int
	retryAt     time.Time
}

// A source lease is independent of the effective kernel deadline for a shared
// IP. An unconfirmed address still holds capacity, but cannot be DNS-renewed.
type domainLease struct {
	dnsUntil  time.Time
	tcpUntil  time.Time
	renewable bool
}

func (l domainLease) deadline() time.Time {
	if l.tcpUntil.After(l.dnsUntil) {
		return l.tcpUntil
	}
	return l.dnsUntil
}

func (e *resolvedDomain) nextExpiry() (earliest time.Time, renewable bool) {
	for _, lease := range e.addresses {
		if lease.renewable {
			expiry := lease.deadline()
			if !renewable || expiry.Before(earliest) {
				earliest = expiry
			}
			renewable = true
		}
	}
	return earliest, renewable
}

func (m *Manager) AddResolvedDomain(ctx context.Context, domain string, ips []ResolvedIP) error {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced.Load() {
		return ErrQuiesced
	}
	if m.staticStateUncertain {
		return errStaticStateUncertain
	}
	if m.domainPolicy == nil {
		return fmt.Errorf("no nftables policy applied; dropping resolved domain %q", domain)
	}
	if domain == "" || m.domainPolicy.Evaluate(domain) != policy.ActionAllow {
		return fmt.Errorf("resolved domain %q is no longer allowed", domain)
	}
	if m.domainPolicy.DefaultAction == policy.ActionAllow || len(ips) == 0 {
		return m.addResolvedIPsLocked(ctx, ips)
	}
	now := m.tracker.now()
	deadlines := answerDeadlines(ips, now, maxDomainAddresses)
	if len(deadlines) == 0 {
		return nil
	}
	if len(deadlines) > maxDomainAddresses {
		return fmt.Errorf("too many DNS addresses to track for %q (limit %d)", domain, maxDomainAddresses)
	}
	entry := &resolvedDomain{addresses: make(map[netip.Addr]domainLease, len(deadlines))}
	for addr, expiry := range deadlines {
		entry.addresses[addr] = domainLease{dnsUntil: expiry, renewable: true}
	}
	var victim string
	if previous := m.domains[domain]; previous != nil {
		entry.lastAttempt = previous.lastAttempt
		// Prefer live observations; use any remaining slots for expired
		// addresses still eligible for a background lookup.
		for address, lease := range previous.addresses {
			if fresh, observed := entry.addresses[address]; observed {
				if fresh.dnsUntil.After(lease.dnsUntil) {
					lease.dnsUntil = fresh.dnsUntil
				}
				lease.renewable = true
				entry.addresses[address] = lease
			} else if m.holdsDomainCapacity(address, lease, now) {
				entry.addresses[address] = lease
				if len(entry.addresses) > maxDomainAddresses {
					return fmt.Errorf("no expired DNS address slot for %q (limit %d)", domain, maxDomainAddresses)
				}
			}
		}
		for address, lease := range previous.addresses {
			if len(entry.addresses) == maxDomainAddresses {
				break
			}
			if _, exists := entry.addresses[address]; !exists && lease.renewable {
				entry.addresses[address] = lease
			}
		}
	} else if len(m.domains) >= maxResolvedDomains {
		for candidate, tracked := range m.domains {
			live := false
			for address, lease := range tracked.addresses {
				if m.holdsDomainCapacity(address, lease, now) {
					live = true
					break
				}
			}
			if !live {
				victim = candidate
				break
			}
		}
		if victim == "" {
			return fmt.Errorf("no expired DNS domain slot for %q (limit %d)", domain, maxResolvedDomains)
		}
	}
	if err := m.addResolvedDeadlinesLocked(ctx, deadlines, now); err != nil {
		return err
	}
	if victim != "" {
		delete(m.domains, victim)
	}
	m.domains[domain] = entry
	return nil
}

// holdsDomainCapacity is called with m.mu held. Activity may extend this
// source's live lease or continue a grace period credited by the last active
// poll, but cannot revive an unrelated expired owner of the shared IP.
func (m *Manager) holdsDomainCapacity(addr netip.Addr, lease domainLease, now time.Time) bool {
	return lease.deadline().After(now) || m.tracker.previouslyCredited(addr, lease.tcpUntil)
}

func (m *Manager) StartDomainRefresh(ctx context.Context, lookup func(context.Context, string) ([]ResolvedIP, error)) {
	if m.isQuiesced() {
		return
	}
	safego.Go(func() {
		timer := time.NewTimer(jitteredRefreshDelay())
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if m.isQuiesced() {
				return
			}
			// Arm the next deadline before the batch: a batch that consumes
			// part of its budget then delays the following tick by only the
			// remaining jitter, keeping the worst tick-to-tick gap at
			// max(jitter, batch budget) rather than their sum.
			timer.Reset(jitteredRefreshDelay())
			m.refreshDomains(ctx, lookup)
		}
	})
}

// jitteredRefreshDelay returns a uniform [interval/2, 3*interval/2) delay
// (mean interval). Per-tick jitter keeps batch-started sidecars from pulsing
// the shared resolver in phase (issue #1805).
func jitteredRefreshDelay() time.Duration {
	return domainRefreshInterval/2 + time.Duration(rand.Int64N(int64(domainRefreshInterval)))
}

func (m *Manager) refreshDomains(ctx context.Context, lookup func(context.Context, string) ([]ResolvedIP, error)) {
	type candidate struct {
		domain      string
		entry       *resolvedDomain
		lastAttempt time.Time
	}
	m.mu.Lock()
	if m.quiesced.Load() || m.staticStateUncertain {
		m.mu.Unlock()
		return
	}
	now := m.tracker.now()
	due := now.Add(domainRefreshLead)
	candidates := make([]candidate, 0, len(m.domains))
	for domain, entry := range m.domains {
		// Only query domains whose earliest observed-address lease expires
		// within the lead window (or has already lapsed), unless a previous
		// failure put the domain on backoff.
		if now.Before(entry.retryAt) {
			continue
		}
		expiry, renewable := entry.nextExpiry()
		if !renewable || expiry.After(due) {
			continue
		}
		candidates = append(candidates, candidate{domain, entry, entry.lastAttempt})
	}
	m.mu.Unlock()
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].lastAttempt.Before(candidates[right].lastAttempt)
	})
	jobs := make(chan candidate, len(candidates))
	for _, item := range candidates {
		jobs <- item
	}
	close(jobs)
	batchCtx, cancel := context.WithTimeout(ctx, domainRefreshBudget)
	defer cancel()
	var workers sync.WaitGroup
	for worker := 0; worker < min(domainRefreshWorkers, len(candidates)); worker++ {
		workers.Add(1)
		safego.Go(func() {
			defer workers.Done()
			for item := range jobs {
				if batchCtx.Err() != nil {
					return
				}
				m.mu.Lock()
				if m.quiesced.Load() {
					m.mu.Unlock()
					return
				}
				current := m.domains[item.domain] == item.entry
				if current {
					item.entry.lastAttempt = m.tracker.now()
				}
				m.mu.Unlock()
				if !current {
					continue
				}
				lookupCtx, lookupCancel := context.WithTimeout(batchCtx, domainLookupTimeout)
				ips, err := lookup(lookupCtx, item.domain)
				if err == nil {
					err = lookupCtx.Err()
				}
				lookupCancel()
				if err != nil {
					var retryIn time.Duration
					m.mu.Lock()
					if m.quiesced.Load() {
						m.mu.Unlock()
						return
					}
					if m.domains[item.domain] == item.entry {
						now := m.tracker.now()
						item.entry.failures++
						// The first failure retries at the base interval (the
						// pre-backoff cadence) so a single transient failure can
						// still recover before the due lease lapses; exponential
						// growth starts from the second failure.
						step := min(item.entry.failures-1, domainBackoffMaxStep)
						retryIn = min(domainRefreshInterval*time.Duration(1<<step), domainBackoffMax)
						// Clamp the delay so one retry stays possible before the
						// earliest tracked lease expires (less one lookup
						// timeout); once the lease has lapsed the full backoff
						// applies.
						if expiry, _ := item.entry.nextExpiry(); expiry.After(now) {
							if budget := expiry.Sub(now) - domainLookupTimeout; budget < retryIn {
								retryIn = max(budget, 0)
							}
						}
						item.entry.retryAt = now.Add(retryIn)
					}
					m.mu.Unlock()
					log.Warnf("[dns] domain revalidation failed for %q (next retry in %s): %v",
						item.domain, retryIn.Round(time.Second), err)
					continue
				}
				m.applyDomainRefresh(batchCtx, item.domain, item.entry, ips)
			}
		})
	}
	workers.Wait()
}

func (m *Manager) applyDomainRefresh(ctx context.Context, domain string, entry *resolvedDomain, ips []ResolvedIP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced.Load() || m.staticStateUncertain || ctx.Err() != nil || m.domains[domain] != entry || m.domainPolicy == nil || m.domainPolicy.Evaluate(domain) != policy.ActionAllow {
		return
	}
	now := m.tracker.now()
	var confirmed, updates map[netip.Addr]time.Time
	for _, ip := range ips {
		addr := ip.Addr.Unmap()
		if !entry.addresses[addr].renewable {
			continue
		}
		expiry := now.Add(ClampTTL(ip.TTL))
		if confirmed == nil {
			confirmed = make(map[netip.Addr]time.Time)
		}
		if expiry.After(confirmed[addr]) {
			confirmed[addr] = expiry
		}
		if !m.tracker.dynamicIPs[addr].After(expiry) && expiry.After(updates[addr]) {
			if updates == nil {
				updates = make(map[netip.Addr]time.Time)
			}
			updates[addr] = expiry
		}
	}
	if len(updates) != 0 {
		updateCtx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
		err := m.addResolvedDeadlinesLocked(updateCtx, updates, now)
		cancel()
		if err != nil {
			log.Warnf("[dns] domain revalidation nft update failed for %q: %v", domain, err)
			return
		}
	}
	for addr, lease := range entry.addresses {
		if expiry, stillObserved := confirmed[addr]; stillObserved {
			if expiry.After(lease.dnsUntil) {
				lease.dnsUntil = expiry
			}
		} else {
			if !m.holdsDomainCapacity(addr, lease, now) {
				delete(entry.addresses, addr)
				continue
			}
			lease.renewable = false
		}
		entry.addresses[addr] = lease
	}
	if len(entry.addresses) == 0 {
		delete(m.domains, domain)
	}
	entry.failures = 0
	entry.retryAt = time.Time{}
}

const (
	dynAllowV4Set  = "dyn_allow_v4"
	dynAllowV6Set  = "dyn_allow_v6"
	dynSetTimeoutS = 360
	// nftTTLSlackSec is added to the DNS TTL before clamping, so allow entries
	// slightly outlive the resolver cache and reduce races with short TTLs.
	nftTTLSlackSec = 60
	minTTLSec      = 60
	maxTTLSec      = 360 // max DNS TTL (300) + nftTTLSlackSec
)

// ResolvedIP is a single IP learned from DNS with TTL for dynamic nft set.
type ResolvedIP struct {
	Addr netip.Addr
	TTL  time.Duration
}

// answerDeadlines keeps the longest deadline per address and stops on the
// first distinct address beyond the caller's budget.
func answerDeadlines(ips []ResolvedIP, now time.Time, limit int) map[netip.Addr]time.Time {
	var deadlines map[netip.Addr]time.Time
	for _, ip := range ips {
		if addr := ip.Addr.Unmap(); addr.IsValid() {
			expiry := now.Add(ClampTTL(ip.TTL))
			if deadlines == nil {
				deadlines = make(map[netip.Addr]time.Time, min(len(ips), limit))
			}
			if expiry.After(deadlines[addr]) {
				deadlines[addr] = expiry
				if len(deadlines) > limit {
					break
				}
			}
		}
	}
	return deadlines
}

// ClampTTL turns a DNS TTL into the lifetime of a DNS-learned set element:
// the TTL plus nftTTLSlackSec, kept within [minTTLSec, maxTTLSec].
func ClampTTL(d time.Duration) time.Duration {
	sec := int(d.Seconds()) + nftTTLSlackSec
	sec = min(max(sec, minTTLSec), maxTTLSec)
	return time.Duration(sec) * time.Second
}

// buildRefreshResolvedIPsScript renders active connection refreshes with the
// full set timeout rather than the DNS TTL. Activity proves the address is
// still in use, and the final refresh after the connection closes makes this
// same bounded timeout the reconnect grace period.
func buildRefreshResolvedIPsScript(table string, ips []netip.Addr) string {
	elements := make([]ResolvedIP, 0, len(ips))
	for _, addr := range ips {
		elements = append(elements, ResolvedIP{
			Addr: addr,
			TTL:  dynSetTimeoutS * time.Second,
		})
	}
	return buildResolvedIPElementsScript(table, elements)
}

func buildResolvedIPElementsScript(table string, elements []ResolvedIP) string {
	var script strings.Builder
	for _, element := range elements {
		addr := element.Addr.Unmap()
		var setName string
		if addr.Is4() {
			setName = dynAllowV4Set
		} else if addr.Is6() {
			setName = dynAllowV6Set
		} else {
			continue
		}
		WriteTimedElement(&script, table, setName, addr, element.TTL)
	}
	return script.String()
}

// WriteTimedElement writes nft commands that leave addr in set with exactly
// ttl left, whether or not it is already there. Adding an element that already
// exists succeeds without changing its kernel expiry on some kernels (#1738),
// so the element is added, deleted and added again within the same script.
func WriteTimedElement(b *strings.Builder, table, set string, addr netip.Addr, ttl time.Duration) {
	fmt.Fprintf(b, "add element inet %s %s { %s }\n", table, set, addr)
	fmt.Fprintf(b, "delete element inet %s %s { %s }\n", table, set, addr)
	fmt.Fprintf(b, "add element inet %s %s { %s timeout %ds }\n", table, set, addr, int(ttl/time.Second))
}
