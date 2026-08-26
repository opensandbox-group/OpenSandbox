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

// Package sandboxnft installs the per-sandbox netns OUTPUT enforcement
// (defense in depth) for the fleet egress profile. Each sandbox netns carries
// one table mirroring its subject's policy at the OUTPUT hook, installed from
// the host/Pod through `nsenter --net=<slot.HostNetnsPath>` (the "installed
// from host" placement of OSEP-0022).
//
// Enforcement model (inside the sandbox netns):
//
//	table inet opensandbox-fleet-ns
//	  chain output { type filter hook output priority 0; policy drop; }
//	    ct state established,related accept   <- continuing flows stay up
//	    oifname "lo" accept                   <- sandbox-internal loopback
//	    udp/tcp dport 53 to slot.Gateway      <- DNS to the gateway proxy
//	    tcp/udp dport 853 drop                <- DoT bypass blocked
//	    [doh_block sets + tcp dport 443 drop] <- DoH (when enabled)
//	    ip daddr @deny_v4 drop                <- per-subject policy
//	    ip daddr @dyn_v4 accept               <- DNS-learned leases
//	    ip daddr @allow_v4 accept             <- per-subject policy
//	    ... default verdict ...
//
// The Pod-netns forward hook (pkg/fleetnft) is the authoritative enforcer;
// this chain catches the traffic the forward hook never sees (sandbox ->
// host-local destinations, which take the INPUT path) and provides
// defense-in-depth in the deny direction. Because it default-drops, it must
// mirror the full per-subject policy including the DNS-learned dynamic sets
// (fed by pkg/fleetnft's connection refresh), otherwise the primary egress
// path would break.
//
// DNS note: sandbox DNS is addressed to slot.Gateway:53 (rewritten resolv);
// the gateway REDIRECT happens in the Pod netns prerouting, so the sandbox
// OUTPUT chain sees the ORIGINAL gateway:53 destination. The DNS exception
// is scoped to slot.Gateway: a sandbox in deny state must not reach a
// host-local resolver it was never configured to use — that path is exactly
// what this layer covers, and an unscoped dport-53 allowance would bypass
// the policy-aware proxy for INPUT-path traffic.
package sandboxnft

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

// TableName is the per-sandbox netns table, kept distinct from the Pod-netns
// fleet table so a stale leftover can never be confused with live rules.
const TableName = "opensandbox-fleet-ns"

const (
	outputChain    = "output"
	dynSetTimeoutS = 360
	nftTTLSlackSec = 60
	minTTLSec      = 60
	maxTTLSec      = 360
	allowV4Set     = "allow_v4"
	allowV6Set     = "allow_v6"
	denyV4Set      = "deny_v4"
	denyV6Set      = "deny_v6"
	dynV4Set       = "dyn_v4"
	dynV6Set       = "dyn_v6"
	dohBlockV4Set  = "doh_block_v4"
	dohBlockV6Set  = "doh_block_v6"
)

// Options mirrors fleetnft.Options: profile-wide master-chain toggles applied
// to every sandbox table (DoH-443 blocking, same semantics as the sidecar).
type Options struct {
	BlockDoH443    bool
	DoHBlocklistV4 []string
	DoHBlocklistV6 []string
}

// Runner executes an nft script inside a network namespace.
type Runner func(ctx context.Context, netnsPath, script string) ([]byte, error)

// DefaultRunner runs the script through `nsenter --net=<netnsPath> nft -f -`.
// The netns path comes from the slot (HostNetnsPath); the deployment mounts
// sandbox netns paths into the egress container (OSEP-0022 precondition).
func DefaultRunner(ctx context.Context, netnsPath, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nsenter", "--net="+netnsPath, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("sandbox nft apply failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// ErrUnknownSubject is returned when an operation targets a subject whose
// deny-first rules have not been installed yet.
var ErrUnknownSubject = fmt.Errorf("sandboxnft: subject rules not installed")

// installedSubject tracks the enforcement state the applier owns in memory.
type installedSubject struct {
	netnsPath string
	gateway   netip.Addr            // DNS exception scope (slot.Gateway at install time)
	pol       *policy.NetworkPolicy // nil while denying
}

// Applier applies one per-sandbox netns table per subject. All methods are
// safe for concurrent use; each operation is one atomic nft transaction
// inside the subject's netns.
type Applier struct {
	mu       sync.Mutex
	run      Runner
	opts     Options
	subjects map[subject.Subject]installedSubject
}

// NewApplier returns an Applier using r (nil selects DefaultRunner). Pass
// Options to mirror profile-wide rules (DoH-443 blocking) into every sandbox
// table.
func NewApplier(r Runner, opts Options) *Applier {
	if r == nil {
		r = DefaultRunner
	}
	return &Applier{run: r, opts: opts, subjects: make(map[subject.Subject]installedSubject)}
}

// applyWithMissingTableFallback runs the script; if the batch fails because
// `delete table` targets a missing table (fresh netns, or a prior install
// already removed it), retry without the delete line. Mirrors pkg/fleetnft.
func (a *Applier) applyWithMissingTableFallback(ctx context.Context, netnsPath, script string) error {
	if _, err := a.run(ctx, netnsPath, script); err == nil {
		return nil
	} else if missingTable(err) {
		if fallback := removeDeleteTableLine(script); fallback != script {
			if _, retryErr := a.run(ctx, netnsPath, fallback); retryErr == nil {
				return nil
			}
		}
		return err
	} else {
		return err
	}
}

// ApplyDenyFirst registers a subject in deny-first state inside its sandbox
// netns: empty sets, drop-default OUTPUT chain. Fails closed: a subject whose
// netns rules cannot be installed stays denying (the controller retries).
//
// Re-registration (fencing rebind / egress restart re-observation): the table
// is deleted and recreated atomically — the previous sandbox's policy and DNS
// leases never survive (the table lives in the sandbox netns, so a new
// sandbox of the same UID has a fresh netns and therefore a fresh table
// anyway).
func (a *Applier) ApplyDenyFirst(ctx context.Context, s subject.Subject, slot slotsource.Slot) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	writeTableHeader(&b)
	if err := writeSubjectSets(&b, nil); err != nil {
		return err
	}
	if err := writeDoHSets(&b, a.opts); err != nil {
		return err
	}
	writeChain(&b)
	if err := writeChainContent(&b, a.opts, slot.Gateway, policy.ActionDeny); err != nil {
		return err
	}
	if err := a.applyWithMissingTableFallback(ctx, slot.HostNetnsPath, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDenyFirst)
		return err
	}
	a.subjects[s] = installedSubject{netnsPath: slot.HostNetnsPath, gateway: slot.Gateway}
	telemetry.RecordNftablesUpdate()
	return nil
}

// ApplyPolicy swaps a subject's sandbox-netns static sets and chain content
// atomically in one transaction; dynamic DNS-learned sets are untouched.
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
	if err := writePolicySwap(&b, pol); err != nil {
		return err
	}
	if err := writeChainContent(&b, a.opts, inst.gateway, pol.DefaultAction); err != nil {
		return err
	}
	if _, err := a.run(ctx, inst.netnsPath, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpStaticApply)
		return err
	}
	inst.pol = pol
	a.subjects[s] = inst
	telemetry.RecordNftablesUpdate()
	return nil
}

// ApplySlotUpdate reconciles the sandbox table after an unchanged-fencing
// slot update (the controller's OnSlotUpdated): when the netns path or the
// gateway moved, the table is rebuilt in the new netns (or in place for a
// gateway-only change) with the CURRENT policy — deny-first when none —
// keeping the defense-in-depth layer aligned with the dispatch layer. The
// stale table in a previous netns is removed best effort. Unknown subjects
// are a no-op (nothing to reconcile).
func (a *Applier) ApplySlotUpdate(ctx context.Context, s subject.Subject, slot slotsource.Slot) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	inst, ok := a.subjects[s]
	if !ok {
		return nil
	}
	if inst.netnsPath == slot.HostNetnsPath && inst.gateway == slot.Gateway {
		return nil // nothing moved
	}
	oldNetns := inst.netnsPath
	pol := inst.pol

	var b strings.Builder
	writeTableHeader(&b)
	if err := writeSubjectSets(&b, pol); err != nil {
		return err
	}
	if err := writeDoHSets(&b, a.opts); err != nil {
		return err
	}
	writeChain(&b)
	if err := writeChainContent(&b, a.opts, slot.Gateway, defaultActionOf(pol)); err != nil {
		return err
	}
	if err := a.applyWithMissingTableFallback(ctx, slot.HostNetnsPath, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDispatch)
		return err
	}
	a.subjects[s] = installedSubject{netnsPath: slot.HostNetnsPath, gateway: slot.Gateway, pol: pol}
	if oldNetns != slot.HostNetnsPath {
		// Best effort: the old netns may already be gone (the rules die
		// with it); the new table is the enforcement of record.
		if _, err := a.run(ctx, oldNetns, deleteTableScript()); err != nil {
			log.Warnf("sandboxnft: slot update remove of old netns %s for subject %s failed, ignoring: %v", oldNetns, s, err)
		}
	}
	telemetry.RecordNftablesUpdate()
	return nil
}

// Reset deletes the enforcement table in every given netns path and clears
// the applier state. Startup recovery protocol: called BEFORE the slot
// rescan, so a sandbox netns that outlived the previous egress generation
// can never keep enforcing its old policy — the sandbox layer is the only
// enforcement for host-local traffic, which never crosses the Pod forward
// hook. Best effort: a missing netns or table is expected and only logged
// at debug level (it is the common case on a fresh netns), while successful
// deletions are counted as nft updates.
func (a *Applier) Reset(ctx context.Context, netnsPaths []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.subjects = make(map[subject.Subject]installedSubject)
	for _, p := range netnsPaths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if _, err := a.run(ctx, p, deleteTableScript()); err != nil {
			log.Debugf("sandboxnft: reset table in netns %s skipped: %v", p, err)
			continue
		}
		telemetry.RecordNftablesUpdate()
	}
}

func defaultActionOf(pol *policy.NetworkPolicy) string {
	if pol == nil {
		return policy.ActionDeny
	}
	return pol.DefaultAction
}

// AddResolvedIPs adds DNS-learned IPs to the subject's sandbox-netns dynamic
// sets with a bounded timeout (fed by the fleet connection refresh, so the
// sandbox layer and the Pod-netns layer carry identical leases).
func (a *Applier) AddResolvedIPs(ctx context.Context, s subject.Subject, ips []nftables.ResolvedIP) error {
	if len(ips) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	inst, ok := a.subjects[s]
	if !ok {
		return ErrUnknownSubject
	}
	var b strings.Builder
	writeResolvedIPsFragment(&b, ips)
	if b.Len() == 0 {
		return nil
	}
	if _, err := a.run(ctx, inst.netnsPath, b.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	telemetry.RecordNftablesUpdate()
	return nil
}

// Remove deletes the subject's sandbox-netns table. Best effort by design:
// the rules die with the sandbox netns, and the netns may already be gone
// when the unload fires (nsenter fails on a missing path). A removal failure
// is logged, never propagated — the Pod-netns layer (authoritative) already
// dropped the subject's enforcement.
func (a *Applier) Remove(ctx context.Context, s subject.Subject) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	inst, ok := a.subjects[s]
	if !ok {
		return nil
	}
	delete(a.subjects, s)
	if _, err := a.run(ctx, inst.netnsPath, deleteTableScript()); err != nil {
		// Expected when the netns is already destroyed; the rules died with
		// it. Never propagate: the Pod-netns layer (authoritative) already
		// dropped the subject's enforcement.
		log.Warnf("sandboxnft: remove for subject %s (netns %s) failed, ignoring: %v", s, inst.netnsPath, err)
		return nil
	}
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
// it can be retried on a fresh table.
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

// writeTableHeader writes the idempotent table header (delete + add). The
// chain carries its own drop policy, so a table without subjects is still
// fail-closed once the chain is added.
func writeTableHeader(b *strings.Builder) {
	fmt.Fprintf(b, "delete table inet %s\n", TableName)
	fmt.Fprintf(b, "add table inet %s\n", TableName)
}

// deleteTableScript drops the whole sandbox table (per netns, one subject —
// no rebuild needed).
func deleteTableScript() string {
	return fmt.Sprintf("delete table inet %s\n", TableName)
}

// writeSubjectSets creates the subject's static (interval) and dynamic
// (timeout) sets; static sets are populated from the policy when non-nil.
// Set names are fixed per netns: one sandbox per netns, one subject per
// table.
func writeSubjectSets(b *strings.Builder, pol *policy.NetworkPolicy) error {
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", TableName, allowV4Set)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", TableName, allowV6Set)
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", TableName, denyV4Set)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", TableName, denyV6Set)
	fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; timeout %ds; }\n", TableName, dynV4Set, dynSetTimeoutS)
	fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; timeout %ds; }\n", TableName, dynV6Set, dynSetTimeoutS)
	if pol == nil {
		return nil
	}
	allowV4, allowV6, denyV4, denyV6 := pol.StaticIPSets()
	if err := writeSetElements(b, allowV4Set, allowV4); err != nil {
		return err
	}
	if err := writeSetElements(b, allowV6Set, allowV6); err != nil {
		return err
	}
	if err := writeSetElements(b, denyV4Set, denyV4); err != nil {
		return err
	}
	if err := writeSetElements(b, denyV6Set, denyV6); err != nil {
		return err
	}
	return nil
}

// writeChain creates the OUTPUT hook chain. Chain policy is drop (fail
// closed); the per-policy default is an explicit trailing verdict, so the
// chain never needs recreation on a policy swap.
func writeChain(b *strings.Builder) {
	fmt.Fprintf(b, "add chain inet %s %s { type filter hook output priority 0; policy drop; }\n", TableName, outputChain)
}

// writeChainContent emits every rule of the OUTPUT chain: the structural
// allowances (established return traffic, loopback, DNS to the slot gateway),
// the encrypted-DNS blocking mirror (DoT always, DoH when enabled), and the
// set-based policy verdicts with the explicit default action. Used both for
// deny-first installs and (after a chain flush) for policy swaps, so a swap
// can never drop the structural rules.
func writeChainContent(b *strings.Builder, opts Options, gateway netip.Addr, defaultAction string) error {
	fmt.Fprintf(b, "add rule inet %s %s ct state established,related accept\n", TableName, outputChain)
	fmt.Fprintf(b, "add rule inet %s %s oifname \"lo\" accept\n", TableName, outputChain)
	// DNS exception scoped to the configured gateway: a sandbox must not
	// reach a host-local resolver it was never configured to use (that
	// INPUT-path traffic is exactly what this layer covers).
	if gateway.Is4() {
		fmt.Fprintf(b, "add rule inet %s %s ip daddr %s udp dport 53 accept\n", TableName, outputChain, gateway)
		fmt.Fprintf(b, "add rule inet %s %s ip daddr %s tcp dport 53 accept\n", TableName, outputChain, gateway)
	} else {
		fmt.Fprintf(b, "add rule inet %s %s ip6 daddr %s udp dport 53 accept\n", TableName, outputChain, gateway)
		fmt.Fprintf(b, "add rule inet %s %s ip6 daddr %s tcp dport 53 accept\n", TableName, outputChain, gateway)
	}
	fmt.Fprintf(b, "add rule inet %s %s tcp dport 853 drop\n", TableName, outputChain)
	fmt.Fprintf(b, "add rule inet %s %s udp dport 853 drop\n", TableName, outputChain)
	if err := writeDoHBlockRules(b, opts); err != nil {
		return err
	}
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s drop\n", TableName, outputChain, denyV4Set)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s drop\n", TableName, outputChain, denyV6Set)
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s accept\n", TableName, outputChain, dynV4Set)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s accept\n", TableName, outputChain, dynV6Set)
	fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s accept\n", TableName, outputChain, allowV4Set)
	fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s accept\n", TableName, outputChain, allowV6Set)
	if defaultAction == policy.ActionDeny {
		fmt.Fprintf(b, "add rule inet %s %s drop\n", TableName, outputChain)
	} else {
		fmt.Fprintf(b, "add rule inet %s %s accept\n", TableName, outputChain)
	}
	return nil
}

// writeDoHSets creates the DoH-443 blocklist sets and elements. Called only
// on fresh installs (deny-first / slot-update rebuild) where the table was
// just (re)created — re-adding an existing set fails with "File exists", so
// the policy-swap path must NOT emit them (see writeDoHBlockRules).
func writeDoHSets(b *strings.Builder, opts Options) error {
	if !opts.BlockDoH443 || (len(opts.DoHBlocklistV4) == 0 && len(opts.DoHBlocklistV6) == 0) {
		return nil
	}
	if len(opts.DoHBlocklistV4) > 0 {
		fmt.Fprintf(b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", TableName, dohBlockV4Set)
		if err := writeSetElements(b, dohBlockV4Set, opts.DoHBlocklistV4); err != nil {
			return fmt.Errorf("doh blocklist v4: %w", err)
		}
	}
	if len(opts.DoHBlocklistV6) > 0 {
		fmt.Fprintf(b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", TableName, dohBlockV6Set)
		if err := writeSetElements(b, dohBlockV6Set, opts.DoHBlocklistV6); err != nil {
			return fmt.Errorf("doh blocklist v6: %w", err)
		}
	}
	return nil
}

// writeDoHBlockRules emits only the DoH-443 drop rules mirroring the
// Pod-netns master chain: per-family drops against the (already existing)
// blocklist sets, or a bare drop of all tcp 443 in strict mode. Never
// re-creates sets — the swap path flushes only the chain, and the sets
// survive from the install (a re-add would fail with "File exists").
func writeDoHBlockRules(b *strings.Builder, opts Options) error {
	if !opts.BlockDoH443 {
		return nil
	}
	if len(opts.DoHBlocklistV4) == 0 && len(opts.DoHBlocklistV6) == 0 {
		fmt.Fprintf(b, "add rule inet %s %s tcp dport 443 drop\n", TableName, outputChain)
		return nil
	}
	if len(opts.DoHBlocklistV4) > 0 {
		fmt.Fprintf(b, "add rule inet %s %s ip daddr @%s tcp dport 443 drop\n", TableName, outputChain, dohBlockV4Set)
	}
	if len(opts.DoHBlocklistV6) > 0 {
		fmt.Fprintf(b, "add rule inet %s %s ip6 daddr @%s tcp dport 443 drop\n", TableName, outputChain, dohBlockV6Set)
	}
	return nil
}

// writePolicySwap flushes the chain and the static sets and re-adds the
// static elements, so the follow-up writeChainContent completes an atomic
// swap. Dynamic sets and DoH sets are untouched.
func writePolicySwap(b *strings.Builder, pol *policy.NetworkPolicy) error {
	fmt.Fprintf(b, "flush chain inet %s %s\n", TableName, outputChain)
	for _, name := range []string{allowV4Set, allowV6Set, denyV4Set, denyV6Set} {
		fmt.Fprintf(b, "flush set inet %s %s\n", TableName, name)
	}
	allowV4, allowV6, denyV4, denyV6 := pol.StaticIPSets()
	if err := writeSetElements(b, allowV4Set, allowV4); err != nil {
		return err
	}
	if err := writeSetElements(b, allowV6Set, allowV6); err != nil {
		return err
	}
	if err := writeSetElements(b, denyV4Set, denyV4); err != nil {
		return err
	}
	if err := writeSetElements(b, denyV6Set, denyV6); err != nil {
		return err
	}
	return nil
}

// writeSetElements writes static set elements, normalized first (an interval
// set rejects overlapping entries; strict subnets are dropped — shared with
// pkg/fleetnft semantics).
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

// writeResolvedIPsFragment adds DNS-learned IPs to the dynamic sets with
// clamped TTLs (same clamping as pkg/fleetnft).
func writeResolvedIPsFragment(b *strings.Builder, ips []nftables.ResolvedIP) {
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
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, dynV4Set, strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", TableName, dynV6Set, strings.Join(v6, ", "))
	}
}

func clampTTL(d time.Duration) time.Duration {
	sec := int(d.Seconds()) + nftTTLSlackSec
	sec = min(max(sec, minTTLSec), maxTTLSec)
	return time.Duration(sec) * time.Second
}
