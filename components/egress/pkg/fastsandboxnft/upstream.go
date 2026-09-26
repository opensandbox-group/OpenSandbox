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

package fastsandboxnft

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
)

// UpstreamProxyEndpoint describes the chained upstream CONNECT proxy under
// the fast-sandbox enforcement model: infrastructure that sandbox traffic
// must never reach directly. A sandbox CONNECTing the proxy itself could
// relay to otherwise-denied destinations on unintercepted ports, so the
// endpoint is dropped for all subjects regardless of policy — including
// default-allow subjects and subjects whose policy happens to allow the
// proxy address.
type UpstreamProxyEndpoint struct {
	Port int
	// LiteralIPs seeds the drop sets permanently. Hostname endpoints start
	// empty and are filled via AddUpstreamProxyIPs / SyncUpstreamProxyIPs as
	// the egress resolves the infra domain (also as permanent elements —
	// expiry is owned by the egress, never by kernel timeouts).
	LiteralIPs []netip.Addr
}

// upstreamProxySetFor maps an address to its drop-set name ("" when invalid).
func upstreamProxySetFor(addr netip.Addr) string {
	if addr.Is4() {
		return upstreamProxyV4Set
	}
	if addr.Is6() {
		return upstreamProxyV6Set
	}
	return ""
}

// writeUpstreamProxyStatic renders the drop sets with literal and
// DNS-learned (mirrored) elements plus the profile-wide drop rules. All
// elements are PERMANENT (no kernel timeout): every other rule in this
// table persists while the egress daemon is down (fail closed), and a
// kernel timeout on the containment would silently lapse it during a
// restart — expiry is owned by the egress instead (SyncUpstreamProxyIPs
// prunes rotation; table rebuilds re-emit this mirror deterministically).
// Callers must hold a.mu.
// The forward rules sit BEFORE the dispatch chain's established accept and
// every per-subject jump: no subject policy — default-allow included — may
// CONNECT the proxy directly, and stale established flows from a previous
// egress generation (one without an upstream proxy) must not survive a
// restart either. The mitmdump dial is locally generated (OUTPUT path), so
// it never matches these forward rules.
func (a *Applier) writeUpstreamProxyStatic(b *strings.Builder) {
	ep := a.opts.UpstreamProxy
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; flags timeout; }\n", TableName, upstreamProxyV4Set)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; flags timeout; }\n", TableName, upstreamProxyV6Set)
	for _, ip := range ep.LiteralIPs {
		addr := ip.Unmap()
		set := upstreamProxySetFor(addr)
		if set == "" {
			continue
		}
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, set, addr)
	}
	for addr := range a.upstreamIPs {
		set := upstreamProxySetFor(addr)
		if set == "" {
			delete(a.upstreamIPs, addr)
			continue
		}
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, set, addr)
	}
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s tcp dport %d drop\n", TableName, dispatchChain, upstreamProxyV4Set, ep.Port)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s tcp dport %d drop\n", TableName, dispatchChain, upstreamProxyV6Set, ep.Port)
}

// writeUpstreamProxyInputRules renders the input-path containment for
// intercepted traffic: a CONNECT whose ORIGINAL destination is the proxy
// endpoint (its port inside the intercepted set) is dropped before the
// input chain's established accept. Only emitted with MITM enabled, after
// the input chain exists and before its first rule.
func (a *Applier) writeUpstreamProxyInputRules(b *strings.Builder) {
	ep := a.opts.UpstreamProxy
	fmt.Fprintf(b, "add rule inet %s %s ct status dnat ct original ip daddr @%s ct original proto-dst %d drop\n",
		TableName, inputChain, upstreamProxyV4Set, ep.Port)
	fmt.Fprintf(b, "add rule inet %s %s ct status dnat ct original ip6 daddr @%s ct original proto-dst %d drop\n",
		TableName, inputChain, upstreamProxyV6Set, ep.Port)
}

// writeUpstreamElement emits the idempotent permanent-element dance: add
// (creates when absent), delete (always present after the add), re-add.
// The final element carries no timeout. Same pattern as the sidecar
// manager's updates, minus the TTL.
func writeUpstreamElement(script *strings.Builder, set string, addr netip.Addr) {
	fmt.Fprintf(script, "add element inet %s %s { %s }\n", TableName, set, addr)
	fmt.Fprintf(script, "delete element inet %s %s { %s }\n", TableName, set, addr)
	fmt.Fprintf(script, "add element inet %s %s { %s }\n", TableName, set, addr)
}

// AddUpstreamProxyIPs feeds DNS-learned proxy addresses into the drop sets
// as PERMANENT elements (the resolver TTL is ignored deliberately): kernel
// timeouts would let containment lapse while the egress daemon is down,
// while every other rule in this table persists. Expiry is owned by the
// egress — the refresh loop's SyncUpstreamProxyIPs prunes rotation, and
// table rebuilds re-emit the in-memory mirror deterministically. The
// mirror is committed only after a successful apply. No-op when no
// upstream proxy is configured.
func (a *Applier) AddUpstreamProxyIPs(ctx context.Context, ips []nftables.ResolvedIP) error {
	if a.opts.UpstreamProxy == nil || len(ips) == 0 {
		return nil
	}
	var script strings.Builder
	learned := make([]netip.Addr, 0, len(ips))
	for _, r := range ips {
		addr := r.Addr.Unmap()
		set := upstreamProxySetFor(addr)
		if set == "" {
			continue
		}
		writeUpstreamElement(&script, set, addr)
		learned = append(learned, addr)
	}
	if script.Len() == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.run(ctx, script.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpUpstreamProxyAdd)
		return err
	}
	for _, addr := range learned {
		a.upstreamIPs[addr] = struct{}{}
	}
	telemetry.RecordNftablesUpdate()
	return nil
}

// SyncUpstreamProxyIPs makes the drop sets match ips exactly: new addresses
// are added (permanent elements) and addresses no longer returned are
// removed — this is the expiry mechanism for the containment, replacing
// kernel timeouts. A failed or empty resolve must never reach this prune:
// stale addresses only leave the sets when a SUCCESSFUL resolve stopped
// returning them (fail-closed retention — extra elements are safe, missing
// ones are not). The mirror is committed only after a successful apply.
// No-op (nil) when no upstream proxy is configured.
func (a *Applier) SyncUpstreamProxyIPs(ctx context.Context, ips []nftables.ResolvedIP) error {
	if a.opts.UpstreamProxy == nil {
		return nil
	}
	desired := make(map[netip.Addr]struct{}, len(ips))
	for _, r := range ips {
		addr := r.Addr.Unmap()
		if upstreamProxySetFor(addr) == "" {
			continue
		}
		desired[addr] = struct{}{}
	}
	if len(desired) == 0 {
		// Never a legitimate "remove everything": a non-empty answer whose
		// entries are all unusable is a failure, not a rotation to nothing.
		return fmt.Errorf("no usable addresses among %d resolved entries", len(ips))
	}
	var script strings.Builder
	for addr := range a.upstreamIPs {
		if _, keep := desired[addr]; keep {
			continue
		}
		set := upstreamProxySetFor(addr)
		if set == "" {
			continue
		}
		fmt.Fprintf(&script, "delete element inet %s %s { %s }\n", TableName, set, addr)
	}
	for addr := range desired {
		writeUpstreamElement(&script, upstreamProxySetFor(addr), addr)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.run(ctx, script.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpUpstreamProxyAdd)
		return err
	}
	a.upstreamIPs = desired
	telemetry.RecordNftablesUpdate()
	return nil
}

const (
	// upstreamProxyRefreshInterval is how often the egress re-resolves the
	// upstream proxy hostname: new addresses are added and addresses both
	// resolver authorities stopped returning are pruned. One query pair per
	// fastlet per tick.
	upstreamProxyRefreshInterval = 30 * time.Second

	// upstreamProxySeedTimeout bounds the first-seed retry window. Past it
	// the egress refuses to start (fail closed, matching ApplyReset, mitm
	// start and dnsproxy init) instead of serving sandbox actions with an
	// empty drop set for a hostname endpoint.
	upstreamProxySeedTimeout = time.Minute

	// Seed-retry backoff: starts at upstreamProxySeedBackoffMin and doubles
	// per attempt, capped at upstreamProxySeedBackoffMax.
	upstreamProxySeedBackoffMin = time.Second
	upstreamProxySeedBackoffMax = 10 * time.Second
)

// StartUpstreamProxyRefresh re-resolves the upstream proxy hostname through
// lookup and syncs the answers into the drop sets, so they stay seeded even
// when no sandbox ever queries the name: sandbox lookups add elements
// through the dnsproxy infra-domain callback, but nothing guarantees such
// queries.
//
// The FIRST sync is fail closed: it retries with bounded backoff until the
// drop set is seeded and returns an error when the seed deadline passes —
// the caller fails startup rather than serving with unseeded containment
// (a sandbox only needs the proxy IP, obtainable out-of-band, to relay).
// Later ticks are best-effort by design: elements are permanent, so a
// failed tick cannot lapse existing containment — it only delays picking
// up rotation, and while both resolver authorities fail equally the shared
// mitmproxy cannot dial the new addresses either. Literal endpoints need
// no loop (their elements are permanent and come from Options).
func (a *Applier) StartUpstreamProxyRefresh(ctx context.Context, domain string, lookup func(context.Context, string) ([]nftables.ResolvedIP, error)) error {
	sync := func() error {
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, err := lookup(syncCtx, domain)
		if err != nil {
			return err
		}
		if len(ips) == 0 {
			return fmt.Errorf("resolved to no addresses")
		}
		return a.SyncUpstreamProxyIPs(syncCtx, ips)
	}
	deadline := a.now().Add(a.upstreamSeedTimeout)
	backoff := min(upstreamProxySeedBackoffMin, a.upstreamSeedTimeout/8)
	maxBackoff := min(upstreamProxySeedBackoffMax, a.upstreamSeedTimeout/2)
	for {
		err := sync()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !a.now().Before(deadline) {
			return fmt.Errorf("containment seed failed: %w", err)
		}
		log.Warnf("fastsandboxnft: upstream proxy seed for %q failed (retrying): %v", domain, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
	safego.Go(func() {
		ticker := time.NewTicker(upstreamProxyRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := sync(); err != nil {
					log.Warnf("fastsandboxnft: upstream proxy refresh for %q failed (elements retained): %v", domain, err)
				}
			}
		}
	})
	return nil
}
