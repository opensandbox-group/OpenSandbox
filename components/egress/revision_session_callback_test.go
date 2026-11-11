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
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

var _ revisionMutationSession = revisionProcessSession((*fakeRevisionProcessSession)(nil))

func TestWithRevisionMutationSessionRejectsUnavailableOrInvalidInputs(t *testing.T) {
	callbackCalled := false
	callback := func(context.Context, revisionMutationSession) error {
		callbackCalled = true
		return nil
	}

	for _, tc := range []struct {
		name string
		m    *mitmTransparent
		ctx  context.Context
		cb   func(context.Context, revisionMutationSession) error
		want error
	}{
		{name: "nil receiver", ctx: context.Background(), want: revision.ErrInvalid},
		{name: "nil context", m: &mitmTransparent{}, cb: callback, want: revision.ErrInvalid},
		{name: "missing running process", m: &mitmTransparent{revisionSession: &fakeRevisionProcessSession{}}, ctx: context.Background(), cb: callback, want: revision.ErrTransportUnavailable},
		{name: "missing session", m: &mitmTransparent{running: &mitmproxy.Running{}}, ctx: context.Background(), cb: callback, want: revision.ErrTransportUnavailable},
		{name: "stopping", m: &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: &fakeRevisionProcessSession{}, stopping: true}, ctx: context.Background(), cb: callback, want: revision.ErrClosed},
		{name: "nil callback", m: &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: &fakeRevisionProcessSession{}}, ctx: context.Background(), want: revision.ErrInvalid},
		{name: "cancelled before entry", m: &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: &fakeRevisionProcessSession{}}, ctx: canceledContext(t), cb: callback, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callbackCalled = false
			err := tc.m.withRevisionMutationSession(tc.ctx, tc.cb)
			require.ErrorIs(t, err, tc.want)
			require.False(t, callbackCalled)
		})
	}
}

func TestWithRevisionMutationSessionPinsExactSessionForUpdateAndReconcile(t *testing.T) {
	session := &fakeRevisionProcessSession{
		updateIdentity:        revision.Identity{DecisionEpoch: 12},
		updateErr:             revision.ErrIndeterminate,
		reconcileUpdateResult: true,
	}
	m := &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: session}
	snapshot := credentialvault.ActiveSnapshot{Revision: 23}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotIdentity revision.Identity
	var gotReconciled bool

	err := m.withRevisionMutationSession(ctx, func(gotContext context.Context, got revisionMutationSession) error {
		require.Same(t, ctx, gotContext)
		require.Same(t, session, got)
		var err error
		gotIdentity, err = got.Update(gotContext, snapshot, 7)
		if !errors.Is(err, revision.ErrIndeterminate) {
			return err
		}
		gotReconciled, err = got.ReconcileUpdate(gotContext, gotIdentity)
		return err
	})

	require.NoError(t, err)
	require.Equal(t, revision.Identity{DecisionEpoch: 12}, gotIdentity)
	require.True(t, gotReconciled)
	require.Equal(t, 1, session.updateCalls)
	require.Same(t, ctx, session.updateContext)
	require.Equal(t, snapshot, session.updateSnapshot)
	require.Equal(t, int64(7), session.updatePolicyEpoch)
	require.Equal(t, 1, session.reconcileUpdateCalls)
	require.Same(t, ctx, session.reconcileUpdateContext)
	require.Equal(t, gotIdentity, session.reconcileUpdateIdentity)
}

func TestWithRevisionMutationSessionHoldsReadLockUntilCallbackReturns(t *testing.T) {
	for _, writer := range []string{"close revision session", "claim for shutdown"} {
		t.Run(writer, func(t *testing.T) {
			session := &fakeRevisionProcessSession{}
			running := &mitmproxy.Running{}
			m := &mitmTransparent{running: running, revisionSession: session, currentGen: 9}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callbackEntered := make(chan struct{})
			releaseCallback := make(chan struct{})
			callbackDone := make(chan error, 1)
			callbackReceivedExactSession := false
			go func() {
				callbackDone <- m.withRevisionMutationSession(ctx, func(_ context.Context, got revisionMutationSession) error {
					callbackReceivedExactSession = got == session
					close(callbackEntered)
					<-releaseCallback
					return nil
				})
			}()
			<-callbackEntered
			require.False(t, m.mu.TryLock(), "callback must exclude lifecycle writers")

			close(releaseCallback)
			require.NoError(t, <-callbackDone)
			require.True(t, callbackReceivedExactSession)
			if writer == "close revision session" {
				m.closeRevisionSession(9)
				require.Equal(t, 1, session.closeCalls)
			} else {
				claimedRunning, claimedSession := m.claimForShutdown()
				require.Same(t, running, claimedRunning)
				require.Same(t, session, claimedSession)
			}
		})
	}
}

func TestWithRevisionMutationSessionAllowsCurrentGenerationRead(t *testing.T) {
	m := &mitmTransparent{
		running:         &mitmproxy.Running{},
		revisionSession: &fakeRevisionProcessSession{},
		currentGen:      17,
	}
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- m.withRevisionMutationSession(
			context.Background(),
			func(context.Context, revisionMutationSession) error {
				close(callbackEntered)
				<-releaseCallback
				return nil
			},
		)
	}()
	released := false
	defer func() {
		if !released {
			close(releaseCallback)
		}
	}()
	entryCtx, entryCancel := context.WithTimeout(context.Background(), time.Second)
	defer entryCancel()
	select {
	case <-callbackEntered:
	case <-entryCtx.Done():
		t.Fatal("revision mutation callback did not start")
	}

	generation := make(chan uint64, 1)
	go func() { generation <- m.getCurrentGen() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case got := <-generation:
		require.Equal(t, uint64(17), got)
	case <-ctx.Done():
		t.Fatal("current generation read blocked behind a lifecycle reader")
	}

	close(releaseCallback)
	released = true
	require.NoError(t, <-callbackDone)
}

func TestWithRevisionMutationSessionReturnsCallbackErrorUnchanged(t *testing.T) {
	want := errors.New("callback failure")
	m := &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: &fakeRevisionProcessSession{}}
	require.Same(t, want, m.withRevisionMutationSession(context.Background(), func(context.Context, revisionMutationSession) error { return want }))
}

func TestWithRevisionMutationSessionAllowsConcurrentReaders(t *testing.T) {
	session := &fakeRevisionProcessSession{}
	m := &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: session}
	entered := make(chan bool, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- m.withRevisionMutationSession(context.Background(), func(_ context.Context, got revisionMutationSession) error {
				entered <- got == session
				<-release
				return nil
			})
		}()
	}
	require.True(t, <-entered)
	require.True(t, <-entered)
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
}

func TestWithRevisionMutationSessionReleasesReadLockWhenCallbackPanics(t *testing.T) {
	m := &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: &fakeRevisionProcessSession{}}
	func() {
		defer func() { require.Equal(t, "callback panic", recover()) }()
		_ = m.withRevisionMutationSession(context.Background(), func(context.Context, revisionMutationSession) error { panic("callback panic") })
	}()
	require.True(t, m.mu.TryLock(), "callback panic must not leak the read lock")
	m.mu.Unlock()
}

func canceledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestWithRevisionMutationSessionShutdownCancellationReleasesPinnedSession(t *testing.T) {
	session := &fakeRevisionProcessSession{}
	running := &mitmproxy.Running{}
	m := &mitmTransparent{running: running, revisionSession: session}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callbackEntered := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- m.withRevisionMutationSession(ctx, func(callbackContext context.Context, _ revisionMutationSession) error {
			close(callbackEntered)
			<-callbackContext.Done()
			return callbackContext.Err()
		})
	}()
	<-callbackEntered
	require.False(t, m.mu.TryLock(), "callback must pin the session against shutdown writers")

	writerReady := make(chan struct{})
	type shutdownClaim struct {
		running *mitmproxy.Running
		session revisionProcessSession
	}
	claimDone := make(chan shutdownClaim, 1)
	go func() {
		close(writerReady)
		claimedRunning, claimedSession := m.claimForShutdown()
		claimDone <- shutdownClaim{running: claimedRunning, session: claimedSession}
	}()
	<-writerReady
	cancel()

	require.ErrorIs(t, <-callbackDone, context.Canceled)
	claim := <-claimDone
	require.Same(t, running, claim.running)
	require.Same(t, session, claim.session)
}
