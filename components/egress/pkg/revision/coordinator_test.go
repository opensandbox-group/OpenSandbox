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

package revision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type peer struct {
	badCommitAck, badAbortAck                bool
	active                                   *Identity
	prepared                                 Identity
	prepares, commits, aborts                int
	prepareErr, commitErr, abortErr, readErr error
	loseCommitAck, corruptAck                bool
	onPrepare                                func(context.Context)
	onCommit                                 func(context.Context)
	onAbort                                  func(context.Context)
}

func (p *peer) Prepare(ctx context.Context, r Identity, data []byte) (Identity, error) {
	p.prepares++
	p.prepared = r
	if p.onPrepare != nil {
		p.onPrepare(ctx)
	}
	if p.corruptAck {
		r.PolicyEpoch++
	}
	return r, p.prepareErr
}
func (p *peer) Commit(ctx context.Context, r Identity) (Identity, error) {
	p.commits++
	if p.commitErr != nil {
		return Identity{}, p.commitErr
	}
	committed := r
	p.active = &committed
	if p.onCommit != nil {
		p.onCommit(ctx)
	}
	if p.loseCommitAck {
		return Identity{}, errors.New("secret-bearing transport detail")
	}
	if p.badCommitAck {
		r.Digest = "wrong"
	}
	return r, nil
}
func (p *peer) Abort(ctx context.Context, r Identity) (Identity, error) {
	p.aborts++
	if p.onAbort != nil {
		p.onAbort(ctx)
	}
	if p.badAbortAck {
		r.DecisionEpoch++
	}
	return r, p.abortErr
}
func (p *peer) Readback(context.Context) (*Identity, error) { return p.active, p.readErr }

func newTestCoordinator(t *testing.T, p *peer) *Coordinator {
	t.Helper()
	c, err := New("control-a", "subject-a", p, 1024)
	require.NoError(t, err)
	return c
}

func TestApplyAcknowledgementAndVaultRecreation(t *testing.T) {
	p := &peer{}
	c := newTestCoordinator(t, p)
	for i, vault := range []int64{0, 1, 0, 1} {
		r, err := c.Apply(context.Background(), vault, 1, []byte("snapshot"))
		require.NoError(t, err)
		require.Equal(t, int64(i+1), r.DecisionEpoch)
		require.Equal(t, vault, r.VaultRevision)
		confirmed, err := c.Confirmed()
		require.NoError(t, err)
		require.Equal(t, r, *confirmed)
		confirmed.Digest = "changed"
		again, err := c.Confirmed()
		require.NoError(t, err)
		require.Equal(t, r, *again)
	}
	require.Zero(t, p.aborts)
}

func TestLostCommitAckRequiresExactReadback(t *testing.T) {
	p := &peer{loseCommitAck: true}
	c := newTestCoordinator(t, p)
	attempt, err := c.Apply(context.Background(), 1, 1, []byte("secret"))
	require.ErrorIs(t, err, ErrIndeterminate)
	require.Equal(t, p.prepared, attempt)
	require.NotContains(t, err.Error(), "secret")
	_, err = c.Confirmed()
	require.ErrorIs(t, err, ErrIndeterminate)
	_, err = c.Apply(context.Background(), 2, 2, []byte("new"))
	require.ErrorIs(t, err, ErrIndeterminate)
	p.readErr = errors.New("secret")
	_, err = c.Reconcile(context.Background())
	require.ErrorIs(t, err, ErrIndeterminate)
	p.readErr = nil
	r, err := c.Reconcile(context.Background())
	require.NoError(t, err)
	require.Equal(t, attempt, *r)
	require.Equal(t, *p.active, *r)
	require.Equal(t, 1, p.prepares)
	require.Equal(t, 1, p.commits)
	require.Zero(t, p.aborts)
}

func TestFailedPrepareMustBeAbortedBeforeNextEpoch(t *testing.T) {
	p := &peer{}
	c := newTestCoordinator(t, p)
	previous, err := c.Apply(context.Background(), 1, 1, []byte("active"))
	require.NoError(t, err)
	p.prepareErr = errors.New("secret")
	p.abortErr = errors.New("lost abort ack")
	attempt, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.ErrorIs(t, err, ErrIndeterminate)
	require.Equal(t, p.prepared, attempt)
	confirmed, err := c.Confirmed()
	require.NoError(t, err)
	require.Equal(t, previous, *confirmed)
	_, err = c.Apply(context.Background(), 2, 1, []byte("blocked"))
	require.ErrorIs(t, err, ErrIndeterminate)
	p.abortErr = nil
	r, err := c.Reconcile(context.Background())
	require.NoError(t, err)
	require.Equal(t, previous, *r)
	require.NotEqual(t, attempt, *r)
	require.Equal(t, 1, p.commits)
	p.prepareErr = nil
	next, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.NoError(t, err)
	require.Equal(t, int64(3), next.DecisionEpoch)
}

func TestBadPrepareAckIsAbortedNotCommitted(t *testing.T) {
	p := &peer{corruptAck: true}
	c := newTestCoordinator(t, p)
	attempt, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.ErrorIs(t, err, ErrPrepareRejected)
	require.Equal(t, Identity{}, attempt)
	require.Equal(t, 1, p.aborts)
	require.Zero(t, p.commits)
}

func TestUnappliedCommitCanBeRetriedWithoutPayload(t *testing.T) {
	p := &peer{}
	c := newTestCoordinator(t, p)
	_, err := c.Apply(context.Background(), 1, 1, []byte("first"))
	require.NoError(t, err)
	p.commitErr = errors.New("not delivered")
	attempt, err := c.Apply(context.Background(), 2, 1, []byte("next"))
	require.ErrorIs(t, err, ErrIndeterminate)
	require.Equal(t, p.prepared, attempt)
	p.commitErr = nil
	r, err := c.Reconcile(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), r.DecisionEpoch)
	require.Equal(t, 2, p.prepares)
	require.Zero(t, p.aborts)
}

func TestForeignReadbackCannotResolveOrTriggerCommit(t *testing.T) {
	p := &peer{loseCommitAck: true}
	c := newTestCoordinator(t, p)
	_, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.ErrorIs(t, err, ErrIndeterminate)
	p.active.SubjectGeneration = "other"
	_, err = c.Reconcile(context.Background())
	require.ErrorIs(t, err, ErrIndeterminate)
	require.Equal(t, 1, p.commits)
}

func TestCloseDoesNotWaitForTransportAndFencesCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p := &peer{onPrepare: func(ctx context.Context) { close(entered); <-release }}
	c := newTestCoordinator(t, p)
	done := make(chan error, 1)
	go func() { _, err := c.Apply(context.Background(), 1, 1, []byte("data")); done <- err }()
	<-entered
	_, err := c.Apply(context.Background(), 2, 1, []byte("data"))
	require.ErrorIs(t, err, ErrBusy)
	_, err = c.Confirmed()
	require.ErrorIs(t, err, ErrBusy)
	c.Close()
	close(release)
	require.ErrorIs(t, <-done, ErrClosed)
	require.Zero(t, p.commits)
	_, err = c.Reconcile(context.Background())
	require.ErrorIs(t, err, ErrClosed)
}

func TestCloseBeforeAmbiguousAbortFinalizationReturnsNoAttempt(t *testing.T) {
	p := &peer{
		prepareErr: errors.New("prepare outcome uncertain"),
		abortErr:   errors.New("abort acknowledgement lost"),
	}
	c := newTestCoordinator(t, p)
	p.onAbort = func(context.Context) { c.Close() }

	attempt, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.ErrorIs(t, err, ErrClosed)
	require.Equal(t, Identity{}, attempt)
	require.Equal(t, 1, p.aborts)
	require.Zero(t, p.commits)
}

func TestCloseBeforeAmbiguousCommitFinalizationReturnsNoAttempt(t *testing.T) {
	p := &peer{loseCommitAck: true}
	c := newTestCoordinator(t, p)
	p.onCommit = func(context.Context) { c.Close() }

	attempt, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.ErrorIs(t, err, ErrClosed)
	require.Equal(t, Identity{}, attempt)
	require.Equal(t, 1, p.commits)
}

func TestMismatchedAcknowledgementsRemainIndeterminate(t *testing.T) {
	for _, prepareFails := range []bool{false, true} {
		p := &peer{badCommitAck: true, badAbortAck: true}
		if prepareFails {
			p.prepareErr = errors.New("prepare failed")
		}
		c := newTestCoordinator(t, p)
		attempt, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
		require.ErrorIs(t, err, ErrIndeterminate)
		require.Equal(t, p.prepared, attempt)
		if prepareFails {
			_, err = c.Reconcile(context.Background())
			require.ErrorIs(t, err, ErrIndeterminate)
			p.badAbortAck = false
		}
		_, err = c.Reconcile(context.Background())
		require.NoError(t, err)
	}
}

func TestReadbackConflictsAcrossEveryIdentityField(t *testing.T) {
	p := &peer{}
	p.onPrepare = func(context.Context) { p.loseCommitAck = true }
	c := newTestCoordinator(t, p)
	_, err := c.Apply(context.Background(), 1, 1, []byte("payload"))
	require.ErrorIs(t, err, ErrIndeterminate)
	actual := *p.active
	for _, mutate := range []func(*Identity){
		func(r *Identity) { r.ControlGeneration = "foreign" },
		func(r *Identity) { r.SubjectGeneration = "foreign" },
		func(r *Identity) { r.DecisionEpoch++ },
		func(r *Identity) { r.VaultRevision++ },
		func(r *Identity) { r.PolicyEpoch++ },
		func(r *Identity) { r.Digest = "wrong" },
	} {
		copy := actual
		mutate(&copy)
		p.active = &copy
		_, err = c.Reconcile(context.Background())
		require.ErrorIs(t, err, ErrIndeterminate)
	}
	require.Equal(t, 1, p.commits)
	p.active = &actual
	_, err = c.Reconcile(context.Background())
	require.NoError(t, err)
}

func TestInitialUnknownAndCancellation(t *testing.T) {
	p := &peer{}
	c := newTestCoordinator(t, p)
	r, err := c.Confirmed()
	require.NoError(t, err)
	require.Nil(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Apply(ctx, 0, 0, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, p.prepares)
	data := []byte("exact serialized bytes")
	digest := sha256.Sum256(data)
	identity, err := c.Apply(context.Background(), 0, 0, data)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(digest[:]), identity.Digest)
}

func TestInvalidInputsNeverReachTransport(t *testing.T) {
	p := &peer{}
	c := newTestCoordinator(t, p)
	for _, tc := range []struct {
		vault, policy int64
		data          []byte
	}{
		{-1, 1, nil}, {1, -1, nil}, {1, 1, make([]byte, 1025)},
	} {
		_, err := c.Apply(context.Background(), tc.vault, tc.policy, tc.data)
		require.ErrorIs(t, err, ErrInvalid)
	}
	require.Zero(t, p.prepares)
	c.epoch = math.MaxInt64
	_, err := c.Apply(context.Background(), 0, 0, nil)
	require.ErrorIs(t, err, ErrInvalid)
	for _, generation := range []string{"", strings.Repeat("x", 129), string([]byte{0xff})} {
		_, err := New(generation, "subject", p, 1)
		require.ErrorIs(t, err, ErrInvalid)
		_, err = New("control", generation, p, 1)
		require.ErrorIs(t, err, ErrInvalid)
	}
	_, err = New("control", "subject", nil, 1)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = New("control", "subject", p, 0)
	require.ErrorIs(t, err, ErrInvalid)
}
