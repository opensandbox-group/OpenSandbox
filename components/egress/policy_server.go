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
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/internal/safego"
	"k8s.io/apimachinery/pkg/util/wait"
)

type policyUpdater interface {
	CurrentPolicy() *policy.NetworkPolicy
	UpdatePolicy(*policy.NetworkPolicy)
	UpdateAlwaysRules(alwaysDeny, alwaysAllow []policy.EgressRule)
}

type alwaysRulesLoader interface {
	CurrentRules() (deny, allow []policy.EgressRule)
	SetCurrentRules(deny, allow []policy.EgressRule)
	RefreshIfDueWithApply(time.Time, func(deny, allow []policy.EgressRule) error) (deny, allow []policy.EgressRule, changed bool, err error)
}

// nftApplier: static allow/deny sets plus dynamic DNS-learned entries; teardown on shutdown.
type nftApplier interface {
	ApplyStatic(context.Context, *policy.NetworkPolicy) error
	Quiesce()
	AddResolvedDomain(context.Context, string, []nftables.ResolvedIP) error
	AddUpstreamProxyIPs(context.Context, []nftables.ResolvedIP) error
	StartConnectionRefresh(context.Context)
	StartDomainRefresh(context.Context, func(context.Context, string) ([]nftables.ResolvedIP, error))
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
	quarantineOwners ...*revisionQuarantine,
) (*http.Server, *policyServer, error) {
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
	if len(quarantineOwners) > 0 {
		handler.quarantine = quarantineOwners[0]
	}
	handler.credentialVault = credentialvault.NewStore(mitmGate, func() bool { return strings.TrimSpace(token) != "" })
	handler.credentialVaultRequireTLS = constants.IsTruthy(os.Getenv(constants.EnvCredentialVaultRequireTLS))
	handler.setAlwaysRules(alwaysDeny, alwaysAllow)
	if constants.IsTruthy(os.Getenv(constants.EnvExperimentalRevisionRuntime)) {
		handler.mu.Lock()
		current := proxy.CurrentPolicy()
		if current == nil {
			current = policy.DefaultDenyPolicy()
		}
		// alwaysAllow already includes the telemetry resolved during startup.
		err := handler.initRevisionRecoveryLocked(effectivePolicyInputs{user: current, alwaysDeny: alwaysDeny, alwaysAllow: alwaysAllow})
		handler.mu.Unlock()
		if err != nil {
			return nil, nil, fmt.Errorf("initialize revision recovery: %w", err)
		}
	}

	mux.HandleFunc("/policy", handler.handlePolicy)
	mux.HandleFunc("/credential-vault", handler.handleCredentialVault)
	mux.HandleFunc("/credential-vault/", handler.handleCredentialVaultSubresource)
	mux.HandleFunc("/healthz", handler.handleHealthz)

	var activeSrv *http.Server
	var cleanupActiveSocket func(context.Context) error
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		socketPath := envOrDefault(constants.EnvCredentialProxySocket, constants.DefaultCredentialProxySocket)
		_, mitmGID, _, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
		if err != nil {
			return nil, nil, fmt.Errorf("lookup credential proxy user %q: %w", mitmproxy.RunAsUser, err)
		}
		activeSrv, cleanupActiveSocket, err = credentialvault.StartActiveSocketServerRequestAware(handler.handleCredentialVaultActive, socketPath, int(mitmGID))
		if err != nil {
			return nil, nil, fmt.Errorf("credential vault active socket: %w", err)
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
		return nil, nil, err
	case <-time.After(200 * time.Millisecond):
		handler.startAlwaysRuleReloadJob()
		safego.Go(func() {
			if err := <-errCh; err != nil {
				log.Errorf("policy server error: %v", err)
			}
		})
		return srv, handler, nil
	}
}

type policyServer struct {
	proxy           policyUpdater
	nft             nftApplier
	server          *http.Server
	token           string
	enforcementMode string
	nameserverIPs   []netip.Addr
	policyFile      string     // optional persistence; experimental owner uses atomicPolicyFile
	maxEgressRules  int        // 0 = unlimited; cap len(Egress) for POST/PATCH
	mu              sync.Mutex // serializes /policy updates with effective-policy reads and Vault writes

	alwaysLoader     alwaysRulesLoader
	stopAlwaysReload chan struct{}

	lastAlwaysFP              uint64
	lastAlwaysFPSet           bool
	credentialVault           *credentialvault.Store
	revisionRecovery          *revisionRecoveryState
	quarantine                *revisionQuarantine
	atomicPolicyFile          atomicPolicyFileStore
	mitmGate                  *mitmproxy.HealthGate
	credentialVaultRequireTLS bool
	// mitm is the sidecar's mitmdump lifecycle owner, attached after
	// startMitmproxyTransparentIfEnabled. Guarded by mu; only the
	// experimental revision runtime consults it.
	mitm *mitmTransparent

	// One-way health projection; reason, base and tickets remain owned under mu.
	revisionRecoveryRequired atomic.Bool
}

type policyStatusResponse struct {
	Status          string `json:"status,omitempty"`
	Mode            string `json:"mode,omitempty"`
	EnforcementMode string `json:"enforcementMode,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Policy          any    `json:"policy,omitempty"`
}

func (s *policyServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	// Probes must not wait for the policy/effect barrier, even when MITM is optional.
	if s.revisionRecoveryRequired.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("revision recovery required\n"))
		return
	}
	if s.mitmGate != nil && s.mitmGate.MitmPending() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("mitmproxy not ready\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
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
	if constants.IsTruthy(os.Getenv(constants.EnvExperimentalRevisionRuntime)) &&
		(r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete) {
		s.handleCredentialVaultWriteRevision(w, r)
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
	state, err := func() (credentialvault.State, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.credentialVault.Create(req, s.effectivePolicy())
	}()
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
	state, err := func() (credentialvault.State, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.credentialVault.Patch(req, s.effectivePolicy())
	}()
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
	err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.credentialVault.Delete()
	}()
	if err != nil {
		credentialvault.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revisionMutationTimeout bounds one live Vault mutation transaction, including
// indeterminate-outcome reconciliation. The transaction holds the policy
// barrier for its full duration, so the bound also bounds that stall.
const revisionMutationTimeout = 10 * time.Second

// setRevisionMitm attaches the sidecar's mitmdump lifecycle owner for the
// experimental revision runtime. Call once after the transparent mitmproxy is
// started; nil keeps revision-gated writes unavailable.
func (s *policyServer) setRevisionMitm(m *mitmTransparent) {
	s.mu.Lock()
	s.mitm = m
	s.mu.Unlock()
}

// revisionMitmLocked returns the attached mitmdump lifecycle owner. The caller
// must hold s.mu.
func (s *policyServer) revisionMitmLocked() *mitmTransparent {
	return s.mitm
}

// revisionMitmAttached reports whether a mitmdump lifecycle owner exists.
func (s *policyServer) revisionMitmAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mitm != nil
}

// handleCredentialVaultWriteRevision serves POST/PATCH/DELETE while the
// experimental revision runtime is enabled. Every write installs the rendered
// candidate on the mitmdump receiver and acknowledges only the exact confirmed
// identity before the public Store is finalized; a failed, canceled, or
// indeterminate transaction leaves the prior acknowledged revision active.
func (s *policyServer) handleCredentialVaultWriteRevision(w http.ResponseWriter, r *http.Request) {
	if !s.revisionMitmAttached() {
		// Fail closed before the readiness wait: without a mitmdump lifecycle
		// owner no write can be acknowledged, so it must not be attempted.
		http.Error(w, "credential vault mutation unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.credentialVault.Ready(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	if s.credentialVaultRequireTLS && !credentialVaultWriteTransportAllowed(r) {
		http.Error(w, "credential vault writes require TLS or loopback transport", http.StatusUpgradeRequired)
		return
	}
	var state credentialvault.State
	var err error
	switch r.Method {
	case http.MethodPost:
		var req credentialvault.CreateRequest
		if err := credentialvault.ReadJSON(r, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid credential vault request: %v", err), http.StatusBadRequest)
			return
		}
		state, err = s.mutateRevisionVaultRequest(r, func(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
			return store.PrepareCreate(req, pol)
		})
	case http.MethodPatch:
		var req credentialvault.MutationRequest
		if err := credentialvault.ReadJSON(r, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid credential vault mutation request: %v", err), http.StatusBadRequest)
			return
		}
		if req.ExpectedRevision == nil {
			http.Error(w, "expectedRevision is required while the experimental revision runtime is enabled", http.StatusBadRequest)
			return
		}
		state, err = s.mutateRevisionVaultRequest(r, func(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
			return store.PreparePatch(req, pol)
		})
	default:
		_, err = s.mutateRevisionVaultRequest(r, func(store *credentialvault.Store, _ *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
			return store.PrepareDelete()
		})
	}
	if err != nil {
		writeRevisionMutationError(w, err)
		return
	}
	switch r.Method {
	case http.MethodPost:
		writeJSON(w, http.StatusCreated, state)
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, state)
	}
}

// mutateRevisionVaultRequest runs one mutation transaction with a bounded
// deadline derived from the request context, so client cancellation and sidecar
// shutdown interrupt an unresolved reconciliation.
func (s *policyServer) mutateRevisionVaultRequest(
	r *http.Request,
	prepare func(*credentialvault.Store, *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error),
) (credentialvault.State, error) {
	ctx, cancel := context.WithTimeout(r.Context(), revisionMutationTimeout)
	defer cancel()
	var m *mitmTransparent
	s.mu.Lock()
	m = s.revisionMitmLocked()
	s.mu.Unlock()
	if m == nil {
		return credentialvault.State{}, revision.ErrTransportUnavailable
	}
	return s.mutateRevisionVault(ctx, m, prepare)
}

// writeRevisionMutationError maps transaction outcomes to fixed, secret-free
// responses. Validation, conflict, and not-found failures keep the legacy
// credential vault semantics; every operational or unknown outcome fails
// closed with 503 and preserves the prior acknowledged revision.
func writeRevisionMutationError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, errRevisionRecoveryRequired):
		http.Error(w, "revision recovery required", http.StatusServiceUnavailable)
	case errors.Is(err, revision.ErrTransportUnavailable),
		errors.Is(err, revision.ErrClosed),
		errors.Is(err, revision.ErrIndeterminate),
		errors.Is(err, revision.ErrBusy),
		errors.Is(err, revision.ErrPrepareRejected):
		http.Error(w, "credential vault mutation unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		http.Error(w, "credential vault mutation did not complete", http.StatusServiceUnavailable)
	case errors.Is(err, revision.ErrInvalid):
		http.Error(w, "invalid credential vault mutation", http.StatusBadRequest)
	case errors.Is(err, errRevisionExpectedRevision):
		http.Error(w, errRevisionExpectedRevision.Error(), http.StatusConflict)
	default:
		credentialvault.WriteError(w, err)
	}
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

func (s *policyServer) handleCredentialVaultActive(w http.ResponseWriter, r *http.Request) {
	handleActiveVaultSnapshot(w, r, s.credentialVault)
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
// Experimental persistence uses atomic replacement and stage-aware recovery.
// Best-effort restoration never clears an uncertain external-effect latch.
func (s *policyServer) commitPolicy(ctx context.Context, w http.ResponseWriter, pol *policy.NetworkPolicy, op string) bool {
	alwaysDeny, alwaysAllow := s.currentAlwaysRules()
	stagedBase, err := s.prepareRevisionBaseReplacementLocked(effectivePolicyInputs{user: pol, alwaysDeny: alwaysDeny, alwaysAllow: alwaysAllow})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errRevisionRecoveryRequired) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "revision policy publication unavailable", status)
		return false
	}
	if stagedBase != nil {
		// Use the same frozen inputs for disk, nft and the authoritative base.
		frozen := cloneEffectivePolicyInputs(stagedBase.inputs)
		pol = frozen.user
		alwaysDeny, alwaysAllow = frozen.alwaysDeny, frozen.alwaysAllow
	}
	restore, persistErr := s.persistPolicyChangeLocked(pol)
	if persistErr != nil {
		logEgressUpdateFailedError(fmt.Sprintf("persist policy: %v", persistErr))
		log.Errorf("policy API: persist policy failed: %v", persistErr)
		http.Error(w, fmt.Sprintf("failed to persist policy: %v", persistErr), http.StatusInternalServerError)
		return false
	}
	merged := policy.MergeAlwaysOverlay(pol, alwaysDeny, alwaysAllow)
	if s.nft != nil {
		s.invalidateRevisionBootstrapLocked()
		nftCtx, nftCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer nftCancel()
		if err := s.nft.ApplyStatic(nftCtx, merged.WithExtraAllowIPs(s.nameserverIPs)); err != nil {
			if s.revisionRecovery != nil && nftables.ApplyEffectOf(err) != nftables.ApplyUnchanged {
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			}
			logEgressUpdateFailedError(fmt.Sprintf("nftables apply (%s): %v", op, err))
			log.Errorf("policy API: nftables apply failed (%s): %v", op, err)
			// Known no-execution failure can remain retryable only after the
			// exact old file (or absence) has been durably restored. Unknown
			// kernel effects have already latched recovery before this attempt.
			if restoreErr := restore(); restoreErr != nil {
				if s.revisionRecovery != nil && nftables.ApplyEffectOf(err) == nftables.ApplyUnchanged {
					s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
				}
				log.Errorf("policy API: restore policy file after failed apply: %v", restoreErr)
			}
			http.Error(w, fmt.Sprintf("failed to apply nftables policy: %v", err), http.StatusInternalServerError)
			return false
		}
	}
	s.proxy.UpdatePolicy(pol)
	if stagedBase != nil {
		s.revisionRecovery.current = stagedBase
		s.invalidateRevisionBootstrapLocked()
	}
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
	alwaysDeny, alwaysAllow := s.currentAlwaysRules()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.revisionRecovery.recoveryErrorLocked(); err != nil {
		return false, err
	}
	var stagedBase *effectivePolicyBase
	var stagedAllow []policy.EgressRule
	deny, _, changed, err := s.alwaysLoader.RefreshIfDueWithApply(time.Now(), func(deny, allow []policy.EgressRule) error {
		// The loader holds its own write lock here. All validation and effects
		// use explicit inputs, never CurrentRules/effectivePolicy reentry.
		stagedAllow = withTelemetryAllow(allow)
		if s.nft == nil && s.revisionRecovery == nil {
			return nil
		}
		current := s.proxy.CurrentPolicy()
		if current == nil {
			current = policy.DefaultDenyPolicy()
		}
		var err error
		stagedBase, err = s.prepareRevisionBaseReplacementLocked(effectivePolicyInputs{user: current, alwaysDeny: deny, alwaysAllow: stagedAllow})
		if err != nil {
			return err
		}
		if stagedBase != nil {
			frozen := cloneEffectivePolicyInputs(stagedBase.inputs)
			current = frozen.user
			deny, stagedAllow = frozen.alwaysDeny, frozen.alwaysAllow
		}
		if s.nft == nil {
			return nil
		}
		merged := policy.MergeAlwaysOverlay(current, deny, stagedAllow)
		s.invalidateRevisionBootstrapLocked()
		nftCtx, nftCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer nftCancel()
		if err := s.nft.ApplyStatic(nftCtx, merged.WithExtraAllowIPs(s.nameserverIPs)); err != nil {
			if s.revisionRecovery != nil && nftables.ApplyEffectOf(err) != nftables.ApplyUnchanged {
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			}
			log.Warnf("policy API: apply reloaded always rules to nftables failed: %v", err)
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if stagedBase != nil {
		deny = append([]policy.EgressRule(nil), stagedBase.inputs.alwaysDeny...)
	}
	s.setAlwaysRules(deny, stagedAllow)
	s.proxy.UpdateAlwaysRules(deny, stagedAllow)
	if stagedBase != nil {
		s.revisionRecovery.current = stagedBase
		s.invalidateRevisionBootstrapLocked()
	}
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

// readPolicyFile returns the policy file's current contents so that a change
// which fails to apply can be undone on disk with restorePolicyFile.
func (s *policyServer) readPolicyFile() (data []byte, exists bool, err error) {
	if s.policyFile == "" {
		return nil, false, nil
	}
	data, err = os.ReadFile(s.policyFile)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *policyServer) restorePolicyFile(data []byte, exists bool) error {
	if s.policyFile == "" {
		return nil
	}
	if !exists {
		if err := os.Remove(s.policyFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(s.policyFile, data, 0o600)
}
