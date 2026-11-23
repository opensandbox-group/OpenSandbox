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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

const (
	tableName                        = "opensandbox"
	chainName                        = "egress"
	allowV4Set                       = "allow_v4"
	allowV6Set                       = "allow_v6"
	denyV4Set                        = "deny_v4"
	denyV6Set                        = "deny_v6"
	dohBlockV4Set                    = "doh_block_v4"
	dohBlockV6Set                    = "doh_block_v6"
	defaultConnectionRefreshInterval = 30 * time.Second
)

type runner func(ctx context.Context, script string) ([]byte, error)

type Options struct {
	// QuiesceOnApplyFailure freezes runtime writes on an unknown static effect.
	// Explicit Quiesce calls always take effect, regardless of this option.
	QuiesceOnApplyFailure bool
	BlockDoT              bool
	BlockDoH443           bool
	DoHBlocklistV4        []string
	DoHBlocklistV6        []string
	// ConnectionRefreshInterval controls how often active TCP connections renew
	// DNS-derived nft leases. Shorter intervals reduce the maximum temporary
	// reconnect gap, but increase /proc scans and nft updates. The 30-second
	// default is half the minimum 60-second DNS lease.
	ConnectionRefreshInterval time.Duration
	// UpstreamProxy, when set, adds an infrastructure-only accept scoped to the
	// mitmproxy UID (or forwarder mark) and the configured proxy endpoint.
	UpstreamProxy *UpstreamProxyEndpoint
	// Telemetry, when set, adds a capability-gated (SO_MARK) and endpoint-scoped
	// accept for the sidecar's own OTLP collector in forward mode (OSEP-0030).
	Telemetry *UpstreamProxyEndpoint
	// MitmRedirectPort, when > 0, is the transparent-MITM listener port the
	// nat OUTPUT REDIRECT rewrites intercepted dports to; the chain must
	// exempt that rewritten tuple ahead of port-scoped rules or every
	// intercepted flow is dropped (see the rule below). 0 emits nothing.
	MitmRedirectPort int
	// ForwardRedirectPort, when > 0, is the forward-mode forwarder listener
	// port (OSEP-0030); exempted like MitmRedirectPort for the same reason.
	ForwardRedirectPort int
}

var errStaticStateUncertain = errors.New("nftables policy state is uncertain; a successful policy apply is required")

type Manager struct {
	run          runner
	opts         Options
	mu           sync.Mutex
	quiesced     atomic.Bool
	tracker      *connectionTracker
	domainPolicy *policy.NetworkPolicy
	domains      map[string]*resolvedDomain

	staticStateUncertain bool // Protected by mu; cleared only by a successful apply.

	// lastDrops is the teardown set currently installed. A plain ApplyStatic
	// re-emits its still-relevant part, because every full apply starts with
	// `delete table` and would otherwise reopen flows a dropEstablished commit
	// closed (e.g. ones still retransmitting).
	lastDrops EstablishedDrops
	// lastDropsAt is when the dropEstablished commit behind lastDrops was
	// applied. Carries keep the original timestamp and stop once the conntrack
	// established timeout has passed, so a sandbox that tightens once does not
	// hold (and re-emit) its teardown set forever.
	lastDropsAt time.Time
	now         func() time.Time
}

func NewManagerWithRunner(r runner) *Manager {
	return newManager(r, Options{BlockDoT: true})
}

func NewManagerWithRunnerAndOptions(r runner, opts Options) *Manager {
	return newManager(r, opts)
}

func NewManagerWithOptions(opts Options) *Manager {
	return newManager(defaultRunner, opts)
}

func newManager(r runner, opts Options) *Manager {
	if opts.ConnectionRefreshInterval <= 0 {
		opts.ConnectionRefreshInterval = defaultConnectionRefreshInterval
	}
	return &Manager{
		run:     r,
		opts:    opts,
		tracker: newConnectionTracker(),
		domains: make(map[string]*resolvedDomain),
		now:     time.Now,
	}
}

func (m *Manager) ApplyStatic(ctx context.Context, p *policy.NetworkPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	var buildDrops staticDropBuilder
	if !m.lastDrops.Empty() && m.now().Sub(m.lastDropsAt) < establishedCarryTTL() {
		buildDrops = func(at time.Time) (EstablishedDrops, time.Time) {
			learned, expiry := m.liveLearnedAddrsLocked(at)
			drops := m.lastDrops.stillRelevant(p, learned)
			return drops, expiry
		}
	}
	_, err := m.applyStaticDropEstablishedLocked(ctx, p, buildDrops, nil, m.lastDropsAt)
	return err
}

// liveLearnedAddrsLocked returns live DNS addresses and their earliest expiry.
func (m *Manager) liveLearnedAddrsLocked(now time.Time) (addrs []netip.Addr, nextExpiry time.Time) {
	for _, entry := range m.domains {
		if entry == nil {
			continue
		}
		for addr, lease := range entry.addresses {
			if expiry := lease.deadline(); expiry.After(now) {
				addrs = append(addrs, addr)
				if nextExpiry.IsZero() || expiry.Before(nextExpiry) {
					nextExpiry = expiry
				}
			}
		}
	}
	return addrs, nextExpiry
}

// applyStaticDropEstablished applies p like ApplyStatic, and when drops is
// non-empty installs destination drops ahead of the established/related accept
// so keep-alive flows to removed allows stop on the next packet. The returned
// count is the number of matching TCP conntrack flows observed just before the
// apply (best-effort; 0 when /proc/net/nf_conntrack is unavailable).
//
// Test-only: drops is a caller-supplied snapshot counted and computed before
// m.mu is taken, so a concurrent AddResolvedDomain can admit an address in
// between. Production code must use ApplyStaticComputingEstablishedDrops,
// which snapshots, computes and applies under one lock.
func (m *Manager) applyStaticDropEstablished(ctx context.Context, p *policy.NetworkPolicy, drops EstablishedDrops) (int, error) {
	var flows []byte
	if !drops.Empty() {
		flows = snapshotTeardownFlows()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	fixedDrops := func(time.Time) (EstablishedDrops, time.Time) { return drops, time.Time{} }
	return m.applyStaticDropEstablishedLocked(ctx, p, fixedDrops, flows, m.now())
}

// ApplyStaticComputingEstablishedDrops snapshots DNS leases, computes the
// teardown set, and applies newPol under one manager lock so AddResolvedDomain
// cannot admit an old-policy address between snapshot and apply.
func (m *Manager) ApplyStaticComputingEstablishedDrops(ctx context.Context, oldPol, newPol *policy.NetworkPolicy) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced.Load() {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: ErrQuiesced}
	}
	if err := ctx.Err(); err != nil {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	if newPol == nil {
		newPol = policy.DefaultDenyPolicy()
	}
	flows := snapshotTeardownFlows()
	buildDrops := func(at time.Time) (EstablishedDrops, time.Time) {
		prior, live := m.snapshotResolvedDomainsLocked(at)
		return computeEstablishedDrops(oldPol, newPol, prior, live), time.Time{}
	}
	return m.applyStaticDropEstablishedLocked(ctx, newPol, buildDrops, flows, m.now())
}

func snapshotTeardownFlows() []byte {
	flows, err := readConntrackFlows()
	if err != nil {
		log.Warnf("nftables: counting conntrack flows for established drops failed: %v", err)
	}
	return flows
}

type staticApplyPlan struct {
	script         string
	drops          EstablishedDrops
	domains        map[string]*resolvedDomain
	leases         map[netip.Addr]time.Time
	active         map[netip.Addr]struct{}
	expiredCredits []dnsRefreshCredit
	nextExpiry     time.Time
}

// In addition to carried grants, plain reloads may consult live addresses
// from removed sources to avoid tearing down flows without an explicit opt-in.
type staticDropBuilder func(time.Time) (EstablishedDrops, time.Time)

func (m *Manager) applyStaticDropEstablishedLocked(ctx context.Context, p *policy.NetworkPolicy, buildDrops staticDropBuilder, flows []byte, committedAt time.Time) (int, error) {
	if m.quiesced.Load() {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: ErrQuiesced}
	}
	if err := ctx.Err(); err != nil {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	plan, err := m.prepareStaticApplyLocked(ctx, p, buildDrops, true)
	if err != nil {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	if _, err = m.run(ctx, plan.script); IsMissingTableError(err, tableName) {
		// The first batch may have spent part or all of a lease. Re-plan before
		// retrying instead of reusing relative timeouts or stale keeps.
		fallback, retryErr := m.prepareStaticApplyLocked(ctx, p, buildDrops, false)
		if retryErr != nil {
			retryErr = &ApplyError{Effect: ApplyUnchanged, Err: retryErr}
		} else {
			_, retryErr = m.run(ctx, fallback.script)
		}
		if retryErr == nil {
			plan, err = fallback, nil
		} else {
			effect := ApplyUnknown
			if ApplyEffectOf(err) == ApplyUnchanged && ApplyEffectOf(retryErr) == ApplyUnchanged {
				effect = ApplyUnchanged
			}
			err = &ApplyError{Effect: effect, Err: fmt.Errorf("nft missing-table fallback failed: %w", errors.Join(err, retryErr))}
		}
	}
	if err != nil {
		// Keep the failure and freeze in the same critical section so queued
		// writers cannot publish old permits before the owner receives it.
		if ApplyEffectOf(err) != ApplyUnchanged {
			m.staticStateUncertain = true
			if m.opts.QuiesceOnApplyFailure {
				m.quiesced.Store(true)
			}
		}
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpStaticApply)
		return 0, err
	}
	for _, credit := range plan.expiredCredits {
		lease := credit.domain.addresses[credit.addr]
		lease.tcpUntil = time.Time{}
		credit.domain.addresses[credit.addr] = lease
	}
	m.tracker.dynamicIPs = plan.leases
	m.tracker.directIPs = nil
	m.tracker.previousActiveIPs = plan.active
	if len(plan.active) == 0 {
		m.tracker.previousActiveGrace = time.Time{}
	}
	m.domainPolicy = p
	m.domains = plan.domains
	m.staticStateUncertain = false
	m.setLastDropsLocked(plan.drops, committedAt)
	telemetry.SetNftablesRuleCount(telemetry.NftRuleCountFromPolicy(p))
	telemetry.RecordNftablesUpdate()
	matched := countMatchingConntrackFlowsData(flows, plan.drops)
	log.Infof("nftables: static policy applied successfully (established_drop_flows=%d)", matched)
	return matched, nil
}

func (m *Manager) prepareStaticApplyLocked(ctx context.Context, p *policy.NetworkPolicy, buildDrops staticDropBuilder, replace bool) (staticApplyPlan, error) {
	// Reserve space for decimal seconds so we can fill timeouts after all
	// address formatting and fallback rewriting. Spaces are nft whitespace.
	const timeoutSlot = "          " // Enough digits for a time.Duration in seconds.
	type timedElement struct {
		offset int
		expiry time.Time
	}
	at := m.tracker.now()
	for {
		if err := ctx.Err(); err != nil {
			return staticApplyPlan{}, err
		}
		plan := m.retainedDomainLeasesLocked(p, at, buildDrops != nil)
		if buildDrops != nil {
			var dropExpiry time.Time
			plan.drops, dropExpiry = buildDrops(at)
			if !dropExpiry.IsZero() && (plan.nextExpiry.IsZero() || dropExpiry.Before(plan.nextExpiry)) {
				plan.nextExpiry = dropExpiry
			}
		}
		script, err := buildRuleset(p, m.opts, plan.drops)
		if err != nil {
			return staticApplyPlan{}, err
		}
		renderedAt := m.tracker.now()
		if !plan.nextExpiry.IsZero() && !plan.nextExpiry.After(renderedAt) {
			at = renderedAt
			continue
		}
		if !replace {
			script = RemoveDeleteTableLine(script, tableName)
		}
		var b bytes.Buffer
		elements := make([]timedElement, 0, len(plan.leases))
		if len(plan.leases) != 0 {
			if !strings.HasSuffix(script, "\n") {
				b.WriteByte('\n')
			}
			for addr, expiry := range plan.leases {
				fmt.Fprintf(&b, "add element inet %s %s { %s timeout ", tableName, resolvedIPSet(addr), addr)
				elements = append(elements, timedElement{offset: b.Len(), expiry: expiry})
				b.WriteString(timeoutSlot + "s }\n")
			}
		}
		if !plan.nextExpiry.IsZero() {
			readyAt := m.tracker.now()
			if !plan.nextExpiry.After(readyAt) {
				// Recompute both grants and teardown when membership changes.
				at = readyAt
				continue
			}
			if len(elements) != 0 {
				raw := b.Bytes()
				var digits [len(timeoutSlot)]byte
				for _, elem := range elements {
					remaining := elem.expiry.Sub(readyAt)
					seconds := remaining / time.Second
					if remaining%time.Second != 0 {
						seconds++
					}
					ttl := strconv.AppendInt(digits[:0], int64(seconds), 10)
					end := elem.offset + len(timeoutSlot)
					copy(raw[end-len(ttl):end], ttl)
				}
				script += string(raw)
			}
		}
		plan.script = script
		return plan, nil
	}
}

func (m *Manager) retainedDomainLeasesLocked(p *policy.NetworkPolicy, now time.Time, trackTeardown bool) staticApplyPlan {
	plan := staticApplyPlan{
		domains: make(map[string]*resolvedDomain, len(m.domains)),
		leases:  make(map[netip.Addr]time.Time),
	}
	keep := p.DefaultAction == policy.ActionDeny
	if !keep && !trackTeardown {
		return plan
	}
	for name, current := range m.domains {
		// Revocations use historical addresses. Only sources the replacement
		// still allows can change the teardown decision when they expire.
		if p.Evaluate(name) != policy.ActionAllow {
			continue
		}
		if keep {
			// Fence lookups begun against the old rules after acknowledgement.
			copy := *current
			plan.domains[name] = &copy
		}
		for addr, lease := range current.addresses {
			expiry := lease.deadline()
			if !expiry.After(now) {
				// Another source may keep this IP active. Do not let that
				// keep the expired source's old poll credit alive as well.
				if keep && m.tracker.previouslyCredited(addr, lease.tcpUntil) {
					plan.expiredCredits = append(plan.expiredCredits, dnsRefreshCredit{domain: plan.domains[name], addr: addr})
				}
				continue
			}
			if plan.nextExpiry.IsZero() || expiry.Before(plan.nextExpiry) {
				plan.nextExpiry = expiry
			}
			if !keep {
				continue
			}
			if expiry.After(plan.leases[addr]) {
				plan.leases[addr] = expiry
			}
			if m.tracker.previouslyCredited(addr, lease.tcpUntil) {
				if plan.active == nil {
					plan.active = make(map[netip.Addr]struct{})
				}
				plan.active[addr] = struct{}{}
			}
		}
	}
	return plan
}

func (m *Manager) setLastDropsLocked(drops EstablishedDrops, committedAt time.Time) {
	m.lastDrops = drops
	if drops.Empty() {
		m.lastDropsAt = time.Time{}
		return
	}
	m.lastDropsAt = committedAt
}

func (m *Manager) AddResolvedIPs(ctx context.Context, ips []ResolvedIP) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced.Load() {
		return ErrQuiesced
	}
	if m.staticStateUncertain {
		return errStaticStateUncertain
	}
	return m.addResolvedIPsLocked(ctx, ips)
}

// addResolvedIPsLocked completes an admitted operation. The caller must hold
// m.mu and have checked admission before changing any associated state. Do not
// recheck here: Quiesce may close admission while that operation is draining.
func (m *Manager) addResolvedIPsLocked(ctx context.Context, ips []ResolvedIP) error {
	if len(ips) == 0 {
		return nil
	}
	now := m.tracker.now()
	deadlines := answerDeadlines(ips, now, len(ips))
	if err := m.addResolvedDeadlinesLocked(ctx, deadlines, now); err != nil {
		return err
	}
	if len(deadlines) != 0 && m.domainPolicy != nil && m.domainPolicy.DefaultAction == policy.ActionDeny {
		if m.tracker.directIPs == nil {
			m.tracker.directIPs = make(map[netip.Addr]struct{})
		}
		for addr := range deadlines {
			m.tracker.directIPs[addr] = struct{}{}
		}
	}
	return nil
}

// addResolvedDeadlinesLocked consumes the caller's batch. The kernel lease
// must not shorten an independently learned lease or a TCP reconnect grace.
func (m *Manager) addResolvedDeadlinesLocked(ctx context.Context, deadlines map[netip.Addr]time.Time, now time.Time) error {
	if len(deadlines) == 0 {
		return nil
	}
	var b strings.Builder
	for addr, expiry := range deadlines {
		if previous := m.tracker.dynamicIPs[addr]; previous.After(expiry) {
			expiry = previous
			deadlines[addr] = expiry
		}
		set := resolvedIPSet(addr)
		// nft uses whole seconds; round up rather than shorten the deadline.
		WriteTimedElement(&b, tableName, set, addr, expiry.Sub(now)+time.Second-time.Nanosecond)
	}
	script := b.String()
	log.Debugf("nftables: adding %d resolved IP(s) to dynamic allow sets with script statement %s", len(deadlines), script)
	_, err := m.run(ctx, script)
	if err != nil {
		// The policy allows these destinations but the kernel does not know it yet, so
		// the chain's final rule drops them. Indistinguishable from a policy denial
		// inside the sandbox, hence its own counter.
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	for addr, expiry := range deadlines {
		m.tracker.dynamicIPs[addr] = expiry
	}
	telemetry.RecordNftablesUpdate()
	return nil
}

func resolvedIPSet(addr netip.Addr) string {
	if addr.Is4() {
		return dynAllowV4Set
	}
	return dynAllowV6Set
}

// StartConnectionRefresh keeps DNS-learned IPs authorized while a TCP
// connection to them is active; the set timeout remains as the grace period
// after the connection closes.
//
// Renewal is best-effort. Default-deny DNS grants can be renewed only while
// a source lease is live or has credit from the previous active poll. The
// final poll after close extends existing credit; it does not admit new
// sources. TCP connections that open and close between polls are not seen.
// UDP and QUIC rely on DNS-driven refresh.
func (m *Manager) StartConnectionRefresh(ctx context.Context) {
	m.tracker.start(ctx, m.opts.ConnectionRefreshInterval, m)
}

// RemoveEnforcement drops inet opensandbox; missing table is not an error.
// Shutdown may remove the table even when quiesced, but never unfreezes writes.
func (m *Manager) RemoveEnforcement(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	script := fmt.Sprintf("delete table inet %s\n", tableName)
	_, err := m.run(ctx, script)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "no such file") && !strings.Contains(msg, "does not exist") {
			telemetry.RecordNftablesUpdateFailed(telemetry.NftOpRemove)
			return err
		}
		log.Infof("nftables: table inet %s already absent", tableName)
	} else {
		log.Infof("nftables: removed table inet %s", tableName)
	}
	m.tracker.clear()
	m.domainPolicy = nil
	m.domains = make(map[string]*resolvedDomain)
	m.setLastDropsLocked(EstablishedDrops{}, time.Time{})
	return nil
}

func buildRuleset(p *policy.NetworkPolicy, opts Options, drops EstablishedDrops) (string, error) {
	allowV4, allowV6, denyV4, denyV6 := p.StaticIPSets()
	var err error
	if allowV4, err = normalizeNFTIntervalSet(allowV4); err != nil {
		return "", err
	}
	if allowV6, err = normalizeNFTIntervalSet(allowV6); err != nil {
		return "", err
	}
	if denyV4, err = normalizeNFTIntervalSet(denyV4); err != nil {
		return "", err
	}
	if denyV6, err = normalizeNFTIntervalSet(denyV6); err != nil {
		return "", err
	}
	dohBlockV4 := opts.DoHBlocklistV4
	dohBlockV6 := opts.DoHBlocklistV6
	if len(dohBlockV4) > 0 {
		if dohBlockV4, err = normalizeNFTIntervalSet(dohBlockV4); err != nil {
			return "", err
		}
	}
	if len(dohBlockV6) > 0 {
		if dohBlockV6, err = normalizeNFTIntervalSet(dohBlockV6); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "delete table inet %s\n", tableName)
	fmt.Fprintf(&b, "add table inet %s\n", tableName)

	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", tableName, allowV4Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", tableName, denyV4Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", tableName, allowV6Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", tableName, denyV6Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; timeout %ds; }\n", tableName, dynAllowV4Set, dynSetTimeoutS)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; timeout %ds; }\n", tableName, dynAllowV6Set, dynSetTimeoutS)

	if len(dohBlockV4) > 0 {
		fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", tableName, dohBlockV4Set)
	}
	if len(dohBlockV6) > 0 {
		fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", tableName, dohBlockV6Set)
	}

	writeElements(&b, allowV4Set, allowV4)
	writeElements(&b, denyV4Set, denyV4)
	writeElements(&b, allowV6Set, allowV6)
	writeElements(&b, denyV6Set, denyV6)
	writeElements(&b, dohBlockV4Set, dohBlockV4)
	writeElements(&b, dohBlockV6Set, dohBlockV6)

	chainPolicy := "drop"
	if p.DefaultAction == policy.ActionAllow {
		chainPolicy = "accept"
	}
	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook output priority 0; policy %s; }\n", tableName, chainName, chainPolicy)
	// Infrastructure accepts come first so established-flow teardown can never
	// catch them: DNS replies from the :15353 redirect (ct established, daddr
	// the sandbox's own address), loopback TCP, marked proxy upstream flows,
	// IPv6 ND and the upstream-proxy dial are all accepted before any drop.
	fmt.Fprintf(&b, "add rule inet %s %s meta mark %s accept\n", tableName, chainName, constants.MarkHex)
	fmt.Fprintf(&b, "add rule inet %s %s oifname \"lo\" accept\n", tableName, chainName)
	// IPv6 neighbor discovery is locally generated ICMPv6 that never matches an allow set: without
	// this rule a host whose default route is IPv6 (a link-local gateway) loses its neighbor entry
	// as soon as the cache goes stale and becomes unreachable in both directions.
	fmt.Fprintf(&b, "add rule inet %s %s icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit } accept\n", tableName, chainName)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr 127.0.0.1 udp dport 15353 accept\n", tableName, chainName)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr 127.0.0.1 tcp dport 15353 accept\n", tableName, chainName)
	// The ip6 OUTPUT REDIRECTs (DNS → :15353, MITM → :18081) land on ::1 with the original egress
	// interface still selected, so oifname "lo" does not match them: ::1 is the v6 loopback rule.
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr ::1 accept\n", tableName, chainName)
	// The v4 MITM redirect lands on 127.0.0.1 with the original egress interface
	// still selected, so oifname "lo" does not match: exempt the rewritten dport
	// ahead of port-scoped rules (mirrors the dnsproxy rules above). Enforcement
	// is intact — mitmproxy's own re-originated connections are not exempt — and
	// direct workload dials already pass via oifname "lo"; v6 is covered by ::1.
	if opts.MitmRedirectPort > 0 {
		fmt.Fprintf(&b, "add rule inet %s %s ip daddr 127.0.0.1 tcp dport %d accept\n", tableName, chainName, opts.MitmRedirectPort)
	}
	if opts.ForwardRedirectPort > 0 {
		fmt.Fprintf(&b, "add rule inet %s %s ip daddr 127.0.0.1 tcp dport %d accept\n", tableName, chainName, opts.ForwardRedirectPort)
	}
	if ep := opts.UpstreamProxy; ep != nil {
		b.WriteString(buildUpstreamProxyStatic(tableName, ep))
	}
	if ep := opts.Telemetry; ep != nil {
		b.WriteString(buildTelemetryStatic(tableName, ep))
	}
	// Explicit denies come before the DNS-learned accepts, mirroring the
	// new-connection path: an allowed domain that re-resolves to an explicitly
	// denied IP (shared CDN) must not resurrect a flow the caller asked to kill.
	writeEstablishedTargets(&b, drops.DenyAddrs, drops.DenyPrefixes, drops.DenyPortDrops, "drop")
	// DNS-learned allows bypass the removed-allow teardown so a later resolve
	// (shared CDN IP) or a carried live lease is not pinned by a stale drop.
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s ct state established,related accept\n", tableName, chainName, dynAllowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s ct state established,related accept\n", tableName, chainName, dynAllowV6Set)
	// Then still-allowed destinations (keeps and the static allow sets), then
	// removed allows, then the blanket verdict.
	writeEstablishedTargets(&b, drops.KeepAddrs, drops.KeepPrefixes, drops.KeepPorts, "accept")
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s ct state established,related accept\n", tableName, chainName, allowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s ct state established,related accept\n", tableName, chainName, allowV6Set)
	writeEstablishedTargets(&b, drops.Addrs, drops.Prefixes, drops.PortDrops, "drop")
	if drops.CatchAll {
		fmt.Fprintf(&b, "add rule inet %s %s ct state established,related drop\n", tableName, chainName)
	} else {
		fmt.Fprintf(&b, "add rule inet %s %s ct state established,related accept\n", tableName, chainName)
	}
	if opts.BlockDoT {
		fmt.Fprintf(&b, "add rule inet %s %s tcp dport 853 drop\n", tableName, chainName)
		fmt.Fprintf(&b, "add rule inet %s %s udp dport 853 drop\n", tableName, chainName)
	}
	if opts.BlockDoH443 {
		if len(dohBlockV4) == 0 && len(dohBlockV6) == 0 {
			// strict: drop all 443 when enabled but no blocklist provided
			fmt.Fprintf(&b, "add rule inet %s %s tcp dport 443 drop\n", tableName, chainName)
		} else {
			if len(dohBlockV4) > 0 {
				fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s tcp dport 443 drop\n", tableName, chainName, dohBlockV4Set)
			}
			if len(dohBlockV6) > 0 {
				fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s tcp dport 443 drop\n", tableName, chainName, dohBlockV6Set)
			}
		}
	}
	portDeny, portAllow := p.PortScopedRules()
	// Port-scoped deny before every allow; port-scoped allow after the
	// plain deny set, before the dynamic/plain allow sets.
	writePortScopedRules(&b, portDeny, "drop")
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s drop\n", tableName, chainName, denyV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s drop\n", tableName, chainName, denyV6Set)
	writePortScopedRules(&b, portAllow, "accept")
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s accept\n", tableName, chainName, dynAllowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s accept\n", tableName, chainName, dynAllowV6Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s accept\n", tableName, chainName, allowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s accept\n", tableName, chainName, allowV6Set)
	if chainPolicy == "drop" {
		fmt.Fprintf(&b, "add rule inet %s %s drop\n", tableName, chainName)
	}

	return b.String(), nil
}

func writeEstablishedTargets(b *strings.Builder, addrs []netip.Addr, prefixes []netip.Prefix, ports []EstablishedPortDrop, verb string) {
	for _, addr := range addrs {
		writeEstablishedAddrRule(b, addr, verb)
	}
	for _, pfx := range prefixes {
		writeEstablishedPrefixRule(b, pfx, verb)
	}
	for _, pd := range ports {
		writeEstablishedPortRule(b, pd, verb)
	}
}

func writeEstablishedAddrRule(b *strings.Builder, addr netip.Addr, verb string) {
	if !addr.IsValid() {
		return
	}
	addr = addr.Unmap()
	if addr.Is4() {
		fmt.Fprintf(b, "add rule inet %s %s ip daddr %s ct state established,related %s\n", tableName, chainName, addr, verb)
	} else if addr.Is6() {
		fmt.Fprintf(b, "add rule inet %s %s ip6 daddr %s ct state established,related %s\n", tableName, chainName, addr, verb)
	}
}

func writeEstablishedPrefixRule(b *strings.Builder, pfx netip.Prefix, verb string) {
	if !pfx.IsValid() {
		return
	}
	pfx = pfx.Masked()
	if pfx.Addr().Is4() {
		fmt.Fprintf(b, "add rule inet %s %s ip daddr %s ct state established,related %s\n", tableName, chainName, pfx, verb)
	} else if pfx.Addr().Is6() {
		fmt.Fprintf(b, "add rule inet %s %s ip6 daddr %s ct state established,related %s\n", tableName, chainName, pfx, verb)
	}
}

func writeEstablishedPortRule(b *strings.Builder, pd EstablishedPortDrop, verb string) {
	members := make([]string, 0, len(pd.Ports)+len(pd.Ranges))
	for _, port := range pd.Ports {
		members = append(members, fmt.Sprintf("%d", port))
	}
	for _, rg := range pd.Ranges {
		members = append(members, fmt.Sprintf("%d-%d", rg.Lo, rg.Hi))
	}
	if len(members) == 0 {
		return
	}
	portsClause := strings.Join(members, ", ")
	switch {
	case pd.Addr.IsValid():
		addr := pd.Addr.Unmap()
		daddr := "ip daddr"
		if addr.Is6() {
			daddr = "ip6 daddr"
		}
		fmt.Fprintf(b, "add rule inet %s %s %s %s tcp dport { %s } ct state established,related %s\n",
			tableName, chainName, daddr, addr, portsClause, verb)
	case pd.Prefix.IsValid():
		pfx := pd.Prefix.Masked()
		daddr := "ip daddr"
		if pfx.Addr().Is6() {
			daddr = "ip6 daddr"
		}
		fmt.Fprintf(b, "add rule inet %s %s %s %s tcp dport { %s } ct state established,related %s\n",
			tableName, chainName, daddr, pfx, portsClause, verb)
	default:
		fmt.Fprintf(b, "add rule inet %s %s tcp dport { %s } ct state established,related %s\n",
			tableName, chainName, portsClause, verb)
	}
}

func writeElements(b *strings.Builder, setName string, elems []string) {
	if len(elems) == 0 {
		return
	}
	fmt.Fprintf(b, "add element inet %s %s { %s }\n", tableName, setName, strings.Join(elems, ", "))
}

// writePortScopedRules emits one "add rule" line per rule, preserving
// declaration order. A rule with no Target applies to all destinations
// (bare "tcp dport { ... }" covers both address families); the dport set
// mixes ports and lo-hi intervals. TCP only.
func writePortScopedRules(b *strings.Builder, rules []policy.PortScopedRule, verb string) {
	for _, r := range rules {
		members := make([]string, 0, len(r.Ports)+len(r.Ranges))
		for _, port := range r.Ports {
			members = append(members, fmt.Sprintf("%d", port))
		}
		for _, rg := range r.Ranges {
			members = append(members, fmt.Sprintf("%d-%d", rg.Lo, rg.Hi))
		}
		portsClause := strings.Join(members, ", ")
		if r.Target == "" {
			fmt.Fprintf(b, "add rule inet %s %s tcp dport { %s } %s\n", tableName, chainName, portsClause, verb)
			continue
		}
		daddr := "ip daddr"
		if r.IsV6 {
			daddr = "ip6 daddr"
		}
		fmt.Fprintf(b, "add rule inet %s %s %s %s tcp dport { %s } %s\n", tableName, chainName, daddr, r.Target, portsClause, verb)
	}
}

func defaultRunner(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	return runNftCommand(ctx, cmd)
}

func runNftCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return output.Bytes(), &ApplyError{Effect: ApplyUnchanged, Err: fmt.Errorf("nft start failed: %w (output: %s)", err, strings.TrimSpace(output.String()))}
	}
	// A nonzero exit or diagnostic text cannot prove that no kernel operation
	// ran. Wait also reports cancellation, signals and I/O-copy failures.
	if err := cmd.Wait(); err != nil {
		return output.Bytes(), &ApplyError{Effect: ApplyUnknown, Err: fmt.Errorf("nft apply failed: %w (output: %s)", err, strings.TrimSpace(output.String()))}
	}
	return output.Bytes(), nil
}

// deleteTableStatement matches `delete table inet <table>` naming the whole
// table, so the statement for "opensandbox" does not also match
// "opensandbox-fast-sandbox".
func deleteTableStatement(table string) *regexp.Regexp {
	return regexp.MustCompile(`delete table inet ` + regexp.QuoteMeta(table) + `(?:[^A-Za-z0-9_./-]|$)`)
}

// IsMissingTableError reports whether a batch failed because its
// `delete table inet <table>` line targets a table that does not exist yet
// (first start, or a reset that already removed it).
func IsMissingTableError(err error, table string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file or directory") && deleteTableStatement(strings.ToLower(table)).MatchString(msg)
}

// RemoveDeleteTableLine drops the `delete table inet <table>` line, and blank
// lines, so a batch that failed on a missing table can be retried.
func RemoveDeleteTableLine(script, table string) string {
	statement := deleteTableStatement(table)
	lines := strings.Split(script, "\n")
	var filtered []string
	for _, l := range lines {
		if loc := statement.FindStringIndex(l); loc != nil && loc[0] == 0 {
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		filtered = append(filtered, l)
	}
	return strings.Join(filtered, "\n")
}
