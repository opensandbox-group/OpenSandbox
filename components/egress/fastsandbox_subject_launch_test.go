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
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func subjectTestOwner(t *testing.T) *fastSandboxSubjectLaunchOwner {
	t.Helper()
	// Keep socket paths below the portable 103-byte limit, including Go's
	// variable-length test name directories. The directory is always removed.
	parent, err := os.MkdirTemp("", "fsb-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
	return &fastSandboxSubjectLaunchOwner{
		cfg:      fastSandboxSubjectSessionConfig{parent: parent, uid: os.Getuid(), gid: os.Getgid()},
		sessions: make(map[*mitmproxy.Running]*fastSandboxSubjectSession),
	}
}

// Runs the production addon with the actual Go session handoff. This is an
// integration-layer fixture, not evidence of real mitmdump/KVM/nft behavior.
func subjectAddonLauncher(t *testing.T, owner *fastSandboxSubjectLaunchOwner) (func(mitmproxy.Config) (*mitmproxy.Running, error), *[]mitmproxy.Config) {
	t.Helper()
	configs := &[]mitmproxy.Config{}
	stops := make(map[*mitmproxy.Running]func())
	owner.stop = func(r *mitmproxy.Running) {
		if stop := stops[r]; stop != nil {
			delete(stops, r)
			stop()
		}
	}
	t.Cleanup(func() {
		for r := range stops {
			owner.stop(r)
			require.NoError(t, owner.close(r))
		}
	})
	launch := func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
		python, err := exec.LookPath("python3")
		require.NoError(t, err)
		cmd := exec.Command(python, "tests/fixtures/subject_runtime_child.py")
		in, err := cmd.StdinPipe()
		require.NoError(t, err)
		out, err := cmd.StdoutPipe()
		require.NoError(t, err)
		cmd.Stderr = io.Discard
		require.NoError(t, cmd.Start())
		r := &mitmproxy.Running{Cmd: cmd}
		stops[r] = func() {
			_ = in.Close()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			_ = out.Close()
			if cfg.OnExit != nil {
				cfg.OnExit(nil)
			}
		}
		h := cfg.SubjectRevisionIPC
		require.NotNil(t, h)
		require.Nil(t, cfg.RevisionIPC)
		require.Contains(t, cfg.ScriptPaths, mitmproxy.SubjectRevisionScriptPath)
		require.NoError(t, json.NewEncoder(in).Encode(map[string]any{
			"MODE": "subject-v2", "SOCKET": h.SocketPath, "TOKEN": h.SessionToken, "CONTROL_GENERATION": h.ControlGeneration,
			"MAX_SUBJECTS": h.MaxSubjects, "MAX_SNAPSHOT_BYTES": h.MaxSnapshotBytes, "RETAINED_PAYLOAD_BUDGET": h.RetainedPayloadBudget,
		}))
		decoder := json.NewDecoder(out)
		ready := make(chan error, 1)
		go func() {
			var response map[string]bool
			err := decoder.Decode(&response)
			if err == nil && !response["ready"] {
				err = errors.New("addon not ready")
			}
			ready <- err
		}()
		select {
		case err := <-ready:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("addon fixture startup timed out")
		}
		*configs = append(*configs, cfg)
		return r, nil
	}
	return launch, configs
}

func TestFastSandboxSubjectAddonRestartIntegration(t *testing.T) {
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	m := &mitmTransparent{subjectOwner: o, cfg: mitmproxy.Config{ListenPort: 18081, ListenHost: "0.0.0.0", ScriptPaths: []string{"/custom.py"}}, restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	deps := mitmLaunchDependencies{launch: launch, listen: func(context.Context, string, time.Duration) error { return nil }, retry: func(context.Context, <-chan struct{}, time.Duration) bool { t.Fatal("unexpected retry"); return false }}
	require.NoError(t, m.startInitial(ctx, deps, func() error { return nil }))
	first := (*configs)[0].SubjectRevisionIPC
	require.Equal(t, fastSandboxRevisionMaxSubjects, first.MaxSubjects)
	require.Equal(t, int64(68719476736), first.RetainedPayloadBudget)
	require.Equal(t, []string{"/custom.py"}, m.cfg.ScriptPaths)
	target, err := fastSandboxSubjectTarget("sandbox", subject.Fencing{RuntimeInstanceID: "runtime", AttachmentID: "attachment"})
	require.NoError(t, err)
	require.Equal(t, "b46aad8c07e9bb4e59e780efbbd321468723bf206fda97f75314ad3d47c6f23b", target.SubjectGeneration)
	session := o.sessions[m.running]
	receipt, err := session.client.Register(ctx, target)
	require.NoError(t, err)
	require.Equal(t, "registered", receipt.Result.Outcome)
	transport, err := revision.NewSubjectUnixTransport(first.SocketPath, first.SessionToken, first.ControlGeneration, target, first.MaxSnapshotBytes)
	require.NoError(t, err)
	active, err := transport.Readback(ctx)
	require.NoError(t, err)
	require.Nil(t, active, "Register must remain known/uninstalled")
	require.ErrorIs(t, revision.CheckFreshSubjectSession(ctx, first.SocketPath, first.SessionToken, first.ControlGeneration), revision.ErrIndeterminate)
	oldRunning, oldGen := m.running, m.currentGen
	o.stop(oldRunning) // reaped before the same seam used by the restart watcher
	m.closeRevisionSession(oldGen)
	_, err = os.Stat(filepath.Dir(first.SocketPath))
	require.True(t, os.IsNotExist(err))
	gate := mitmproxy.NewHealthGate()
	m.restartWithBackoffUsing(ctx, gate, deps)
	require.Len(t, *configs, 2)
	second := (*configs)[1].SubjectRevisionIPC
	require.NotEqual(t, first.ControlGeneration, second.ControlGeneration)
	require.NotEqual(t, first.SessionToken, second.SessionToken)
	require.NotEqual(t, first.SocketPath, second.SocketPath)
	receipt, err = o.sessions[m.running].client.Register(ctx, target)
	require.NoError(t, err)
	require.Equal(t, int64(1), receipt.LifecycleSequence, "restart must have a new sequence owner")
	probe := &subjectIPCChild{socket: second.SocketPath, token: second.SessionToken}
	raw, _ := json.Marshal(map[string]any{"controlGeneration": first.ControlGeneration, "lifecycleSequence": 2, "target": target})
	status, body := probe.response(t, "/v2/subjects/register", raw, second.SessionToken, false)
	require.Equal(t, 409, status)
	require.JSONEq(t, `{"error":"control_generation_mismatch"}`, string(body))
	require.Equal(t, 401, probe.post(t, "/v2/subjects/register", raw, first.SessionToken, false))
	m.closeRevisionSession(oldGen) // stale cleanup cannot remove the current session
	require.Len(t, o.sessions, 1)
	m.shutdown(time.Second)
	require.Empty(t, o.sessions)
	entries, err := os.ReadDir(o.cfg.parent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestFastSandboxSubjectLaunchFailures(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		o, err := newFastSandboxSubjectLaunchOwner(false, -1, -1)
		require.NoError(t, err)
		require.Nil(t, o)
	})
	for _, name := range []string{"launch", "nil-child", "receiver-timeout", "listen", "prepare", "exited-before-publish"} {
		t.Run(name, func(t *testing.T) {
			o := subjectTestOwner(t)
			launch, _ := subjectAddonLauncher(t, o)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m := &mitmTransparent{subjectOwner: o, restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{})}
			failure := errors.New("fixture failure")
			if name == "launch" {
				launch = func(mitmproxy.Config) (*mitmproxy.Running, error) { return nil, failure }
			}
			if name == "nil-child" {
				launch = func(mitmproxy.Config) (*mitmproxy.Running, error) { return nil, nil }
			}
			if name == "receiver-timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				launch = func(mitmproxy.Config) (*mitmproxy.Running, error) { return &mitmproxy.Running{}, nil }
			}
			if name == "exited-before-publish" {
				original := launch
				launch = func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
					r, err := original(cfg)
					cfg.OnExit(nil)
					return r, err
				}
			}
			deps := mitmLaunchDependencies{launch: launch, listen: func(context.Context, string, time.Duration) error {
				if name == "listen" {
					return failure
				}
				return nil
			}}
			err := m.startInitial(ctx, deps, func() error {
				if name == "prepare" {
					return failure
				}
				return nil
			})
			require.Error(t, err)
			require.Empty(t, o.sessions)
			entries, readErr := os.ReadDir(o.cfg.parent)
			require.NoError(t, readErr)
			require.Empty(t, entries)
		})
	}
}

func TestFastSandboxSubjectRestartPrePublishExit(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		name := "retry-with-fresh-control-generation"
		if stopping {
			name = "stopping-is-terminal"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT", "true")
			o := subjectTestOwner(t)
			launch, configs := subjectAddonLauncher(t, o)
			m := &mitmTransparent{subjectOwner: o, restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			deps := mitmLaunchDependencies{launch: launch, listen: func(context.Context, string, time.Duration) error { return nil }}
			require.NoError(t, m.startInitial(ctx, deps, func() error { return nil }))
			o.stop(m.running)
			ev := <-m.restartCh
			require.Equal(t, m.currentGen, ev.gen)
			m.closeRevisionSession(ev.gen)
			gate := mitmproxy.NewHealthGate()
			gate.SetReady(false)
			var delays []time.Duration
			deps.retry = func(_ context.Context, _ <-chan struct{}, delay time.Duration) bool {
				delays = append(delays, delay)
				require.True(t, gate.MitmPending())
				require.Nil(t, m.running)
				require.Empty(t, o.sessions, "failed session must be closed before retry")
				return len(delays) <= 2
			}
			deps.listen = func(context.Context, string, time.Duration) error {
				h := (*configs)[len(*configs)-1].SubjectRevisionIPC
				require.NoError(t, revision.CheckFreshSubjectSession(ctx, h.SocketPath, h.SessionToken, h.ControlGeneration))
				if len(*configs) <= 3 {
					if stopping {
						m.claimForShutdown()
					}
					// The receiver is ready and this listener check succeeds, but
					// the child is reaped and OnExit recorded before publication.
					o.stop(m.pending.running)
				}
				return nil
			}
			// Use the same restart loop as watchMitmproxy after the current exit.
			m.restartWithBackoffUsing(ctx, gate, deps)
			if stopping {
				require.Len(t, *configs, 2)
				require.Empty(t, delays, "stopping must not schedule another launch")
				require.Nil(t, m.running)
				require.True(t, gate.MitmPending())
			} else {
				require.Equal(t, []time.Duration{time.Second, 2 * time.Second}, delays)
				require.Len(t, *configs, 4)
				require.NotNil(t, m.running)
				require.Equal(t, uint64(4), m.currentGen)
				require.False(t, gate.MitmPending())
				require.Len(t, o.sessions, 1)
			}
			controls := make(map[string]bool)
			for _, cfg := range *configs {
				h := cfg.SubjectRevisionIPC
				require.False(t, controls[h.ControlGeneration], "every attempt needs a fresh controlGeneration")
				controls[h.ControlGeneration] = true
				if m.running == nil || h != (*configs)[len(*configs)-1].SubjectRevisionIPC {
					_, err := os.Stat(filepath.Dir(h.SocketPath))
					require.True(t, os.IsNotExist(err), "unpublished session directory must be removed")
				}
			}
			m.shutdown(time.Second)
			require.Empty(t, o.sessions)
		})
	}
}

func TestFastSandboxSubjectSessionDirectorySafety(t *testing.T) {
	o := subjectTestOwner(t)
	require.NoError(t, os.Chmod(o.cfg.parent, 0o777))
	_, err := newFastSandboxSubjectSession(o.cfg)
	require.ErrorIs(t, err, revision.ErrInvalid)
	require.NoError(t, os.Chmod(o.cfg.parent, 0o700))
	s, err := newFastSandboxSubjectSession(o.cfg)
	require.NoError(t, err)
	original := filepath.Join(o.cfg.parent, s.directory)
	moved := original + "-old"
	require.NoError(t, os.Rename(original, moved))
	require.NoError(t, os.Mkdir(original, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(original, "replacement"), []byte("keep"), 0o600))
	require.ErrorIs(t, s.close(), revision.ErrTransportUnavailable)
	_, err = os.Stat(filepath.Join(original, "replacement"))
	require.NoError(t, err)
}

// The subject drain handoff reuses the sidecar operator setting and parser:
// default, accepted bounds, and EnvIntRange fallback for invalid values.
func TestFastSandboxSubjectSessionDrainTimeoutHandoff(t *testing.T) {
	parent, err := os.MkdirTemp("", "r2-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
	for _, tc := range []struct {
		value     string
		admission bool
		want      int
	}{
		{"", true, constants.DefaultRevisionDrainTimeoutSeconds},
		{"1", true, 1},
		{"300", true, 300},
		{" 45 ", true, 45},
		{"0", true, constants.DefaultRevisionDrainTimeoutSeconds},
		{"301", true, constants.DefaultRevisionDrainTimeoutSeconds},
		{"abc", true, constants.DefaultRevisionDrainTimeoutSeconds},
		{"5", false, 0},
	} {
		t.Setenv(constants.EnvRevisionDrainTimeoutSeconds, tc.value)
		session, err := newFastSandboxSubjectSession(fastSandboxSubjectSessionConfig{parent: parent, uid: os.Getuid(), gid: os.Getgid(), tlsAdmission: tc.admission})
		require.NoError(t, err)
		require.Equal(t, tc.want, session.handoff.DrainTimeoutSeconds, "value %q", tc.value)
		_, err = mitmproxy.WithSubjectRevision(mitmproxy.Config{}, session.handoff)
		require.NoError(t, err)
		require.NoError(t, session.close())
	}
}
