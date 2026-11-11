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

package revisionruntime

import (
	"context"
	"os"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

func TestProcessSessionUpdateRequiresCompletedBootstrap(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })

	_, err = session.Update(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)

	server := serveBootstrapReceiver(t, session)
	_, err = session.Update(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	prepares, commits := server.commandCounts()
	require.Zero(t, prepares)
	require.Zero(t, commits)
}

func TestProcessSessionUpdateInstallsExactSnapshotAfterBootstrap(t *testing.T) {
	session, server := newBootstrapSession(t)
	first, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)

	snapshot := credentialvault.ActiveSnapshot{Revision: 9}
	identity, err := session.Update(context.Background(), snapshot, 17)
	require.NoError(t, err)
	require.Equal(t, first.DecisionEpoch+1, identity.DecisionEpoch)
	require.Equal(t, int64(9), identity.VaultRevision)
	require.Equal(t, int64(17), identity.PolicyEpoch)
	require.Equal(t, &identity, server.activeIdentity())
	require.JSONEq(t, `{
		"version": 1,
		"vaultRevision": 9,
		"effectivePolicyEpoch": 17,
		"interceptionMode": "credential-bound",
		"state": "active-empty",
		"tlsBindingHostSelectors": [],
		"fullRenderedBindings": [],
		"redactions": []
	}`, string(server.payload()))
}

func TestProcessSessionUpdateRejectsReplacedParentBeforeIPC(t *testing.T) {
	parent := processSessionParent(t)
	session, server := newBootstrapSessionWithParent(t, parent)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	prepares, commits := server.commandCounts()
	readbacks := server.readbackCount()

	oldPath := parent + ".old"
	require.NoError(t, os.Rename(parent, oldPath))
	require.NoError(t, os.Mkdir(parent, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
	_, err = session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 1}, 1)
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	newPrepares, newCommits := server.commandCounts()
	require.Equal(t, prepares, newPrepares)
	require.Equal(t, commits, newCommits)
	require.Equal(t, readbacks, server.readbackCount())
	require.NoError(t, session.Close())
	require.NoError(t, os.RemoveAll(oldPath))
}

func TestProcessSessionReconcileUpdateConfirmsLostCommitAcknowledgement(t *testing.T) {
	session, server := newBootstrapSession(t)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	server.rejectCommitResponse = true

	attempt, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 2}, 3)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	require.NotZero(t, attempt.DecisionEpoch)
	prepares, commits := server.commandCounts()
	readbacks := server.readbackCount()
	forged := attempt
	forged.DecisionEpoch++
	forged.Digest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	activated, err := session.ReconcileUpdate(context.Background(), forged)
	require.False(t, activated)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	afterPrepares, afterCommits := server.commandCounts()
	require.Equal(t, prepares, afterPrepares)
	require.Equal(t, commits, afterCommits)
	require.Equal(t, readbacks, server.readbackCount())
	zero, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 3}, 4)
	require.Equal(t, revision.Identity{}, zero)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	afterPrepares, afterCommits = server.commandCounts()
	require.Equal(t, prepares, afterPrepares)
	require.Equal(t, commits, afterCommits)
	require.Equal(t, readbacks, server.readbackCount())
	server.rejectCommitResponse = false

	activated, err = session.ReconcileUpdate(context.Background(), attempt)
	require.NoError(t, err)
	require.True(t, activated)
	require.Equal(t, &attempt, server.activeIdentity())
	prepares, commits = server.commandCounts()
	readbacks = server.readbackCount()
	activated, err = session.ReconcileUpdate(context.Background(), attempt)
	require.False(t, activated)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	afterPrepares, afterCommits = server.commandCounts()
	require.Equal(t, prepares, afterPrepares)
	require.Equal(t, commits, afterCommits)
	require.Equal(t, readbacks, server.readbackCount())
}

func TestProcessSessionReconcileUpdateConfirmsLostAbortAndKeepsPreviousIdentity(t *testing.T) {
	session, server := newBootstrapSession(t)
	previous, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	server.rejectPrepareResponse = true
	server.rejectAbortResponse = true

	attempt, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 3}, 4)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	require.NotZero(t, attempt.DecisionEpoch)
	server.rejectAbortResponse = false

	activated, err := session.ReconcileUpdate(context.Background(), attempt)
	require.NoError(t, err)
	require.False(t, activated)
	require.Equal(t, &previous, server.activeIdentity())
	coordinator, err := session.Coordinator()
	require.NoError(t, err)
	confirmed, err := coordinator.Confirmed()
	require.NoError(t, err)
	require.Equal(t, &previous, confirmed)
	prepares, commits := server.commandCounts()
	readbacks := server.readbackCount()
	activated, err = session.ReconcileUpdate(context.Background(), attempt)
	require.False(t, activated)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	newPrepares, newCommits := server.commandCounts()
	require.Equal(t, prepares, newPrepares)
	require.Equal(t, commits, newCommits)
	require.Equal(t, readbacks, server.readbackCount())
}

func TestProcessSessionReconcileUpdateKeepsUnknownReceiverStateIndeterminate(t *testing.T) {
	session, server := newBootstrapSession(t)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	server.rejectCommitResponse = true
	attempt, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 4}, 5)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	require.NotZero(t, attempt.DecisionEpoch)

	server.mu.Lock()
	server.active = &revision.Identity{
		ControlGeneration: attempt.ControlGeneration,
		SubjectGeneration: attempt.SubjectGeneration,
		DecisionEpoch:     attempt.DecisionEpoch + 10,
		VaultRevision:     attempt.VaultRevision,
		PolicyEpoch:       attempt.PolicyEpoch,
		Digest:            "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	server.mu.Unlock()
	activated, err := session.ReconcileUpdate(context.Background(), attempt)
	require.False(t, activated)
	require.ErrorIs(t, err, revision.ErrIndeterminate)

	server.mu.Lock()
	server.active = &attempt
	server.rejectCommitResponse = false
	server.mu.Unlock()
	activated, err = session.ReconcileUpdate(context.Background(), attempt)
	require.NoError(t, err)
	require.True(t, activated)
}

func TestProcessSessionReconcileUpdateRejectsStaleAttemptAfterLaterActivation(t *testing.T) {
	session, server := newBootstrapSession(t)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	staleAttempt, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 1}, 1)
	require.NoError(t, err)
	latest, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 2}, 2)
	require.NoError(t, err)
	require.Greater(t, latest.DecisionEpoch, staleAttempt.DecisionEpoch)
	prepares, commits := server.commandCounts()
	readbacks := server.readbackCount()

	activated, err := session.ReconcileUpdate(context.Background(), staleAttempt)
	require.False(t, activated)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	newPrepares, newCommits := server.commandCounts()
	require.Equal(t, prepares, newPrepares)
	require.Equal(t, commits, newCommits)
	require.Equal(t, readbacks, server.readbackCount())
}

func TestProcessSessionReconcileUpdateRejectsForeignAndMalformedAttemptsBeforeReadback(t *testing.T) {
	session, server := newBootstrapSession(t)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	readbacks := server.readbackCount()
	config, err := session.MitmproxyConfig()
	require.NoError(t, err)
	foreign := revision.Identity{
		ControlGeneration: config.ControlGeneration,
		SubjectGeneration: "another-subject",
		DecisionEpoch:     1,
		Digest:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	zero := revision.Identity{}
	badDigest := foreign
	badDigest.SubjectGeneration = config.SubjectGeneration
	badDigest.Digest = "not-a-digest"
	for _, attempt := range []revision.Identity{foreign, zero, badDigest} {
		_, err := session.ReconcileUpdate(context.Background(), attempt)
		require.ErrorIs(t, err, revision.ErrInvalid)
	}
	require.Equal(t, readbacks, server.readbackCount())
}

func TestProcessSessionUpdateAndReconcileRespectClosedAndReplacedParentPath(t *testing.T) {
	parent := processSessionParent(t)
	session, server := newBootstrapSessionWithParent(t, parent)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	config, err := session.MitmproxyConfig()
	require.NoError(t, err)
	attempt := revision.Identity{
		ControlGeneration: config.ControlGeneration,
		SubjectGeneration: config.SubjectGeneration,
		DecisionEpoch:     1,
		Digest:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	readbacks := server.readbackCount()

	require.NoError(t, session.Close())
	_, err = session.Update(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrClosed)
	_, err = session.ReconcileUpdate(context.Background(), attempt)
	require.ErrorIs(t, err, revision.ErrClosed)
	require.Equal(t, readbacks, server.readbackCount())

	replacedParent := processSessionParent(t)
	other, otherServer := newBootstrapSessionWithParent(t, replacedParent)
	_, err = other.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	otherServer.rejectCommitResponse = true
	otherAttempt, err := other.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 1}, 1)
	require.ErrorIs(t, err, revision.ErrIndeterminate)
	oldPath := replacedParent + ".old"
	require.NoError(t, os.Rename(replacedParent, oldPath))
	require.NoError(t, os.Mkdir(replacedParent, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(replacedParent)) })
	parentReadbacks := otherServer.readbackCount()
	_, err = other.ReconcileUpdate(context.Background(), otherAttempt)
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	require.Equal(t, parentReadbacks, otherServer.readbackCount())
	require.NoError(t, other.Close())
	require.NoError(t, os.RemoveAll(oldPath))
}

func TestProcessSessionUpdateReturnsBusyWhileBootstrapIsInProgress(t *testing.T) {
	session, server := newBootstrapSession(t)
	prepareSeen := make(chan struct{})
	releasePrepare := make(chan struct{})
	server.prepareSeen = prepareSeen
	server.releasePrepare = releasePrepare
	bootstrapDone := make(chan error, 1)
	go func() {
		_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
		bootstrapDone <- err
	}()
	<-prepareSeen
	_, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.ErrorIs(t, err, revision.ErrBusy)
	close(releasePrepare)
	require.NoError(t, <-bootstrapDone)
	prepares, commits := server.commandCounts()
	require.Equal(t, 1, prepares)
	require.Equal(t, 1, commits)
}

func TestProcessSessionUpdateRacingCloseDoesNotReturnSuccess(t *testing.T) {
	session, server := newBootstrapSession(t)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	commitSeen := make(chan struct{})
	releaseCommit := make(chan struct{})
	server.commitSeen = commitSeen
	server.releaseCommit = releaseCommit
	updateDone := make(chan struct {
		identity revision.Identity
		err      error
	}, 1)
	go func() {
		identity, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 1}, 1)
		updateDone <- struct {
			identity revision.Identity
			err      error
		}{identity, err}
	}()
	<-commitSeen
	require.NoError(t, session.Close())
	close(releaseCommit)
	result := <-updateDone
	require.ErrorIs(t, result.err, revision.ErrClosed)
	require.Equal(t, revision.Identity{}, result.identity)
}

func TestProcessSessionUpdateParentFenceFailureAfterCommitIsTerminal(t *testing.T) {
	parent := processSessionParent(t)
	session, server := newBootstrapSessionWithParent(t, parent)
	_, err := session.Bootstrap(context.Background(), credentialvault.ActiveSnapshot{}, 0)
	require.NoError(t, err)
	basePrepares, baseCommits := server.commandCounts()
	commitSeen := make(chan struct{})
	releaseCommit := make(chan struct{})
	server.commitSeen = commitSeen
	server.releaseCommit = releaseCommit
	updateDone := make(chan struct {
		identity revision.Identity
		err      error
	}, 1)
	go func() {
		identity, err := session.Update(context.Background(), credentialvault.ActiveSnapshot{Revision: 1}, 1)
		updateDone <- struct {
			identity revision.Identity
			err      error
		}{identity, err}
	}()
	<-commitSeen
	oldPath := parent + ".old"
	require.NoError(t, os.Rename(parent, oldPath))
	require.NoError(t, os.Mkdir(parent, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
	close(releaseCommit)
	result := <-updateDone
	require.ErrorIs(t, result.err, revision.ErrTransportUnavailable)
	require.Equal(t, revision.Identity{}, result.identity)
	prepares, commits := server.commandCounts()
	require.Equal(t, basePrepares+1, prepares)
	require.Equal(t, baseCommits+1, commits)
	require.NoError(t, session.Close())
	require.NoError(t, os.RemoveAll(oldPath))
}

func newBootstrapSessionWithParent(t *testing.T, parent string) (*ProcessSession, *bootstrapReceiver) {
	t.Helper()
	session, err := NewProcessSession(processSessionConfig(parent))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	return session, serveBootstrapReceiver(t, session)
}
