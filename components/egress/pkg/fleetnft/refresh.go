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

package fleetnft

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
)

// connectionRefreshInterval is how often active TCP connections renew their
// dynamic set leases (matches the sidecar profile's default).
const connectionRefreshInterval = 30 * time.Second

// conntrackEntry is one Pod-netns conntrack flow in its ORIGINAL direction
// (src = the sandbox's own IP, which REDIRECT preserves).
type conntrackEntry struct {
	src   netip.Addr
	dst   netip.Addr
	state string // TCP state name (e.g. ESTABLISHED)
}

// refreshState is the per-subject lease mirror: which dynamic-set IPs the
// subject currently carries and when they expire, plus the previous tick's
// active set (used for the end-of-activity final refresh) and a pending
// set of IPs whose redelivery to the sandbox layer is still owed.
type refreshState struct {
	dyn     map[netip.Addr]time.Time
	prev    map[netip.Addr]struct{}
	pending map[netip.Addr]struct{}
}

// readConntrack reads the Pod netns conntrack table. The forward hook
// traverses every sandbox flow, so the table holds each sandbox's
// connections keyed by source IP — one read serves all subjects (the OSEP's
// bucketed-per-subject refresh).
func readConntrack(ctx context.Context) ([]conntrackEntry, error) {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		return nil, err
	}
	return parseConntrackTCPEntries(data), nil
}

// parseConntrackTCPEntries parses /proc/net/nf_conntrack lines. Only TCP
// entries with a parseable original-direction src/dst are returned; the
// first src=/dst= tokens on a line are the original direction (the reply
// direction follows for NAT'd flows).
func parseConntrackTCPEntries(data []byte) []conntrackEntry {
	var out []conntrackEntry
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[2] != "tcp" {
			continue
		}
		var e conntrackEntry
		e.state = f[5]
		for _, tok := range f[6:] {
			if v, ok := strings.CutPrefix(tok, "src="); ok && !e.src.IsValid() {
				if a, err := netip.ParseAddr(v); err == nil {
					e.src = a
				}
				continue
			}
			if v, ok := strings.CutPrefix(tok, "dst="); ok && !e.dst.IsValid() {
				if a, err := netip.ParseAddr(v); err == nil {
					e.dst = a
				}
			}
		}
		if e.src.IsValid() && e.dst.IsValid() {
			out = append(out, e)
		}
	}
	return out
}

// activeConntrackTCPState reports whether a conntrack TCP state represents
// an active connection (mirrors the sidecar's activeTCPState; conntrack
// collapses FIN_WAIT1/2 into FIN_WAIT, and TIME_WAIT is deliberately
// excluded — the final refresh covers the reconnect grace period instead).
func activeConntrackTCPState(state string) bool {
	switch state {
	case "ESTABLISHED", "SYN_SENT", "SYN_RECV", "FIN_WAIT", "CLOSE_WAIT", "CLOSING", "LAST_ACK":
		return true
	default:
		return false
	}
}

// StartConnectionRefresh keeps each subject's DNS-learned IPs authorized
// while TCP connections to them are active; the set timeout remains as the
// grace period after the connection closes. sandboxMir, when non-nil, is
// invoked per subject with the refreshed IPs so a mirror layer (e.g. a
// per-sandbox netns enforcement) carries identical leases; the fleet profile
// passes nil (no mirror layer).
//
// Renewal is best-effort, per subject: a connection that starts and closes
// between polls is never observed (needs a later DNS lookup), an entry that
// expired before its first observation is restored on the next poll, and an
// nft failure extends the gap (existing connections survive through the
// `ct state established` rule). Only TCP is tracked — UDP/QUIC (HTTP/3,
// DoH-over-UDP) sessions are never renewed and rely on their DNS lease
// TTLs (sidecar parity). Renewal runs as a background goroutine; a
// conntrack read failure clears the previous-activity state and skips the
// tick.
//
// A refresh whose Pod-netns apply succeeds but whose sandbox mirror fails
// marks the IPs as pending redelivery: the next tick re-adds them
// unconditionally (even without conntrack activity), so a transient
// sandbox-layer miss never leaves the subject self-locked until the lease
// expires and the client re-resolves.
func (a *Applier) StartConnectionRefresh(ctx context.Context, sandboxMir func(context.Context, subject.Subject, []nftables.ResolvedIP) error) {
	a.mu.Lock()
	a.sandboxMir = sandboxMir
	a.mu.Unlock()
	safego.Go(func() {
		ticker := time.NewTicker(connectionRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.refreshTick(ctx)
			}
		}
	})
}

// refreshTick performs one refresh pass: bucket the Pod-netns conntrack table
// by source IP, compute each subject's refresh plan, and apply ALL subjects
// in ONE nft transaction — the applier lock is held only for the planning
// (one conntrack read + one exec), never across per-subject exec chains, so
// a slow tick cannot stall policy operations. The sandbox mirror runs after
// the lock is released; a mirror failure marks the IPs pending redelivery.
func (a *Applier) refreshTick(ctx context.Context) {
	type subjectPlan struct {
		s   subject.Subject
		ips []nftables.ResolvedIP
	}
	var plans []subjectPlan

	a.mu.Lock()
	entries, err := a.conntrack(ctx)
	if err != nil {
		// No trustworthy activity data: drop the previous-activity set so no
		// stale "was active" state keeps leases alive beyond their expiry.
		for _, st := range a.states {
			st.prev = make(map[netip.Addr]struct{})
		}
		a.mu.Unlock()
		log.Warnf("fleetnft: conntrack read failed, skipping refresh: %v", err)
		return
	}
	srcIdx := a.buildSrcIndexLocked()
	activeBySubject := bucketActive(entries, srcIdx)
	var b strings.Builder
	for s, st := range a.states {
		ips := st.plan(activeBySubject[s], a.now())
		if len(ips) == 0 {
			continue
		}
		plans = append(plans, subjectPlan{s: s, ips: ips})
		writeResolvedIPsFragment(&b, s, ips)
	}
	if b.Len() > 0 {
		if _, err := a.run(ctx, b.String()); err != nil {
			// The lease mirror was already extended in the plan phase: mark
			// every planned IP pending so the next tick redelivers
			// unconditionally (self-healing, no self-lock).
			for _, p := range plans {
				a.markPendingLocked(p.s, p.ips)
			}
			a.mu.Unlock()
			telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
			log.Warnf("fleetnft: refresh dynamic sets failed: %v", err)
			return
		}
		telemetry.RecordNftablesUpdate()
	}
	a.mu.Unlock()

	for _, p := range plans {
		if a.sandboxMir == nil {
			continue
		}
		if err := a.sandboxMir(ctx, p.s, p.ips); err != nil {
			log.Warnf("fleetnft: sandbox mirror refresh for subject %s failed: %v", p.s, err)
			a.markPendingRedelivery(p.s, p.ips)
			continue
		}
		a.clearPending(p.s, p.ips)
	}
}

// buildSrcIndexLocked maps each installed subject's source IP to its subject
// (built once per tick instead of scanning the subject map per conntrack
// entry).
func (a *Applier) buildSrcIndexLocked() map[netip.Addr]subject.Subject {
	idx := make(map[netip.Addr]subject.Subject, len(a.subjects))
	for s, inst := range a.subjects {
		idx[inst.att.IP] = s
	}
	return idx
}

// bucketActive groups the active conntrack flows by subject, keyed on the
// source IP (the dispatch key; REDIRECT preserves it).
func bucketActive(entries []conntrackEntry, srcIdx map[netip.Addr]subject.Subject) map[subject.Subject]map[netip.Addr]struct{} {
	out := make(map[subject.Subject]map[netip.Addr]struct{})
	for _, e := range entries {
		if !activeConntrackTCPState(e.state) {
			continue
		}
		s, ok := srcIdx[e.src.Unmap()]
		if !ok {
			continue
		}
		m := out[s]
		if m == nil {
			m = make(map[netip.Addr]struct{})
			out[s] = m
		}
		m[e.dst.Unmap()] = struct{}{}
	}
	return out
}

// markPendingRedelivery records IPs whose sandbox mirror delivery failed, so
// the next tick re-adds them unconditionally.
func (a *Applier) markPendingRedelivery(s subject.Subject, ips []nftables.ResolvedIP) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.markPendingLocked(s, ips)
}

func (a *Applier) markPendingLocked(s subject.Subject, ips []nftables.ResolvedIP) {
	if _, ok := a.subjects[s]; !ok {
		return // subject unloaded meanwhile: nothing to redeliver
	}
	st := a.states[s]
	if st == nil {
		st = &refreshState{}
		a.states[s] = st
	}
	if st.pending == nil {
		st.pending = make(map[netip.Addr]struct{})
	}
	for _, r := range ips {
		st.pending[r.Addr.Unmap()] = struct{}{}
	}
}

// clearPending drops IPs from the pending set after their mirror delivery
// succeeded.
func (a *Applier) clearPending(s subject.Subject, ips []nftables.ResolvedIP) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[s]
	if st == nil || len(st.pending) == 0 {
		return
	}
	for _, r := range ips {
		delete(st.pending, r.Addr.Unmap())
	}
}

// plan computes the refresh for one subject given its currently active
// remote IPs: renew active leases, drop expired leases that are neither
// active nor ending, issue one final refresh when activity ends so the
// re-added timeout becomes the bounded reconnect grace period (sidecar
// parity), and redeliver any pending IPs unconditionally (a previous
// sandbox-mirror failure must not leave the subject self-locked). Mutates
// the state's dyn/prev mirrors.
func (st *refreshState) plan(active map[netip.Addr]struct{}, now time.Time) []nftables.ResolvedIP {
	if st.dyn == nil {
		st.dyn = make(map[netip.Addr]time.Time)
	}
	if st.prev == nil {
		st.prev = make(map[netip.Addr]struct{})
	}
	for addr, expiresAt := range st.dyn {
		if expiresAt.After(now) {
			continue
		}
		if _, isActive := active[addr]; isActive {
			continue
		}
		if _, wasActive := st.prev[addr]; wasActive {
			continue
		}
		delete(st.dyn, addr)
		delete(st.prev, addr)
	}

	current := make(map[netip.Addr]struct{}, len(active))
	refresh := make(map[netip.Addr]struct{})
	for addr := range active {
		if _, ok := st.dyn[addr]; ok {
			current[addr] = struct{}{}
			refresh[addr] = struct{}{}
		}
	}
	// A final refresh when activity ends: the re-added timeout is the
	// reconnect grace period instead of extending every DNS answer globally.
	for addr := range st.prev {
		if _, stillActive := current[addr]; stillActive {
			continue
		}
		if _, known := st.dyn[addr]; known {
			refresh[addr] = struct{}{}
		}
	}
	// Pending redelivery: owed regardless of conntrack activity.
	for addr := range st.pending {
		refresh[addr] = struct{}{}
	}

	ips := make([]nftables.ResolvedIP, 0, len(refresh))
	for addr := range refresh {
		ips = append(ips, nftables.ResolvedIP{Addr: addr, TTL: time.Duration(dynSetTimeoutS) * time.Second})
		st.dyn[addr] = now.Add(time.Duration(dynSetTimeoutS) * time.Second)
	}
	st.prev = current
	return ips
}
