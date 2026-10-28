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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

// Embed a real Manager so assertions cover its write admission, while observing
// the required owner call separately from automatic static-failure quiescence.
type observedQuiescenceNft struct {
	*nftables.Manager
	quiesceCalls atomic.Int32
	quiesceStart chan struct{}
}

func (n *observedQuiescenceNft) Quiesce() {
	n.quiesceCalls.Add(1)
	if n.quiesceStart != nil {
		n.quiesceStart <- struct{}{}
	}
	n.Manager.Quiesce()
}

func quiescenceIPs() []nftables.ResolvedIP {
	return []nftables.ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.23"), TTL: time.Minute}}
}

func quiescencePendingResult(s *policyServer, ticket *revisionBootstrapTicket) (*mitmTransparent, *revisionLaunchResult) {
	result := &revisionLaunchResult{running: &mitmproxy.Running{}, session: &fakeRevisionProcessSession{}, ticket: ticket, generation: 1}
	m := &mitmTransparent{revisionOwner: &revisionLaunchOwner{server: s}, pending: result, launchGen: result.generation}
	return m, result
}

func assertQuiescenceRejectsWrites(t *testing.T, m *nftables.Manager) {
	t.Helper()
	ctx := context.Background()
	require.ErrorIs(t, m.AddResolvedDomain(ctx, "api.example.com", quiescenceIPs()), nftables.ErrQuiesced)
	require.ErrorIs(t, m.AddResolvedIPs(ctx, quiescenceIPs()), nftables.ErrQuiesced)
	require.ErrorIs(t, m.AddUpstreamProxyIPs(ctx, quiescenceIPs()), nftables.ErrQuiesced)
	require.ErrorIs(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()), nftables.ErrQuiesced)
}

// Removing the owner's Quiesce call must make each source leave live dynamic
// writes enabled, even when the existing recovery and HTTP behavior is correct.
func TestRevisionNftQuiescence_RecoverySources(t *testing.T) {
	for _, source := range []string{"policy-persist", "policy-static", "always-static", "current-session-cleanup", "launch-session-cleanup", "vault-session-cleanup"} {
		t.Run(source, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			var calls atomic.Int32
			var failStatic atomic.Bool
			// Automatic freeze is deliberately disabled to isolate owner wiring.
			m := nftables.NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
				calls.Add(1)
				if failStatic.Load() && strings.Contains(script, "add chain") {
					return nil, errors.New("injected static outcome unknown")
				}
				return nil, nil
			})
			require.NoError(t, m.ApplyStatic(context.Background(), s.proxy.CurrentPolicy()))
			require.NoError(t, m.AddResolvedDomain(context.Background(), "api.example.com", quiescenceIPs()))
			n := &observedQuiescenceNft{Manager: m}
			s.nft = n
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			pending, result := quiescencePendingResult(s, ticket)
			wantReason := revisionRecoveryExternalEffectsUnknown
			switch source {
			case "policy-persist", "policy-static":
				if source == "policy-persist" {
					injectUnknownPolicySave(s)
				} else {
					failStatic.Store(true)
				}
				w := httptest.NewRecorder()
				s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
				require.Equal(t, http.StatusInternalServerError, w.Code)
			case "always-static":
				failStatic.Store(true)
				s.alwaysLoader = &stagedTestAlwaysLoader{candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "new.example.com")}, pending: true}
				changed, err := s.reloadAlwaysRules()
				require.Error(t, err)
				require.False(t, changed)
			case "vault-session-cleanup":
				// The terminal detach cannot prove the remote outcome, so the
				// sticky latch fires before the session close; the cleanup
				// failure is a second recovery source but the first reason is
				// preserved.
				wantReason = revisionRecoveryExternalEffectsUnknown
				session := &mutationTestSession{
					fakeRevisionProcessSession: fakeRevisionProcessSession{config: validFakeRevisionIPCConfig()},
					update: func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
						return revision.Identity{}, revision.ErrTransportUnavailable
					},
					close: func() error { return errors.New("injected cleanup failure") },
				}
				stops := 0
				child := &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: session, currentGen: 7,
					revisionOwner: &revisionLaunchOwner{server: s, stop: func(*mitmproxy.Running) { stops++ }}}
				_, err := s.mutateRevisionVault(mutationContext(t), child, prepareEmptyVault)
				require.ErrorIs(t, err, revision.ErrTransportUnavailable)
				require.Equal(t, 1, stops)
				require.Equal(t, 1, session.closeCalls)
				require.Nil(t, child.running)
				require.Nil(t, child.revisionSession)
			default:
				wantReason = revisionRecoverySessionCleanupFailed
				session := &publicationSession{fakeRevisionProcessSession: &fakeRevisionProcessSession{}, closeErr: errors.New("injected cleanup failure")}
				owner := &revisionLaunchOwner{server: s}
				if source == "current-session-cleanup" {
					child := &mitmTransparent{revisionOwner: owner, running: &mitmproxy.Running{}, revisionSession: session, currentGen: 7}
					child.closeRevisionSession(7)
					require.Nil(t, child.running)
					require.Nil(t, child.revisionSession)
				} else {
					require.ErrorIs(t, owner.closeSession(session), revision.ErrTransportUnavailable)
				}
				require.Equal(t, 1, session.closeCalls)
			}
			require.Equal(t, http.StatusServiceUnavailable, recoveryHealthzProbe(s).Code)
			require.Equal(t, wantReason, s.revisionRecovery.reason)
			require.ErrorIs(t, publishRevisionReady(context.Background(), pending, result), errRevisionRecoveryRequired)
			require.Nil(t, pending.running)
			_, _, fresh, err := s.captureRevisionBootstrap(context.Background())
			require.ErrorIs(t, err, errRevisionRecoveryRequired)
			require.Nil(t, fresh)
			require.Equal(t, int32(1), n.quiesceCalls.Load(), "recovery owner must synchronously call the required method")
			before := calls.Load()
			assertQuiescenceRejectsWrites(t, m)
			require.Equal(t, before, calls.Load(), "recovery must reject actual Manager writers before runner execution")
			w := httptest.NewRecorder()
			s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			require.Equal(t, int32(1), n.quiesceCalls.Load())
		})
	}
}

func TestRevisionNftQuiescence_HealthWhileWriterDrains(t *testing.T) {
	s := recoveryPolicyFixture(t)
	injectUnknownPolicySave(s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	writerStarted, releaseWriter := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWriter) }) }
	defer release()
	var calls atomic.Int32
	m := nftables.NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
		calls.Add(1)
		if strings.Contains(script, "dyn_allow_v4 { 192.0.2.23") {
			close(writerStarted)
			select {
			case <-releaseWriter:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, nil
	})
	require.NoError(t, m.ApplyStatic(ctx, s.proxy.CurrentPolicy()))
	n := &observedQuiescenceNft{Manager: m, quiesceStart: make(chan struct{}, 1)}
	s.nft = n
	writerDone := make(chan error, 1)
	go func() { writerDone <- m.AddResolvedDomain(ctx, "api.example.com", quiescenceIPs()) }()
	select {
	case <-writerStarted:
	case <-ctx.Done():
		t.Fatal("Manager writer did not acquire its lock")
	}
	recoveryDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
		recoveryDone <- w
	}()
	select {
	case <-n.quiesceStart:
	case w := <-recoveryDone:
		t.Fatalf("owner returned HTTP %d without waiting for Manager quiescence", w.Code)
	case <-ctx.Done():
		t.Fatal("recovery did not reach Quiesce")
	}
	probeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { probeDone <- recoveryHealthzProbe(s) }()
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	defer probeCancel()
	select {
	case w := <-probeDone:
		require.Equal(t, http.StatusServiceUnavailable, w.Code, "health restriction must precede the blocking Manager lock")
	case <-probeCtx.Done():
		t.Fatal("health handler waited for the policy or Manager lock")
	}
	select {
	case <-recoveryDone:
		t.Fatal("recovery returned while the writer still held Manager.mu")
	default:
	}
	release()
	select {
	case err := <-writerDone:
		require.NoError(t, err, "a writer already admitted may finish before the freeze point")
	case <-ctx.Done():
		t.Fatal("admitted writer did not finish")
	}
	select {
	case w := <-recoveryDone:
		require.Equal(t, http.StatusInternalServerError, w.Code)
	case <-ctx.Done():
		t.Fatal("recovery did not finish after the writer released its lock")
	}
	require.Equal(t, int32(2), calls.Load())
	assertQuiescenceRejectsWrites(t, m)
	require.Equal(t, int32(2), calls.Load())
}

func TestRevisionNftQuiescence_NonRecoveryControls(t *testing.T) {
	for _, control := range []string{"stale-ticket", "clean-child-crash", "successful-policy", "legacy-static-failure"} {
		t.Run(control, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			var calls atomic.Int32
			var failStatic atomic.Bool
			m := nftables.NewManagerWithRunner(func(_ context.Context, script string) ([]byte, error) {
				calls.Add(1)
				if failStatic.Load() && strings.Contains(script, "add chain") {
					return nil, errors.New("legacy static failure")
				}
				return nil, nil
			})
			require.NoError(t, m.ApplyStatic(context.Background(), s.proxy.CurrentPolicy()))
			n := &observedQuiescenceNft{Manager: m}
			s.nft = n
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			switch control {
			case "stale-ticket":
				s.mu.Lock()
				err = s.replaceRevisionBaseLocked(policyCandidateInputs(t))
				s.mu.Unlock()
				require.NoError(t, err)
				pending, result := quiescencePendingResult(s, ticket)
				require.ErrorIs(t, publishRevisionReady(context.Background(), pending, result), errStaleRevisionBootstrap)
			case "clean-child-crash":
				session := &fakeRevisionProcessSession{}
				child := &mitmTransparent{revisionOwner: &revisionLaunchOwner{server: s}, running: &mitmproxy.Running{}, revisionSession: session, currentGen: 7}
				child.closeRevisionSession(7)
				require.Equal(t, 1, session.closeCalls)
				require.Nil(t, child.running)
			case "legacy-static-failure":
				s.revisionRecovery = nil
				failStatic.Store(true)
				w := httptest.NewRecorder()
				s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
				require.Equal(t, http.StatusInternalServerError, w.Code)
				require.Nil(t, s.revisionRecovery)
				failStatic.Store(false)
				fallthrough
			case "successful-policy":
				w := httptest.NewRecorder()
				s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code)
			}
			require.Zero(t, n.quiesceCalls.Load())
			require.False(t, s.revisionRecoveryRequired.Load())
			if s.revisionRecovery != nil {
				_, _, fresh, err := s.captureRevisionBootstrap(context.Background())
				require.NoError(t, err)
				require.NotNil(t, fresh)
			}
			before := calls.Load()
			require.NoError(t, m.AddResolvedDomain(context.Background(), "api.example.com", quiescenceIPs()))
			require.Equal(t, before+1, calls.Load(), "normal DNS writer remains admitted")
		})
	}
	t.Run("dns-only", func(t *testing.T) {
		s := recoveryPolicyFixture(t)
		s.nft = nil
		s.enforcementMode = "dns"
		w := httptest.NewRecorder()
		s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
		require.Equal(t, http.StatusOK, w.Code)
		s.policyFile = filepath.Join(t.TempDir(), "missing", "policy.json")
		w = httptest.NewRecorder()
		s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"deny"}`)))
		require.Equal(t, http.StatusInternalServerError, w.Code)
		require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code, "known pre-rename failure keeps the experimental DNS owner healthy")
	})
}
