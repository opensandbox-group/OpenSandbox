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
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
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
func startPolicyServer(proxy policyUpdater, nft nftApplier, enforcementMode string, addr string, token string, nameserverIPs []netip.Addr, policyFile string, alwaysDeny, alwaysAllow []policy.EgressRule, mitmGate *mitmproxy.HealthGate) (*http.Server, error) {
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
	}
	handler.setAlwaysRules(alwaysDeny, alwaysAllow)

	mux.HandleFunc("/policy", handler.handlePolicy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if mitmGate != nil && mitmGate.MitmPending() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("mitmproxy not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	handler.server = srv
	srv.RegisterOnShutdown(func() {
		select {
		case <-handler.stopAlwaysReload:
		default:
			close(handler.stopAlwaysReload)
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
	default:
		w.Header().Set("Allow", "GET, POST, PUT, PATCH")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
	if err != nil || raw == "" {
		if err != nil {
			logEgressUpdateFailedWarn(fmt.Sprintf("failed to read body: %v", err))
		} else {
			logEgressUpdateFailedWarn("empty patch body")
		}
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
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
	log.Infof("policy API: reloaded always rules applied (deny=%d allow=%d)", len(alwaysDeny), len(alwaysAllow))
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
