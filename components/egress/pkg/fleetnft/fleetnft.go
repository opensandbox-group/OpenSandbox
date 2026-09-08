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

// Package fleetnft builds and applies the per-subject nftables ruleset for the
// fleet egress profile. It is a new layer on top of the egress
// engines: pkg/nftables, pkg/dnsproxy, and pkg/credentialvault are untouched.
//
// Enforcement model (Pod netns):
//
//	table inet opensandbox-fleet
//	  chain mark { hook prerouting, priority 0 }      <- per-subject allow marks
//	    ip saddr <ip> jump mark_<id>                  <- one rule per subject
//	  chain mark_<id> (regular chain)                 <- what to mark
//	    ip daddr @subj_<id>_allow_v4 meta mark set 0x2
//	    ip daddr @subj_<id>_dyn_v4    meta mark set 0x2
//	    (default-allow subjects: unconditional mark)
//	  chain dispatch { hook forward, policy ACCEPT }  <- master chain
//	    ct state established,related accept           <- return traffic
//	    tcp/udp dport 853 drop                        <- DoT bypass blocked
//	    ip saddr <ip> jump subj_<id>                  <- one rule per subject
//	    meta mark & 0x2 != 0x2 drop                   <- fail-closed tail
//	  chain subj_<id> (regular chain)                 <- deny sets only
//	    ip daddr @subj_<id>_deny_v4/v6 drop
//
// Dispatch matches the sandbox source IP only — never iifname: with
// net.bridge.bridge-nf-call-iptables=1 (the fast-sandbox Firecracker bridge
// topology), frames entering the bridge destined to the bridge itself are
// pulled into the IP stack with skb->dev = the BRIDGE, so an iifname match on
// the pod-side veth would never fire. Source-IP unforgeability rests on IPAM
// (per-sandbox unique IP) and the sandbox lacking NET_ADMIN/NET_RAW.
//
// NO explicit accept verdicts exist in the forward path: with
// net.bridge.bridge-nf-call-iptables=1 (the fast-sandbox Firecracker bridge
// topology), an accept verdict from the forward hook returns the frame to the
// bridge L2 path — a frame whose destination is the bridge itself is then
// treated as local delivery and dropped, so postrouting (SNAT) is never
// reached. Only "not hitting a drop rule" lets the frame continue IP routing.
// The master chain therefore defaults to ACCEPT and drops everything that was
// not explicitly marked as allowed in prerouting; per-subject allow/dyn set
// members are marked there. Unregistered sources and deny-first subjects are
// unmarked and dropped by the tail, so fail-closed is preserved. Mark 0x2 is
// distinct from the DNS proxy's SO_MARK 0x1 bypass (pkg/constants MarkValue).
package fleetnft

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

// TableName is the fleet-profile nftables table, kept distinct from the
// sidecar profile's "opensandbox" table (the two profiles never run in the
// same process, but distinct names make a stale leftover impossible to
// confuse with live rules).
const TableName = "opensandbox-fleet"

const (
	dispatchChain    = "dispatch"
	dispatchPriority = 0
	// markChain is the shared prerouting hook chain where per-subject allow
	// marks are set (see the package comment: the forward path never accepts
	// explicitly — it only drops, so bridge topologies keep routing). The
	// name avoids the nft keyword `mark` (chain names cannot be keywords).
	markChain    = "marking"
	markPriority = 0
	allowMark    = 0x2 // distinct from the DNS proxy's SO_MARK 0x1 bypass
	// inputChain is the authoritative enforcement layer for MITM traffic:
	// intercepted HTTP(S) is DNATed to the shared mitmproxy port and
	// delivered locally, so it never traverses the forward hook. The input
	// chain (policy accept, matching ONLY ct-status-DNAT packets) runs the
	// same subject policy on the conntrack ORIGINAL destination — a
	// compromised sandbox flushing its own OUTPUT table cannot bypass the
	// authoritative layers, and Pod's own traffic never matches (no DNAT).
	inputChain     = "input"
	inputPriority  = 0
	dynSetTimeoutS = 360
	// nftTTLSlackSec is added to the DNS TTL before clamping (mirrors
	// pkg/nftables so both profiles behave identically).
	nftTTLSlackSec = 60
	minTTLSec      = 60
	maxTTLSec      = 360

	dohBlockV4Set = "doh_block_v4"
	dohBlockV6Set = "doh_block_v6"
)

// Runner executes an nft script; the default invokes `nft -f -`.
type Runner func(ctx context.Context, script string) ([]byte, error)

// DefaultRunner runs the script through `nft -f -`.
func DefaultRunner(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("nft apply failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// ErrUnknownSubject is returned when an operation targets a subject whose
// deny-first rules have not been installed yet.
var ErrUnknownSubject = fmt.Errorf("fleetnft: subject rules not installed")

// Options carries fleet-profile-wide enforcement toggles, loaded once at
// startup and applied to every table (re)build.
type Options struct {
	// BlockDoH443 drops TCP 443 to the DoH blocklist (or all TCP 443 when
	// no blocklist is provided — strict mode). Same semantics as the sidecar
	// profile's OPENSANDBOX_EGRESS_BLOCK_DOH_443: the rules are global, in
	// the master dispatch chain, so they apply to every subject regardless
	// of policy.
	BlockDoH443 bool
	// DoHBlocklistV4/V6 are IP/CIDR lists of known DoH endpoints (from
	// OPENSANDBOX_EGRESS_DOH_BLOCKLIST); tcp 443 to them is dropped.
	DoHBlocklistV4 []string
	DoHBlocklistV6 []string
	// MitmRedirectPort is the shared mitmproxy port that intercepted
	// HTTP(S) is DNATed to. DNATed traffic is delivered locally (INPUT), so
	// the forward hook never sees it; the input enforcement chain (below)
	// executes the same policy on it using the conntrack ORIGINAL
	// destination. 0 disables the input chain entirely (no MITM).
	MitmRedirectPort int
}

// installedSubject tracks the enforcement state the applier owns in memory;
// it is the source for table rebuilds (subject removal) and the idempotency
// guard for deny-first installs.
type installedSubject struct {
	att actionhandler.NetworkAttachment
	pol *policy.NetworkPolicy // nil while denying
}

// Applier applies per-subject rules to table TableName. All methods are safe
// for concurrent use; each operation is one atomic nft transaction.
type Applier struct {
	mu         sync.Mutex
	run        Runner
	opts       Options
	tableReady bool
	subjects   map[subject.Subject]installedSubject

	// Per-subject dynamic-lease mirror used by the connection refresh loop
	// (refresh.go): which IPs each subject's dyn sets currently authorize and
	// when they expire. Kept in sync by AddResolvedIPs and cleared on
	// deny-first resets and unloads.
	states     map[subject.Subject]*refreshState
	conntrack  func(context.Context) ([]conntrackEntry, error) // injectable for tests
	now        func() time.Time                                // injectable for tests
	sandboxMir func(context.Context, subject.Subject, []nftables.ResolvedIP) error
}

// NewApplier returns an Applier using r (nil selects DefaultRunner). Pass
// Options (DoH-443 blocking) to enable profile-wide master-chain rules.
func NewApplier(r Runner, opts ...Options) *Applier {
	if r == nil {
		r = DefaultRunner
	}
	a := &Applier{
		run:       r,
		subjects:  make(map[subject.Subject]installedSubject),
		states:    make(map[subject.Subject]*refreshState),
		conntrack: readConntrack,
		now:       time.Now,
	}
	if len(opts) > 0 {
		a.opts = opts[0]
	}
	return a
}

// ApplyReset atomically swaps the ruleset for an EMPTY master drop chain:
// the drop-by-default dispatch chain (with its established/DoT rules) stays
// installed with no subjects, so unregistered sources remain denied while the
// handler replays bindings after an egress restart — the fail-closed
// guarantee must not have a window where the hook is gone. Recovery protocol:
// the caller must reset before serving action requests at startup, so stale
// rules from a previous egress generation can never carry old policy into a
// new sandbox. A missing table is not an error (fallback retry without the
// delete line).
func (a *Applier) ApplyReset(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	if err := a.writeTableHeader(&b); err != nil {
		return err
	}
	if err := a.applyWithMissingTableFallback(ctx, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpReset)
		return err
	}
	a.tableReady = true
	a.subjects = make(map[subject.Subject]installedSubject)
	a.states = make(map[subject.Subject]*refreshState)
	a.recordRuleCountLocked()
	telemetry.RecordNftablesUpdate()
	return nil
}

// recordRuleCountLocked refreshes the egress.nftables.rules.count gauge:
// summed across every installed subject's policy (0 for deny-first), so
// deny-first installs, dispatch updates, and rebuilds never leave the gauge
// drifting from the real rule count.
func (a *Applier) recordRuleCountLocked() {
	var total int64
	for _, inst := range a.subjects {
		if inst.pol != nil {
			total += telemetry.NftRuleCountFromPolicy(inst.pol)
		}
	}
	telemetry.SetNftablesRuleCount(total)
}

// applyWithMissingTableFallback runs the script; if the batch fails because
// `delete table` targets a missing table (e.g. first boot, or a prior reset
// already removed it), retry without the delete line. Mirrors the sidecar
// manager's fallback.
func (a *Applier) applyWithMissingTableFallback(ctx context.Context, script string) error {
	if _, err := a.run(ctx, script); err == nil {
		return nil
	} else if missingTable(err) {
		if fallback := removeDeleteTableLine(script); fallback != script {
			if _, retryErr := a.run(ctx, fallback); retryErr == nil {
				return nil
			}
		}
		return err
	} else {
		return err
	}
}

// ApplyDenyFirst registers a subject in deny-first state: empty static sets,
// drop policy chain, and a dispatch rule keyed on the sandbox source IP
// (the dispatch key; see the package comment for why iifname is not used).
// The first call also installs the master dispatch chain.
//
// Re-registration (e.g. a fencing rebind, where the controller re-observes
// the same subject): the subject is force-reset to deny-first — chain, static
// sets, and DNS-learned dynamic leases are wiped — so a previous sandbox's
// policy can never carry into a new sandbox. (Registry + DNS already fail
// closed on rebind; this closes the nft layer, which keeps the old allow sets
// otherwise.)
func (a *Applier) ApplyDenyFirst(ctx context.Context, s subject.Subject, att actionhandler.NetworkAttachment) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	if !a.tableReady {
		if err := a.writeTableHeader(&b); err != nil {
			return err
		}
		a.tableReady = true
	}
	if _, ok := a.subjects[s]; ok {
		if err := writeSubjectResetFragment(&b, s, att, a.opts.MitmRedirectPort); err != nil {
			return err
		}
	} else {
		if err := writeSubjectDenyFirstFragment(&b, s, att, a.opts.MitmRedirectPort); err != nil {
			return err
		}
	}
	if err := a.applyWithMissingTableFallback(ctx, b.String()); err != nil {
		// The batch is atomic: on failure nothing was installed, so keep the
		// flag consistent (the controller retries).
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDenyFirst)
		return err
	}
	a.subjects[s] = installedSubject{att: att}
	delete(a.states, s) // deny-first: no policy, no leases (nft dyn sets were flushed)
	a.recordRuleCountLocked()
	telemetry.RecordNftablesUpdate()
	return nil
}

// ApplyPolicy swaps a subject's static sets and chain content atomically in
// one transaction; dynamic DNS-learned sets are untouched. Deny-first
// subjects move to active only via this call.
func (a *Applier) ApplyPolicy(ctx context.Context, s subject.Subject, pol *policy.NetworkPolicy) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	inst, ok := a.subjects[s]
	if !ok {
		return ErrUnknownSubject
	}
	if pol == nil {
		pol = policy.DefaultDenyPolicy()
	}
	var b strings.Builder
	if err := writeSubjectPolicySwapFragment(&b, s, pol, a.opts.MitmRedirectPort); err != nil {
		return err
	}
	if _, err := a.run(ctx, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpStaticApply)
		return err
	}
	inst.pol = pol
	a.subjects[s] = inst
	a.recordRuleCountLocked()
	telemetry.RecordNftablesUpdate()
	return nil
}

// AddResolvedIPs adds DNS-learned IPs to a subject's dynamic allow sets with
// a bounded timeout (mirrors the sidecar profile's lease behavior).
func (a *Applier) AddResolvedIPs(ctx context.Context, s subject.Subject, ips []nftables.ResolvedIP) error {
	if len(ips) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.subjects[s]; !ok {
		return ErrUnknownSubject
	}
	var b strings.Builder
	writeResolvedIPsFragment(&b, s, ips)
	if b.Len() == 0 {
		return nil
	}
	if _, err := a.run(ctx, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	a.trackDynamicIPs(s, ips)
	telemetry.RecordNftablesUpdate()
	return nil
}

// trackDynamicIPs mirrors the just-added leases into the refresh state, so
// the connection refresh loop knows which IPs each subject's dyn sets carry
// and when they expire (same clamping as the nft elements written above).
func (a *Applier) trackDynamicIPs(s subject.Subject, ips []nftables.ResolvedIP) {
	st := a.states[s]
	if st == nil {
		st = &refreshState{}
		a.states[s] = st
	}
	now := a.now()
	for _, r := range ips {
		addr := r.Addr.Unmap()
		if !addr.IsValid() {
			continue
		}
		if st.dyn == nil {
			st.dyn = make(map[netip.Addr]time.Time)
		}
		st.dyn[addr] = now.Add(clampTTL(r.TTL))
	}
}

// Remove deletes a subject's enforcement. nftables deletes rules only by
// handle (no handle-less match), and verdict maps cannot jump to chains
// (EOPNOTSUPP on add element), so the master-chain dispatch rule cannot be
// removed per subject. Instead the whole table is rebuilt from the remaining
// in-memory state in one atomic transaction — deterministic and O(n), which
// is fine at the target density (removals are rare). Removing the last
// subject swaps in the empty master drop chain (fail-closed, never a bare
// table).
func (a *Applier) Remove(ctx context.Context, s subject.Subject) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.subjects[s]; !ok {
		return nil
	}
	delete(a.subjects, s)
	delete(a.states, s)
	if len(a.subjects) == 0 {
		var b strings.Builder
		if err := a.writeTableHeader(&b); err != nil {
			return err
		}
		if err := a.applyWithMissingTableFallback(ctx, b.String()); err != nil {
			telemetry.RecordNftablesUpdateFailed(telemetry.NftOpRemove)
			return err
		}
		a.tableReady = true
		a.recordRuleCountLocked()
		telemetry.RecordNftablesUpdate()
		return nil
	}
	var b strings.Builder
	if err := a.writeTableHeader(&b); err != nil {
		return err
	}
	for subj, inst := range a.subjects {
		var err error
		if inst.pol == nil {
			err = writeSubjectDenyFirstFragment(&b, subj, inst.att, a.opts.MitmRedirectPort)
		} else {
			err = writeSubjectInitialPolicyFragment(&b, subj, inst.att, inst.pol, a.opts.MitmRedirectPort)
		}
		if err != nil {
			return err
		}
	}
	if _, err := a.run(ctx, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpRemove)
		return err
	}
	a.tableReady = true
	a.recordRuleCountLocked()
	telemetry.RecordNftablesUpdate()
	return nil
}

// missingTable reports whether an nft error means the table does not exist.
func missingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist")
}

// removeDeleteTableLine strips the table-reset line from a failed script so
// it can be retried on a fresh table (mirrors the sidecar manager).
func removeDeleteTableLine(script string) string {
	lines := strings.Split(script, "\n")
	var filtered []string
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "delete table inet "+TableName) {
			continue
		}
		filtered = append(filtered, l)
	}
	return strings.Join(filtered, "\n")
}

// ---------------------------------------------------------------------------
// Script builders (pure string generation, unit-testable).
// ---------------------------------------------------------------------------

// writeTableHeader writes the idempotent table, the mark hook chain, the
// master dispatch chain, and (with MITM) the input chain. The dispatch chain
// policy is ACCEPT with an unmarked-drop tail: the forward path never issues
// an explicit accept (bridge-netfilter semantics, see the package comment),
// so allowed traffic is marked in prerouting and simply not dropped, while
// unregistered/deny-first/unmarked traffic is dropped by the tail — fail
// closed. The DoH-443 blocking rules (when enabled) are global — they live in
// the master chain ahead of the dispatch jumps, so they apply to every
// subject regardless of policy, matching the sidecar profile's semantics.
func (a *Applier) writeTableHeader(b *strings.Builder) error {
	fmt.Fprintf(b, "delete table inet %s\n", TableName)
	fmt.Fprintf(b, "add table inet %s\n", TableName)
	fmt.Fprintf(b, "add chain inet %s %s { type filter hook prerouting priority %d; }\n",
		TableName, markChain, markPriority)
	fmt.Fprintf(b, "add chain inet %s %s { type filter hook forward priority %d; policy accept; }\n",
		TableName, dispatchChain, dispatchPriority)
	fmt.Fprintf(b, "add rule inet %s %s ct state established,related accept\n", TableName, dispatchChain)
	fmt.Fprintf(b, "add rule inet %s %s tcp dport 853 drop\n", TableName, dispatchChain)
	fmt.Fprintf(b, "add rule inet %s %s udp dport 853 drop\n", TableName, dispatchChain)
	// input enforcement chain for MITM traffic: accept-by-default, only
	// ct-status-DNAT packets (the intercepted HTTP(S) delivered locally) are
	// dispatched. Pod's own traffic is never DNATed, so it never matches.
	if a.opts.MitmRedirectPort > 0 {
		fmt.Fprintf(b, "add chain inet %s %s { type filter hook input priority %d; policy accept; }\n",
			TableName, inputChain, inputPriority)
		fmt.Fprintf(b, "add rule inet %s %s ct state established,related accept\n", TableName, inputChain)
	}
	if !a.opts.BlockDoH443 {
		writeFailClosedTail(b)
		return nil
	}
	if err := a.writeDoHBlockFragment(b); err != nil {
		return err
	}
	writeFailClosedTail(b)
	return nil
}

// writeFailClosedTail appends the master-chain rule that drops every
// unmarked outbound packet: the forward path only drops (never accepts), so
// per-subject allow/dyn marks set in prerouting are the ONLY way a packet
// passes the tail. Unregistered sources and deny-first subjects carry no
// mark and are denied here (the empty table after ApplyReset therefore stays
// fail-closed). The tail's position relative to the per-subject jumps is
// irrelevant: marked and unmarked packets are disjoint predicates.
func writeFailClosedTail(b *strings.Builder) {
	fmt.Fprintf(b, "add rule inet %s %s meta mark & 0x%x != 0x%x drop\n",
		TableName, dispatchChain, allowMark, allowMark)
}

// writeDoHBlockFragment emits the DoH-443 blocking sets and rules for BOTH
// paths: the forward chain (non-MITM traffic sees the real destination) and
// the input chain (MITM traffic is DNATed; the real destination and port
// come from conntrack). With a blocklist: interval sets + per-family drop
// rules. Without one (strict mode): a bare drop of all tcp 443. Mirrors the
// sidecar manager's BlockDoH443 handling.
func (a *Applier) writeDoHBlockFragment(b *strings.Builder) error {
	if len(a.opts.DoHBlocklistV4) == 0 && len(a.opts.DoHBlocklistV6) == 0 {
		// strict: drop all 443 when enabled but no blocklist provided
		fmt.Fprintf(b, "add rule inet %s %s tcp dport 443 drop\n", TableName, dispatchChain)
		if a.opts.MitmRedirectPort > 0 {
			fmt.Fprintf(b, "add rule inet %s %s ct status dnat ct original proto-dst 443 drop\n", TableName, inputChain)
		}
		return nil
	}
	if len(a.opts.DoHBlocklistV4) > 0 {
		fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", TableName, dohBlockV4Set)
		if err := writeSetElements(b, dohBlockV4Set, a.opts.DoHBlocklistV4); err != nil {
			return fmt.Errorf("doh blocklist v4: %w", err)
		}
		fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s tcp dport 443 drop\n", TableName, dispatchChain, dohBlockV4Set)
		if a.opts.MitmRedirectPort > 0 {
			fmt.Fprintf(b, "add rule inet %s %s ct status dnat ct original ip daddr @%s ct original proto-dst 443 drop\n",
				TableName, inputChain, dohBlockV4Set)
		}
	}
	if len(a.opts.DoHBlocklistV6) > 0 {
		fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", TableName, dohBlockV6Set)
		if err := writeSetElements(b, dohBlockV6Set, a.opts.DoHBlocklistV6); err != nil {
			return fmt.Errorf("doh blocklist v6: %w", err)
		}
		fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s tcp dport 443 drop\n", TableName, dispatchChain, dohBlockV6Set)
		if a.opts.MitmRedirectPort > 0 {
			fmt.Fprintf(b, "add rule inet %s %s ct status dnat ct original ip6 daddr @%s ct original proto-dst 443 drop\n",
				TableName, inputChain, dohBlockV6Set)
		}
	}
	return nil
}

// writeSubjectDenyFirstFragment installs a subject in deny-first state:
// empty static sets, a drop-only forward chain, NO prerouting marks (the
// master-chain fail-closed tail drops all unmarked outbound), and the
// dispatch rules keyed on the sandbox source IP. Applies against an existing
// table (or right after the header).
func writeSubjectDenyFirstFragment(b *strings.Builder, s subject.Subject, att actionhandler.NetworkAttachment, mitmPort int) error {
	if err := writeSubjectSets(b, s, nil); err != nil {
		return err
	}
	writeSubjectChain(b, s, mitmPort)
	writeDispatchRule(b, s, att, mitmPort)
	writeSubjectForwardDenyRules(b, s, true)
	writeSubjectInputVerdictRules(b, s, policy.ActionDeny, mitmPort)
	return nil
}

// writeSubjectInitialPolicyFragment installs a subject with full policy
// content on a fresh table (used by rebuilds after Remove).
func writeSubjectInitialPolicyFragment(b *strings.Builder, s subject.Subject, att actionhandler.NetworkAttachment, pol *policy.NetworkPolicy, mitmPort int) error {
	if err := writeSubjectSets(b, s, pol); err != nil {
		return err
	}
	writeSubjectChain(b, s, mitmPort)
	writeDispatchRule(b, s, att, mitmPort)
	writeSubjectForwardDenyRules(b, s, false)
	writeSubjectMarkRules(b, s, pol.DefaultAction)
	writeSubjectInputVerdictRules(b, s, pol.DefaultAction, mitmPort)
	return nil
}

// writeSubjectResetFragment force-resets an already-installed subject back to
// deny-first: chain and all sets (static + dynamic) are FLUSHED (not deleted —
// the master-chain dispatch rule references the chain, and deleting a
// referenced chain/set fails with EBUSY) and the deny-first content is
// re-added, with the dispatch rule for the (possibly changed) attachment.
// Used on re-registration so a previous sandbox's policy, DNS leases, and
// prerouting marks never survive. A dispatch rule from a previous attachment
// key is harmless (a stale source key never matches; the same key yields an
// identical duplicate rule with the same verdict).
func writeSubjectResetFragment(b *strings.Builder, s subject.Subject, att actionhandler.NetworkAttachment, mitmPort int) error {
	fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, subjectChain(s))
	fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, markChainName(s))
	if mitmPort > 0 {
		fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, subjectChainIn(s))
	}
	for _, name := range allSetNames(s) {
		fmt.Fprintf(b, "flush set inet %s %s\n", TableName, name)
	}
	writeDispatchRule(b, s, att, mitmPort)
	writeSubjectForwardDenyRules(b, s, true)
	writeSubjectInputVerdictRules(b, s, policy.ActionDeny, mitmPort)
	return nil
}

// writeSubjectPolicySwapFragment atomically replaces a subject's chain rules
// and static set elements. The chain and set objects are FLUSHED, not
// deleted: the master-chain dispatch rule references the chain (and the
// verdict rules reference the sets), and deleting referenced objects fails
// with EBUSY. Dynamic DNS-learned sets and the dispatch rules are untouched.
// Regular chains have no policy, so re-adding the rules after the flush is a
// complete swap; the prerouting mark chain is flushed and rebuilt alongside
// (a default-action change flips the mark strategy).
func writeSubjectPolicySwapFragment(b *strings.Builder, s subject.Subject, pol *policy.NetworkPolicy, mitmPort int) error {
	fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, subjectChain(s))
	fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, markChainName(s))
	if mitmPort > 0 {
		fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, subjectChainIn(s))
	}
	for _, name := range staticSetNames(s) {
		fmt.Fprintf(b, "flush set inet %s %s\n", TableName, name)
	}
	allowV4, allowV6, denyV4, denyV6 := pol.StaticIPSets()
	if err := writeSetElements(b, allowSetName(s, "v4"), allowV4); err != nil {
		return err
	}
	if err := writeSetElements(b, allowSetName(s, "v6"), allowV6); err != nil {
		return err
	}
	if err := writeSetElements(b, denySetName(s, "v4"), denyV4); err != nil {
		return err
	}
	if err := writeSetElements(b, denySetName(s, "v6"), denyV6); err != nil {
		return err
	}
	writeSubjectForwardDenyRules(b, s, false)
	writeSubjectMarkRules(b, s, pol.DefaultAction)
	writeSubjectInputVerdictRules(b, s, pol.DefaultAction, mitmPort)
	return nil
}

// writeSubjectSets creates the subject's static (interval) and dynamic
// (timeout) sets; static sets are populated from the policy when non-nil.
func writeSubjectSets(b *strings.Builder, s subject.Subject, pol *policy.NetworkPolicy) error {
	for _, name := range staticSetNames(s) {
		fmt.Fprintf(b, "add set inet %s %s { type %s; flags interval; }\n", TableName, name, ipSetType(name))
	}
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; timeout %ds; }\n", TableName, dynSetName(s, "v4"), dynSetTimeoutS)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; timeout %ds; }\n", TableName, dynSetName(s, "v6"), dynSetTimeoutS)
	if pol == nil {
		return nil
	}
	allowV4, allowV6, denyV4, denyV6 := pol.StaticIPSets()
	if err := writeSetElements(b, allowSetName(s, "v4"), allowV4); err != nil {
		return err
	}
	if err := writeSetElements(b, allowSetName(s, "v6"), allowV6); err != nil {
		return err
	}
	if err := writeSetElements(b, denySetName(s, "v4"), denyV4); err != nil {
		return err
	}
	if err := writeSetElements(b, denySetName(s, "v6"), denyV6); err != nil {
		return err
	}
	return nil
}

// writeSetElements writes static set elements, normalized first: an interval
// set rejects overlapping entries in one add element ("conflicting intervals
// specified"), e.g. an always-deny host 10.99.0.9 inside a policy deny CIDR
// 10.99.0.0/24. Normalization drops strict subnets (shared with the sidecar's
// nftables manager semantics).
func writeSetElements(b *strings.Builder, setName string, elems []string) error {
	if len(elems) == 0 {
		return nil
	}
	normalized, err := nftables.NormalizeIntervalSet(elems)
	if err != nil {
		return fmt.Errorf("normalize interval set %s: %w", setName, err)
	}
	if len(normalized) == 0 {
		return nil
	}
	fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, setName, strings.Join(normalized, ", "))
	return nil
}

// writeSubjectChain creates the subject chains as REGULAR (non-hook) chains:
// nf_tables rejects `jump` to hook-bound chains (EOPNOTSUPP), and subject
// chains are only ever entered through the master dispatch jumps. The
// per-subject prerouting mark chain and the per-subject input chain (MITM)
// are created alongside.
func writeSubjectChain(b *strings.Builder, s subject.Subject, mitmPort int) {
	fmt.Fprintf(b, "add chain inet %s %s\n", TableName, subjectChain(s))
	fmt.Fprintf(b, "add chain inet %s %s\n", TableName, markChainName(s))
	if mitmPort > 0 {
		fmt.Fprintf(b, "add chain inet %s %s\n", TableName, subjectChainIn(s))
	}
}

// writeSubjectForwardDenyRules emits the forward-path subject chain content:
// the deny-set drops ONLY. No accept verdicts exist in the forward path
// (bridge-netfilter semantics — an explicit accept would return the frame to
// the bridge L2 path and drop it); allowed traffic is marked in prerouting
// and simply not dropped by the master-chain tail. denyFirst additionally
// appends a bare drop so a deny-first subject is blocked even before any
// marks could exist.
func writeSubjectForwardDenyRules(b *strings.Builder, s subject.Subject, denyFirst bool) {
	chain := subjectChain(s)
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s drop\n", TableName, chain, denySetName(s, "v4"))
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s drop\n", TableName, chain, denySetName(s, "v6"))
	if denyFirst {
		fmt.Fprintf(b, "add rule inet %s %s drop\n", TableName, chain)
	}
}

// writeSubjectMarkRules emits the prerouting mark content for a subject:
// default-deny policies mark allow/dyn set members (everything else stays
// unmarked and is dropped by the master-chain tail); default-allow policies
// mark EVERY outbound packet (the deny sets still drop). Deny-first subjects
// carry no mark rules at all.
func writeSubjectMarkRules(b *strings.Builder, s subject.Subject, defaultAction string) {
	chain := markChainName(s)
	if defaultAction == policy.ActionAllow {
		fmt.Fprintf(b, "add rule inet %s %s meta mark set 0x%x\n", TableName, chain, allowMark)
		return
	}
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s meta mark set 0x%x\n", TableName, chain, allowSetName(s, "v4"), allowMark)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s meta mark set 0x%x\n", TableName, chain, allowSetName(s, "v6"), allowMark)
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s meta mark set 0x%x\n", TableName, chain, dynSetName(s, "v4"), allowMark)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s meta mark set 0x%x\n", TableName, chain, dynSetName(s, "v6"), allowMark)
}

// writeSubjectInputVerdictRules emits the INPUT-path (MITM) enforcement: the
// intercepted traffic is DNATed and delivered locally, so it never traverses
// the forward hook. The input chain keeps the full set-based verdicts —
// accept semantics are normal for local delivery, and the conntrack ORIGINAL
// destination is matched. The trailing rule carries the default action: drop
// for default-deny (deny-first and enforcing), accept for default-allow.
func writeSubjectInputVerdictRules(b *strings.Builder, s subject.Subject, defaultAction string, mitmPort int) {
	if mitmPort <= 0 {
		return
	}
	in := subjectChainIn(s)
	fmt.Fprintf(b, "add rule inet %s %s ct original ip daddr @%s drop\n", TableName, in, denySetName(s, "v4"))
	fmt.Fprintf(b, "add rule inet %s %s ct original ip6 daddr @%s drop\n", TableName, in, denySetName(s, "v6"))
	fmt.Fprintf(b, "add rule inet %s %s ct original ip daddr @%s accept\n", TableName, in, dynSetName(s, "v4"))
	fmt.Fprintf(b, "add rule inet %s %s ct original ip6 daddr @%s accept\n", TableName, in, dynSetName(s, "v6"))
	fmt.Fprintf(b, "add rule inet %s %s ct original ip daddr @%s accept\n", TableName, in, allowSetName(s, "v4"))
	fmt.Fprintf(b, "add rule inet %s %s ct original ip6 daddr @%s accept\n", TableName, in, allowSetName(s, "v6"))
	if defaultAction == policy.ActionDeny {
		fmt.Fprintf(b, "add rule inet %s %s drop\n", TableName, in)
	} else {
		fmt.Fprintf(b, "add rule inet %s %s accept\n", TableName, in)
	}
}

// writeDispatchRule adds the dispatch jumps for a subject: the forward-path
// jump (master dispatch -> subject chain), the prerouting jump (mark chain ->
// subject mark chain, where allow/dyn marks are set), and — with MITM — the
// input dispatch. All match the sandbox source IP ONLY: with
// net.bridge.bridge-nf-call-iptables=1 (the fast-sandbox Firecracker bridge
// topology), frames entering the bridge destined to the bridge itself are
// pulled into the IP stack with skb->dev = the BRIDGE, so any iifname match
// on the pod-side veth would never fire and every rule would be inert. The
// source IP is the dispatch key; unforgeability rests on IPAM (per-sandbox
// unique IP) plus the sandbox lacking NET_ADMIN/NET_RAW, not on iifname. The
// input dispatch additionally requires ct status dnat on the mitmproxy port,
// so only intercepted traffic enters the input enforcement chain; a DIRECT
// connection to the mitm port (no DNAT — a default-allow sandbox talking
// proxy protocol to the gateway) falls through to a drop, closing the
// transparent-interception bypass. Verdict maps cannot jump to chains
// (EOPNOTSUPP on add element), so dispatch is a plain rule per subject;
// removal rebuilds the table (see Remove).
func writeDispatchRule(b *strings.Builder, s subject.Subject, att actionhandler.NetworkAttachment, mitmPort int) {
	if att.IP.Is4() {
		fmt.Fprintf(b, "add rule inet %s %s ip saddr %s jump %s\n",
			TableName, dispatchChain, att.IP, subjectChain(s))
		fmt.Fprintf(b, "add rule inet %s %s ip saddr %s jump %s\n",
			TableName, markChain, att.IP, markChainName(s))
		if mitmPort > 0 {
			fmt.Fprintf(b, "add rule inet %s %s ip saddr %s tcp dport %d ct status dnat jump %s\n",
				TableName, inputChain, att.IP, mitmPort, subjectChainIn(s))
			if att.Gateway.IsValid() {
				fmt.Fprintf(b, "add rule inet %s %s ip saddr %s ip daddr %s tcp dport %d drop\n",
					TableName, inputChain, att.IP, att.Gateway, mitmPort)
			}
		}
		return
	}
	fmt.Fprintf(b, "add rule inet %s %s ip6 saddr %s jump %s\n",
		TableName, dispatchChain, att.IP, subjectChain(s))
	fmt.Fprintf(b, "add rule inet %s %s ip6 saddr %s jump %s\n",
		TableName, markChain, att.IP, markChainName(s))
	if mitmPort > 0 {
		fmt.Fprintf(b, "add rule inet %s %s ip6 saddr %s tcp dport %d ct status dnat jump %s\n",
			TableName, inputChain, att.IP, mitmPort, subjectChainIn(s))
		if att.Gateway.IsValid() {
			fmt.Fprintf(b, "add rule inet %s %s ip6 saddr %s ip6 daddr %s tcp dport %d drop\n",
				TableName, inputChain, att.IP, att.Gateway, mitmPort)
		}
	}
}

// writeResolvedIPsFragment adds DNS-learned IPs to a subject's dynamic sets
// with clamped TTLs (mirrors pkg/nftables lease behavior).
func writeResolvedIPsFragment(b *strings.Builder, s subject.Subject, ips []nftables.ResolvedIP) {
	var v4, v6 []string
	for _, r := range ips {
		addr := r.Addr.Unmap()
		ttl := clampTTL(r.TTL)
		value := fmt.Sprintf("%s timeout %ds", addr, int(ttl/time.Second))
		if addr.Is4() {
			v4 = append(v4, value)
		} else if addr.Is6() {
			v6 = append(v6, value)
		}
	}
	if len(v4) > 0 {
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, dynSetName(s, "v4"), strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, dynSetName(s, "v6"), strings.Join(v6, ", "))
	}
}

func clampTTL(d time.Duration) time.Duration {
	sec := int(d.Seconds()) + nftTTLSlackSec
	sec = min(max(sec, minTTLSec), maxTTLSec)
	return time.Duration(sec) * time.Second
}

// Subject names appear in nft identifiers: sanitize to [a-z0-9_].
func subjectChain(s subject.Subject) string {
	return "subj_" + sanitize(string(s))
}

// markChainName is the subject's regular prerouting mark chain (reached via
// the shared mark hook chain's per-subject jump).
func markChainName(s subject.Subject) string {
	return "mark_" + sanitize(string(s))
}

// subjectChainIn is the per-subject INPUT enforcement chain (MITM traffic).
func subjectChainIn(s subject.Subject) string {
	return subjectChain(s) + "_in"
}

func sanitize(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func staticSetNames(s subject.Subject) []string {
	return []string{
		allowSetName(s, "v4"), allowSetName(s, "v6"),
		denySetName(s, "v4"), denySetName(s, "v6"),
	}
}

// allSetNames includes the dynamic DNS-learned sets; used by the reset
// fragment so a rebind also drops the previous sandbox's leases.
func allSetNames(s subject.Subject) []string {
	return append(staticSetNames(s), dynSetName(s, "v4"), dynSetName(s, "v6"))
}

func allowSetName(s subject.Subject, fam string) string { return subjectChain(s) + "_allow_" + fam }
func denySetName(s subject.Subject, fam string) string  { return subjectChain(s) + "_deny_" + fam }
func dynSetName(s subject.Subject, fam string) string   { return subjectChain(s) + "_dyn_" + fam }

func ipSetType(name string) string {
	if strings.HasSuffix(name, "_v6") {
		return "ipv6_addr"
	}
	return "ipv4_addr"
}
