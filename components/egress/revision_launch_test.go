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
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/revisionruntime"
	"github.com/stretchr/testify/require"
)

func TestRevisionLaunchBootstrapsBeforeReturning(t *testing.T) {
	events := []string{}
	ticket := &revisionBootstrapTicket{}
	session := &fakeRevisionProcessSession{
		config: &mitmproxy.RevisionIPCConfig{
			SocketPath:        "/run/opensandbox/revision/session.sock",
			SessionToken:      "abcdefghijklmnopqrstuvwxyz012345",
			ControlGeneration: "control-a",
			SubjectGeneration: "subject-a",
			MaxSnapshotBytes:  4096,
		},
		events: &events,
	}
	owner := revisionLaunchOwner{
		config: revisionruntime.ProcessSessionConfig{},
		newSession: func(revisionruntime.ProcessSessionConfig) (revisionProcessSession, error) {
			events = append(events, "new-session")
			return session, nil
		},
		snapshot: func(context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
			events = append(events, "snapshot")
			return credentialvault.ActiveSnapshot{Revision: 7}, 11, ticket, nil
		},
		stop: func(*mitmproxy.Running) { events = append(events, "stop") },
	}
	base := mitmproxy.Config{ListenPort: 18081}
	result, err := owner.launch(context.Background(), base, func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
		events = append(events, "launch")
		require.Equal(t, session.config, cfg.RevisionIPC)
		return &mitmproxy.Running{}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, result.running)
	require.Same(t, session, result.session)
	require.Same(t, ticket, result.ticket)
	require.Zero(t, result.generation, "only the tagged lifecycle owner assigns generations")
	require.Equal(t, []string{"snapshot", "new-session", "config", "launch", "bootstrap"}, events)
	require.Equal(t, credentialvault.ActiveSnapshot{Revision: 7}, session.snapshots[0])
	require.Equal(t, int64(11), session.policyEpochs[0])
}

func TestRevisionLaunchReconcilesLostCommitAcknowledgement(t *testing.T) {
	identity := &revision.Identity{DecisionEpoch: 1}
	session := &fakeRevisionProcessSession{
		config:        validFakeRevisionIPCConfig(),
		bootstrapErrs: []error{revision.ErrIndeterminate},
		reconcile:     []*revision.Identity{identity},
	}
	owner := fakeRevisionLaunchOwner(session)

	_, err := owner.launch(context.Background(), mitmproxy.Config{ListenPort: 18081}, fakeMitmLaunch)
	require.NoError(t, err)
	require.Equal(t, 1, session.bootstrapCalls)
	require.Equal(t, 1, session.reconcileCalls)
}

func TestRevisionLaunchRetriesAfterConfirmedAbort(t *testing.T) {
	session := &fakeRevisionProcessSession{
		config:        validFakeRevisionIPCConfig(),
		bootstrapErrs: []error{revision.ErrIndeterminate, nil},
		reconcile:     []*revision.Identity{nil},
	}
	owner := fakeRevisionLaunchOwner(session)

	_, err := owner.launch(context.Background(), mitmproxy.Config{ListenPort: 18081}, fakeMitmLaunch)
	require.NoError(t, err)
	require.Equal(t, 2, session.bootstrapCalls)
	require.Equal(t, 1, session.reconcileCalls)
}

func TestRevisionLaunchSanitizesSnapshotFailureBeforeStartingChild(t *testing.T) {
	events := []string{}
	session := &fakeRevisionProcessSession{config: validFakeRevisionIPCConfig(), events: &events}
	owner := fakeRevisionLaunchOwner(session)
	owner.snapshot = func(context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
		events = append(events, "snapshot")
		return credentialvault.ActiveSnapshot{}, 0, nil, errors.New("source detail must not escape")
	}
	owner.stop = func(*mitmproxy.Running) { events = append(events, "stop") }

	_, err := owner.launch(context.Background(), mitmproxy.Config{ListenPort: 18081}, func(mitmproxy.Config) (*mitmproxy.Running, error) {
		events = append(events, "launch")
		return &mitmproxy.Running{}, nil
	})
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	require.NotContains(t, err.Error(), "source detail")
	require.Equal(t, []string{"snapshot"}, events)
	require.Zero(t, session.closeCalls)
}

func TestRevisionLaunchStopsChildBeforeClosingFailedBootstrap(t *testing.T) {
	events := []string{}
	session := &fakeRevisionProcessSession{
		config:        validFakeRevisionIPCConfig(),
		bootstrapErrs: []error{revision.ErrInvalid},
		events:        &events,
	}
	owner := fakeRevisionLaunchOwner(session)
	owner.snapshot = func(context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
		events = append(events, "snapshot")
		return credentialvault.ActiveSnapshot{}, 0, nil, nil
	}
	owner.newSession = func(revisionruntime.ProcessSessionConfig) (revisionProcessSession, error) {
		events = append(events, "new-session")
		return session, nil
	}
	owner.stop = func(*mitmproxy.Running) { events = append(events, "stop") }

	_, err := owner.launch(context.Background(), mitmproxy.Config{ListenPort: 18081}, func(mitmproxy.Config) (*mitmproxy.Running, error) {
		events = append(events, "launch")
		return &mitmproxy.Running{}, nil
	})
	require.ErrorIs(t, err, revision.ErrInvalid)
	require.Equal(t, []string{"snapshot", "new-session", "config", "launch", "bootstrap", "stop", "close"}, events)
}

func TestRevisionLaunchCancellationWhileSnapshotBlockedNeverStartsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	newSessionCalls := 0
	launchCalls := 0
	owner := revisionLaunchOwner{
		newSession: func(revisionruntime.ProcessSessionConfig) (revisionProcessSession, error) {
			newSessionCalls++
			return &fakeRevisionProcessSession{config: validFakeRevisionIPCConfig()}, nil
		},
		snapshot: func(ctx context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
			close(started)
			<-ctx.Done()
			return credentialvault.ActiveSnapshot{}, 0, nil, ctx.Err()
		},
		stop: func(*mitmproxy.Running) {},
	}
	result := make(chan error, 1)
	go func() {
		_, err := owner.launch(ctx, mitmproxy.Config{ListenPort: 18081}, func(mitmproxy.Config) (*mitmproxy.Running, error) {
			launchCalls++
			return &mitmproxy.Running{}, nil
		})
		result <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	require.Zero(t, newSessionCalls)
	require.Zero(t, launchCalls)
}

func TestSidecarRevisionLaunchOwnerDefaultsOffBeforeRuntimeLookup(t *testing.T) {
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "")
	owner, err := newSidecarRevisionLaunchOwner(nil)
	require.NoError(t, err)
	require.Nil(t, owner)
}

func TestExperimentalRevisionRuntimeRejectsVaultWritesWithoutMitm(t *testing.T) {
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	server := &policyServer{
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			request := httptest.NewRequest(method, "/credential-vault", nil)
			response := httptest.NewRecorder()
			server.handleCredentialVault(response, request)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Contains(t, response.Body.String(), "mutation unavailable")
		})
	}
	_, err := server.credentialVault.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrNotFound)
}

func TestMitmTransparentShutdownFencesLaterProcessPublication(t *testing.T) {
	currentSession := &fakeRevisionProcessSession{}
	currentRunning := &mitmproxy.Running{}
	m := &mitmTransparent{
		running:         currentRunning,
		revisionSession: currentSession,
		currentGen:      3,
	}

	running, session := m.claimForShutdown()
	require.Same(t, currentRunning, running)
	require.Same(t, currentSession, session)
	require.True(t, m.stopping)
	require.Nil(t, m.running)
	require.Nil(t, m.revisionSession)

	newSession := &fakeRevisionProcessSession{}
	require.False(t, m.publishRunning(&mitmproxy.Running{}, newSession, 4))
	require.Nil(t, m.running)
	require.Nil(t, m.revisionSession)
}

func TestMitmTransparentClosesOnlyMatchingRevisionSession(t *testing.T) {
	current := &fakeRevisionProcessSession{}
	m := &mitmTransparent{currentGen: 3, revisionSession: current}

	m.closeRevisionSession(2)
	require.Equal(t, 0, current.closeCalls)
	m.closeRevisionSession(3)
	require.Equal(t, 1, current.closeCalls)
	require.Nil(t, m.revisionSession)
}

func TestPolicyServerRevisionBootstrapSnapshotDistinguishesMissingAndEmpty(t *testing.T) {
	server := &policyServer{
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}

	snapshot, policyEpoch, err := server.revisionBootstrapSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, credentialvault.ActiveSnapshot{}, snapshot)
	require.Zero(t, policyEpoch)

	_, err = server.credentialVault.Create(credentialvault.CreateRequest{}, policy.DefaultDenyPolicy())
	require.NoError(t, err)
	snapshot, policyEpoch, err = server.revisionBootstrapSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), snapshot.Revision)
	require.Empty(t, snapshot.Bindings)
	require.Zero(t, policyEpoch)
}

type fakeRevisionProcessSession struct {
	config                  *mitmproxy.RevisionIPCConfig
	bootstrapErrs           []error
	reconcile               []*revision.Identity
	events                  *[]string
	snapshots               []credentialvault.ActiveSnapshot
	policyEpochs            []int64
	bootstrapCalls          int
	reconcileCalls          int
	closeCalls              int
	updateIdentity          revision.Identity
	updateErr               error
	updateContext           context.Context
	updateSnapshot          credentialvault.ActiveSnapshot
	updatePolicyEpoch       int64
	updateCalls             int
	reconcileUpdateResult   bool
	reconcileUpdateErr      error
	reconcileUpdateContext  context.Context
	reconcileUpdateIdentity revision.Identity
	reconcileUpdateCalls    int
}

func (s *fakeRevisionProcessSession) MitmproxyConfig() (*mitmproxy.RevisionIPCConfig, error) {
	if s.events != nil {
		*s.events = append(*s.events, "config")
	}
	copy := *s.config
	return &copy, nil
}

func (s *fakeRevisionProcessSession) Bootstrap(
	_ context.Context,
	snapshot credentialvault.ActiveSnapshot,
	policyEpoch int64,
) (revision.Identity, error) {
	if s.events != nil {
		*s.events = append(*s.events, "bootstrap")
	}
	s.snapshots = append(s.snapshots, snapshot)
	s.policyEpochs = append(s.policyEpochs, policyEpoch)
	index := s.bootstrapCalls
	s.bootstrapCalls++
	if index < len(s.bootstrapErrs) && s.bootstrapErrs[index] != nil {
		return revision.Identity{}, s.bootstrapErrs[index]
	}
	return revision.Identity{DecisionEpoch: int64(s.bootstrapCalls)}, nil
}

func (s *fakeRevisionProcessSession) ReconcileBootstrap(context.Context) (*revision.Identity, error) {
	index := s.reconcileCalls
	s.reconcileCalls++
	if index >= len(s.reconcile) {
		return nil, revision.ErrIndeterminate
	}
	return s.reconcile[index], nil
}

func (s *fakeRevisionProcessSession) Update(
	ctx context.Context,
	snapshot credentialvault.ActiveSnapshot,
	policyEpoch int64,
) (revision.Identity, error) {
	s.updateCalls++
	s.updateContext = ctx
	s.updateSnapshot = snapshot
	s.updatePolicyEpoch = policyEpoch
	return s.updateIdentity, s.updateErr
}

func (s *fakeRevisionProcessSession) ReconcileUpdate(
	ctx context.Context,
	identity revision.Identity,
) (bool, error) {
	s.reconcileUpdateCalls++
	s.reconcileUpdateContext = ctx
	s.reconcileUpdateIdentity = identity
	return s.reconcileUpdateResult, s.reconcileUpdateErr
}

func (s *fakeRevisionProcessSession) Close() error {
	s.closeCalls++
	if s.events != nil {
		*s.events = append(*s.events, "close")
	}
	return nil
}

func validFakeRevisionIPCConfig() *mitmproxy.RevisionIPCConfig {
	return &mitmproxy.RevisionIPCConfig{
		SocketPath:        "/run/opensandbox/revision/session.sock",
		SessionToken:      "abcdefghijklmnopqrstuvwxyz012345",
		ControlGeneration: "control-a",
		SubjectGeneration: "subject-a",
		MaxSnapshotBytes:  4096,
	}
}

func fakeRevisionLaunchOwner(session revisionProcessSession) revisionLaunchOwner {
	return revisionLaunchOwner{
		newSession: func(revisionruntime.ProcessSessionConfig) (revisionProcessSession, error) {
			return session, nil
		},
		snapshot: func(context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
			return credentialvault.ActiveSnapshot{}, 0, nil, nil
		},
		stop: func(*mitmproxy.Running) {},
	}
}

func fakeMitmLaunch(mitmproxy.Config) (*mitmproxy.Running, error) {
	return &mitmproxy.Running{}, nil
}
