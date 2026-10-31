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
	// mitmproxy UID and the configured proxy endpoint (IP + TCP port). The
	// proxy dial must not depend on the sandbox allow sets: those sets are
	// IP-only and unscoped by UID, so putting the proxy there would let
	// sandbox workloads CONNECT it directly and bounce to denied destinations.
	UpstreamProxy *UpstreamProxyEndpoint
	// MitmRedirectPort, when > 0, is the transparent-MITM listener port the
	// nat OUTPUT REDIRECT rewrites intercepted dports to; the chain must
	// exempt that rewritten tuple ahead of port-scoped rules or every
	// intercepted flow is dropped (see the rule below). 0 emits nothing.
	MitmRedirectPort int
}

type Manager struct {
	run          runner
	opts         Options
	mu           sync.Mutex
	quiesced     atomic.Bool
	tracker      *connectionTracker
	domainPolicy *policy.NetworkPolicy
	domains      map[string]*resolvedDomain
	// lastDrops is the teardown set currently installed. A plain ApplyStatic
	// re-emits its still-relevant part, because every full apply starts with
	// `delete table` and would otherwise reopen flows a dropEstablished commit
	// closed (e.g. ones still retransmitting).
	lastDrops EstablishedDrops
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
	}
}

func (m *Manager) ApplyStatic(ctx context.Context, p *policy.NetworkPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	var carry EstablishedDrops
	if !m.lastDrops.Empty() {
		carry = m.lastDrops.stillRelevant(p, m.learnedAddrsLocked())
	}
	_, err := m.applyStaticDropEstablishedLocked(ctx, p, carry, 0)
	return err
}

// learnedAddrsLocked returns every DNS-learned address currently tracked.
func (m *Manager) learnedAddrsLocked() []netip.Addr {
	var out []netip.Addr
	for _, entry := range m.domains {
		if entry == nil {
			continue
		}
		for addr := range entry.addresses {
			out = append(out, addr)
		}
	}
	return out
}

// ApplyStaticDropEstablished applies p like ApplyStatic, and when drops is
// non-empty installs destination drops ahead of the established/related accept
// so keep-alive flows to removed allows stop on the next packet. The returned
// count is the number of matching TCP conntrack flows observed just before the
// apply (best-effort; 0 when /proc/net/nf_conntrack is unavailable).
func (m *Manager) ApplyStaticDropEstablished(ctx context.Context, p *policy.NetworkPolicy, drops EstablishedDrops) (int, error) {
	matched, countErr := countMatchingConntrackFlows(drops)
	if countErr != nil {
		log.Warnf("nftables: counting conntrack flows for established drops failed: %v", countErr)
		matched = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyStaticDropEstablishedLocked(ctx, p, drops, matched)
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
	prior := m.snapshotResolvedDomainsLocked()
	drops := ComputeEstablishedDrops(oldPol, newPol, prior)
	matched, countErr := countMatchingConntrackFlows(drops)
	if countErr != nil {
		log.Warnf("nftables: counting conntrack flows for established drops failed: %v", countErr)
		matched = 0
	}
	return m.applyStaticDropEstablishedLocked(ctx, newPol, drops, matched)
}

func (m *Manager) applyStaticDropEstablishedLocked(ctx context.Context, p *policy.NetworkPolicy, drops EstablishedDrops, matched int) (int, error) {
	if m.quiesced.Load() {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: ErrQuiesced}
	}
	if err := ctx.Err(); err != nil {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	allowV4, allowV6, denyV4, denyV6 := p.StaticIPSets()
	log.Infof("nftables: applying static policy: default=%s, allow_v4=%d, allow_v6=%d, deny_v4=%d, deny_v6=%d, established_drops=%d catch_all=%v",
		p.DefaultAction, len(allowV4), len(allowV6), len(denyV4), len(denyV6), len(drops.DenyAddrs)+len(drops.DenyPrefixes)+len(drops.DenyPortDrops)+len(drops.Addrs)+len(drops.Prefixes)+len(drops.PortDrops), drops.CatchAll)
	script, err := buildRuleset(p, m.opts, drops)
	if err != nil {
		return 0, &ApplyError{Effect: ApplyUnchanged, Err: err}
	}
	if _, err := m.run(ctx, script); err != nil {
		if isMissingTableError(err) {
			fallback := removeDeleteTableLine(script)
			if fallback != script {
				if _, retryErr := m.run(ctx, fallback); retryErr == nil {
					m.tracker.clear()
					m.domainPolicy = p
					m.domains = make(map[string]*resolvedDomain)
					m.lastDrops = drops
					telemetry.SetNftablesRuleCount(telemetry.NftRuleCountFromPolicy(p))
					telemetry.RecordNftablesUpdate()
					return matched, nil
				} else {
					effect := ApplyUnknown
					if ApplyEffectOf(err) == ApplyUnchanged && ApplyEffectOf(retryErr) == ApplyUnchanged {
						effect = ApplyUnchanged
					}
					err = &ApplyError{Effect: effect, Err: fmt.Errorf("nft missing-table fallback failed: %w", errors.Join(err, retryErr))}
				}
			}
		}
		// Keep the failure and freeze in the same critical section so queued
		// writers cannot publish old permits before the owner receives it.
		if m.opts.QuiesceOnApplyFailure && ApplyEffectOf(err) != ApplyUnchanged {
			m.quiesced.Store(true)
		}
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpStaticApply)
		return 0, err
	}
	m.tracker.clear()
	m.domainPolicy = p
	m.domains = make(map[string]*resolvedDomain)
	m.lastDrops = drops
	telemetry.SetNftablesRuleCount(telemetry.NftRuleCountFromPolicy(p))
	telemetry.RecordNftablesUpdate()
	log.Infof("nftables: static policy applied successfully (established_drop_flows=%d)", matched)
	return matched, nil
}

func (m *Manager) AddResolvedIPs(ctx context.Context, ips []ResolvedIP) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced.Load() {
		return ErrQuiesced
	}
	return m.addResolvedIPsLocked(ctx, ips)
}

// addResolvedIPsLocked completes an admitted operation. The caller must hold
// m.mu and have checked admission before changing any associated state. Do not
// recheck here: Quiesce may close admission while that operation is draining.
func (m *Manager) addResolvedIPsLocked(ctx context.Context, ips []ResolvedIP) error {
	script := buildAddResolvedIPsScript(tableName, ips)
	if script == "" {
		return nil
	}
	log.Debugf("nftables: adding %d resolved IP(s) to dynamic allow sets with script statement %s", len(ips), script)
	_, err := m.run(ctx, script)
	if err != nil {
		// The policy allows these destinations but the kernel does not know it yet, so
		// the chain's final rule drops them. Indistinguishable from a policy denial
		// inside the sandbox, hence its own counter.
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	m.tracker.setDynamicIPs(ips)
	telemetry.RecordNftablesUpdate()
	return nil
}

// StartConnectionRefresh keeps DNS-learned IPs authorized while a TCP
// connection to them is active; the set timeout remains as the grace period
// after the connection closes.
//
// Renewal is best-effort: a connection that starts and closes between polls
// is never observed (needs a later DNS lookup), an entry expired before its
// first observation is restored on the next poll, and nft failures extend the
// gap. Only TCP is tracked; UDP and QUIC rely on DNS-driven refresh. Existing
// connections survive these gaps through conntrack, and the final observation
// after close provides the bounded reconnect grace period.
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
	m.lastDrops = EstablishedDrops{}
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
	if ep := opts.UpstreamProxy; ep != nil {
		b.WriteString(buildUpstreamProxyStatic(tableName, ep))
	}
	// DNS-learned allows bypass established teardown so a later resolve
	// (shared CDN IP) is not pinned by a stale drop from a prior apply; the
	// dyn sets are recreated empty on every full apply.
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s ct state established,related accept\n", tableName, chainName, dynAllowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s ct state established,related accept\n", tableName, chainName, dynAllowV6Set)
	// Explicit denies first, then still-allowed destinations (keeps and the
	// static allow sets), then removed allows, then the blanket verdict.
	writeEstablishedTargets(&b, drops.DenyAddrs, drops.DenyPrefixes, drops.DenyPortDrops, "drop")
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

func isMissingTableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file or directory") && strings.Contains(msg, "delete table inet "+tableName)
}

func removeDeleteTableLine(script string) string {
	lines := strings.Split(script, "\n")
	var filtered []string
	for _, l := range lines {
		if strings.HasPrefix(l, "delete table inet "+tableName) {
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		filtered = append(filtered, l)
	}
	return strings.Join(filtered, "\n")
}
