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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func applyEffectRequest(s *policyServer, method string) *httptest.ResponseRecorder {
	body := `{"defaultAction":"allow"}`
	if method == http.MethodPatch {
		body = `[{"action":"allow","target":"new.example.com"}]`
	}
	if method == http.MethodDelete {
		body = `["api.example.com"]`
	}
	w := httptest.NewRecorder()
	// The public dispatcher also checks auth; these tests exercise the same
	// method handlers under their normal owner lock.
	request := httptest.NewRequest(method, "/policy", strings.NewReader(body))
	switch method {
	case http.MethodPost:
		s.handlePost(w, request)
	case http.MethodPatch:
		s.handlePatch(w, request)
	case http.MethodDelete:
		s.handleDelete(w, request)
	}
	return w
}

func TestRevisionNftEffectsPolicyMutations(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		for _, effect := range []nftables.ApplyEffect{nftables.ApplyUnchanged, nftables.ApplyUnknown} {
			for _, storage := range []string{"none", "restored", "restore-error", "restore-unchanged-error", "restore-unknown-success"} {
				t.Run(fmt.Sprintf("%s-%d-%s", method, effect, storage), func(t *testing.T) {
					s := recoveryPolicyFixture(t)
					cause := errors.New("nft failure")
					n := &stubNft{err: &nftables.ApplyError{Effect: effect, Err: cause}}
					s.nft = n
					f := &faultPolicyStore{state: policy.FileCommitted}
					if storage != "none" {
						s.policyFile = "injected-policy.json"
						s.atomicPolicyFile = f
					}
					switch storage {
					case "restore-error":
						f.restoreErr = errors.New("restore failed")
					case "restore-unchanged-error":
						f.restoreErr = errors.New("restore failed before rename")
						state := policy.FileUnchanged
						f.restoreState = &state
					case "restore-unknown-success":
						state := policy.FileUnknown
						f.restoreState = &state
					}
					oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
					_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
					require.NoError(t, err)
					pending, result := quiescencePendingResult(s, ticket)
					f.onRestore = func() {
						require.Equal(t, effect == nftables.ApplyUnknown, s.revisionRecoveryRequired.Load(), "Unknown must latch before restore; Unchanged only after restore failure")
						require.Equal(t, map[bool]int{true: 1, false: 0}[effect == nftables.ApplyUnknown], n.quiesces)
					}
					w := applyEffectRequest(s, method)
					require.Equal(t, 500, w.Code, w.Body.String())
					recovery := effect == nftables.ApplyUnknown || (storage != "none" && storage != "restored")
					require.Equal(t, recovery, s.revisionRecoveryRequired.Load())
					require.Same(t, oldBase, s.revisionRecovery.current)
					require.Same(t, oldProxy, s.proxy.CurrentPolicy())
					require.Equal(t, 1, n.calls)
					if storage == "none" {
						require.Zero(t, f.restores)
					} else {
						require.Equal(t, 1, f.restores)
						require.Equal(t, 1, f.saves)
					}
					require.Equal(t, map[bool]int{true: 503, false: 200}[recovery], recoveryHealthzProbe(s).Code)
					if recovery {
						require.ErrorIs(t, publishRevisionReady(context.Background(), pending, result), errRevisionRecoveryRequired)
						n.err = nil
						require.Equal(t, 503, applyEffectRequest(s, method).Code)
						require.Equal(t, 1, n.calls)
					} else {
						require.ErrorIs(t, publishRevisionReady(context.Background(), pending, result), errStaleRevisionBootstrap, "failed effect must not undo bootstrap invalidation")
						_, _, fresh, err := s.captureRevisionBootstrap(context.Background())
						require.NoError(t, err)
						require.NoError(t, validateRecoveryTicket(s, fresh))
						n.err = nil
						require.Equal(t, 200, applyEffectRequest(s, method).Code)
						require.Equal(t, 2, n.calls)
						require.Zero(t, n.quiesces)
					}
				})
			}
		}
	}
}

func TestRevisionNftEffectsAlwaysReload(t *testing.T) {
	for _, effect := range []nftables.ApplyEffect{nftables.ApplyUnchanged, nftables.ApplyUnknown} {
		t.Run(fmt.Sprint(effect), func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			loader := &stagedTestAlwaysLoader{candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "new.example.com")}, pending: true}
			s.alwaysLoader = loader
			n := &stubNft{err: &nftables.ApplyError{Effect: effect, Err: errors.New("nft failed")}}
			s.nft = n
			oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			changed, err := s.reloadAlwaysRules()
			require.Error(t, err)
			require.False(t, changed)
			require.True(t, loader.pending)
			require.Empty(t, loader.allow)
			require.Empty(t, s.proxy.(*stubProxy).allow)
			require.Same(t, oldBase, s.revisionRecovery.current)
			require.Same(t, oldProxy, s.proxy.CurrentPolicy())
			if effect == nftables.ApplyUnchanged {
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
				require.False(t, s.revisionRecoveryRequired.Load())
				require.Zero(t, n.quiesces)
				n.err = nil
				changed, err = s.reloadAlwaysRules()
				require.NoError(t, err)
				require.True(t, changed)
				require.NotSame(t, oldBase, s.revisionRecovery.current)
				require.Equal(t, loader.allow, s.proxy.(*stubProxy).allow)
			} else {
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
				n.err = nil
				changed, err = s.reloadAlwaysRules()
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
				require.False(t, changed)
				require.Equal(t, 1, n.calls)
			}
		})
	}
}

func TestRevisionNftEffectsPreserveRecovery(t *testing.T) {
	s := recoveryPolicyFixture(t)
	n := &stubNft{err: &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: errors.New("unused")}}
	s.nft = n
	s.mu.Lock()
	s.requireRevisionRecoveryLocked(revisionRecoverySessionCleanupFailed)
	identity := s.revisionRecovery.identity
	s.mu.Unlock()
	require.Equal(t, 503, applyEffectRequest(s, http.MethodPost).Code)
	require.Equal(t, revisionRecoverySessionCleanupFailed, s.revisionRecovery.reason)
	require.Same(t, identity, s.revisionRecovery.identity)
	require.Zero(t, n.calls)
	require.Equal(t, 1, n.quiesces)
}

func TestLegacyNftUnchangedDoesNotLatch(t *testing.T) {
	s := recoveryPolicyFixture(t)
	s.revisionRecovery = nil
	s.nft = &stubNft{err: &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: errors.New("start failed")}}
	old := s.proxy.CurrentPolicy()
	require.Equal(t, 500, applyEffectRequest(s, http.MethodPost).Code)
	require.Same(t, old, s.proxy.CurrentPolicy())
	require.False(t, s.revisionRecoveryRequired.Load())
	require.Zero(t, s.nft.(*stubNft).quiesces)
}

func TestNftSetupUnchangedStillExits(t *testing.T) {
	if os.Getenv("OPENSANDBOX_SETUP_FAILURE_HELPER") == "1" {
		setupNft(context.Background(), &stubNft{err: &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: errors.New("pre-start failure")}}, policy.DefaultDenyPolicy(), nil, nil, nil, nil)
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNftSetupUnchangedStillExits$")
	cmd.Env = append(os.Environ(), "OPENSANDBOX_SETUP_FAILURE_HELPER=1", "OTEL_SDK_DISABLED=true")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode())
	require.Contains(t, string(out), "nftables static apply failed")
}

func TestRevisionNftEffectBootstrapPublicationRace(t *testing.T) {
	for _, effect := range []nftables.ApplyEffect{nftables.ApplyUnchanged, nftables.ApplyUnknown} {
		t.Run(fmt.Sprint(effect), func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			pending, result := quiescencePendingResult(s, ticket)
			started, done := make(chan struct{}), make(chan error, 1)
			s.nft = &stubNft{err: &nftables.ApplyError{Effect: effect, Err: errors.New("nft failed")}, onApply: func(*policy.NetworkPolicy) {
				go func() { close(started); done <- publishRevisionReady(context.Background(), pending, result) }()
				<-started
				select {
				case err := <-done:
					t.Fatalf("bootstrap passed held owner lock: %v", err)
				default:
				}
			}}
			require.Equal(t, 500, applyEffectRequest(s, http.MethodPost).Code)
			err = <-done
			if effect == nftables.ApplyUnchanged {
				require.ErrorIs(t, err, errStaleRevisionBootstrap)
			} else {
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
			}
			require.Nil(t, pending.running)
			require.NotNil(t, result.running)
		})
	}
}
