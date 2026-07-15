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

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/internal/safego"
	"k8s.io/apimachinery/pkg/util/wait"
)

type policyUpdater interface {
	CurrentPolicy() *policy.NetworkPolicy
	UpdatePolicy(*policy.NetworkPolicy)
	UpdateAlwaysRules(alwaysDeny, alwaysAllow []policy.EgressRule)
}

// nftApplier: static allow/deny sets plus dynamic DNS-learned entries; teardown on shutdown.
type nftApplier interface {
	ApplyStatic(context.Context, *policy.NetworkPolicy) error
	AddResolvedIPs(context.Context, []nftables.ResolvedIP) error
	RemoveEnforcement(context.Context) error
}

// startPolicyServer: runtime POST/GET /policy, GET /healthz. nameserverIPs are merged into every nft
// static apply so the pod’s resolv / private DNS still works alongside user egress rules.
func startPolicyServer(
	proxy policyUpdater,
	nft nftApplier,
	enforcementMode string,
	addr string,
	token string,
	nameserverIPs []netip.Addr,
	policyFile string,
	alwaysDeny, alwaysAllow []policy.EgressRule,
	mitmGate *mitmproxy.HealthGate,
) (*http.Server, error) {
	maxEgressRules := maxEgressRulesFromEnv()
	if maxEgressRules > 0 {
		log.Infof("policy API: max egress rules per policy (POST/PATCH) = %d (set %s=0 to disable)", maxEgressRules, constants.EnvMaxEgressRules)
	}

	mux := http.NewServeMux()
	handler := &policyServer{
		proxy:            proxy,
		nft:              nft,
		token:            token,
		enforcementMode:  enforcementMode,
		nameserverIPs:    nameserverIPs,
		policyFile:       strings.TrimSpace(policyFile),
		maxEgressRules:   maxEgressRules,
		alwaysLoader:     policy.NewAlwaysRuleLoader(time.Minute),
		stopAlwaysReload: make(chan struct{}),
		mitmGate:         mitmGate,
	}
	handler.credentialVault = credentialvault.NewStore(mitmGate, func() bool { return strings.TrimSpace(token) != "" })
	handler.credentialVaultRequireTLS = constants.IsTruthy(os.Getenv(constants.EnvCredentialVaultRequireTLS))
	handler.setAlwaysRules(alwaysDeny, alwaysAllow)

	mux.HandleFunc("/policy", handler.handlePolicy)
	mux.HandleFunc("/credential-vault", handler.handleCredentialVault)
	mux.HandleFunc("/credential-vault/", handler.handleCredentialVaultSubresource)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if mitmGate != nil && mitmGate.MitmPending() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("mitmproxy not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	var activeSrv *http.Server
	var cleanupActiveSocket func(context.Context) error
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		socketPath := envOrDefault(constants.EnvCredentialProxySocket, constants.DefaultCredentialProxySocket)
		_, mitmGID, _, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
		if err != nil {
			return nil, fmt.Errorf("lookup credential proxy user %q: %w", mitmproxy.RunAsUser, err)
		}
		activeSrv, cleanupActiveSocket, err = credentialvault.StartActiveSocketServer(handler.handleCredentialVaultActive, socketPath, int(mitmGID))
		if err != nil {
			return nil, fmt.Errorf("credential vault active socket: %w", err)
		}
		log.Infof("credential vault active API listening on unix socket %s", socketPath)
	}

	srv := &http.Server{Addr: addr, Handler: mux}
	handler.server = srv
	srv.RegisterOnShutdown(func() {
		select {
		case <-handler.stopAlwaysReload:
		default:
			close(handler.stopAlwaysReload)
		}
		if activeSrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := cleanupActiveSocket(shutdownCtx); err != nil {
				log.Errorf("credential vault active socket shutdown error: %v", err)
			}
		}
	})

	errCh := make(chan error, 1)
	safego.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	})

	select {
	case err := <-errCh:
		if activeSrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if cleanupErr := cleanupActiveSocket(shutdownCtx); cleanupErr != nil {
				log.Errorf("credential vault active socket shutdown error: %v", cleanupErr)
			}
			cancel()
		}
		return nil, err
	case <-time.After(200 * time.Millisecond):
		handler.startAlwaysRuleReloadJob()
		safego.Go(func() {
			if err := <-errCh; err != nil {
				log.Errorf("policy server error: %v", err)
			}
		})
		return srv, nil
	}
}

type policyServer struct {
	proxy           policyUpdater
	nft             nftApplier
	server          *http.Server
	token           string
	enforcementMode string
	nameserverIPs   []netip.Addr
	policyFile      string     // if set, successful /policy changes persist (truncate+write+fsync)
	maxEgressRules  int        // 0 = unlimited; cap len(Egress) for POST/PATCH
	mu              sync.Mutex // serializes /policy handlers (no lost update across POST vs PATCH)

	alwaysLoader     *policy.AlwaysRuleLoader
	stopAlwaysReload chan struct{}

	lastAlwaysFP              uint64
	lastAlwaysFPSet           bool
	credentialVault           *credentialvault.Store
	mitmGate                  *mitmproxy.HealthGate
	credentialVaultRequireTLS bool
}

type policyStatusResponse struct {
	Status          string `json:"status,omitempty"`
	Mode            string `json:"mode,omitempty"`
	EnforcementMode string `json:"enforcementMode,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Policy          any    `json:"policy,omitempty"`
}

func (s *policyServer) handlePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w)
	case http.MethodPost, http.MethodPut:
		s.handlePost(w, r)
	case http.MethodPatch:
		s.handlePatch(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *policyServer) handleCredentialVault(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleCredentialVaultGet(w)
	case http.MethodPost:
		s.handleCredentialVaultPost(w, r)
	case http.MethodPatch:
		s.handleCredentialVaultPatch(w, r)
	case http.MethodDelete:
		s.handleCredentialVaultDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *policyServer) handleCredentialVaultSubresource(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/credential-vault/")
	switch {
	case path == "_active":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch {
	case path == "credentials":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCredentialVaultCredentials(w)
	case strings.HasPrefix(path, "credentials/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCredentialVaultCredential(w, strings.TrimPrefix(path, "credentials/"))
	case path == "bindings":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCredentialVaultBindings(w)
	case strings.HasPrefix(path, "bindings/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCredentialVaultBinding(w, strings.TrimPrefix(path, "bindings/"))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *policyServer) handleCredentialVaultGet(w http.ResponseWriter) {
	state, err := s.credentialVault.Sanitized()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *policyServer) handleCredentialVaultPost(w http.ResponseWriter, r *http.Request) {
	if err := s.credentialVault.Ready(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	if s.credentialVaultRequireTLS && !credentialVaultWriteTransportAllowed(r) {
		http.Error(w, "credential vault writes require TLS or loopback transport", http.StatusUpgradeRequired)
		return
	}
	var req credentialvault.CreateRequest
	if err := credentialvault.ReadJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid credential vault request: %v", err), http.StatusBadRequest)
		return
	}
	state, err := s.credentialVault.Create(req, s.effectivePolicy())
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, state)
}

func (s *policyServer) handleCredentialVaultPatch(w http.ResponseWriter, r *http.Request) {
	if err := s.credentialVault.Ready(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	if s.credentialVaultRequireTLS && !credentialVaultWriteTransportAllowed(r) {
		http.Error(w, "credential vault writes require TLS or loopback transport", http.StatusUpgradeRequired)
		return
	}
	var req credentialvault.MutationRequest
	if err := credentialvault.ReadJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid credential vault mutation request: %v", err), http.StatusBadRequest)
		return
	}
	state, err := s.credentialVault.Patch(req, s.effectivePolicy())
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *policyServer) handleCredentialVaultDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.credentialVault.Ready(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	if s.credentialVaultRequireTLS && !credentialVaultWriteTransportAllowed(r) {
		http.Error(w, "credential vault writes require TLS or loopback transport", http.StatusUpgradeRequired)
		return
	}
	if err := s.credentialVault.Delete(); err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *policyServer) handleCredentialVaultCredentials(w http.ResponseWriter) {
	state, err := s.credentialVault.Sanitized()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, credentialvault.ListResponse{Revision: state.Revision, Credentials: state.Credentials})
}

func (s *policyServer) handleCredentialVaultCredential(w http.ResponseWriter, name string) {
	state, err := s.credentialVault.Sanitized()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	name = strings.TrimSpace(name)
	for _, credential := range state.Credentials {
		if credential.Name == name {
			writeJSON(w, http.StatusOK, credential)
			return
		}
	}
	http.Error(w, "credential not found", http.StatusNotFound)
}

func (s *policyServer) handleCredentialVaultBindings(w http.ResponseWriter) {
	state, err := s.credentialVault.Sanitized()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, credentialvault.BindingListResponse{Revision: state.Revision, Bindings: state.Bindings})
}

func (s *policyServer) handleCredentialVaultBinding(w http.ResponseWriter, name string) {
	state, err := s.credentialVault.Sanitized()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	name = strings.TrimSpace(name)
	for _, binding := range state.Bindings {
		if binding.Name == name {
			writeJSON(w, http.StatusOK, binding)
			return
		}
	}
	http.Error(w, "binding not found", http.StatusNotFound)
}

func (s *policyServer) handleCredentialVaultActive(w http.ResponseWriter) {
	snapshot, err := s.credentialVault.ActiveSnapshot()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *policyServer) handleGet(w http.ResponseWriter) {
	current := s.proxy.CurrentPolicy()
	mode := modeFromPolicy(current)
	writeJSON(w, http.StatusOK, policyStatusResponse{
		Status:          "ok",
		Mode:            mode,
		EnforcementMode: s.enforcementMode,
		Policy:          current,
	})
}

func (s *policyServer) handlePost(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := readPolicyRequestBody(r)
	if err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("failed to read body: %v", err))
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}

	if raw == "" {
		log.Infof("policy API: reset to default deny-all")
		def := policy.DefaultDenyPolicy()
		if err := s.validateCredentialVaultPolicyUpdate(def); err != nil {
			logEgressUpdateFailedWarn(fmt.Sprintf("credential vault policy validation: %v", err))
			http.Error(w, fmt.Sprintf("credential vault policy validation: %v", err), http.StatusBadRequest)
			return
		}
		if !s.commitPolicy(r.Context(), w, def, "reset") {
			return
		}
		logEgressUpdated(def.DefaultAction, nil)
		log.Infof("policy API: proxy and nftables updated to deny_all")
		writeJSON(w, http.StatusOK, policyStatusResponse{
			Status: "ok",
			Mode:   "deny_all",
			Reason: "policy reset to default deny-all",
		})
		return
	}

	pol, err := policy.ParsePolicy(raw)
	if err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("invalid policy: %v", err))
		http.Error(w, fmt.Sprintf("invalid policy: %v", err), http.StatusBadRequest)
		return
	}
	if !s.enforceEgressRuleLimit(w, len(pol.Egress)) {
		return
	}

	mode := modeFromPolicy(pol)
	log.Infof("policy API: updating policy to mode=%s, enforcement=%s", mode, s.enforcementMode)
	if err := s.validateCredentialVaultPolicyUpdate(pol); err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("credential vault policy validation: %v", err))
		http.Error(w, fmt.Sprintf("credential vault policy validation: %v", err), http.StatusBadRequest)
		return
	}
	if !s.commitPolicy(r.Context(), w, pol, "post") {
		return
	}
	logEgressUpdated(pol.DefaultAction, pol.Egress)
	log.Infof("policy API: proxy and nftables updated successfully")
	writeJSON(w, http.StatusOK, policyStatusResponse{
		Status:          "ok",
		Mode:            mode,
		EnforcementMode: s.enforcementMode,
	})
}

func (s *policyServer) handlePatch(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := readPolicyRequestBody(r)
	if err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("failed to read body: %v", err))
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}
	if raw == "" {
		logEgressUpdateFailedWarn("empty patch body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	var patchRules []policy.EgressRule
	if err := json.Unmarshal([]byte(raw), &patchRules); err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("invalid patch rules: %v", err))
		http.Error(w, fmt.Sprintf("invalid patch rules: %v", err), http.StatusBadRequest)
		return
	}
	if len(patchRules) == 0 {
		logEgressUpdateFailedWarn("empty patch rules array")
		http.Error(w, "invalid patch rules: empty array", http.StatusBadRequest)
		return
	}

	newPolicy, err := patchMergedPolicy(s.proxy.CurrentPolicy(), patchRules)
	if err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("invalid merged policy: %v", err))
		http.Error(w, fmt.Sprintf("invalid merged policy: %v", err), http.StatusBadRequest)
		return
	}
	if !s.enforceEgressRuleLimit(w, len(newPolicy.Egress)) {
		return
	}

	mode := modeFromPolicy(newPolicy)
	log.Infof("policy API: patching policy with %d new rule(s), mode=%s, enforcement=%s", len(patchRules), mode, s.enforcementMode)
	if err := s.validateCredentialVaultPolicyUpdate(newPolicy); err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("credential vault policy validation: %v", err))
		http.Error(w, fmt.Sprintf("credential vault policy validation: %v", err), http.StatusBadRequest)
		return
	}
	if !s.commitPolicy(r.Context(), w, newPolicy, "patch") {
		return
	}
	logEgressUpdated(newPolicy.DefaultAction, patchRules)
	log.Infof("policy API: patch applied successfully")
	writeJSON(w, http.StatusOK, policyStatusResponse{
		Status:          "ok",
		Mode:            mode,
		EnforcementMode: s.enforcementMode,
	})
}

func (s *policyServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := readPolicyRequestBody(r)
	if err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("failed to read body: %v", err))
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}
	if raw == "" {
		logEgressUpdateFailedWarn("empty delete body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	var targets []string
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("invalid delete targets: %v", err))
		http.Error(w, fmt.Sprintf("invalid delete targets: %v", err), http.StatusBadRequest)
		return
	}
	if len(targets) == 0 {
		logEgressUpdateFailedWarn("empty delete targets array")
		http.Error(w, "invalid delete targets: empty array", http.StatusBadRequest)
		return
	}

	base := s.proxy.CurrentPolicy()
	if base == nil {
		base = policy.DefaultDenyPolicy()
	}
	oldCount := len(base.Egress)
	newEgress, removedRules := removeRulesByTarget(base.Egress, targets)
	removed := oldCount - len(newEgress)

	if removed == 0 {
		mode := modeFromPolicy(base)
		writeJSON(w, http.StatusOK, policyStatusResponse{
			Status:          "ok",
			Mode:            mode,
			EnforcementMode: s.enforcementMode,
			Reason:          "no matching targets found",
		})
		return
	}

	rawMerged, err := json.Marshal(policy.NetworkPolicy{
		DefaultAction: base.DefaultAction,
		Egress:        newEgress,
	})
	if err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("failed to marshal updated policy: %v", err))
		http.Error(w, fmt.Sprintf("internal error: %v", err), http.StatusInternalServerError)
		return
	}
	newPolicy, err := policy.ParsePolicy(string(rawMerged))
	if err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("invalid policy after delete: %v", err))
		http.Error(w, fmt.Sprintf("internal error: %v", err), http.StatusInternalServerError)
		return
	}

	mode := modeFromPolicy(newPolicy)
	log.Infof("policy API: deleting %d egress rule(s) by target, removed=%d, mode=%s, enforcement=%s", len(targets), removed, mode, s.enforcementMode)
	if err := s.validateCredentialVaultPolicyUpdate(newPolicy); err != nil {
		logEgressUpdateFailedWarn(fmt.Sprintf("credential vault policy validation: %v", err))
		http.Error(w, fmt.Sprintf("credential vault policy validation: %v", err), http.StatusBadRequest)
		return
	}
	if !s.commitPolicy(r.Context(), w, newPolicy, "delete") {
		return
	}
	logEgressUpdated(newPolicy.DefaultAction, removedRules)
	log.Infof("policy API: delete applied successfully")
	writeJSON(w, http.StatusOK, policyStatusResponse{
		Status:          "ok",
		Mode:            mode,
		EnforcementMode: s.enforcementMode,
	})
}

// commitPolicy applies one logical change: optional disk persist → merge always file rules → nft
// static (with nameserver allow-IPs) → then update in-memory user policy (POST/PATCH/GET view).
func (s *policyServer) commitPolicy(ctx context.Context, w http.ResponseWriter, pol *policy.NetworkPolicy, op string) bool {
	if err := s.persistPolicy(pol); err != nil {
		logEgressUpdateFailedError(fmt.Sprintf("persist policy: %v", err))
		log.Errorf("policy API: persist policy failed: %v", err)
		http.Error(w, fmt.Sprintf("failed to persist policy: %v", err), http.StatusInternalServerError)
		return false
	}
	alwaysDeny, alwaysAllow := s.currentAlwaysRules()
	merged := policy.MergeAlwaysOverlay(pol, alwaysDeny, alwaysAllow)
	if s.nft != nil {
		nftCtx, nftCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer nftCancel()
		if err := s.nft.ApplyStatic(nftCtx, merged.WithExtraAllowIPs(s.nameserverIPs)); err != nil {
			logEgressUpdateFailedError(fmt.Sprintf("nftables apply (%s): %v", op, err))
			log.Errorf("policy API: nftables apply failed (%s): %v", op, err)
			http.Error(w, fmt.Sprintf("failed to apply nftables policy: %v", err), http.StatusInternalServerError)
			return false
		}
	}
	s.proxy.UpdatePolicy(pol)
	return true
}

func (s *policyServer) startAlwaysRuleReloadJob() {
	safego.Go(func() {
		wait.Until(s.reloadAlwaysRulesJob, time.Minute, s.stopAlwaysReload)
	})
}

func (s *policyServer) reloadAlwaysRulesJob() {
	changed, reloadErr := s.reloadAlwaysRules()
	if reloadErr != nil {
		log.Warnf("policy API: periodic reload of always rules failed: %v", reloadErr)
		return
	}
	if !changed {
		return
	}
	current := s.proxy.CurrentPolicy()
	alwaysDeny, alwaysAllow := s.currentAlwaysRules()
	merged := policy.MergeAlwaysOverlay(current, alwaysDeny, alwaysAllow)
	if s.nft != nil {
		if applyErr := s.nft.ApplyStatic(context.Background(), merged.WithExtraAllowIPs(s.nameserverIPs)); applyErr != nil {
			log.Warnf("policy API: apply reloaded always rules to nftables failed: %v", applyErr)
			return
		}
	}
	fp := fingerprintRules(alwaysDeny, alwaysAllow)
	if s.lastAlwaysFPSet && fp == s.lastAlwaysFP {
		return
	}
	s.lastAlwaysFP = fp
	s.lastAlwaysFPSet = true
	log.Infof("policy API: reloaded always rules applied (deny=%d allow=%d fp=%016x)", len(alwaysDeny), len(alwaysAllow), fp)
}

func fingerprintRules(deny, allow []policy.EgressRule) uint64 {
	h := fnv.New64a()
	writeSet := func(rs []policy.EgressRule) {
		keys := make([]string, len(rs))
		for i, r := range rs {
			keys[i] = r.Action + "|" + r.Target
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = h.Write([]byte(k))
			_, _ = h.Write([]byte{0})
		}
	}
	writeSet(deny)
	_, _ = h.Write([]byte{0xff})
	writeSet(allow)
	return h.Sum64()
}

func (s *policyServer) reloadAlwaysRules() (bool, error) {
	if s.alwaysLoader == nil {
		return false, nil
	}
	deny, allow, changed, err := s.alwaysLoader.RefreshIfDue(time.Now())
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	s.proxy.UpdateAlwaysRules(deny, allow)
	return true, nil
}

func (s *policyServer) setAlwaysRules(deny, allow []policy.EgressRule) {
	if s.alwaysLoader == nil {
		s.alwaysLoader = policy.NewAlwaysRuleLoader(time.Minute)
	}
	s.alwaysLoader.SetCurrentRules(deny, allow)
}

func (s *policyServer) currentAlwaysRules() (deny, allow []policy.EgressRule) {
	if s.alwaysLoader == nil {
		return nil, nil
	}
	return s.alwaysLoader.CurrentRules()
}

func (s *policyServer) effectivePolicy() *policy.NetworkPolicy {
	current := s.proxy.CurrentPolicy()
	if current == nil {
		current = policy.DefaultDenyPolicy()
	}
	alwaysDeny, alwaysAllow := s.currentAlwaysRules()
	return policy.MergeAlwaysOverlay(current, alwaysDeny, alwaysAllow)
}

func (s *policyServer) validateCredentialVaultPolicyUpdate(pol *policy.NetworkPolicy) error {
	if s.credentialVault == nil {
		return nil
	}
	alwaysDeny, alwaysAllow := s.currentAlwaysRules()
	return s.credentialVault.ValidateActiveAgainstPolicy(policy.MergeAlwaysOverlay(pol, alwaysDeny, alwaysAllow))
}

func (s *policyServer) authorize(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	provided := r.Header.Get(constants.EgressAuthTokenHeader)
	if provided == "" {
		return false
	}
	if len(provided) != len(s.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

func credentialVaultWriteTransportAllowed(r *http.Request) bool {
	if r.TLS != nil || isLoopbackRequest(r) {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return false
	}
	remoteIP := requestRemoteIP(r)
	if !remoteIP.IsValid() {
		return false
	}
	for _, raw := range strings.Split(os.Getenv(constants.EnvCredentialVaultTrustedProxyCIDRs), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err == nil && prefix.Contains(remoteIP) {
			return true
		}
		addr, err := netip.ParseAddr(raw)
		if err == nil && addr == remoteIP {
			return true
		}
	}
	return false
}

func isLoopbackRequest(r *http.Request) bool {
	ip := requestRemoteIP(r)
	return ip.IsValid() && ip.IsLoopback()
}

func requestRemoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

func (s *policyServer) enforceEgressRuleLimit(w http.ResponseWriter, egressCount int) bool {
	if s.maxEgressRules <= 0 {
		return true
	}
	if egressCount > s.maxEgressRules {
		logEgressUpdateFailedWarn(fmt.Sprintf("egress rule total count %d exceeds limit %d", egressCount, s.maxEgressRules))
		http.Error(w, fmt.Sprintf("egress rule total count %d exceeds limit %d", egressCount, s.maxEgressRules), http.StatusRequestEntityTooLarge)
		return false
	}
	return true
}

func (s *policyServer) persistPolicy(p *policy.NetworkPolicy) error {
	if s.policyFile == "" {
		return nil
	}
	return policy.SavePolicyFile(s.policyFile, p)
}
