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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/stretchr/testify/require"
)

func TestRevisionRecoveryHealthzWithoutTransparent(t *testing.T) {
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvMitmproxyTransparent, "false")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	gate := mitmproxy.NewHealthGate()
	nft := &stubNft{err: errors.New("uncertain effect")}
	srv, s, err := startPolicyServer(&stubProxy{updated: policyCandidateInputs(t).user}, nft, "", "127.0.0.1:0", "", nil, "", nil, nil, gate)
	require.NoError(t, err)
	s.mu.Lock()
	attachPolicyTestSession(t, s)
	s.mu.Unlock()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, w.Code)
	w = httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	s.mu.Lock()
	reason := s.revisionRecovery.reason
	s.mu.Unlock()
	require.Equal(t, revisionRecoveryExternalEffectsUnknown, reason)
	w = httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "latched experimental recovery must keep /healthz not-ready even when transparent is disabled")
}

// Exercise the production health handler without transparent-mode startup's
// unrelated credential socket/user requirements. The startup regression above
// separately verifies that the real server mux routes /healthz to this handler.
func recoveryHealthzFixture(t *testing.T, experimental, transparent, withGate bool) *policyServer {
	t.Helper()
	t.Setenv(constants.EnvExperimentalRevisionRuntime, fmt.Sprint(experimental))
	t.Setenv(constants.EnvMitmproxyTransparent, fmt.Sprint(transparent))
	t.Setenv("OTEL_SDK_DISABLED", "true")
	inputs := policyCandidateInputs(t)
	s := &policyServer{proxy: &stubProxy{updated: inputs.user}, alwaysLoader: &stagedTestAlwaysLoader{}}
	if withGate {
		s.mitmGate = mitmproxy.NewHealthGate()
	}
	s.credentialVault = credentialvault.NewStore(s.mitmGate, nil)
	if experimental {
		s.mu.Lock()
		err := s.initRevisionRecoveryLocked(inputs)
		s.mu.Unlock()
		require.NoError(t, err)
		if withGate {
			attachPolicyTestSession(t, s)
		}
	}
	return s
}

func recoveryHealthzProbe(s *policyServer) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleHealthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return w
}

func TestRevisionRecoveryHealthzReadinessMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		experimental, transparent, withGate bool
	}{
		{"revision-only", true, false, true},
		{"revision-transparent", true, true, true},
		{"legacy", false, false, true},
		{"legacy-transparent", false, true, true},
		{"revision-nil-gate", true, false, false},
		{"legacy-nil-gate", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := recoveryHealthzFixture(t, tc.experimental, tc.transparent, tc.withGate)
			initial := recoveryHealthzProbe(s)
			if tc.transparent {
				require.Equal(t, http.StatusServiceUnavailable, initial.Code)
				require.Equal(t, "mitmproxy not ready\n", initial.Body.String())
			} else {
				require.Equal(t, http.StatusOK, initial.Code)
				require.Equal(t, "ok", initial.Body.String())
			}
			s.mitmGate.SetReady(true)
			require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code)

			if tc.experimental && !tc.withGate {
				unavailable := httptest.NewRecorder()
				s.handlePolicy(unavailable, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
				require.Equal(t, http.StatusServiceUnavailable, unavailable.Code)
				require.False(t, s.revisionRecoveryRequired.Load(), "missing runtime has no new external effects")
				s.mu.Lock()
				s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
				s.mu.Unlock()
				require.Equal(t, http.StatusServiceUnavailable, recoveryHealthzProbe(s).Code)
				return
			}
			invalid := httptest.NewRecorder()
			s.handlePolicy(invalid, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader("invalid")))
			require.Equal(t, http.StatusBadRequest, invalid.Code)
			require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code, "pure parse failures must not latch recovery")

			s.nft = &stubNft{err: errors.New("private-healthz-effect-detail")}
			failed := httptest.NewRecorder()
			s.handlePolicy(failed, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
			require.Equal(t, http.StatusInternalServerError, failed.Code)
			if !tc.experimental {
				require.Nil(t, s.revisionRecovery)
				require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code, "legacy effect errors retain existing health behavior")
				return
			}
			latched := recoveryHealthzProbe(s)
			require.Equal(t, http.StatusServiceUnavailable, latched.Code)
			require.Equal(t, "revision recovery required\n", latched.Body.String(), "recovery health must use a fixed, sensitive-safe response before the optional MITM gate")
			s.mu.Lock()
			firstReason := s.revisionRecovery.reason
			firstIdentity := s.revisionRecovery.identity
			s.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
			retainedReason := s.revisionRecovery.reason
			lastIdentity := s.revisionRecovery.identity
			s.mu.Unlock()
			require.Equal(t, revisionRecoveryExternalEffectsUnknown, firstReason)
			require.Equal(t, firstReason, retainedReason, "repeated require preserves the first classification")
			require.NotSame(t, firstIdentity, lastIdentity, "repeated require still invalidates tickets")
			s.mitmGate.SetReady(true)
			repeated := recoveryHealthzProbe(s)
			require.Equal(t, http.StatusServiceUnavailable, repeated.Code, "SetReady(true) cannot clear recovery")
			require.Equal(t, latched.Body.String(), repeated.Body.String())
		})
	}
}

func TestRevisionRecoveryHealthzNonblocking(t *testing.T) {
	s := recoveryHealthzFixture(t, true, false, false)
	s.mu.Lock()
	s.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
	// Keep the policy/effect barrier held while probing, as during slow effects.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- recoveryHealthzProbe(s) }()
	select {
	case response := <-done:
		s.mu.Unlock()
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, "revision recovery required\n", response.Body.String())
	case <-time.After(time.Second):
		s.mu.Unlock()
		<-done
		t.Fatal("health probe blocked behind the policy/effect barrier")
	}
}

func TestRevisionRecoveryHealthzConcurrent(t *testing.T) {
	s := recoveryHealthzFixture(t, true, true, true)
	s.mitmGate.SetReady(true)
	require.Equal(t, http.StatusOK, recoveryHealthzProbe(s).Code)
	latched := make(chan struct{})
	failures := make(chan string, 4)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-latched
			for range 64 {
				response := recoveryHealthzProbe(s)
				if response.Code != http.StatusServiceUnavailable || response.Body.String() != "revision recovery required\n" {
					failures <- fmt.Sprintf("health after recovery = %d %q", response.Code, response.Body.String())
					return
				}
			}
		}()
	}
	s.mu.Lock()
	s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
	s.mu.Unlock()
	close(latched)
	for range 64 {
		s.mu.Lock()
		s.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
		s.mu.Unlock()
		s.mitmGate.SetReady(true)
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	s.mu.Lock()
	retained := s.revisionRecovery.reason
	s.mu.Unlock()
	require.Equal(t, revisionRecoveryExternalEffectsUnknown, retained)
}
