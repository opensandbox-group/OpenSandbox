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
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/revisionruntime"
	"github.com/stretchr/testify/require"
)

type publicationSession struct {
	*fakeRevisionProcessSession
	bootstrapHook func()
	closeHook     func()
	configErr     error
	closeErr      error
}

func (s *publicationSession) MitmproxyConfig() (*mitmproxy.RevisionIPCConfig, error) {
	if s.configErr != nil {
		return nil, s.configErr
	}
	return s.fakeRevisionProcessSession.MitmproxyConfig()
}
func (s *publicationSession) Bootstrap(ctx context.Context, snap credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
	if s.bootstrapHook != nil {
		s.bootstrapHook()
	}
	return s.fakeRevisionProcessSession.Bootstrap(ctx, snap, epoch)
}
func (s *publicationSession) Close() error {
	_ = s.fakeRevisionProcessSession.Close()
	if s.closeHook != nil {
		s.closeHook()
	}
	return s.closeErr
}

type publicationFixture struct {
	server     *policyServer
	mitm       *mitmTransparent
	deps       mitmLaunchDependencies
	mu         sync.Mutex
	sessions   []*publicationSession
	children   []*mitmproxy.Running
	configs    []mitmproxy.Config
	events     []string
	stops      map[*mitmproxy.Running]int
	newSession func(*publicationSession)
	stopHook   func()
}

func recoveryPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	s := recoveryTestOwner(t, policyCandidateInputs(t), credentialvault.NewStore(nil, nil))
	s.mitmGate = mitmproxy.NewHealthGate()
	f := &publicationFixture{server: s, stops: map[*mitmproxy.Running]int{}}
	owner := &revisionLaunchOwner{server: s, snapshot: s.captureRevisionBootstrap}
	owner.newSession = func(revisionruntime.ProcessSessionConfig) (revisionProcessSession, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		session := &publicationSession{fakeRevisionProcessSession: &fakeRevisionProcessSession{config: validFakeRevisionIPCConfig()}}
		index := len(f.sessions)
		session.closeHook = func() { f.event(fmt.Sprintf("close-%d", index)) }
		if f.newSession != nil {
			f.newSession(session)
		}
		f.sessions = append(f.sessions, session)
		return session, nil
	}
	owner.stop = func(r *mitmproxy.Running) {
		f.mu.Lock()
		f.stops[r]++
		index := -1
		for i, child := range f.children {
			if child == r {
				index = i
				break
			}
		}
		f.events = append(f.events, fmt.Sprintf("stop-%d", index))
		hook := f.stopHook
		var onExit func(error)
		if index >= 0 {
			onExit = f.configs[index].OnExit
		}
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		if onExit != nil {
			onExit(nil)
		}
	}
	f.mitm = &mitmTransparent{cfg: mitmproxy.Config{ListenPort: 18081}, revisionOwner: owner, restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{})}
	f.deps = mitmLaunchDependencies{
		launch: func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			child := &mitmproxy.Running{}
			f.children = append(f.children, child)
			f.configs = append(f.configs, cfg)
			return child, nil
		},
		listen: func(context.Context, string, time.Duration) error { return nil },
		retry:  func(context.Context, <-chan struct{}, time.Duration) bool { return false },
	}
	return f
}
func (f *publicationFixture) event(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}
func (f *publicationFixture) quarantine() {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	f.server.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
}
func (f *publicationFixture) invalidate() {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	f.server.invalidateRevisionBootstrapLocked()
}
func (f *publicationFixture) assertDisposed(t *testing.T, index int) {
	t.Helper()
	require.Equal(t, 1, f.stops[f.children[index]])
	require.Equal(t, 1, f.sessions[index].closeCalls)
	require.Equal(t, []string{fmt.Sprintf("stop-%d", index), fmt.Sprintf("close-%d", index)}, f.events[len(f.events)-2:])
}
func (f *publicationFixture) launch(t *testing.T) *revisionLaunchResult {
	t.Helper()
	result, err := f.mitm.launchAndListen(context.Background(), f.deps)
	require.NoError(t, err)
	return result
}

func TestRevisionRecoveryInitialPublication(t *testing.T) {
	for _, stage := range []string{"before-capture", "captured", "IPC", "listen", "prepared"} {
		t.Run(stage, func(t *testing.T) {
			f := recoveryPublicationFixture(t)
			if stage == "before-capture" {
				f.quarantine()
				err := f.mitm.startInitial(context.Background(), f.deps, func() error { return nil })
				require.ErrorIs(t, err, errRevisionRecoveryRequired)
				require.Empty(t, f.children)
				require.Empty(t, f.sessions)
				return
			}
			entered, release := make(chan struct{}), make(chan struct{})
			barrier := func() { close(entered); <-release }
			prepare := func() error { return nil }
			switch stage {
			case "captured":
				capture := f.mitm.revisionOwner.snapshot
				f.mitm.revisionOwner.snapshot = func(ctx context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
					snapshot, epoch, ticket, err := capture(ctx)
					barrier()
					return snapshot, epoch, ticket, err
				}
			case "IPC":
				f.newSession = func(session *publicationSession) { session.bootstrapHook = barrier }
			case "listen":
				f.deps.listen = func(context.Context, string, time.Duration) error { barrier(); return nil }
			case "prepared":
				prepare = func() error { barrier(); return nil }
			}
			done := make(chan error, 1)
			go func() { done <- f.mitm.startInitial(context.Background(), f.deps, prepare) }()
			<-entered
			require.True(t, f.server.mitmGate.MitmPending())
			require.True(t, f.server.mu.TryLock(), "capture/IPC/listen/preparation must not retain the policy barrier")
			f.server.mu.Unlock()
			f.quarantine()
			close(release)
			require.ErrorIs(t, <-done, errRevisionRecoveryRequired)
			require.True(t, f.server.mitmGate.MitmPending())
			require.Nil(t, f.mitm.running)
			require.Nil(t, f.mitm.revisionSession)
			f.assertDisposed(t, 0)
		})
	}
	for _, change := range []string{"policy", "always", "vault"} {
		t.Run("stale-"+change, func(t *testing.T) {
			f := recoveryPublicationFixture(t)
			prepareCalls := 0
			err := f.mitm.startInitial(context.Background(), f.deps, func() error {
				prepareCalls++
				f.server.mu.Lock()
				defer f.server.mu.Unlock()
				switch change {
				case "policy":
					return f.server.replaceRevisionBaseLocked(policyCandidateInputs(t))
				case "always":
					inputs := policyCandidateInputs(t)
					inputs.alwaysAllow = inputs.user.Egress
					return f.server.replaceRevisionBaseLocked(inputs)
				default:
					_, err := f.server.credentialVault.Create(credentialvault.CreateRequest{}, policyCandidateInputs(t).user)
					return err
				}
			})
			require.NoError(t, err)
			require.Equal(t, 1, prepareCalls, "redirect and CA preparation must not repeat")
			require.Len(t, f.children, 2)
			f.assertDisposed(t, 0)
			require.Same(t, f.children[1], f.mitm.running)
			require.Same(t, f.sessions[1], f.mitm.revisionSession)
			require.Equal(t, uint64(2), f.mitm.currentGen)
			require.False(t, f.server.mitmGate.MitmPending())
		})
	}
	t.Run("live-context-after-listen", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		var listenContext context.Context
		f.deps.listen = func(ctx context.Context, _ string, _ time.Duration) error { listenContext = ctx; return nil }
		err := f.mitm.startInitial(context.Background(), f.deps, func() error {
			require.ErrorIs(t, listenContext.Err(), context.Canceled)
			require.True(t, f.server.mitmGate.MitmPending())
			return nil
		})
		require.NoError(t, err)
		require.False(t, f.server.mitmGate.MitmPending())
		require.Same(t, f.children[0], f.mitm.running)
		require.Same(t, f.sessions[0], f.mitm.revisionSession)
		require.Equal(t, uint64(1), f.mitm.currentGen)
	})
	t.Run("caller-cancelled-before-publication", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := f.mitm.startInitial(ctx, f.deps, func() error { cancel(); return nil })
		require.ErrorIs(t, err, context.Canceled)
		require.True(t, f.server.mitmGate.MitmPending())
		f.assertDisposed(t, 0)
	})
}

// Always-rule reload must wait for a ready attached generation. It cannot
// invalidate an in-flight initial bootstrap using the old local-only path.
func TestRevisionRecoveryInitialStaleRetry(t *testing.T) {
	t.Run("always-reload-waits-for-initial-publication", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		t.Setenv("OTEL_SDK_DISABLED", "true")
		inputs := policyCandidateInputs(t)
		inputs.alwaysAllow = []policy.EgressRule{mustRule(t, policy.ActionAllow, "always.example.com")}
		f.server.mu.Lock()
		err := f.server.replaceRevisionBaseLocked(inputs)
		f.server.mitm = f.mitm
		f.server.mu.Unlock()
		require.NoError(t, err)
		f.server.proxy = &stubProxy{updated: inputs.user}
		loader := &stagedTestAlwaysLoader{allow: inputs.alwaysAllow, candidateAllow: inputs.alwaysAllow, pending: true}
		f.server.alwaysLoader = loader
		nft := &stubNft{}
		f.server.nft = nft
		var ticket *revisionBootstrapTicket
		capture := f.mitm.revisionOwner.snapshot
		f.mitm.revisionOwner.snapshot = func(ctx context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
			snapshot, epoch, captured, err := capture(ctx)
			ticket = captured
			return snapshot, epoch, captured, err
		}
		entered, release := make(chan struct{}), make(chan struct{})
		f.deps.listen = func(context.Context, string, time.Duration) error { close(entered); <-release; return nil }
		done := make(chan error, 1)
		go func() { done <- f.mitm.startInitial(context.Background(), f.deps, func() error { return nil }) }()
		<-entered
		changed, err := f.server.reloadAlwaysRules()
		require.ErrorIs(t, err, revision.ErrTransportUnavailable)
		require.False(t, changed)
		require.True(t, loader.pending)
		require.NoError(t, validateRecoveryTicket(f.server, ticket))
		require.Zero(t, nft.calls)
		require.True(t, f.server.mitmGate.MitmPending())
		close(release)
		require.NoError(t, <-done)
		require.Len(t, f.sessions, 1)
		require.Empty(t, f.events)
		require.False(t, f.server.mitmGate.MitmPending())
		f.sessions[0].updateIdentity = mutationIdentity(t, credentialvault.ActiveSnapshot{}, 1)
		changed, err = f.server.reloadAlwaysRules()
		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, loader.pending)
		require.Equal(t, int64(1), f.server.revisionRecovery.current.epoch)
		require.Equal(t, 1, f.sessions[0].updateCalls)
		require.Equal(t, 1, nft.calls)
		require.ErrorIs(t, validateRecoveryTicket(f.server, ticket), errStaleRevisionBootstrap)
		f.mitm.shutdown(time.Second)
		f.mitm.shutdown(time.Second)
		f.assertDisposed(t, 0)
	})
	t.Run("bounded-stale-retries", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		f.deps.listen = func(context.Context, string, time.Duration) error { f.invalidate(); return nil }
		prepareCalls := 0
		err := f.mitm.startInitial(context.Background(), f.deps, func() error { prepareCalls++; return nil })
		require.ErrorIs(t, err, errStaleRevisionBootstrap)
		require.Len(t, f.children, 3, "initial startup permits at most three complete attempts")
		require.Equal(t, 1, prepareCalls)
		require.Len(t, f.mitm.restartCh, 3, "bounded cleanup must fit before the watcher starts")
		for i := range f.children {
			require.Equal(t, 1, f.stops[f.children[i]])
			require.Equal(t, 1, f.sessions[i].closeCalls)
		}
		require.Equal(t, []string{"stop-0", "close-0", "stop-1", "close-1", "stop-2", "close-2"}, f.events)
		require.Nil(t, f.mitm.running)
		require.Nil(t, f.mitm.pending)
		require.Zero(t, f.mitm.launchGen)
		require.True(t, f.server.mitmGate.MitmPending())
	})
	for _, stop := range []string{"cancel", "shutdown", "quarantine", "cleanup-failure"} {
		t.Run("stale-cleanup-"+stop+"-stops-retry", func(t *testing.T) {
			f := recoveryPublicationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errRevisionRecoveryRequired
			f.newSession = func(session *publicationSession) {
				switch stop {
				case "cancel":
					f.stopHook = cancel
					want = context.Canceled
				case "shutdown":
					f.stopHook = func() { f.mitm.shutdown(time.Second) }
					want = revision.ErrClosed
				case "quarantine":
					f.stopHook = f.quarantine
				case "cleanup-failure":
					session.closeErr = errors.New("private cleanup detail")
				}
			}
			prepareCalls := 0
			err := f.mitm.startInitial(ctx, f.deps, func() error { prepareCalls++; f.invalidate(); return nil })
			require.ErrorIs(t, err, want)
			require.NotContains(t, err.Error(), "private cleanup detail")
			require.Equal(t, 1, prepareCalls)
			require.Len(t, f.children, 1)
			require.Len(t, f.sessions, 1)
			f.assertDisposed(t, 0)
			require.Nil(t, f.mitm.running)
			require.Nil(t, f.mitm.pending)
			require.True(t, f.server.mitmGate.MitmPending())
		})
	}
	t.Run("preparation-error-is-never-retried", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		prepareCalls := 0
		err := f.mitm.startInitial(context.Background(), f.deps, func() error {
			prepareCalls++
			return errStaleRevisionBootstrap
		})
		require.ErrorIs(t, err, errStaleRevisionBootstrap)
		require.Equal(t, 1, prepareCalls)
		require.Len(t, f.children, 1)
		f.assertDisposed(t, 0)
	})
	t.Run("non-stale-publication-error-is-never-retried", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		err := f.mitm.startInitial(context.Background(), f.deps, func() error { f.configs[0].OnExit(nil); return nil })
		require.ErrorIs(t, err, revision.ErrTransportUnavailable)
		require.Len(t, f.children, 1)
		f.assertDisposed(t, 0)
	})
}

func TestRevisionRecoveryRestartPublication(t *testing.T) {
	t.Run("stale-retries-fresh-ticket", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		listens, retries := 0, 0
		f.deps.listen = func(context.Context, string, time.Duration) error {
			listens++
			if listens == 1 {
				f.invalidate()
			}
			return nil
		}
		f.deps.retry = func(context.Context, <-chan struct{}, time.Duration) bool {
			retries++
			require.True(t, f.server.mitmGate.MitmPending())
			f.assertDisposed(t, 0)
			return true
		}
		f.mitm.restartWithBackoffUsing(context.Background(), f.server.mitmGate, f.deps)
		require.Equal(t, 1, retries)
		require.Len(t, f.children, 2)
		require.Same(t, f.children[1], f.mitm.running)
		require.Equal(t, uint64(2), f.mitm.currentGen)
		require.Zero(t, f.sessions[1].closeCalls)
		require.False(t, f.server.mitmGate.MitmPending())
	})
	for _, stage := range []string{"before-capture", "IPC", "listen"} {
		t.Run("sticky-"+stage, func(t *testing.T) {
			f := recoveryPublicationFixture(t)
			if stage == "before-capture" {
				f.quarantine()
			}
			if stage == "IPC" {
				f.newSession = func(session *publicationSession) { session.bootstrapHook = f.quarantine }
			}
			if stage == "listen" {
				f.deps.listen = func(context.Context, string, time.Duration) error { f.quarantine(); return nil }
			}
			retries := 0
			f.deps.retry = func(context.Context, <-chan struct{}, time.Duration) bool { retries++; return false }
			f.mitm.restartWithBackoffUsing(context.Background(), f.server.mitmGate, f.deps)
			require.Zero(t, retries, "sticky recovery must stop without backoff or a busy retry")
			if stage == "before-capture" {
				require.Empty(t, f.children)
			} else {
				require.Len(t, f.children, 1)
				f.assertDisposed(t, 0)
			}
			require.True(t, f.server.mitmGate.MitmPending())
			require.Nil(t, f.mitm.running)
			f.mitm.shutdown(time.Second)
			require.True(t, f.mitm.stopping)
		})
	}
	t.Run("clean-crash-and-stale-exit", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		require.NoError(t, f.mitm.startInitial(context.Background(), f.deps, func() error { return nil }))
		f.configs[0].OnExit(errors.New("ordinary crash"))
		ev := <-f.mitm.restartCh
		require.Equal(t, uint64(1), ev.gen)
		require.True(t, f.server.mitmGate.MitmPending())
		f.mitm.closeRevisionSession(ev.gen)
		require.Equal(t, 1, f.sessions[0].closeCalls)
		require.Zero(t, f.stops[f.children[0]], "OnExit has already reaped the child")
		f.mitm.restartWithBackoffUsing(context.Background(), f.server.mitmGate, f.deps)
		require.Equal(t, revisionRecoveryNone, f.server.revisionRecovery.reason)
		require.False(t, f.server.mitmGate.MitmPending())
		f.configs[0].OnExit(nil)
		ev = <-f.mitm.restartCh
		f.mitm.closeRevisionSession(ev.gen)
		require.False(t, f.server.mitmGate.MitmPending())
		require.Zero(t, f.sessions[1].closeCalls)
		require.Same(t, f.children[1], f.mitm.running)
		f.mitm.shutdown(time.Second)
		f.mitm.shutdown(time.Second)
		require.Equal(t, 1, f.stops[f.children[1]])
		require.Equal(t, 1, f.sessions[1].closeCalls)
	})
}

type publicationBarrierContext struct {
	context.Context
	entered, release chan struct{}
}

func (c publicationBarrierContext) Err() error { close(c.entered); <-c.release; return c.Context.Err() }
func TestRevisionRecoveryPublishRace(t *testing.T) {
	for _, contender := range []string{"recovery", "exit", "shutdown"} {
		t.Run(contender, func(t *testing.T) {
			f := recoveryPublicationFixture(t)
			result := f.launch(t)
			ctx := publicationBarrierContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
			published := make(chan error, 1)
			go func() { published <- publishRevisionReady(ctx, f.mitm, result) }()
			<-ctx.entered
			require.False(t, f.server.mu.TryLock(), "ticket check and ready write must retain s.mu")
			require.False(t, f.mitm.mu.TryLock(), "exit state and ready write must retain m.mu")
			require.True(t, f.server.mitmGate.MitmPending())
			entered, done := make(chan struct{}), make(chan struct{})
			go func() {
				close(entered)
				switch contender {
				case "recovery":
					f.quarantine()
				case "exit":
					f.configs[0].OnExit(nil)
				case "shutdown":
					f.mitm.shutdown(time.Second)
				}
				close(done)
			}()
			<-entered
			select {
			case <-done:
				t.Fatal("contender bypassed the publication barrier")
			default:
			}
			close(ctx.release)
			require.NoError(t, <-published)
			<-done
			require.True(t, f.server.mitmGate.MitmPending(), "a recorded exit, shutdown, or quarantine must dominate readiness")
		})
	}
	t.Run("exit-recorded-before-publication", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		result := f.launch(t)
		f.configs[0].OnExit(nil)
		require.ErrorIs(t, publishRevisionReady(context.Background(), f.mitm, result), revision.ErrTransportUnavailable)
		require.Nil(t, f.mitm.running)
		f.mitm.cleanupLaunch(result)
		f.assertDisposed(t, 0)
	})
	t.Run("wrong-attempt-cannot-publish", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		result := f.launch(t)
		copy := *result
		require.ErrorIs(t, publishRevisionReady(context.Background(), f.mitm, &copy), errStaleRevisionBootstrap)
		require.NoError(t, publishRevisionReady(context.Background(), f.mitm, result))
		require.ErrorIs(t, publishRevisionReady(context.Background(), f.mitm, result), errInvalidRevisionBootstrap)
	})
}

func TestRevisionRecoveryExitAndShutdown(t *testing.T) {
	t.Run("shutdown-during-listen", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		f.deps.listen = func(context.Context, string, time.Duration) error { close(entered); <-release; return nil }
		done := make(chan error, 1)
		go func() { done <- f.mitm.startInitial(context.Background(), f.deps, func() error { return nil }) }()
		<-entered
		f.mitm.shutdown(time.Second)
		close(release)
		require.ErrorIs(t, <-done, revision.ErrClosed)
		f.assertDisposed(t, 0)
		require.Nil(t, f.mitm.running)
		require.True(t, f.server.mitmGate.MitmPending())
	})
	t.Run("shutdown-during-rejected-cleanup", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		f.stopHook = func() { close(entered); <-release }
		done := make(chan error, 1)
		go func() {
			done <- f.mitm.startInitial(context.Background(), f.deps, func() error { f.invalidate(); return nil })
		}()
		<-entered
		require.True(t, f.server.mu.TryLock())
		f.server.mu.Unlock()
		require.True(t, f.mitm.mu.TryLock())
		f.mitm.mu.Unlock()
		f.mitm.shutdown(time.Second)
		close(release)
		require.ErrorIs(t, <-done, revision.ErrClosed)
		f.assertDisposed(t, 0)
	})
	for _, stage := range []string{"handoff", "launch", "bootstrap", "listen", "publication", "exit", "shutdown"} {
		t.Run("cleanup-failure-"+stage, func(t *testing.T) {
			f := recoveryPublicationFixture(t)
			f.newSession = func(session *publicationSession) {
				session.closeErr = errors.New("private cleanup detail")
				if stage == "handoff" {
					session.configErr = revision.ErrInvalid
				}
				if stage == "bootstrap" {
					session.bootstrapErrs = []error{revision.ErrInvalid}
				}
			}
			if stage == "launch" {
				f.deps.launch = func(mitmproxy.Config) (*mitmproxy.Running, error) { return nil, revision.ErrTransportUnavailable }
			}
			if stage == "listen" {
				f.deps.listen = func(context.Context, string, time.Duration) error { return revision.ErrTransportUnavailable }
			}
			err := f.mitm.startInitial(context.Background(), f.deps, func() error {
				if stage == "publication" {
					f.invalidate()
				}
				return nil
			})
			if stage == "exit" {
				require.NoError(t, err)
				f.configs[0].OnExit(nil)
				f.mitm.closeRevisionSession(1)
			} else if stage == "shutdown" {
				require.NoError(t, err)
				f.mitm.shutdown(time.Second)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "private cleanup detail")
			}
			_, _, _, captureErr := f.server.captureRevisionBootstrap(context.Background())
			require.ErrorIs(t, captureErr, errRevisionRecoveryRequired)
			require.Equal(t, revisionRecoverySessionCleanupFailed, f.server.revisionRecovery.reason)
			require.True(t, f.server.mitmGate.MitmPending())
			require.Equal(t, 1, f.sessions[0].closeCalls)
			if len(f.children) > 0 && stage != "exit" {
				f.assertDisposed(t, 0)
			}
			retryCalls := 0
			f.deps.retry = func(context.Context, <-chan struct{}, time.Duration) bool { retryCalls++; return false }
			f.mitm.restartWithBackoffUsing(context.Background(), f.server.mitmGate, f.deps)
			require.Zero(t, retryCalls)
			require.Len(t, f.sessions, 1)
		})
	}
	t.Run("quarantined-watcher-still-shuts-down", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		require.NoError(t, f.mitm.startInitial(context.Background(), f.deps, func() error { return nil }))
		f.quarantine()
		captureRejected := make(chan struct{})
		capture := f.mitm.revisionOwner.snapshot
		f.mitm.revisionOwner.snapshot = func(ctx context.Context) (credentialvault.ActiveSnapshot, int64, *revisionBootstrapTicket, error) {
			snapshot, epoch, ticket, err := capture(ctx)
			close(captureRejected)
			return snapshot, epoch, ticket, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.mitm.watchDone = make(chan struct{})
		f.mitm.watchMitmproxy(ctx, f.server.mitmGate)
		f.configs[0].OnExit(nil)
		<-captureRejected
		f.mitm.shutdown(time.Second)
		select {
		case <-f.mitm.watchDone:
		default:
			t.Fatal("quarantine blocked watcher shutdown")
		}
		require.Equal(t, 1, f.sessions[0].closeCalls)
		require.Zero(t, f.stops[f.children[0]])
		require.Len(t, f.sessions, 1)
		require.NoError(t, ctx.Err(), "explicit shutdown must not require caller cancellation")
	})

	t.Run("legacy-initial-and-restart", func(t *testing.T) {
		f := recoveryPublicationFixture(t)
		f.mitm.revisionOwner = nil
		require.NoError(t, f.mitm.startInitial(context.Background(), f.deps, func() error { return nil }))
		require.True(t, f.server.mitmGate.MitmPending(), "legacy initial MarkStackReady remains the main caller's job")
		f.server.mitmGate.MarkStackReady()
		f.mitm.closeRevisionSession(1)
		f.server.mitmGate.SetReady(false)
		f.mitm.restartWithBackoffUsing(context.Background(), f.server.mitmGate, f.deps)
		require.False(t, f.server.mitmGate.MitmPending())
		require.Equal(t, uint64(2), f.mitm.currentGen)
		require.Empty(t, f.sessions)
	})
}
