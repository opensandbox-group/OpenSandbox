// Copyright 2026 Alibaba Group Holding Ltd.
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
	addresses    map[netip.Addr]struct{}
	lastObserved time.Time
	lastAttempt  time.Time
	failures     int
	retryAt      time.Time
}

// nextExpiry returns the earliest dynamic-lease expiry across the entry's
// observed addresses. An address the tracker no longer carries (expired and
// inactive) yields the zero time, making the domain immediately due.
func (e *resolvedDomain) nextExpiry(tracker *connectionTracker) time.Time {
	var earliest time.Time
	for address := range e.addresses {
		expiresAt := tracker.dynamicIPs[address]
		if earliest.IsZero() || expiresAt.Before(earliest) {
			earliest = expiresAt
		}
	}
	return earliest
}

func (m *Manager) AddResolvedDomain(ctx context.Context, domain string, ips []ResolvedIP) error {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.domainPolicy == nil {
		return fmt.Errorf("no nftables policy applied; dropping resolved domain %q", domain)
	}
	if domain == "" || m.domainPolicy.Evaluate(domain) != policy.ActionAllow {
		return fmt.Errorf("resolved domain %q is no longer allowed", domain)
	}
	if err := m.addResolvedIPsLocked(ctx, ips); err != nil {
		return err
	}
	if m.domainPolicy.DefaultAction == policy.ActionAllow || len(ips) == 0 {
		return nil
	}
	entry := &resolvedDomain{addresses: make(map[netip.Addr]struct{}), lastObserved: m.tracker.now()}
	for _, ip := range ips {
		if ip.Addr.IsValid() && len(entry.addresses) < maxDomainAddresses {
			entry.addresses[ip.Addr.Unmap()] = struct{}{}
		}
	}
	if len(entry.addresses) == 0 {
		return nil
	}
	if previous := m.domains[domain]; previous != nil {
		entry.lastAttempt = previous.lastAttempt
		for address := range previous.addresses {
			if len(entry.addresses) < maxDomainAddresses {
				entry.addresses[address] = struct{}{}
			}
		}
	} else if len(m.domains) >= maxResolvedDomains {
		var oldest string
		for candidate, tracked := range m.domains {
			if oldest == "" || tracked.lastObserved.Before(m.domains[oldest].lastObserved) {
				oldest = candidate
			}
		}
		delete(m.domains, oldest)
	}
	m.domains[domain] = entry
	return nil
}

func (m *Manager) StartDomainRefresh(ctx context.Context, lookup func(context.Context, string) ([]ResolvedIP, error)) {
	safego.Go(func() {
		timer := time.NewTimer(jitteredRefreshDelay())
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
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
	now := m.tracker.now()
	due := now.Add(domainRefreshLead)
	candidates := make([]candidate, 0, len(m.domains))
	for domain, entry := range m.domains {
		// Only query domains whose earliest observed-address lease expires
		// within the lead window (or has already lapsed), unless a previous
		// failure put the domain on backoff.
		if now.Before(entry.retryAt) || entry.nextExpiry(m.tracker).After(due) {
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
						if expiry := item.entry.nextExpiry(m.tracker); expiry.After(now) {
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
	if ctx.Err() != nil || m.domains[domain] != entry || m.domainPolicy == nil || m.domainPolicy.Evaluate(domain) != policy.ActionAllow {
		return
	}
	entry.failures = 0
	entry.retryAt = time.Time{}
	var confirmed []ResolvedIP
	addresses := make(map[netip.Addr]struct{})
	now := m.tracker.now()
	for _, ip := range ips {
		address := ip.Addr.Unmap()
		if _, observed := entry.addresses[address]; observed {
			if !m.tracker.dynamicIPs[address].After(now.Add(clampTTL(ip.TTL))) {
				confirmed = append(confirmed, ip)
			}
			addresses[address] = struct{}{}
		}
	}
	if len(addresses) == 0 {
		delete(m.domains, domain)
		return
	}
	updateCtx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()
	if err := m.addResolvedIPsLocked(updateCtx, confirmed); err != nil {
		log.Warnf("[dns] domain revalidation nft update failed for %q: %v", domain, err)
		return
	}
	entry.addresses = addresses
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

// buildAddResolvedIPsScript returns a nft script fragment that
// adds resolved IPs to dyn_allow_v4/v6 with timeout.
func buildAddResolvedIPsScript(table string, ips []ResolvedIP) string {
	elements := make([]ResolvedIP, 0, len(ips))
	for _, r := range ips {
		elements = append(elements, ResolvedIP{
			Addr: r.Addr,
			TTL:  clampTTL(r.TTL),
		})
	}
	return buildResolvedIPElementsScript(table, elements)
}

func clampTTL(d time.Duration) time.Duration {
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
		fmt.Fprintf(&script, "add element inet %s %s { %s }\n", table, setName, addr)
		fmt.Fprintf(&script, "delete element inet %s %s { %s }\n", table, setName, addr)
		fmt.Fprintf(&script, "add element inet %s %s { %s timeout %ds }\n", table, setName, addr, int(element.TTL/time.Second))
	}
	return script.String()
}
