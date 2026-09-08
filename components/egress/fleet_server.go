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

// Fleet-profile control plane surface: one listener on the Pod
// netns loopback, N subjects. Subject lifecycle is driven by the fast-sandbox
// Sandbox Actions Handler protocol (SET_BINDING / LIFECYCLE_HOOK /
// REMOVE_BINDING delivered by the Fastlet over /_fastlet/v1/actions); policy
// and credential operations ride the proxy route, routed per subject by the
// X-Fast-Sandbox-Uid header injected by fastlet-proxy (the only peers: the
// listener binds 127.0.0.1 and sandbox netns cannot reach it).
//
// Create-then-configure semantics: a push for a UID whose binding has not
// been observed yet is cached as pending (bounded TTL) and applied when
// SET_BINDING registers the subject; the subject is deny-first from
// registration until its data-plane-ready Hook activates the policy, so the
// push can be late, never early-open. When the push carries the optional
// X-Fast-Sandbox-Generation header, a mismatch with the subject's current
// spec generation (recorded at SET_BINDING) drops the pending entry instead
// of applying it (a reset can never carry old policy into a new sandbox).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/alibaba/opensandbox/internal/safego"
)

// subjectHeaderPattern restricts header values to opaque subject IDs.
func validSubjectID(uid string) bool {
	if uid == "" || len(uid) > 128 {
		return false
	}
	for _, r := range uid {
		if r == '/' || r == '\\' || r == 0 {
			return false
		}
	}
	return true
}

// pendingRequest is a cached policy/credential push for a UID whose slot is
// not observed yet.
type pendingRequest struct {
	method   string
	path     string
	body     []byte
	gen      uint64
	hasGen   bool
	deadline time.Time
}

// fleetNftApplier is the per-subject nft surface used by the fleet control plane
// (implemented by fleetnft.Applier; narrowed here for testability).
type fleetNftApplier interface {
	ApplyDenyFirst(ctx context.Context, s subject.Subject, att actionhandler.NetworkAttachment) error
	ApplyPolicy(ctx context.Context, s subject.Subject, pol *policy.NetworkPolicy) error
	Remove(ctx context.Context, s subject.Subject) error
}

// fleetPolicyServer is the multi-subject control plane. It implements
// subject.LifecycleHooks: OnRegistered installs deny-first enforcement
// (nft + gateway DNS redirect + MITM interception) under the registry lock;
// OnRegisteredComplete flushes any cached pending push for the subject.
type fleetPolicyServer struct {
	ctx        context.Context
	reg        *subject.MemoryRegistry
	nft        fleetNftApplier
	pendingTTL time.Duration

	mu      sync.Mutex
	pending map[subject.Subject][]*pendingRequest
	vaults  map[subject.Subject]*credentialvault.Store
	// pendingPolicies holds the SET_BINDING input of a still-denying subject
	// until its data-plane-ready Hook activates it. Deliberately NOT stored
	// in the registry: DNS dispatch must keep denying (fail closed) while the
	// policy has not been made effective.
	pendingPolicies map[subject.Subject]*policy.NetworkPolicy
	// subjGen records the spec generation of the latest SET_BINDING, used to
	// fence cached pending pushes (X-Fast-Sandbox-Generation).
	subjGen map[subject.Subject]uint64
	// subjAtt records the latest network attachment, used for terminal
	// cleanup (gateway refcounts) even when the REMOVE_BINDING envelope omits
	// the attachment block.
	subjAtt map[subject.Subject]actionhandler.NetworkAttachment

	// gatewayDNSRefs maps each subject to its gateway so the shared
	// prerouting REDIRECT (sandbox DNS -> loopback proxy) is installed once
	// per gateway and removed when the last subject using it is gone. Keyed
	// per SUBJECT (not per gateway) so at-least-once SET_BINDING delivery is
	// idempotent: a duplicate registration is a map no-op instead of a
	// double refcount. Injected fns keep the hooks testable without iptables.
	gwMu               sync.Mutex
	gatewayDNSRefs     map[subject.Subject]netip.Addr
	dnsRedirectInstall func(gateway netip.Addr, port int) error
	dnsRedirectRemove  func() error

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

func newFleetPolicyServer(ctx context.Context, reg *subject.MemoryRegistry, nft fleetNftApplier, pendingTTL time.Duration) *fleetPolicyServer {
	if pendingTTL <= 0 {
		pendingTTL = time.Duration(constants.DefaultPendingPushTTL) * time.Second
	}
	return &fleetPolicyServer{
		ctx:                ctx,
		reg:                reg,
		nft:                nft,
		pendingTTL:         pendingTTL,
		pending:            make(map[subject.Subject][]*pendingRequest),
		vaults:             make(map[subject.Subject]*credentialvault.Store),
		pendingPolicies:    make(map[subject.Subject]*policy.NetworkPolicy),
		subjGen:            make(map[subject.Subject]uint64),
		subjAtt:            make(map[subject.Subject]actionhandler.NetworkAttachment),
		gatewayDNSRefs:     make(map[subject.Subject]netip.Addr),
		dnsRedirectInstall: iptables.SetupGatewayDNSRedirect,
		dnsRedirectRemove:  iptables.RemoveGatewayDNSRedirect,
		mitmEntries:        make(map[subject.Subject]iptables.MitmRedirectEntry),
		instanceID:         newHandlerInstanceID(),
	}
}

// SetMitm wires the shared mitmproxy into the control plane: the healthz gate
// and the per-subject interception redirect install/remove. Called once at
// assembly, before the controller starts; a nil gate (MITM disabled) leaves
// the lifecycle hooks skipping interception.
func (s *fleetPolicyServer) SetMitm(gate *mitmproxy.HealthGate, port int, dports []int) {
	s.mitmGate = gate
	if gate == nil {
		return
	}
	s.mitmInstall = func(entries []iptables.MitmRedirectEntry) error {
		return iptables.InstallMitmRedirects(entries, port, dports)
	}
	s.mitmRemove = iptables.RemoveMitmRedirects
}

// mitmRedirectRebuild installs the interception table from the current entry
// map. Callers hold mitmMu. A nil installer (MITM disabled) is a no-op.
func (s *fleetPolicyServer) mitmRedirectRebuild() error {
	if s.mitmInstall == nil {
		return nil
	}
	entries := make([]iptables.MitmRedirectEntry, 0, len(s.mitmEntries))
	for _, e := range s.mitmEntries {
		entries = append(entries, e)
	}
	return s.mitmInstall(entries)
}

// fleetDNSProxyPort is where the shared DNS proxy listens on loopback; the
// per-subject gateway REDIRECT forwards sandbox DNS here.
const fleetDNSProxyPort = 15353

// installGatewayDNSRedirect records a subject's gateway and installs (once)
// the prerouting REDIRECT for it. Idempotent under at-least-once SET_BINDING
// delivery: a duplicate registration (or a retried deny-first install) is a
// no-op. A rebind that moved the subject to a different gateway releases the
// old gateway first. Fails closed: a subject whose DNS cannot reach the
// proxy must not register as usable.
func (s *fleetPolicyServer) installGatewayDNSRedirect(subj subject.Subject, gateway netip.Addr) error {
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	if old, ok := s.gatewayDNSRefs[subj]; ok {
		if old == gateway {
			return nil // duplicate delivery: already counted for this gateway
		}
		// The subject moved gateways (rebind): release the old one first.
		delete(s.gatewayDNSRefs, subj)
		if s.countGatewayUsersLocked(old) == 0 && s.dnsRedirectRemove != nil {
			_ = s.dnsRedirectRemove()
		}
	}
	s.gatewayDNSRefs[subj] = gateway
	if s.dnsRedirectInstall == nil {
		return nil
	}
	if s.countGatewayUsersLocked(gateway) > 1 {
		return nil // already installed for this gateway
	}
	if err := s.dnsRedirectInstall(gateway, fleetDNSProxyPort); err != nil {
		delete(s.gatewayDNSRefs, subj)
		return err
	}
	return nil
}

// countGatewayUsersLocked counts the subjects currently mapped to a gateway.
// Callers hold gwMu.
func (s *fleetPolicyServer) countGatewayUsersLocked(gateway netip.Addr) int {
	n := 0
	for _, g := range s.gatewayDNSRefs {
		if g == gateway {
			n++
		}
	}
	return n
}

// releaseGatewayDNSRedirect drops the subject's gateway mapping and removes
// the shared REDIRECT table when the last subject using that gateway is gone.
// Idempotent: a duplicate unload is a no-op.
func (s *fleetPolicyServer) releaseGatewayDNSRedirect(subj subject.Subject) {
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	gateway, ok := s.gatewayDNSRefs[subj]
	if !ok {
		return
	}
	delete(s.gatewayDNSRefs, subj)
	if s.countGatewayUsersLocked(gateway) > 0 {
		return
	}
	if s.dnsRedirectRemove != nil {
		if err := s.dnsRedirectRemove(); err != nil {
			log.Warnf("gateway DNS redirect remove (ignored): %v", err)
		}
	}
}

// Handler returns the fleet-profile HTTP mux: the Sandbox Actions Handler
// endpoints (Fastlet, envelope-driven) plus the proxy-route policy and
// credential surfaces (UID-header routed).
func (s *fleetPolicyServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(constants.ActionsStatusPath, s.handleActionsStatus)
	mux.HandleFunc(constants.ActionsDispatchPath, s.handleActions)
	mux.HandleFunc("/policy", s.handlePolicy)
	mux.HandleFunc("/credential-vault", s.handleCredentialVault)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if s.mitmGate != nil && s.mitmGate.MitmPending() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("mitmproxy not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleCredentialVaultActive is the fleet-profile active vault API: one
// shared socket, dispatch inside. The addon carries the flow's client IP
// (REDIRECT/DNAT preserves the source), and the handler resolves clientIp ->
// subject -> that subject's vault snapshot. Unknown IPs 404 (the addon treats
// that as no-vault, no injection). The sidecar's single-vault handler is
// unchanged.
func (s *fleetPolicyServer) handleCredentialVaultActive(w http.ResponseWriter, r *http.Request) {
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
	subj, ok := s.reg.Resolve(subject.SubjectKey{SourceIP: ip})
	if !ok {
		http.Error(w, "no subject for clientIp", http.StatusNotFound)
		return
	}
	snapshot, err := s.vaultFor(subj).ActiveSnapshot()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// subjectOf extracts and validates the routing header. The proxy is the only
// peer on the loopback listener; the header is the subject key, not an auth
// credential (the proxy verifies the route credential before forwarding).
func subjectOf(r *http.Request) (subject.Subject, bool) {
	uid := strings.TrimSpace(r.Header.Get(constants.EgressSubjectUIDHeader))
	if !validSubjectID(uid) {
		return "", false
	}
	return subject.FromSandboxUID(uid), true
}

// pendingGeneration reads the optional fencing header on a push.
func pendingGeneration(r *http.Request) (gen uint64, hasGen bool) {
	raw := strings.TrimSpace(r.Header.Get(constants.EgressSubjectGenerationHeader))
	if raw == "" {
		return 0, false
	}
	gen, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return gen, true
}

func (s *fleetPolicyServer) handlePolicy(w http.ResponseWriter, r *http.Request) {
	subj, ok := subjectOf(r)
	if !ok {
		http.Error(w, "missing or invalid "+constants.EgressSubjectUIDHeader, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handlePolicyGet(w, subj)
	case http.MethodPost, http.MethodPut:
		s.handlePolicyReplace(w, r, subj)
	case http.MethodPatch:
		s.handlePolicyPatch(w, r, subj)
	case http.MethodDelete:
		s.handlePolicyDelete(w, r, subj)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *fleetPolicyServer) handlePolicyGet(w http.ResponseWriter, subj subject.Subject) {
	user := s.reg.UserPolicy(subj)
	state, ok := s.reg.Get(subj)
	if !ok {
		http.Error(w, "unknown subject", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, policyStatusResponse{
		Status: state.String(),
		Mode:   modeFromPolicy(user),
		Policy: user,
	})
}

// applyPolicy applies a policy to a subject. Ordering: nft FIRST, registry
// AFTER — a failed kernel apply leaves the registry (and therefore DNS and
// GET /policy) on the previous policy, so the documented atomic transition
// stays fail-closed: a failed tightening update never leaves the API
// reporting the new policy while the kernel still enforces the old one.
// The nft swap uses the always-rule MERGED policy (reg.EffectiveOf), so
// allow.always/deny.always are enforced at the IP layer too — matching the
// sidecar profile's commitPolicy behavior. The always files are loaded once
// at startup; runtime file changes are not picked up (sidecar reloads them
// every minute).
func (s *fleetPolicyServer) applyPolicy(subj subject.Subject, pol *policy.NetworkPolicy) error {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	eff := s.reg.EffectiveOf(pol)
	nftCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.nft.ApplyPolicy(nftCtx, subj, eff); err != nil {
		return fmt.Errorf("nft policy apply: %w", err)
	}
	if err := s.reg.ApplyPolicy(subj, pol); err != nil {
		return err
	}
	return nil
}

// resolvePolicyPush applies or caches the parsed policy. rawBody is the
// request body as read once by the handler — it is cached verbatim so the
// pending replay applies the EXACT policy the client pushed (the body is
// consumed by parsing, so it must be passed here explicitly).
//
// Lifecycle barrier and authority: the SET_BINDING input (the declarative
// binding) is the authoritative desired value — sandbox.data-plane-ready
// applies exactly what the binding carried. A runtime /policy push for a
// still-DENYING subject is therefore accepted (202) but NEVER stored as the
// pending policy: storing it would override the binding and change what
// data-plane-ready activates (the "first create with an allow policy stays
// DNS-denied" bug). Pushes take effect only once the subject is active (the
// in-place apply path below) or arrive via the registration flush for an
// already-active subject.
func (s *fleetPolicyServer) resolvePolicyPush(w http.ResponseWriter, r *http.Request, subj subject.Subject, pol *policy.NetworkPolicy, rawBody string) {
	state, ok := s.reg.Get(subj)
	if !ok {
		s.cachePending(r, subj, []byte(rawBody))
		writeJSON(w, http.StatusAccepted, policyStatusResponse{
			Status: "pending",
			Reason: "subject not registered yet; push cached",
		})
		return
	}
	if state == subject.StateDenying {
		// The binding input is authoritative while the subject is denying;
		// a runtime push must not override what data-plane-ready will apply.
		writeJSON(w, http.StatusAccepted, policyStatusResponse{
			Status: "pending",
			Reason: "subject denying; SET_BINDING input is authoritative",
		})
		return
	}
	if err := s.applyPolicy(subj, pol); err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("fleet policy apply (%s): %v", subj, err))
		http.Error(w, fmt.Sprintf("policy apply failed: %v", err), http.StatusInternalServerError)
		return
	}
	logEgressUpdated(pol.DefaultAction, pol.Egress)
	writeJSON(w, http.StatusOK, policyStatusResponse{
		Status:          "ok",
		Mode:            modeFromPolicy(pol),
		EnforcementMode: constants.PolicyDnsNft,
	})
}

func (s *fleetPolicyServer) handlePolicyReplace(w http.ResponseWriter, r *http.Request, subj subject.Subject) {
	raw, err := readPolicyRequestBody(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}
	var pol *policy.NetworkPolicy
	if strings.TrimSpace(raw) == "" {
		pol = policy.DefaultDenyPolicy() // empty push = reset to deny-all
	} else {
		pol, err = policy.ParsePolicy(raw)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid policy: %v", err), http.StatusBadRequest)
			return
		}
	}
	s.resolvePolicyPush(w, r, subj, pol, raw)
}

func (s *fleetPolicyServer) handlePolicyPatch(w http.ResponseWriter, r *http.Request, subj subject.Subject) {
	raw, err := readPolicyRequestBody(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}
	var patchRules []policy.EgressRule
	if err := json.Unmarshal([]byte(raw), &patchRules); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch rules: %v", err), http.StatusBadRequest)
		return
	}
	if len(patchRules) == 0 {
		http.Error(w, "invalid patch rules: empty array", http.StatusBadRequest)
		return
	}
	newPolicy, err := patchMergedPolicy(s.reg.UserPolicy(subj), patchRules)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid merged policy: %v", err), http.StatusBadRequest)
		return
	}
	s.resolvePolicyPush(w, r, subj, newPolicy, raw)
}

func (s *fleetPolicyServer) handlePolicyDelete(w http.ResponseWriter, r *http.Request, subj subject.Subject) {
	raw, err := readPolicyRequestBody(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}
	var targets []string
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		http.Error(w, fmt.Sprintf("invalid delete targets: %v", err), http.StatusBadRequest)
		return
	}
	base := s.reg.UserPolicy(subj)
	if base == nil {
		http.Error(w, "unknown subject", http.StatusNotFound)
		return
	}
	newEgress, _ := removeRulesByTarget(base.Egress, targets)
	if len(newEgress) == len(base.Egress) {
		writeJSON(w, http.StatusOK, policyStatusResponse{Status: "ok", Mode: modeFromPolicy(base), Reason: "no matching targets found"})
		return
	}
	newPolicy, err := policy.ParsePolicy(string(mustJSON(policy.NetworkPolicy{DefaultAction: base.DefaultAction, Egress: newEgress})))
	if err != nil {
		http.Error(w, fmt.Sprintf("internal error: %v", err), http.StatusInternalServerError)
		return
	}
	s.resolvePolicyPush(w, r, subj, newPolicy, raw)
}

func (s *fleetPolicyServer) handleCredentialVault(w http.ResponseWriter, r *http.Request) {
	subj, ok := subjectOf(r)
	if !ok {
		http.Error(w, "missing or invalid "+constants.EgressSubjectUIDHeader, http.StatusBadRequest)
		return
	}
	// Read the body once up front so pending caching replays the EXACT
	// pushed revision (ReadJSON below would otherwise consume it).
	var body []byte
	if r.Method != http.MethodGet {
		body, _ = io.ReadAll(io.LimitReader(r.Body, maxPolicyBodyBytes))
	}
	if _, ok := s.reg.Get(subj); !ok {
		// Credential pushes ride the same pending path as policy pushes.
		if r.Method == http.MethodGet {
			http.Error(w, "unknown subject", http.StatusNotFound)
			return
		}
		s.cachePending(r, subj, body)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	vault := s.vaultFor(subj)
	switch r.Method {
	case http.MethodGet:
		state, err := vault.Sanitized()
		if err != nil {
			credentialvault.WriteError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case http.MethodPost:
		var req credentialvault.CreateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid credential vault request: %v", err), http.StatusBadRequest)
			return
		}
		state, err := vault.Create(req, s.reg.EffectivePolicy(subj))
		if err != nil {
			credentialvault.WriteError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, state)
	case http.MethodPatch:
		var req credentialvault.MutationRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid credential vault mutation request: %v", err), http.StatusBadRequest)
			return
		}
		state, err := vault.Patch(req, s.reg.EffectivePolicy(subj))
		if err != nil {
			credentialvault.WriteError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case http.MethodDelete:
		if err := vault.Delete(); err != nil {
			credentialvault.WriteError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// vaultFor returns the memory-only per-subject vault, created on first use.
func (s *fleetPolicyServer) vaultFor(subj subject.Subject) *credentialvault.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vaults[subj]; ok {
		return v
	}
	// Fleet profile: no token/mitm gating; the proxy route is the auth. The
	// vault holds complete revisions memory-only (OSEP-0012 model).
	v := credentialvault.NewStore(nil, func() bool { return true })
	s.vaults[subj] = v
	return v
}

// ---------------------------------------------------------------------------
// Pending cache
// ---------------------------------------------------------------------------

func (s *fleetPolicyServer) cachePending(r *http.Request, subj subject.Subject, body []byte) {
	gen, hasGen := pendingGeneration(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[subj] = append(s.pending[subj], &pendingRequest{
		method:   r.Method,
		path:     r.URL.Path,
		body:     body,
		gen:      gen,
		hasGen:   hasGen,
		deadline: time.Now().Add(s.pendingTTL),
	})
}

// OnRegistered implements subject.LifecycleHooks: deny-first enforcement
// (nft rules + gateway DNS redirect + MITM interception). Runs under the
// registry write lock, so no registry calls here.
func (s *fleetPolicyServer) OnRegistered(subj subject.Subject, att actionhandler.NetworkAttachment) error {
	nftCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.nft.ApplyDenyFirst(nftCtx, subj, att); err != nil {
		return err
	}
	if err := s.installGatewayDNSRedirect(subj, att.Gateway); err != nil {
		// Fail closed: sandbox DNS addressed to gateway:53 must reach the
		// proxy; without the redirect the sandbox would fall back to a
		// resolver the policy cannot see.
		return err
	}
	if err := s.setMitmRedirect(subj, att, false); err != nil {
		// Fail closed: a sandbox whose HTTP(S) is not intercepted must not
		// register as usable (it could exfiltrate credentials-bearing
		// traffic the MITM layer is responsible for). Roll back the gateway
		// DNS redirect installed above — the caller retries OnRegistered,
		// and an unreleased mapping would keep the gateway redirect forever.
		s.releaseGatewayDNSRedirect(subj)
		return err
	}
	log.Infof("subject %s deny-first enforced (nft + gateway redirect + mitm redirect)", subj)
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
func (s *fleetPolicyServer) setMitmRedirect(subj subject.Subject, att actionhandler.NetworkAttachment, keepOnError bool) error {
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

// OnRegisteredComplete implements subject.LifecycleHooks: after the registry
// lock is released, flush every cached pending push for the subject IN
// ORDER (policy and vault pushes are kept independently, so create-then-
// configure replays both). specGen is the spec generation of the current
// SET_BINDING. Best effort: a failure leaves the affected operation
// unapplied and the server re-pushes (idempotent).
func (s *fleetPolicyServer) OnRegisteredComplete(subj subject.Subject, att actionhandler.NetworkAttachment, specGen uint64) {
	for _, p := range s.takePendingAll(subj, specGen) {
		if err := s.replayPending(p, subj); err != nil {
			logEgressUpdateFailedError(fmt.Sprintf("pending push flush for %s failed: %v", subj, err))
		}
	}
}

// takePendingAll atomically removes and returns every pending request for the
// subject, in arrival order. When a push carried a generation header, a
// mismatch with the subject's current spec generation drops that entry
// instead — a delayed push from a previous sandbox of the same UID can never
// carry old policy into a new sandbox.
func (s *fleetPolicyServer) takePendingAll(subj subject.Subject, specGen uint64) []*pendingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	qs := s.pending[subj]
	delete(s.pending, subj)
	out := qs[:0]
	for _, p := range qs {
		if p.hasGen && p.gen != specGen {
			log.Infof("subject %s: dropped pending push (generation %d != spec generation %d)",
				subj, p.gen, specGen)
			continue
		}
		if time.Now().After(p.deadline) {
			log.Infof("subject %s: dropped expired pending push (%s %s)", subj, p.method, p.path)
			continue
		}
		out = append(out, p)
	}
	return out
}

// replayPending dispatches a cached push through the normal handler path.
func (s *fleetPolicyServer) replayPending(p *pendingRequest, subj subject.Subject) error {
	r, err := http.NewRequestWithContext(s.ctx, p.method, p.path, strings.NewReader(string(p.body)))
	if err != nil {
		return err
	}
	r.Header.Set(constants.EgressSubjectUIDHeader, strings.TrimPrefix(string(subj), "s-"))
	if p.hasGen {
		r.Header.Set(constants.EgressSubjectGenerationHeader, fmt.Sprintf("%d", p.gen))
	}
	rec := &recordingResponseWriter{header: http.Header{}}
	s.Handler().ServeHTTP(rec, r)
	if rec.status >= 400 {
		return fmt.Errorf("replay %s %s: http %d: %s", p.method, p.path, rec.status, rec.body.String())
	}
	return nil
}

// StartPendingSweep drops expired pending entries in the background.
func (s *fleetPolicyServer) StartPendingSweep(ctx context.Context) {
	safego.Go(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				s.mu.Lock()
				for subj, qs := range s.pending {
					kept := qs[:0]
					for _, p := range qs {
						if now.After(p.deadline) {
							continue
						}
						kept = append(kept, p)
					}
					if len(kept) == 0 {
						delete(s.pending, subj)
					} else {
						s.pending[subj] = kept
					}
				}
				s.mu.Unlock()
			}
		}
	})
}

// OnUnloaded implements subject.LifecycleHooks: remove enforcement and drop
// any cached push, pending policy, and vault (stale for a new sandbox of the
// same UID). The gateway refcount is released when the last subject using it
// goes away.
//
// Enforcement removal runs FIRST: a transient nft failure leaves every other
// teardown step undone, and the caller keeps the subject registered so the
// retried terminal cleanup re-runs everything (no double gateway release, no
// stale rules).
func (s *fleetPolicyServer) OnUnloaded(subj subject.Subject, att actionhandler.NetworkAttachment) error {
	nftCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.nft.Remove(nftCtx, subj); err != nil {
		return err
	}
	s.dropSubjectState(subj)
	s.releaseGatewayDNSRedirect(subj)
	s.removeMitmRedirect(subj)
	log.Infof("subject %s enforcement removed", subj)
	return nil
}

// dropSubjectState removes every piece of per-subject bookkeeping (pending
// pushes, vault, pending policy, spec generation, attachment).
func (s *fleetPolicyServer) dropSubjectState(subj subject.Subject) {
	s.mu.Lock()
	delete(s.pending, subj)
	delete(s.vaults, subj)
	delete(s.pendingPolicies, subj)
	delete(s.subjGen, subj)
	delete(s.subjAtt, subj)
	s.mu.Unlock()
}

// dropPendingPushes removes the cached pending pushes for a subject (binding
// removal: the cached pushes are stale for the removed binding).
func (s *fleetPolicyServer) dropPendingPushes(subj subject.Subject) {
	s.mu.Lock()
	delete(s.pending, subj)
	s.mu.Unlock()
}

// recordBindingState stores the SET_BINDING bookkeeping for a registered
// subject: the spec generation (pending-push fencing) and the network
// attachment (terminal cleanup). Called after the registry lock is released.
func (s *fleetPolicyServer) recordBindingState(subj subject.Subject, att actionhandler.NetworkAttachment, specGen uint64) {
	s.mu.Lock()
	s.subjGen[subj] = specGen
	s.subjAtt[subj] = att
	s.mu.Unlock()
}

// attachment returns the last observed network attachment for a registered
// subject (used by terminal cleanup when the REMOVE_BINDING envelope omits
// the attachment block).
func (s *fleetPolicyServer) attachment(subj subject.Subject) (actionhandler.NetworkAttachment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	att, ok := s.subjAtt[subj]
	return att, ok
}

// storePendingPolicy holds the SET_BINDING policy of a still-denying subject
// until its data-plane-ready Hook activates it. DNS dispatch keeps denying
// while the policy is only pending (fail closed).
func (s *fleetPolicyServer) storePendingPolicy(subj subject.Subject, pol *policy.NetworkPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPolicies[subj] = pol
	log.Infof("subject %s: policy stored (deny-first until data-plane-ready)", subj)
}

// clearPendingPolicy drops a subject's pending policy (binding removal or
// unload).
func (s *fleetPolicyServer) clearPendingPolicy(subj subject.Subject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pendingPolicies, subj)
}

// pendingPolicy returns the subject's pending policy without consuming it
// (a failed data-plane-ready apply must leave it in place for the retry).
func (s *fleetPolicyServer) pendingPolicy(subj subject.Subject) *policy.NetworkPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingPolicies[subj]
}

// revertToDenyFirst returns an active subject to deny-first (SET_BINDING with
// a null input: the binding was removed from a still-live sandbox, so the
// sandbox must be fully blocked again). nft commits before the registry
// state, so the transition stays fail-closed.
func (s *fleetPolicyServer) revertToDenyFirst(subj subject.Subject, att actionhandler.NetworkAttachment) error {
	nftCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
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
func (s *fleetPolicyServer) removeMitmRedirect(subj subject.Subject) {
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

// recordingResponseWriter captures handler output for pending replays.
type recordingResponseWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *recordingResponseWriter) Header() http.Header         { return w.header }
func (w *recordingResponseWriter) WriteHeader(status int)      { w.status = status }
func (w *recordingResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
