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

// Fast Sandbox-profile control plane surface: one listener on the Pod
// netns loopback, N subjects. Subject lifecycle is driven by the fast-sandbox
// Sandbox Actions Handler protocol (SET_BINDING / LIFECYCLE_HOOK /
// REMOVE_BINDING delivered by the Fastlet over /_fastlet/v1/actions); the
// binding input is the authoritative policy and the credential vault is
// egress-held behind the FastPath handler-relay (OSEP-0022) — the listener
// itself has no proxy-route surface (the only peers: the listener binds
// 127.0.0.1 and sandbox netns cannot reach it).
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// fastSandboxNftApplier is the per-subject nft surface used by the fast-sandbox control plane
// (implemented by fastsandboxnft.Applier; narrowed here for testability).
type fastSandboxNftApplier interface {
	ApplyDenyFirst(ctx context.Context, s subject.Subject, att actionhandler.NetworkAttachment) error
	ApplyPolicy(ctx context.Context, s subject.Subject, pol *policy.NetworkPolicy) error
	Remove(ctx context.Context, s subject.Subject) error
}

// fastSandboxPolicyServer is the multi-subject control plane. It implements
// subject.LifecycleHooks: OnRegistered installs deny-first enforcement
// (nft + gateway DNS redirect + MITM interception) under the registry lock.
type fastSandboxPolicyServer struct {
	ctx context.Context
	reg *subject.MemoryRegistry
	nft fastSandboxNftApplier

	// actionGate serializes registry writers without making cancellation wait
	// for a mutex holder. Vault transactions use mu, never this gate.
	actionGate chan struct{}

	// Never acquire mu while holding a registry lock (including callbacks).
	// Registry lookups also stay outside mu so kernel I/O cannot pin mu.
	// Pre-read identities are validated against subjFence under mu.
	mu      sync.Mutex
	rebinds map[subject.Subject]*subjectRebind
	// pendingPolicies holds the SET_BINDING input of a still-denying subject
	// until its data-plane-ready Hook activates it. Deliberately NOT stored
	// in the registry: DNS dispatch must keep denying (fail closed) while the
	// policy has not been made effective.
	pendingPolicies map[subject.Subject]*policy.NetworkPolicy
	vaults          map[subject.Subject]vaultEntry
	// subjAtt records the latest validated registration attempt, including
	// failed enforcement, for same-fence comparison and old-IP cleanup.
	subjAtt map[subject.Subject]actionhandler.NetworkAttachment
	// subjFence mirrors the registered fence per subject: the relay paths
	// finish registry pre-reads before taking s.mu; no registry lock spans mu.
	// The mirror is published after successful registration, and
	// this in-lock comparison rejects requests whose pre-read went stale
	// across a rebind.
	subjFence map[subject.Subject]subject.Fencing

	// vaultFiles owns the per-subject vault files; nil keeps the vault
	// memory-only (the default, and the sidecar posture).
	vaultFiles *vaultPersistence
	// vaultRecovery is the sticky recovery latch: non-nil freezes vault
	// writes until the process restarts (first reason wins; reads are
	// lock-free so healthz never contends).
	vaultRecovery atomic.Pointer[vaultRecoveryState]

	// policies tracks the recorded policy of each subject: the revision the
	// relay preconditions fence on, the runtime fence the state was
	// materialized under, and whether the policy is file-backed (loaded from
	// the persisted file — the binding input no longer drives it). The
	// policy itself lives in the registry (active subjects) or
	// pendingPolicies (denying subjects); this entry carries only the
	// bookkeeping the vault entry carries for vault state.
	policies map[subject.Subject]*policyEntry
	// policyFiles owns the per-subject policy files. Always non-nil in the
	// fast-sandbox profile (startup fails without it); nil only in unit
	// tests that assemble the server directly.
	policyFiles *policyPersistence
	// policyRecovery is the policy persistence recovery latch (same
	// mechanics as vaultRecovery).
	policyRecovery atomic.Pointer[policyRecoveryState]
	// policyCommitMu serializes policy file-commit operations process-wide
	// (relay writers from different server replicas do not serialize with
	// each other or with a concurrent SET_BINDING commit).
	policyCommitMu sync.Mutex

	// gatewayDNSRefs maps each subject to its gateway; the shared prerouting
	// REDIRECT table is rebuilt from its complete gateway set. The staged map
	// is published only after the nft replacement succeeds, so failures retain
	// ownership for a later lifecycle retry. gwMu serializes map/table updates.
	gwMu             sync.Mutex
	gatewayDNSRefs   map[subject.Subject]netip.Addr
	dnsRedirectApply func(ctx context.Context, gateways []netip.Addr, port int) error

	// mitmMu guards the per-subject interception entries; the Pod-netns table
	// is rebuilt wholesale from this map on every change (see
	// pkg/iptables.InstallMitmRedirects). nil mitmInstall = MITM disabled
	// (the hooks skip interception entirely).
	mitmMu      sync.Mutex
	mitmEntries map[subject.Subject]iptables.MitmRedirectEntry
	mitmInstall func(entries []iptables.MitmRedirectEntry) error
	mitmRemove  func() error
	mitmGate    *mitmproxy.HealthGate // nil when MITM disabled

	policyMu sync.Mutex // serializes policy applies (registry + nft stay ordered)

	// instanceID identifies this Handler process incarnation; a changed value
	// makes the Fastlet invalidate Binding readiness and replay SET_BINDING
	// followed by the reached Hooks (restart recovery).
	instanceID string
}

func newFastSandboxPolicyServer(ctx context.Context, reg *subject.MemoryRegistry, nft fastSandboxNftApplier) *fastSandboxPolicyServer {
	return &fastSandboxPolicyServer{
		ctx:              ctx,
		reg:              reg,
		nft:              nft,
		actionGate:       make(chan struct{}, 1),
		rebinds:          make(map[subject.Subject]*subjectRebind),
		vaults:           make(map[subject.Subject]vaultEntry),
		pendingPolicies:  make(map[subject.Subject]*policy.NetworkPolicy),
		policies:         make(map[subject.Subject]*policyEntry),
		subjAtt:          make(map[subject.Subject]actionhandler.NetworkAttachment),
		subjFence:        make(map[subject.Subject]subject.Fencing),
		gatewayDNSRefs:   make(map[subject.Subject]netip.Addr),
		dnsRedirectApply: iptables.ReplaceGatewayDNSRedirects,
		mitmEntries:      make(map[subject.Subject]iptables.MitmRedirectEntry),
		instanceID:       newHandlerInstanceID(),
	}
}

// SetMitm wires the shared mitmproxy into the control plane: the healthz gate
// and the per-subject interception redirect install/remove. Called once at
// assembly, before the controller starts; a nil gate (MITM disabled) leaves
// the lifecycle hooks skipping interception. A non-nil gate with port <= 0 is
// the addon-down assembly (pinned CA unavailable): the gate stays wired so
// /healthz and the actions status payload surface the down reason, while the
// lifecycle hooks skip interception.
func (s *fastSandboxPolicyServer) SetMitm(gate *mitmproxy.HealthGate, port int, dports []int) {
	s.mitmGate = gate
	if gate == nil || port <= 0 {
		return
	}
	s.mitmInstall = func(entries []iptables.MitmRedirectEntry) error {
		return iptables.InstallMitmRedirects(entries, port, dports)
	}
	s.mitmRemove = iptables.RemoveMitmRedirects
}

// mitmRedirectRebuild installs the interception table from the current entry
// map. Callers hold mitmMu. A nil installer (MITM disabled) is a no-op.
func (s *fastSandboxPolicyServer) mitmRedirectRebuild() error {
	if s.mitmInstall == nil {
		return nil
	}
	entries := make([]iptables.MitmRedirectEntry, 0, len(s.mitmEntries))
	for _, e := range s.mitmEntries {
		entries = append(entries, e)
	}
	return s.mitmInstall(entries)
}

// fastSandboxDNSProxyPort is where the shared DNS proxy listens on loopback; the
// per-subject gateway REDIRECT forwards sandbox DNS here.
const fastSandboxDNSProxyPort = 15353

// policyApplyTimeout bounds one kernel policy transaction on every path
// that runs under policyCommitMu (see applyPolicy).
const policyApplyTimeout = 10 * time.Second

// installGatewayDNSRedirect stages a subject's gateway and atomically
// rebuilds the shared prerouting table from the complete desired set. Always
// reconcile, including duplicate delivery: a prior command may have applied
// but returned an ambiguous error. Fails closed: a subject whose DNS cannot
// reach the proxy must not register as usable.
func (s *fastSandboxPolicyServer) installGatewayDNSRedirect(ctx context.Context, subj subject.Subject, gateway netip.Addr) error {
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	next := cloneGatewayDNSRefs(s.gatewayDNSRefs)
	next[subj] = gateway.Unmap()
	if err := s.reconcileGatewayDNSRedirectsLocked(ctx, next); err != nil {
		return fmt.Errorf("gateway DNS redirect install: %w", err)
	}
	s.gatewayDNSRefs = next
	return nil
}

// releaseGatewayDNSRedirect stages ownership removal and reconciles the full
// set even when the subject is absent. A previous apply may have succeeded
// while its result was lost, leaving no committed map entry to drive cleanup.
// On failure, retain the old map for a retry.
func (s *fastSandboxPolicyServer) releaseGatewayDNSRedirect(ctx context.Context, subj subject.Subject) error {
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	next := cloneGatewayDNSRefs(s.gatewayDNSRefs)
	delete(next, subj)
	if err := s.reconcileGatewayDNSRedirectsLocked(ctx, next); err != nil {
		return fmt.Errorf("gateway DNS redirect release: %w", err)
	}
	s.gatewayDNSRefs = next
	return nil
}

// reconcileGatewayDNSRedirectsLocked derives a normalized gateway set from
// the staged subject map. Callers hold gwMu; the applier atomically replaces
// the table and treats an empty set as removal of the table.
func (s *fastSandboxPolicyServer) reconcileGatewayDNSRedirectsLocked(ctx context.Context, refs map[subject.Subject]netip.Addr) error {
	if s.dnsRedirectApply == nil {
		return nil
	}
	set := make(map[netip.Addr]struct{}, len(refs))
	for _, gateway := range refs {
		set[gateway.Unmap()] = struct{}{}
	}
	gateways := make([]netip.Addr, 0, len(set))
	for gateway := range set {
		gateways = append(gateways, gateway)
	}
	return s.dnsRedirectApply(ctx, gateways, fastSandboxDNSProxyPort)
}

func cloneGatewayDNSRefs(refs map[subject.Subject]netip.Addr) map[subject.Subject]netip.Addr {
	clone := make(map[subject.Subject]netip.Addr, len(refs))
	for subj, gateway := range refs {
		clone[subj] = gateway
	}
	return clone
}

// Handler returns the fast-sandbox-profile HTTP mux: the Sandbox Actions
// Handler endpoints, the relay surfaces (/policy/{uid}, /vault/{uid}), and
// the healthz probe. No proxy-route surface.
func (s *fastSandboxPolicyServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(constants.ActionsStatusPath, s.handleActionsStatus)
	mux.HandleFunc(constants.ActionsDispatchPath, s.handleActions)
	mux.HandleFunc(constants.VaultRelayPathPrefix, s.handleVaultRelay)
	mux.HandleFunc(constants.PolicyRelayPathPrefix, s.handlePolicyRelay)
	mux.HandleFunc("/healthz", s.handleHealthz)
	return mux
}

// healthzResponse is the /healthz body: status plus the pinned-CA
// observability fields (OSEP-0022). The pre-create probe consumes the HTTP
// status; the body is for operators and dashboards.
type healthzResponse struct {
	Status string `json:"status"`
	// MitmCaFingerprint is the pinned CA's SHA-256 fingerprint (lowercase
	// hex); empty when MITM is disabled or no CA is provisioned.
	MitmCaFingerprint string `json:"mitmCaFingerprint,omitempty"`
	// MitmCaDown is the explicit addon-down signal: the pinned CA is missing
	// or unloadable, the MITM addon refused to start, and the down state is
	// latched until process restart.
	MitmCaDown  bool   `json:"mitmCaDown,omitempty"`
	MitmCaError string `json:"mitmCaError,omitempty"`
	// VaultHealthIssue is the non-empty vault-persistence reason (recovery
	// latch; see fastsandbox_vault_persist.go). Status mirrors its class.
	VaultHealthIssue string `json:"vaultHealthIssue,omitempty"`
	// PolicyHealthIssue is the non-empty policy-persistence reason (recovery
	// latch; see fastsandbox_policy_persist.go).
	PolicyHealthIssue string `json:"policyHealthIssue,omitempty"`
}

// handleHealthz: 503 while the mitm stack is not ready (pre-create probe) and
// 503 with an explicit addon-down signal when the pinned CA failed (permanent
// until process restart) — the runtime driver refuses sandbox creation for
// both; policy actions keep serving regardless. The vault recovery latch
// reports 503 the same way.
func (s *fastSandboxPolicyServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if s.mitmGate != nil {
		if reason, down := s.mitmGate.DownReason(); down {
			writeJSON(w, http.StatusServiceUnavailable, healthzResponse{
				Status:      "mitm-addon-down",
				MitmCaDown:  true,
				MitmCaError: reason,
			})
			return
		}
		if issue := s.vaultHealthIssue(); issue != "" {
			writeJSON(w, http.StatusServiceUnavailable, healthzResponse{
				Status:           "vault-unhealthy",
				VaultHealthIssue: issue,
			})
			return
		}
		if issue := s.policyHealthIssue(); issue != "" {
			writeJSON(w, http.StatusServiceUnavailable, healthzResponse{
				Status:            "policy-unhealthy",
				PolicyHealthIssue: issue,
			})
			return
		}
		if s.mitmGate.MitmPending() {
			writeJSON(w, http.StatusServiceUnavailable, healthzResponse{Status: "mitmproxy-not-ready"})
			return
		}
		writeJSON(w, http.StatusOK, healthzResponse{
			Status:            "ok",
			MitmCaFingerprint: s.mitmGate.CAFingerprint(),
		})
		return
	}
	if issue := s.vaultHealthIssue(); issue != "" {
		writeJSON(w, http.StatusServiceUnavailable, healthzResponse{
			Status:           "vault-unhealthy",
			VaultHealthIssue: issue,
		})
		return
	}
	if issue := s.policyHealthIssue(); issue != "" {
		writeJSON(w, http.StatusServiceUnavailable, healthzResponse{
			Status:            "policy-unhealthy",
			PolicyHealthIssue: issue,
		})
		return
	}
	writeJSON(w, http.StatusOK, healthzResponse{Status: "ok"})
}

// handleCredentialVaultActive is the fast-sandbox-profile active vault API: one
// shared socket, dispatch inside. The addon carries the flow's client IP
// (REDIRECT/DNAT preserves the source), and the handler resolves clientIp ->
// subject -> that subject's vault snapshot. Unknown IPs and subjects without a
// vault return 404. Conditional requests return 304 when the subject's opaque
// active-snapshot tag is unchanged.
func (s *fastSandboxPolicyServer) handleCredentialVaultActive(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("clientIp"))
	if raw == "" {
		http.Error(w, "clientIp query parameter required", http.StatusBadRequest)
		return
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid clientIp %q", raw), http.StatusBadRequest)
		return
	}
	subj, resolved := s.reg.Resolve(subject.SubjectKey{SourceIP: ip})
	fence, registered := s.reg.Fence(subj)
	respond := func() func(http.ResponseWriter) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !resolved {
			return func(w http.ResponseWriter) { http.Error(w, "no subject for clientIp", http.StatusNotFound) }
		}
		if s.subjectRebindingLocked(subj) {
			return func(w http.ResponseWriter) {
				http.Error(w, "subject rebind in progress", http.StatusServiceUnavailable)
			}
		}
		if !registered || !s.fenceMatchesLocked(subj, fence) || s.subjAtt[subj].IP != ip {
			return func(w http.ResponseWriter) { http.Error(w, "no subject for clientIp", http.StatusNotFound) }
		}
		store := s.vaultForLocked(subj, fence)
		if s.vaultSealedLocked(subj) {
			return func(w http.ResponseWriter) { http.Error(w, "subject vault is sealed", http.StatusConflict) }
		}
		if entry := s.vaults[subj]; entry.vaultStateUnknown {
			return func(w http.ResponseWriter) { http.Error(w, "vault state is unknown", http.StatusServiceUnavailable) }
		}
		return captureActiveVaultSnapshot(r, store)
	}()
	respond(w)
}

// applyPolicy applies a policy to a subject. Ordering: nft FIRST, registry
// AFTER — a failed kernel apply leaves the registry (and therefore DNS
// dispatch) on the previous policy, so the documented atomic transition
// stays fail-closed: a failed tightening update never leaves the registry
// reporting the new policy while the kernel still enforces the old one.
// The nft swap uses the always-rule MERGED policy (reg.EffectiveOf), so
// allow.always/deny.always are enforced at the IP layer too — matching the
// sidecar profile's commitPolicy behavior. The always files are loaded once
// at startup; runtime file changes are not picked up (sidecar reloads them
// every minute).
func (s *fastSandboxPolicyServer) applyPolicy(subj subject.Subject, pol *policy.NetworkPolicy) error {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	eff := s.reg.EffectiveOf(pol)
	// Bounded (policyApplyTimeout): every applyPolicy caller runs under
	// policyCommitMu, so one slow nft transaction would head-of-line-block
	// every subject's writes.
	nftCtx, cancel := context.WithTimeout(s.ctx, policyApplyTimeout)
	defer cancel()
	if err := s.nft.ApplyPolicy(nftCtx, subj, eff); err != nil {
		return fmt.Errorf("nft policy apply: %w", err)
	}
	if err := s.reg.ApplyPolicy(subj, pol); err != nil {
		return err
	}
	log.Infof("subject %s: policy applied (defaultAction=%s, rules=%d)", subj, pol.DefaultAction, len(pol.Egress))
	return nil
}

// vaultEntry binds a vault store to the registration fence it was
// materialized under. degraded is non-empty while the persisted vault could
// not be reloaded: the slice stays empty (interception fails closed for that
// subject, policy unaffected) until a successful relay write clears it.
// sealed marks a paused subject whose vault file was exported into the
// snapshot bundle: every vault op answers 409; enforcement is unchanged.
type vaultEntry struct {
	store             *credentialvault.Store
	fence             subject.Fencing
	degraded          string
	sealed            bool
	fileMeta          vaultFileMetadata
	vaultStateUnknown bool
	// vaultStateProtected records that this live fence previously observed a
	// non-v1 or unreadable source. A later missing/legacy file cannot erase that
	// provenance and reopen the v1 writer path.
	vaultStateProtected bool
	// lazyVaultProbe limits retry-on-read to entries first created without a
	// binding reload; ordinary reload failures still require SET_BINDING replay.
	lazyVaultProbe bool
}

// vaultFileMetadata keeps the complete decoded ordering/initialization
// identity beside the read-only live slice. The source file remains the
// durable authority; this metadata prevents a reload from silently dropping
// lineage information even though OS-1 does not write v2 yet.
type vaultFileMetadata struct {
	version          int
	revision         int64
	state            credentialvault.FileState
	lineage          string
	stateRevision    uint64
	initializationID string
}

// vaultFor returns nil for absent or rebinding subjects. Otherwise the vault
// is created on first use (rehydrated
// from its persisted file at registration, OSEP-0022 stage 2).
// A store is only valid under the fence it was created with: a mismatch
// (same-UID rebind without REMOVE_BINDING, or a TOCTOU store materialized
// against an unregistered subject) is dropped, never inherited — a reset
// never carries old credentials into a new sandbox (OSEP-0022). The fence is
// pre-read outside s.mu and checked against the published mirror under s.mu.
func (s *fastSandboxPolicyServer) vaultFor(subj subject.Subject) *credentialvault.Store {
	fence, registered := s.reg.Fence(subj)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subjectRebindingLocked(subj) {
		return nil
	}
	if !registered || !s.fenceMatchesLocked(subj, fence) {
		return nil
	}
	return s.vaultForLocked(subj, fence)
}

// vaultForLocked implements vaultFor; callers hold s.mu.
func (s *fastSandboxPolicyServer) vaultForLocked(subj subject.Subject, fence subject.Fencing) *credentialvault.Store {
	if fence == (subject.Fencing{}) {
		return nil
	}
	if e, ok := s.vaults[subj]; ok {
		if e.fence == fence {
			if e.lazyVaultProbe && e.vaultStateUnknown && !e.vaultStateProtected {
				s.inspectLazyVaultStateLocked(subj, &e)
			}
			if s.vaultFiles != nil && s.vaultFiles.isSealed(subj) {
				e.sealed = true
			}
			s.vaults[subj] = e
			return e.store
		}
		delete(s.vaults, subj)
	}
	v := credentialvault.NewStore(nil, func() bool { return true })
	e := vaultEntry{store: v, fence: fence, lazyVaultProbe: true}
	s.inspectLazyVaultStateLocked(subj, &e)
	if s.vaultFiles != nil && s.vaultFiles.isSealed(subj) {
		e.sealed = true
	}
	s.vaults[subj] = e
	return v
}

// dropVault purges the subject's vault slice (rebind cleanup): memory and,
// when persistence is enabled, the per-subject file. The caller holds s.mu.
func (s *fastSandboxPolicyServer) dropVault(subj subject.Subject) error {
	delete(s.vaults, subj)
	return s.purgeVaultFile(subj)
}

// OnRegistered implements subject.LifecycleHooks: deny-first enforcement
// (nft rules + gateway DNS redirect + MITM interception). Runs under the
// registry write lock, so no registry calls here.
func (s *fastSandboxPolicyServer) OnRegistered(subj subject.Subject, att actionhandler.NetworkAttachment) error {
	// Bounded (policyApplyTimeout): SET_BINDING runs under policyCommitMu,
	// so a slow deny-first install would head-of-line-block every subject's
	// policy writes.
	nftCtx, cancel := context.WithTimeout(s.ctx, policyApplyTimeout)
	defer cancel()
	denyFirstStarted := time.Now()
	if err := s.nft.ApplyDenyFirst(nftCtx, subj, att); err != nil {
		return err
	}
	dnsStarted := time.Now()
	if err := s.installGatewayDNSRedirect(nftCtx, subj, att.Gateway); err != nil {
		// Fail closed: sandbox DNS addressed to gateway:53 must reach the
		// proxy; without the redirect the sandbox would fall back to a
		// resolver the policy cannot see.
		return err
	}
	dnsRedirect := time.Since(dnsStarted)
	mitmStarted := time.Now()
	if err := s.setMitmRedirect(subj, att, false); err != nil {
		// Fail closed: a sandbox whose HTTP(S) is not intercepted must not
		// register as usable (it could exfiltrate credentials-bearing
		// traffic the MITM layer is responsible for). Roll back the gateway
		// DNS redirect installed above — the caller retries OnRegistered,
		// and an unreleased mapping would keep the gateway redirect forever.
		if rollbackErr := s.releaseGatewayDNSRedirect(nftCtx, subj); rollbackErr != nil {
			return fmt.Errorf("mitm redirect: %w (DNS rollback failed: %v)", err, rollbackErr)
		}
		return err
	}
	log.Infof("subject %s deny-first enforced (nft + gateway redirect + mitm redirect) total=%dms denyFirst=%dms dnsRedirect=%dms mitm=%dms",
		subj, time.Since(denyFirstStarted).Milliseconds(), dnsStarted.Sub(denyFirstStarted).Milliseconds(), dnsRedirect.Milliseconds(), time.Since(mitmStarted).Milliseconds())
	return nil
}

// setMitmRedirect upserts the subject's interception entry and rebuilds the
// Pod-netns table. On failure, keepOnError decides whether the entry is
// rolled back (registration: the subject must stay unregistered) or kept
// (the next rebuild converges; a failed rebuild is transactional and leaves
// the previous table live). The entry is validated before the rebuild: the
// gateway must be valid and same-family as the sandbox IP — a cross-family
// rule is an illegal nft expression that would abort the whole transactional
// rebuild.
func (s *fastSandboxPolicyServer) setMitmRedirect(subj subject.Subject, att actionhandler.NetworkAttachment, keepOnError bool) error {
	s.mitmMu.Lock()
	defer s.mitmMu.Unlock()
	if s.mitmInstall == nil {
		return nil
	}
	entry := iptables.MitmRedirectEntry{SandboxIP: att.IP, Gateway: att.Gateway}
	if !entry.SandboxIP.IsValid() || !entry.Gateway.IsValid() {
		return fmt.Errorf("mitm redirect: subject %s has no valid sandbox IP/gateway in attachment", subj)
	}
	if entry.SandboxIP.Unmap().Is4() != entry.Gateway.Unmap().Is4() {
		return fmt.Errorf("mitm redirect: subject %s sandbox IP %s and gateway %s are different families", subj, entry.SandboxIP, entry.Gateway)
	}
	s.mitmEntries[subj] = entry
	if err := s.mitmRedirectRebuild(); err != nil {
		if !keepOnError {
			delete(s.mitmEntries, subj)
		}
		return err
	}
	return nil
}

// OnUnloaded implements subject.LifecycleHooks: remove enforcement only.
// The caller unregisters and then purges local state outside the registry lock.
// Gateway membership is reconciled from the remaining subject owners; a
// failed DNS update is returned so terminal cleanup can retry.
//
// Enforcement removal runs FIRST. A transient nft failure leaves later
// teardown steps undone; a DNS reconcile failure also prevents the caller from
// unregistering the subject and is retried with the same terminal cleanup.
func (s *fastSandboxPolicyServer) OnUnloaded(subj subject.Subject, _ actionhandler.NetworkAttachment) error {
	if err := s.unloadEnforcement(subj); err != nil {
		return err
	}
	log.Infof("subject %s enforcement removed", subj)
	return nil
}

// unloadEnforcement tears down kernel/redirect state only. It must never
// acquire s.mu or read the registry: takeover calls it under the registry
// write lock. Registry calls and their callbacks never span s.mu acquisition.
// nft removal goes first; nft or DNS failure retains the existing cleanup
// retry boundary, and DNS ownership is committed only after reconciliation.
func (s *fastSandboxPolicyServer) unloadEnforcement(subj subject.Subject) error {
	nftCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.nft.Remove(nftCtx, subj); err != nil {
		return err
	}
	if err := s.releaseGatewayDNSRedirect(nftCtx, subj); err != nil {
		return err
	}
	s.removeMitmRedirect(subj)
	return nil
}

// takeoverStaleSubject implements the OSEP-0022 stale-subject defense: an
// incoming SET_BINDING claimed the dispatch key of a still-live subject —
// authoritative that the previous owner is gone. The stale nft rules must go
// first (they match the slot IP: until removed, first-match dispatch would
// send the successor's traffic to the stale policy). Runs under the registry
// write lock, which evicts the stale subject in the same critical section; a
// failure fails the successor's registration (the Fastlet retries).
func (s *fastSandboxPolicyServer) takeoverStaleSubject(stale subject.Subject) error {
	if err := s.unloadEnforcement(stale); err != nil {
		return err
	}
	log.Infof("subject %s taken over (dispatch key claimed by successor)", stale)
	return nil
}

// dropSubjectState removes every piece of per-subject bookkeeping (vault,
// policy record, pending policy, attachment) plus the persisted files —
// terminal cleanup.
//
// Purges run after registry eviction, never in a registry callback.
// The action gate prevents another registration from racing victim cleanup.
func (s *fastSandboxPolicyServer) dropSubjectState(subj subject.Subject) {
	s.mu.Lock()
	if err := s.dropSubjectStateLocked(subj); err != nil {
		s.mu.Unlock()
		log.Warnf("subject %s: local state cleanup refused: %v", subj, err)
		return
	}
	cleared := s.rebinds[subj] != nil
	delete(s.rebinds, subj)
	s.mu.Unlock()
	if cleared {
		log.Infof("subject_rebind_cleared")
	}
}

// dropSubjectStateLocked preserves an in-flight rebind token. Superseding
// SETs use it only after full kernel removal and Unregister; Vault must stay
// guarded until the successor has completed local publication.
func (s *fastSandboxPolicyServer) dropSubjectStateLocked(subj subject.Subject) error {
	if _, err := s.vaultPurgeStatusLocked(subj); err != nil {
		return err
	}
	// v1 lifecycle cleanup is intentionally best-effort: a failed unlink is
	// already logged by purgeVaultFile and the remaining bytes stay fenced to
	// their recorded owner. v2/unknown schemas were refused by the preflight.
	_ = s.dropVault(subj)
	delete(s.policies, subj)
	s.purgePolicyFile(subj)
	delete(s.subjFence, subj)
	delete(s.pendingPolicies, subj)
	delete(s.subjAtt, subj)
	return nil
}

// recordBindingState records a validated network attempt before enforcement.
// A failed initial deny-first may already have kernel effects; retries must
// still compare its network rather than treat missing success metadata as
// evidence that no rules exist. Called outside the registry lock.
func (s *fastSandboxPolicyServer) recordBindingState(subj subject.Subject, att actionhandler.NetworkAttachment) {
	s.mu.Lock()
	s.subjAtt[subj] = att
	s.mu.Unlock()
}

// recordRegisteredFence publishes relay bookkeeping after registration succeeds.
func (s *fastSandboxPolicyServer) recordRegisteredFence(subj subject.Subject, fence subject.Fencing) {
	s.mu.Lock()
	s.subjFence[subj] = fence
	s.mu.Unlock()
}

// fenceMatchesLocked reports whether the recorded fence equals the request
// fence (commit-time re-validation for the pre-read window). Callers hold
// s.mu.
func (s *fastSandboxPolicyServer) fenceMatchesLocked(subj subject.Subject, fence subject.Fencing) bool {
	got, ok := s.subjFence[subj]
	return ok && got == fence
}

// attachment returns the last observed network attachment for a registered
// subject (used by terminal cleanup when the REMOVE_BINDING envelope omits
// the attachment block).
func (s *fastSandboxPolicyServer) attachment(subj subject.Subject) (actionhandler.NetworkAttachment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	att, ok := s.subjAtt[subj]
	return att, ok
}

// storePendingPolicy holds the SET_BINDING policy of a still-denying subject
// until its data-plane-ready Hook activates it. DNS dispatch keeps denying
// while the policy is only pending (fail closed).
func (s *fastSandboxPolicyServer) storePendingPolicy(subj subject.Subject, pol *policy.NetworkPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPolicies[subj] = pol
	log.Infof("subject %s: policy stored (deny-first until data-plane-ready)", subj)
}

// clearPendingPolicy drops a subject's pending policy (binding removal or
// unload).
func (s *fastSandboxPolicyServer) clearPendingPolicy(subj subject.Subject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pendingPolicies, subj)
}

// pendingPolicy returns the subject's pending policy without consuming it
// (a failed data-plane-ready apply must leave it in place for the retry).
func (s *fastSandboxPolicyServer) pendingPolicy(subj subject.Subject) *policy.NetworkPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingPolicies[subj]
}

// revertToDenyFirst returns an active subject to deny-first (SET_BINDING with
// a null input: the binding was removed from a still-live sandbox, so the
// sandbox must be fully blocked again). nft commits before the registry
// state, so the transition stays fail-closed.
func (s *fastSandboxPolicyServer) revertToDenyFirst(subj subject.Subject, att actionhandler.NetworkAttachment) error {
	nftCtx, cancel := context.WithTimeout(s.ctx, policyApplyTimeout)
	defer cancel()
	if err := s.nft.ApplyDenyFirst(nftCtx, subj, att); err != nil {
		return fmt.Errorf("revert to deny-first: %w", err)
	}
	return s.reg.UnsetPolicy(subj)
}

// removeMitmRedirect drops the subject's interception entry and rebuilds the
// table. Best effort: a leftover rule for a dead sandbox's IP is inert (the
// IP is gone with the sandbox; a reused IP is re-registered over a fresh
// rebuild), and a rebuild failure keeps the previous table live.
func (s *fastSandboxPolicyServer) removeMitmRedirect(subj subject.Subject) {
	s.mitmMu.Lock()
	defer s.mitmMu.Unlock()
	if s.mitmInstall == nil {
		return
	}
	if _, ok := s.mitmEntries[subj]; !ok {
		return
	}
	delete(s.mitmEntries, subj)
	if err := s.mitmRedirectRebuild(); err != nil {
		log.Warnf("mitm redirect rebuild after subject %s unload failed, ignoring: %v", subj, err)
	}
}
