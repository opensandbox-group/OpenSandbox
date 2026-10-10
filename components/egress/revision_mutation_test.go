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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

type mutationTestSession struct {
	fakeRevisionProcessSession
	update    func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error)
	reconcile func(context.Context, revision.Identity) (bool, error)
	close     func() error
}

func (s *mutationTestSession) Update(ctx context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
	s.updateCalls++
	return s.update(ctx, snapshot, epoch)
}
func (s *mutationTestSession) ReconcileUpdate(ctx context.Context, id revision.Identity) (bool, error) {
	s.reconcileUpdateCalls++
	return s.reconcile(ctx, id)
}
func (s *mutationTestSession) Close() error {
	s.closeCalls++
	if s.close != nil {
		return s.close()
	}
	return nil
}
func mutationIdentity(t *testing.T, snapshot credentialvault.ActiveSnapshot, epoch int64) revision.Identity {
	t.Helper()
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, epoch)
	require.NoError(t, err)
	digest := sha256.Sum256(payload)
	return revision.Identity{ControlGeneration: "control-a", SubjectGeneration: "subject-a", DecisionEpoch: 2, VaultRevision: snapshot.Revision, PolicyEpoch: epoch, Digest: hex.EncodeToString(digest[:])}
}
func mutationFixture(t *testing.T) (*policyServer, *mitmTransparent, *mutationTestSession) {
	t.Helper()
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	gate := mitmproxy.NewHealthGate()
	gate.SetReady(true)
	s := &policyServer{proxy: &stubProxy{}, mitmGate: gate, credentialVault: credentialvault.NewStore(nil, func() bool { return true })}
	session := &mutationTestSession{fakeRevisionProcessSession: fakeRevisionProcessSession{config: validFakeRevisionIPCConfig()}}
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		return mutationIdentity(t, snapshot, epoch), nil
	}
	m := &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: session, currentGen: 7, revisionOwner: &revisionLaunchOwner{stop: func(*mitmproxy.Running) { t.Error("unexpected stop") }}}
	return s, m, session
}
func mutationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}
func prepareEmptyVault(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
	return store.PrepareCreate(credentialvault.CreateRequest{}, pol)
}

func TestRevisionVaultMutationPublishesOnlyAfterExactConfirmation(t *testing.T) {
	s, m, session := mutationFixture(t)
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		_, err := s.credentialVault.Sanitized()
		require.ErrorIs(t, err, credentialvault.ErrNotFound)
		require.Zero(t, epoch)
		return mutationIdentity(t, snapshot, epoch), nil
	}
	state, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
	actual, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	require.Equal(t, state, actual)
	require.Equal(t, 1, session.updateCalls)
	require.Zero(t, session.closeCalls)
	require.False(t, s.mitmGate.MitmPending())
}
func TestRevisionVaultMutationRequiresDeadlineBeforePreparation(t *testing.T) {
	s, m, session := mutationFixture(t)
	called := false
	_, err := s.mutateRevisionVault(context.Background(), m, func(*credentialvault.Store, *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		called = true
		return nil, nil
	})
	require.ErrorIs(t, err, revision.ErrInvalid)
	require.False(t, called)
	require.Zero(t, session.updateCalls)
}
func TestRevisionVaultMutationReconcilesExactAttempt(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "aborted"}[active], func(t *testing.T) {
			s, m, session := mutationFixture(t)
			var attempt revision.Identity
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				attempt = mutationIdentity(t, snapshot, epoch)
				return attempt, revision.ErrIndeterminate
			}
			session.reconcile = func(_ context.Context, id revision.Identity) (bool, error) {
				require.Equal(t, attempt, id)
				_, err := s.credentialVault.Sanitized()
				require.ErrorIs(t, err, credentialvault.ErrNotFound)
				return active, nil
			}
			state, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
			if active {
				require.NoError(t, err)
				require.Equal(t, int64(1), state.Revision)
			} else {
				require.ErrorIs(t, err, revision.ErrPrepareRejected)
				_, err = s.credentialVault.Sanitized()
				require.ErrorIs(t, err, credentialvault.ErrNotFound)
				// An exact-previous reconcile is provably uncommitted: no latch.
				require.True(t, s.revisionRecovery == nil || s.revisionRecovery.reason == revisionRecoveryNone)
			}
			require.Equal(t, 1, session.updateCalls)
			require.Equal(t, 1, session.reconcileUpdateCalls)
			require.Zero(t, session.closeCalls)
		})
	}
}
func TestRevisionVaultMutationTerminalFailureDisposesExactChildBeforeDiscard(t *testing.T) {
	s, m, session := mutationFixture(t)
	running := m.running
	var candidate *credentialvault.MutationCandidate
	events := []string{}
	session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
		return revision.Identity{}, revision.ErrTransportUnavailable
	}
	m.revisionOwner.stop = func(got *mitmproxy.Running) {
		require.Same(t, running, got)
		require.Nil(t, m.running)
		require.Nil(t, m.revisionSession)
		require.True(t, s.mitmGate.MitmPending())
		_, err := candidate.Sanitized()
		require.NoError(t, err)
		events = append(events, "stop")
	}
	session.close = func() error { events = append(events, "close"); return errors.New("private-session-detail") }
	_, err := s.mutateRevisionVault(mutationContext(t), m, func(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		var err error
		candidate, err = prepareEmptyVault(store, pol)
		return candidate, err
	})
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	require.NotContains(t, err.Error(), "private-session-detail")
	require.Equal(t, []string{"stop", "close"}, events)
	require.Equal(t, uint64(7), m.currentGen)
	_, err = candidate.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrCandidateClosed)
	require.True(t, s.mitmGate.MitmPending())
	require.Equal(t, 1, session.closeCalls)
	m.shutdown(time.Millisecond)
	require.Equal(t, 1, session.closeCalls)
}

func TestRevisionVaultMutationNonemptyCreatePatchDeleteRecreate(t *testing.T) {
	s, m, session := mutationFixture(t)
	request := credentialvault.CreateRequest{Credentials: []credentialvault.Credential{{Name: "token", Source: []byte(`{"type":"inline","value":"private-original"}`)}}, Bindings: []credentialvault.Binding{{Name: "api", Match: credentialvault.Match{Hosts: []string{"api.example.com"}, Methods: []string{"GET"}, Paths: []string{"/v1/*"}}, Auth: credentialvault.Auth{Type: "apiKey", Name: "X-API-Key", Credential: "token"}}}}
	pol, err := policy.ParsePolicy(`{"defaultAction":"allow"}`)
	require.NoError(t, err)
	s.proxy.(*stubProxy).updated = pol
	create := func(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		return store.PrepareCreate(request, pol)
	}
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		if snapshot.Revision == 0 {
			require.Empty(t, snapshot.Bindings)
		} else {
			require.Len(t, snapshot.Bindings, 1)
			require.NotEmpty(t, snapshot.Bindings[0].Headers[0].Value)
		}
		return mutationIdentity(t, snapshot, epoch), nil
	}
	state, err := s.mutateRevisionVault(mutationContext(t), m, create)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
	_, err = s.mutateRevisionVault(mutationContext(t), m, func(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		return store.PreparePatch(credentialvault.MutationRequest{Credentials: &credentialvault.CredentialMutationSet{Replace: []credentialvault.Credential{{Name: "token", Source: []byte(`{"type":"inline","value":"private-rotated"}`)}}}}, pol)
	})
	require.NoError(t, err)
	snapshot, err := s.credentialVault.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int64(2), snapshot.Revision)
	require.Equal(t, "private-rotated", snapshot.Bindings[0].Headers[0].Value)
	_, err = s.mutateRevisionVault(mutationContext(t), m, func(store *credentialvault.Store, _ *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		return store.PrepareDelete()
	})
	require.NoError(t, err)
	_, err = s.credentialVault.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrNotFound)
	state, err = s.mutateRevisionVault(mutationContext(t), m, create)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
	require.Equal(t, 4, session.updateCalls)
}

func TestRevisionVaultMutationRejectsUnconfirmedIdentity(t *testing.T) {
	for _, field := range []string{"control", "subject", "epoch", "vault", "policy", "digest", "zero-uncertain"} {
		t.Run(field, func(t *testing.T) {
			s, m, session := mutationFixture(t)
			stopped := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stopped++ }
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				id := mutationIdentity(t, snapshot, epoch)
				switch field {
				case "control":
					id.ControlGeneration = "other"
				case "subject":
					id.SubjectGeneration = "other"
				case "epoch":
					id.DecisionEpoch = 0
				case "vault":
					id.VaultRevision++
				case "policy":
					id.PolicyEpoch++
				case "digest":
					id.Digest = "private-invalid-digest"
				case "zero-uncertain":
					return revision.Identity{}, revision.ErrIndeterminate
				}
				return id, nil
			}
			_, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
			require.ErrorIs(t, err, revision.ErrTransportUnavailable)
			require.NotContains(t, err.Error(), "private")
			require.Equal(t, 1, stopped)
			require.Equal(t, 1, session.closeCalls)
			require.Zero(t, session.reconcileUpdateCalls)
			_, err = s.credentialVault.Sanitized()
			require.ErrorIs(t, err, credentialvault.ErrNotFound)
		})
	}
}

func TestRevisionVaultMutationReconcileRetriesWithoutReplayingUpdate(t *testing.T) {
	s, m, session := mutationFixture(t)
	var attempt revision.Identity
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		attempt = mutationIdentity(t, snapshot, epoch)
		return attempt, revision.ErrIndeterminate
	}
	session.reconcile = func(_ context.Context, id revision.Identity) (bool, error) {
		require.Equal(t, attempt, id)
		if session.reconcileUpdateCalls < 3 {
			return false, revision.ErrIndeterminate
		}
		return true, nil
	}
	_, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
	require.NoError(t, err)
	require.Equal(t, 1, session.updateCalls)
	require.Equal(t, 3, session.reconcileUpdateCalls)
}

func TestRevisionVaultMutationDeadlineDisposesUncertainGeneration(t *testing.T) {
	s, m, session := mutationFixture(t)
	stopped := 0
	m.revisionOwner.stop = func(*mitmproxy.Running) { stopped++ }
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		return mutationIdentity(t, snapshot, epoch), revision.ErrIndeterminate
	}
	session.reconcile = func(ctx context.Context, _ revision.Identity) (bool, error) { <-ctx.Done(); return false, ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := s.mutateRevisionVault(ctx, m, prepareEmptyVault)
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	require.Equal(t, 1, stopped)
	require.Equal(t, 1, session.closeCalls)
	require.True(t, s.mitmGate.MitmPending())
	require.Equal(t, 1, session.updateCalls)
	_, err = s.credentialVault.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrNotFound)
}

func TestRevisionVaultMutationStaleLocalFinalizationRecoversAfterACK(t *testing.T) {
	for _, aba := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent-create", true: "delete-recreate-ABA"}[aba], func(t *testing.T) {
			s, m, session := mutationFixture(t)
			stopped := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stopped++ }
			var prepare = prepareEmptyVault
			if aba {
				_, err := s.credentialVault.Create(credentialvault.CreateRequest{}, s.effectivePolicy())
				require.NoError(t, err)
				prepare = func(store *credentialvault.Store, _ *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
					return store.PrepareDelete()
				}
			}
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				if aba {
					require.NoError(t, s.credentialVault.Delete())
				}
				_, err := s.credentialVault.Create(credentialvault.CreateRequest{}, s.effectivePolicy())
				require.NoError(t, err)
				return mutationIdentity(t, snapshot, epoch), nil
			}
			_, err := s.mutateRevisionVault(mutationContext(t), m, prepare)
			require.ErrorIs(t, err, revision.ErrTransportUnavailable)
			require.Equal(t, 1, stopped)
			require.Equal(t, 1, session.closeCalls)
			require.True(t, s.mitmGate.MitmPending())
			state, err := s.credentialVault.Sanitized()
			require.NoError(t, err)
			require.Equal(t, int64(1), state.Revision)
		})
	}
}

func TestRevisionVaultMutationKnownPrepareRejectionPreservesGeneration(t *testing.T) {
	s, m, session := mutationFixture(t)
	running := m.running
	session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
		return revision.Identity{}, revision.ErrPrepareRejected
	}
	_, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
	require.ErrorIs(t, err, revision.ErrPrepareRejected)
	require.Same(t, running, m.running)
	require.Same(t, session, m.revisionSession)
	require.Zero(t, session.closeCalls)
	require.False(t, s.mitmGate.MitmPending())
	// A known prepare rejection is provably uncommitted: no recovery latch.
	require.True(t, s.revisionRecovery == nil || s.revisionRecovery.reason == revisionRecoveryNone)
}

func TestRevisionVaultMutationCancellationBeforeSendPreservesGeneration(t *testing.T) {
	s, m, session := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.mutateRevisionVault(ctx, m, func(store *credentialvault.Store, pol *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		candidate, err := prepareEmptyVault(store, pol)
		cancel()
		return candidate, err
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, session.updateCalls)
	require.Zero(t, session.closeCalls)
	require.NotNil(t, m.running)
	// Cancellation before any IPC send is provably uncommitted: no latch.
	require.True(t, s.revisionRecovery == nil || s.revisionRecovery.reason == revisionRecoveryNone)
}

func TestRevisionVaultMutationCleanupHoldsPolicyAndLifecycleBarriers(t *testing.T) {
	for _, contender := range []string{"shutdown", "session-close", "bootstrap", "vault-write", "policy-write", "always-reload"} {
		t.Run(contender, func(t *testing.T) {
			s, m, session := mutationFixture(t)
			s.alwaysLoader = &stagedTestAlwaysLoader{}
			session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
				return revision.Identity{}, revision.ErrClosed
			}
			stopping := make(chan struct{})
			finishStop := make(chan struct{})
			finished := make(chan error, 1)
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++; close(stopping); <-finishStop }
			go func() { _, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault); finished <- err }()
			select {
			case <-stopping:
			case <-time.After(time.Second):
				t.Fatal("cleanup did not start")
			}
			// Both exact resource detachment and the not-ready fence precede disposal.
			require.False(t, s.mu.TryLock())
			require.False(t, m.mu.TryLock())
			require.True(t, s.mitmGate.MitmPending())
			entered := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				close(entered)
				switch contender {
				case "shutdown":
					m.shutdown(time.Millisecond)
				case "session-close":
					m.closeRevisionSession(7)
				case "bootstrap":
					_, _, _ = s.revisionBootstrapSnapshot(context.Background())
				case "vault-write":
					_, _ = s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
				case "policy-write":
					s.handlePost(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader("invalid")))
				case "always-reload":
					_, _ = s.reloadAlwaysRules()
				}
			}()
			<-entered
			select {
			case <-done:
				t.Fatal("competing operation passed terminal cleanup barrier")
			case <-time.After(20 * time.Millisecond):
			}
			close(finishStop)
			select {
			case err := <-finished:
				require.ErrorIs(t, err, revision.ErrTransportUnavailable)
			case <-time.After(time.Second):
				t.Fatal("mutation did not return")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("competing operation did not resume")
			}
			require.Equal(t, 1, stops)
			require.Equal(t, 1, session.closeCalls)
			require.Nil(t, m.running)
			require.Nil(t, m.revisionSession)
		})
	}
}

func TestRevisionVaultMutationConfirmedStateWinsCancellation(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "reconcile"}[reconcile], func(t *testing.T) {
			s, m, session := mutationFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				id := mutationIdentity(t, snapshot, epoch)
				if reconcile {
					return id, revision.ErrIndeterminate
				}
				cancel()
				return id, nil
			}
			session.reconcile = func(context.Context, revision.Identity) (bool, error) { cancel(); return true, nil }
			state, err := s.mutateRevisionVault(ctx, m, prepareEmptyVault)
			require.NoError(t, err)
			require.Equal(t, int64(1), state.Revision)
			require.Zero(t, session.closeCalls)
		})
	}
}

func TestRevisionVaultMutationInvalidInputDoesNotInvokePreparation(t *testing.T) {
	for _, kind := range []string{"nil-context", "cancelled", "nil-gate", "nil-store", "nil-proxy", "nil-process", "stopping", "not-ready", "missing-stop", "missing-session"} {
		t.Run(kind, func(t *testing.T) {
			s, m, session := mutationFixture(t)
			ctx := mutationContext(t)
			switch kind {
			case "nil-context":
				ctx = nil
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "nil-gate":
				s.mitmGate = nil
			case "nil-store":
				s.credentialVault = nil
			case "nil-proxy":
				s.proxy = nil
			case "nil-process":
				m = nil
			case "stopping":
				m.stopping = true
			case "not-ready":
				s.mitmGate.SetReady(false)
			case "missing-stop":
				m.revisionOwner.stop = nil
			case "missing-session":
				m.revisionSession = nil
			}
			prepared := false
			_, err := s.mutateRevisionVault(ctx, m, func(*credentialvault.Store, *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
				prepared = true
				return nil, nil
			})
			require.Error(t, err)
			require.False(t, prepared)
			require.Zero(t, session.updateCalls)
			require.Zero(t, session.closeCalls)
		})
	}
}

func TestRevisionVaultMutationSanitizesCandidateAndSessionFailures(t *testing.T) {
	for _, phase := range []string{"prepare", "update", "reconcile"} {
		t.Run(phase, func(t *testing.T) {
			s, m, session := mutationFixture(t)
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			prepare := prepareEmptyVault
			if phase == "prepare" {
				prepare = func(*credentialvault.Store, *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
					return nil, errors.New("private-candidate-secret")
				}
			}
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				if phase == "update" {
					return revision.Identity{}, errors.New("private-session-secret")
				}
				return mutationIdentity(t, snapshot, epoch), revision.ErrIndeterminate
			}
			session.reconcile = func(context.Context, revision.Identity) (bool, error) {
				return false, errors.Join(revision.ErrClosed, errors.New("private-reconcile-secret"))
			}
			_, err := s.mutateRevisionVault(mutationContext(t), m, prepare)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private")
			require.NotContains(t, err.Error(), "secret")
			if phase == "prepare" {
				require.Zero(t, stops)
				require.Zero(t, session.updateCalls)
			} else {
				require.Equal(t, 1, stops)
				require.True(t, s.mitmGate.MitmPending())
			}
		})
	}
}

func TestRevisionRecoveryMutationBlocked(t *testing.T) {
	s, m, session := mutationFixture(t)
	s.mu.Lock()
	require.NoError(t, s.initRevisionRecoveryLocked(effectivePolicyInputs{user: policy.DefaultDenyPolicy()}))
	s.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
	s.mu.Unlock()
	prepared := false
	_, err := s.mutateRevisionVault(mutationContext(t), m, func(*credentialvault.Store, *policy.NetworkPolicy) (*credentialvault.MutationCandidate, error) {
		prepared = true
		return nil, nil
	})
	require.ErrorIs(t, err, errRevisionRecoveryRequired)
	require.False(t, prepared)
	require.Zero(t, session.updateCalls)
	require.Zero(t, session.closeCalls)
	require.NotNil(t, m.running)
	require.True(t, s.mitmGate.MitmPending())
}

func TestRevisionRecoveryCleanupFailure(t *testing.T) {
	for _, kind := range []string{"clean", "cleanup-failed", "legacy-cleanup-failed"} {
		t.Run(kind, func(t *testing.T) {
			s, m, session := mutationFixture(t)
			if kind != "legacy-cleanup-failed" {
				s.mu.Lock()
				err := s.initRevisionRecoveryLocked(effectivePolicyInputs{user: policy.DefaultDenyPolicy()})
				s.mu.Unlock()
				require.NoError(t, err)
			}
			running := m.running
			events := []string{}
			m.revisionOwner.stop = func(got *mitmproxy.Running) {
				require.Same(t, running, got)
				require.Nil(t, m.running)
				require.Nil(t, m.revisionSession)
				events = append(events, "stop")
			}
			session.close = func() error {
				events = append(events, "close")
				if kind != "clean" {
					return errors.New("private-cleanup-secret")
				}
				return nil
			}
			session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
				return revision.Identity{}, revision.ErrTransportUnavailable
			}
			_, err := s.mutateRevisionVault(mutationContext(t), m, prepareEmptyVault)
			require.ErrorIs(t, err, revision.ErrTransportUnavailable)
			require.NotContains(t, err.Error(), "private-cleanup-secret")
			require.Equal(t, []string{"stop", "close"}, events)
			require.True(t, s.mitmGate.MitmPending())
			// Every terminal detach latches the unknown-effects recovery first,
			// even without a prior recovery owner; a later cleanup failure can
			// never replace that first classification.
			require.NotNil(t, s.revisionRecovery)
			require.Equal(t, revisionRecoveryExternalEffectsUnknown, s.revisionRecovery.reason)
			_, epoch, ticket, captureErr := s.captureRevisionBootstrap(context.Background())
			require.Zero(t, epoch)
			require.ErrorIs(t, captureErr, errRevisionRecoveryRequired)
			require.Nil(t, ticket)
			require.NotContains(t, captureErr.Error(), "private-cleanup-secret")
		})
	}
}
