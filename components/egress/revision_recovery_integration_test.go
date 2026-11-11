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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/revisionruntime"
	"github.com/stretchr/testify/require"
)

type recoveryIPCSession struct {
	revisionProcessSession
	t          *testing.T
	child      *revisionIPCChild
	closeCalls int
}

func (s *recoveryIPCSession) Close() error {
	s.closeCalls++
	// Observe the real ProcessSession cleanup, including its failure result.
	if s.child != nil && s.child.running.Cmd.ProcessState == nil {
		s.t.Error("session Close occurred before the exact child was reaped")
	}
	return s.revisionProcessSession.Close()
}

// This guarded fixture shares the real child, authenticated Unix endpoint and
// installation-only receiver with the mutation fixture. The latter deliberately
// remains a nil-server legacy adapter. Neither fixture launches mitmdump or TLS.
type recoveryIPCFixture struct {
	server   *policyServer
	mitm     *mitmTransparent
	deps     mitmLaunchDependencies
	children []*revisionIPCChild
	sessions []*recoveryIPCSession
	configs  []mitmproxy.Config
}

func newRecoveryIPCFixture(t *testing.T) *recoveryIPCFixture {
	t.Helper()
	_, err := exec.LookPath("python3")
	require.NoError(t, err, "real revision recovery IPC requires python3; setup failures must not skip")
	inputs := policyCandidateInputs(t)
	s := revisionIntegrationServer(t)
	s.credentialVault = policyCandidateStore(t, inputs) // CommitCandidate-pinned rendered snapshot.
	s.mu.Lock()
	err = s.initRevisionRecoveryLocked(inputs)
	s.mu.Unlock()
	require.NoError(t, err)
	parent, err := os.MkdirTemp("/tmp", "osri-recovery-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
	f := &recoveryIPCFixture{server: s}
	owner := &revisionLaunchOwner{
		server: s,
		config: revisionruntime.ProcessSessionConfig{
			ParentDir: parent, UID: os.Getuid(), GID: os.Getgid(),
			SubjectGeneration: "recovery-integration-subject", MaxSnapshotBytes: 65536,
		},
		snapshot: s.captureRevisionBootstrap,
	}
	owner.newSession = func(config revisionruntime.ProcessSessionConfig) (revisionProcessSession, error) {
		session, err := newRevisionProcessSession(config)
		if err != nil {
			return nil, err
		}
		observed := &recoveryIPCSession{revisionProcessSession: session, t: t}
		f.sessions = append(f.sessions, observed)
		// The raw fallback runs after launchRevisionIPCChild's registered reap.
		t.Cleanup(func() { require.NoError(t, session.Close()) })
		return observed, nil
	}
	owner.stop = func(running *mitmproxy.Running) {
		for i, child := range f.children {
			if child.running == running {
				require.Zero(t, child.stops, "only the exact unpublished child may be stopped once")
				child.stop()
				require.NotNil(t, running.Cmd.ProcessState, "stop must reap before session Close")
				f.configs[i].OnExit(nil) // The real launcher's callback also runs after Wait.
				return
			}
		}
		t.Fatal("cleanup attempted to stop a foreign child")
	}
	f.mitm = &mitmTransparent{
		cfg: mitmproxy.Config{ListenPort: 18081}, revisionOwner: owner,
		restartCh: make(chan exitEvent, 16), shutdownCh: make(chan struct{}),
	}
	f.deps = mitmLaunchDependencies{
		launch: func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
			child := launchRevisionIPCChild(t, cfg)
			f.children = append(f.children, child)
			f.configs = append(f.configs, cfg)
			f.sessions[len(f.sessions)-1].child = child
			// Run lifecycle teardown before the child's fallback t.Cleanup reap.
			t.Cleanup(func() { f.mitm.shutdown(time.Second) })
			return child.running, nil
		},
		// The subprocess is an IPC receiver, not a TCP mitmdump listener. Only
		// this boundary is supplied; installation and publication are real.
		listen: func(context.Context, string, time.Duration) error { return nil },
		retry:  func(context.Context, <-chan struct{}, time.Duration) bool { t.Error("unexpected retry"); return false },
	}
	t.Cleanup(func() { f.mitm.shutdown(time.Second) })
	return f
}

func (f *recoveryIPCFixture) assertUnpublished(t *testing.T, result *revisionLaunchResult) {
	t.Helper()
	require.True(t, f.server.mitmGate.MitmPending())
	require.Nil(t, f.mitm.running)
	require.Nil(t, f.mitm.revisionSession)
	require.Same(t, result, f.mitm.pending)
	require.Equal(t, result.generation, f.mitm.launchGen)
	require.Same(t, f.children[0].running, result.running)
	require.Same(t, f.sessions[0], result.session)
}

func TestRevisionRecoveryIPCInstalledButStale(t *testing.T) {
	for _, change := range []string{"policy-base", "vault-aba", "recovery-latch", "cleanup-failure"} {
		t.Run(change, func(t *testing.T) {
			f := newRecoveryIPCFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Production launch/listen returns the real installed result. Keep
			// final publication paused here while the shared owner changes.
			result, err := f.mitm.launchAndListen(ctx, f.deps)
			require.NoError(t, err)
			child, session := f.children[0], f.sessions[0]
			config, err := session.MitmproxyConfig()
			require.NoError(t, err)
			snapshot := result.ticket.vault.Snapshot()
			installed := assertRevisionChildSnapshot(t, child, snapshot)
			require.Equal(t, 1, installed.Counts.Prepare)
			require.Equal(t, 1, installed.Counts.Commit)
			require.Equal(t, config.ControlGeneration, installed.Active.ControlGeneration)
			f.assertUnpublished(t, result)
			require.True(t, f.server.mu.TryLock(), "slow bootstrap/listen must release the policy barrier")
			f.server.mu.Unlock()

			// Even an authenticated matching installed identity is insufficient:
			// publication also needs the owner ticket and exact pending result.
			ticket := result.ticket
			result.ticket = nil
			require.ErrorIs(t, publishRevisionReady(ctx, f.mitm, result), errInvalidRevisionBootstrap)
			foreign := recoveryTestOwner(t, policyCandidateInputs(t), f.server.credentialVault)
			_, _, result.ticket, err = foreign.captureRevisionBootstrap(ctx)
			require.NoError(t, err)
			require.ErrorIs(t, publishRevisionReady(ctx, f.mitm, result), errStaleRevisionBootstrap)
			result.ticket = ticket
			copy := *result
			require.ErrorIs(t, publishRevisionReady(ctx, f.mitm, &copy), errStaleRevisionBootstrap)
			f.assertUnpublished(t, result)

			f.server.mu.Lock()
			switch change {
			case "vault-aba":
				err = f.server.credentialVault.Delete()
				if err == nil {
					// Recreate and pin the identical bytes and public revision.
					recreateRecoveryIPCVault(t, f.server)
				}
			case "recovery-latch":
				f.server.requireRevisionRecoveryLocked(revisionRecoveryExternalEffectsUnknown)
			default:
				// Identical rules and epoch still replace the private base handle.
				err = f.server.replaceRevisionBaseLocked(policyCandidateInputs(t))
			}
			f.server.mu.Unlock()
			require.NoError(t, err)
			after := assertRevisionChildSnapshot(t, child, snapshot)
			require.Equal(t, installed.Active, after.Active, "installed IPC state survives local invalidation")
			want := errStaleRevisionBootstrap
			if change == "recovery-latch" {
				want = errRevisionRecoveryRequired
			}
			require.ErrorIs(t, publishRevisionReady(ctx, f.mitm, result), want)
			f.assertUnpublished(t, result)
			require.Zero(t, child.stops)
			require.Zero(t, session.closeCalls)

			if change == "cleanup-failure" {
				// Force the real anchored Close to refuse an alien replacement.
				dir := filepath.Dir(config.SocketPath)
				require.NoError(t, os.Rename(dir, dir+"-retained"))
				require.NoError(t, os.Mkdir(dir, 0o700))
			}
			f.mitm.cleanupLaunch(result)
			require.Equal(t, 1, child.stops)
			require.NotNil(t, child.running.Cmd.ProcessState)
			require.Equal(t, 1, session.closeCalls)
			require.Nil(t, f.mitm.pending)
			require.Zero(t, f.mitm.launchGen)
			require.True(t, f.server.mitmGate.MitmPending())
			_, err = session.MitmproxyConfig()
			require.ErrorIs(t, err, revision.ErrClosed)
			if change == "cleanup-failure" {
				require.DirExists(t, filepath.Dir(config.SocketPath), "cleanup must preserve the alien directory")
				require.DirExists(t, filepath.Dir(config.SocketPath)+"-retained")
				require.Equal(t, revisionRecoverySessionCleanupFailed, f.server.revisionRecovery.reason)
			} else {
				require.NoDirExists(t, filepath.Dir(config.SocketPath))
			}

			if change == "recovery-latch" || change == "cleanup-failure" {
				_, _, rejected, captureErr := f.server.captureRevisionBootstrap(ctx)
				require.ErrorIs(t, captureErr, errRevisionRecoveryRequired)
				require.Nil(t, rejected)
				f.mitm.restartWithBackoffUsing(ctx, f.server.mitmGate, f.deps)
				require.Len(t, f.children, 1, "ordinary restart must not clear sticky recovery")
				require.Len(t, f.sessions, 1)
				require.Nil(t, f.mitm.running)
				require.True(t, f.server.mitmGate.MitmPending())
				f.mitm.shutdown(time.Second)
				require.True(t, f.mitm.stopping)
				require.Equal(t, 1, child.stops)
				return
			}
			f.mitm.restartWithBackoffUsing(ctx, f.server.mitmGate, f.deps)
			require.Len(t, f.children, 2)
			require.False(t, f.server.mitmGate.MitmPending())
			require.Same(t, f.children[1].running, f.mitm.running)
			require.Same(t, f.sessions[1], f.mitm.revisionSession)
			require.Greater(t, f.mitm.currentGen, result.generation)
			freshConfig, err := f.sessions[1].MitmproxyConfig()
			require.NoError(t, err)
			require.NotEqual(t, config.ControlGeneration, freshConfig.ControlGeneration)
			require.True(t, config.SessionToken != freshConfig.SessionToken)
			freshSnapshot, err := f.server.credentialVault.ActiveSnapshot()
			require.NoError(t, err)
			fresh := assertRevisionChildSnapshot(t, f.children[1], freshSnapshot)
			require.Equal(t, installed.Digest, fresh.Digest, "matching wire bytes do not revive a stale ticket")
			f.mitm.closeRevisionSession(result.generation) // Reaped old exit must not claim the fresh child.
			require.Same(t, f.children[1].running, f.mitm.running)
			require.False(t, f.server.mitmGate.MitmPending())
			require.Zero(t, f.children[1].stops)
			require.Zero(t, f.sessions[1].closeCalls)
		})
	}

	t.Run("clean-crash-restart", func(t *testing.T) {
		f := newRecoveryIPCFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, f.mitm.startInitial(ctx, f.deps, func() error { return nil }))
		require.False(t, f.server.mitmGate.MitmPending())
		old := f.children[0]
		old.stop() // The subprocess has actually exited and been reaped.
		f.configs[0].OnExit(nil)
		var ev exitEvent
		select {
		case ev = <-f.mitm.restartCh:
		case <-ctx.Done():
			t.Fatalf("clean-crash restart did not receive the reaped child's tagged exit: %v", ctx.Err())
		}
		require.True(t, f.server.mitmGate.MitmPending())
		f.mitm.closeRevisionSession(ev.gen)
		require.Equal(t, 1, f.sessions[0].closeCalls)
		f.mitm.restartWithBackoffUsing(ctx, f.server.mitmGate, f.deps)
		require.Len(t, f.children, 2)
		require.Equal(t, revisionRecoveryNone, f.server.revisionRecovery.reason)
		require.False(t, f.server.mitmGate.MitmPending())
		snapshot, err := f.server.credentialVault.ActiveSnapshot()
		require.NoError(t, err)
		assertRevisionChildSnapshot(t, f.children[1], snapshot)
		f.configs[0].OnExit(nil)
		f.mitm.closeRevisionSession(ev.gen)
		require.Same(t, f.children[1].running, f.mitm.running)
		require.Zero(t, f.sessions[1].closeCalls)
		require.False(t, f.server.mitmGate.MitmPending())
	})
}

func recreateRecoveryIPCVault(t *testing.T, s *policyServer) {
	t.Helper()
	store := s.credentialVault
	candidate, err := store.PrepareCreate(integrationVaultRequest("private-policy-candidate"), policyCandidateInputs(t).user)
	require.NoError(t, err)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = store.CommitCandidate(candidate)
	require.NoError(t, err)
}
