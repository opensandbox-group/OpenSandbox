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
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func lifecycleTarget(t *testing.T, uid, runtimeID, attachmentID string) revision.SubjectTarget {
	t.Helper()
	target, err := fastSandboxSubjectTarget(uid, subject.Fencing{RuntimeInstanceID: runtimeID, AttachmentID: attachmentID})
	require.NoError(t, err)
	return target
}

func waitSubjectKnown(t *testing.T, h *mitmproxy.SubjectRevisionIPCConfig, target revision.SubjectTarget, known bool) {
	t.Helper()
	transport, err := revision.NewSubjectUnixTransport(h.SocketPath, h.SessionToken, h.ControlGeneration, target, h.MaxSnapshotBytes)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := transport.Readback(ctx)
		if known {
			return err == nil
		}
		return errors.Is(err, revision.ErrTransportRejected)
	}, 5*time.Second, 10*time.Millisecond, "receiver known=%v", known)
}

func waitLifecycleIdle(t *testing.T, s *fastSandboxPolicyServer, h *mitmproxy.SubjectRevisionIPCConfig, sequence int64) {
	t.Helper()
	probe := &subjectIPCChild{socket: h.SocketPath, token: h.SessionToken}
	raw, _ := json.Marshal(map[string]string{"controlGeneration": h.ControlGeneration})
	require.Eventually(t, func() bool {
		_, body := probe.response(t, "/v2/subjects/lifecycle-status", raw, h.SessionToken, false)
		var response struct {
			LastSequence int64 `json:"lastSequence"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		s.mu.Lock()
		queued := len(s.subjectDrain.queue)
		s.mu.Unlock()
		return response.LastSequence == sequence && queued == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestFastSandboxSubjectDrainLifecycleIntegration(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	m := &mitmTransparent{subjectOwner: o, restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := mitmLaunchDependencies{launch: launch, listen: func(context.Context, string, time.Duration) error { return nil }, retry: func(context.Context, <-chan struct{}, time.Duration) bool { t.Fatal("unexpected retry"); return false }}
	require.NoError(t, m.startInitial(ctx, deps, func() error { return nil }))
	o.attach(s, m.running)
	defer m.shutdown(time.Second)
	first := (*configs)[0].SubjectRevisionIPC
	target := lifecycleTarget(t, testUID, testFenceR, testFenceA)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
	waitSubjectKnown(t, first, target, true)
	waitLifecycleIdle(t, s, first, 1)
	// Stale registered fence, pending-rebind fence, and already absent REMOVE
	// each return 200 without allocating a lifecycle sequence.
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, "stale", testFenceA)).Code)
	s.mu.Lock()
	subj := subject.FromSandboxUID(testUID)
	s.rebinds[subj] = &subjectRebind{expectedNew: subject.Fencing{RuntimeInstanceID: "next", AttachmentID: "next"}}
	s.mu.Unlock()
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, "stale", testFenceA)).Code)
	s.mu.Lock()
	delete(s.rebinds, subj)
	s.mu.Unlock()
	require.Equal(t, 200, doAction(t, s, removeBody(t, "absent", testFenceR, testFenceA)).Code)
	waitLifecycleIdle(t, s, first, 1)
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	waitSubjectKnown(t, first, target, false)
	waitLifecycleIdle(t, s, first, 2)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, nil)).Code)
	waitSubjectKnown(t, first, target, true)
	// Same UID, changed fence: exact old owner closes before new registration.
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "r2", "a2", 3, nil)).Code)
	secondTarget := lifecycleTarget(t, testUID, "r2", "a2")
	waitSubjectKnown(t, first, secondTarget, true)
	waitSubjectKnown(t, first, target, false)
	waitLifecycleIdle(t, s, first, 5)
	// Source-IP reuse is dispatch takeover, never generation inheritance.
	require.Equal(t, 200, doAction(t, s, bindBody(t, "successor", testIP, "r3", "a3", 1, nil)).Code)
	successor := lifecycleTarget(t, "successor", "r3", "a3")
	waitSubjectKnown(t, first, successor, true)
	waitSubjectKnown(t, first, secondTarget, false)
	waitLifecycleIdle(t, s, first, 7)
	require.Empty(t, s.subjectLifecycleDegraded())
	o.stop(m.running)
	m.closeRevisionSession(m.currentGen)
	m.restartWithBackoffUsing(ctx, mitmproxy.NewHealthGate(), deps)
	require.Len(t, *configs, 2)
	second := (*configs)[1].SubjectRevisionIPC
	require.NotEqual(t, first.ControlGeneration, second.ControlGeneration)
	// Attach after publication automatically replays only the surviving set.
	waitSubjectKnown(t, second, successor, true)
	waitSubjectKnown(t, second, target, false)
	waitLifecycleIdle(t, s, second, 1)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "later", "10.0.0.9", "r4", "a4", 1, nil)).Code)
	waitSubjectKnown(t, second, lifecycleTarget(t, "later", "r4", "a4"), true)
	waitLifecycleIdle(t, s, second, 2)
}

func TestFastSandboxSubjectDrainAbsentRemoveUsesMirror(t *testing.T) {
	for _, mirrored := range []bool{false, true} {
		name := "no-mirror"
		if mirrored {
			name = "old-mirror"
		}
		t.Run(name, func(t *testing.T) {
			s, reg, _ := fastSandboxTestServer(t)
			s.startSubjectDrain()
			defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
			subj := subject.FromSandboxUID(testUID)
			old := subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}
			next := subject.Fencing{RuntimeInstanceID: "next", AttachmentID: "next-att"}
			s.mu.Lock()
			s.rebinds[subj] = &subjectRebind{old: old, expectedNew: next}
			if mirrored {
				s.subjectDrain.fences[subj] = old
			}
			s.mu.Unlock()
			_, registered := reg.Fence(subj)
			require.False(t, registered)
			require.Equal(t, http.StatusOK, doAction(t, s, removeBody(t, testUID, next.RuntimeInstanceID, next.AttachmentID)).Code)
			s.mu.Lock()
			defer s.mu.Unlock()
			require.NotContains(t, s.subjectDrain.fences, subj, "REMOVE clears the residual drain mirror")
			require.NotContains(t, s.rebinds, subj)
			if mirrored {
				require.Len(t, s.subjectDrain.queue, 1)
				intent := s.subjectDrain.queue[0]
				require.True(t, intent.unload)
				require.Equal(t, old, intent.fence)
				require.Equal(t, lifecycleTarget(t, testUID, testFenceR, testFenceA), intent.target)
			} else {
				require.Empty(t, s.subjectDrain.queue, "never unload an unregistered expectedNew generation")
			}
		})
	}
}

// The first lifecycle preflight succeeds; the purge-time snapshot and its
// status recheck fail. This exercises the error before any file deletion.
type subjectDrainPurgeFailure struct {
	vaultFileStore
	snapshots int
}

func (f *subjectDrainPurgeFailure) Snapshot() (*policy.PolicyFileSnapshot, error) {
	f.snapshots++
	if f.snapshots > 1 {
		return nil, errors.New("injected purge snapshot failure")
	}
	return f.vaultFileStore.Snapshot()
}

func TestFastSandboxSubjectDrainPurgeFailureStillUnloads(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "rebind"
		if reset {
			name = "needs-reset"
		}
		t.Run(name, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			s := h.srv
			o := subjectTestOwner(t)
			launch, configs := subjectAddonLauncher(t, o)
			m := &mitmTransparent{subjectOwner: o, restartCh: make(chan exitEvent, 64), shutdownCh: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deps := mitmLaunchDependencies{launch: launch, listen: func(context.Context, string, time.Duration) error { return nil }, retry: func(context.Context, <-chan struct{}, time.Duration) bool { t.Fatal("unexpected retry"); return false }}
			require.NoError(t, m.startInitial(ctx, deps, func() error { return nil }))
			o.attach(s, m.running)
			defer m.shutdown(time.Second)
			first := (*configs)[0].SubjectRevisionIPC
			require.Equal(t, http.StatusOK, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
			target := lifecycleTarget(t, testUID, testFenceR, testFenceA)
			waitSubjectKnown(t, first, target, true)
			waitLifecycleIdle(t, s, first, 1)
			subj := subject.FromSandboxUID(testUID)
			old, ok := h.reg.Fence(subj)
			require.True(t, ok)
			if reset {
				s.mu.Lock()
				s.rebinds[subj] = &subjectRebind{old: old, expectedNew: subject.Fencing{RuntimeInstanceID: "next", AttachmentID: "next-att"}, oldNetwork: s.subjAtt[subj], newNetwork: s.subjAtt[subj], needsReset: true}
				s.mu.Unlock()
			}
			probe := &subjectDrainPurgeFailure{vaultFileStore: h.fileFor(testUID)}
			h.vp.mu.Lock()
			h.vp.files[subj] = probe
			h.vp.mu.Unlock()
			response := doAction(t, s, bindBody(t, testUID, testIP, "next", "next-att", 2, nil))
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Contains(t, response.Body.String(), "purge")
			require.Equal(t, 3, probe.snapshots, "preflight, failed purge, status recheck")
			_, registered := h.reg.Fence(subj)
			require.Equal(t, !reset, registered, "only needsReset unregisters before purge")
			waitSubjectKnown(t, first, target, false)
			waitLifecycleIdle(t, s, first, 2)
			s.mu.Lock()
			_, mirrored := s.subjectDrain.fences[subj]
			s.mu.Unlock()
			require.False(t, mirrored, "purge failure cannot retain the old replay entry")
			o.stop(m.running)
			m.closeRevisionSession(m.currentGen)
			m.restartWithBackoffUsing(ctx, mitmproxy.NewHealthGate(), deps)
			require.Len(t, *configs, 2)
			second := (*configs)[1].SubjectRevisionIPC
			require.NotEqual(t, first.ControlGeneration, second.ControlGeneration)
			waitSubjectKnown(t, second, target, false)
			waitLifecycleIdle(t, s, second, 0)
			// A new command proves replay did not silently spend a sequence.
			require.Equal(t, http.StatusOK, doAction(t, s, bindBody(t, "later", "10.0.0.9", "r3", "a3", 1, nil)).Code)
			waitSubjectKnown(t, second, lifecycleTarget(t, "later", "r3", "a3"), true)
			waitLifecycleIdle(t, s, second, 1)
		})
	}
}

// A real Unix HTTP fault proxy surrounds the production addon. It loses a
// consumed Register response and blocks lifecycle-status until explicitly
// released; every observed sequence/raw body is retained for exact assertions.
type lifecycleFaultProxy struct {
	mu       sync.Mutex
	commands [][]byte
	paths    []string
	consumed chan struct{}
	release  chan struct{}
	once     sync.Once
	client   *http.Client
}

func startLifecycleFaultProxy(t *testing.T, h *mitmproxy.SubjectRevisionIPCConfig, beforeConsume bool) (*lifecycleFaultProxy, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fl-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "p.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.SocketPath)
	}}
	f := &lifecycleFaultProxy{consumed: make(chan struct{}), release: make(chan struct{}), client: &http.Client{Transport: transport}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "lifecycle-status") {
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		lost := false
		if strings.HasSuffix(r.URL.Path, "register") || strings.HasSuffix(r.URL.Path, "unload") {
			f.mu.Lock()
			f.commands = append(f.commands, append([]byte(nil), raw...))
			lost = len(f.commands) == 1
			f.mu.Unlock()
		}
		if lost && beforeConsume {
			f.once.Do(func() { close(f.consumed) })
			<-r.Context().Done()
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://addon"+r.URL.Path, strings.NewReader(string(raw)))
		if err != nil {
			return
		}
		request.Header = r.Header.Clone()
		response, err := f.client.Do(request)
		if err != nil {
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return
		}
		if lost {
			f.once.Do(func() { close(f.consumed) })
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); transport.CloseIdleConnections() })
	return f, path
}

func TestFastSandboxSubjectDrainSlowIPCAndReconcile(t *testing.T) {
	for _, beforeConsume := range []bool{false, true} {
		name := "consumed-reply-lost"
		if beforeConsume {
			name = "unconsumed-exact-retry"
		}
		t.Run(name, func(t *testing.T) { testSubjectDrainSlowIPC(t, beforeConsume) })
	}
}

func testSubjectDrainSlowIPC(t *testing.T, beforeConsume bool) {
	s, reg, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	// Start the real addon without attaching the owner: the fault proxy is the
	// only transport given to the production worker.
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(child)
	h := (*configs)[0].SubjectRevisionIPC
	fault, path := startLifecycleFaultProxy(t, h, beforeConsume)
	client, err := revision.NewSubjectLifecycleClient(path, h.SessionToken, h.ControlGeneration)
	require.NoError(t, err)
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	s.mu.Lock()
	s.subjectDrain.deadline = 100 * time.Millisecond
	s.mu.Unlock()
	s.publishSubjectClient(client, *h)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	select {
	case <-fault.consumed:
	case <-time.After(5 * time.Second):
		t.Fatal("command not consumed")
	}
	require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) > 0 }, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"subject_ipc_unavailable"}, s.subjectLifecycleDegraded())
	// A deliberately unresolved IPC spans actions for the SAME subject as
	// well as another subject. No gate, stripe, policy barrier or s.mu is held.
	actionsDone := make(chan struct{})
	go func() {
		defer close(actionsDone)
		require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
		require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady)).Code)
		require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)
		require.True(t, s.actionStatusResponse().Ready)
		require.Equal(t, 201, doVault(t, s, http.MethodPost, testUID, vaultCredential(), "").Code)
		require.Equal(t, 200, doPolicy(t, s, http.MethodPut, testUID, policyPutDoc(policyAllow(), 0, false, false), "").Code)
		require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
		setBindingAndReady(t, s, reg, "other", "10.0.0.9", vaultAllowPolicy())
	}()
	select {
	case <-actionsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("actions or relays blocked on lifecycle IPC")
	}
	s.mu.Lock()
	queued := len(s.subjectDrain.queue)
	s.mu.Unlock()
	require.Equal(t, 4, queued, "duplicate SET, Unload, Register and latest install must remain queued during Reconcile")
	fault.mu.Lock()
	require.Len(t, fault.commands, 1)
	fault.mu.Unlock()
	status := doRequest(t, s, http.MethodGet, constants.ActionsStatusPath, "")
	require.Contains(t, status.Body.String(), `"subjectLifecycleDegraded":["subject_ipc_unavailable"]`)
	require.Contains(t, status.Body.String(), `"ready":true`)
	close(fault.release)
	waitSubjectKnown(t, h, lifecycleTarget(t, "other", testFenceR, testFenceA), true)
	waitSubjectKnown(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), false)
	waitLifecycleIdle(t, s, h, 3)
	fault.mu.Lock()
	defer fault.mu.Unlock()
	commands := fault.commands
	if beforeConsume {
		require.Len(t, commands, 4)
		require.Equal(t, commands[0], commands[1], "unconsumed command must retry frozen bytes at the same sequence")
		commands = commands[1:]
	}
	require.Len(t, commands, 3, "lost acknowledgement must not allocate a new sequence or repeat consumed command")
	for i, raw := range commands {
		var command struct {
			Sequence int `json:"lifecycleSequence"`
		}
		require.NoError(t, json.Unmarshal(raw, &command))
		require.Equal(t, i+1, command.Sequence)
	}
	require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) == 0 }, time.Second, 10*time.Millisecond)
}

func TestFastSandboxSubjectDrainStalePendingReportsIPCUnavailable(t *testing.T) {
	for _, beforeConsume := range []bool{false, true} {
		name := "consumed-reply-lost"
		if beforeConsume {
			name = "unconsumed-exact-retry"
		}
		t.Run(name, func(t *testing.T) {
			s, _, _ := fastSandboxTestServer(t)
			o := subjectTestOwner(t)
			launch, configs := subjectAddonLauncher(t, o)
			child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
			require.NoError(t, err)
			defer o.stop(child)
			h := (*configs)[0].SubjectRevisionIPC
			fault, path := startLifecycleFaultProxy(t, h, beforeConsume)
			client, err := revision.NewSubjectLifecycleClient(path, h.SessionToken, h.ControlGeneration)
			require.NoError(t, err)
			s.startSubjectDrain()
			defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
			s.mu.Lock()
			s.subjectDrain.deadline = 500 * time.Millisecond
			s.mu.Unlock()
			s.publishSubjectClient(client, *h)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			select {
			case <-fault.consumed:
			case <-time.After(5 * time.Second):
				t.Fatal("command not consumed")
			}
			// Retire the fence while its Register is still pending: the FIFO is
			// blocked by Reconcile, so the outage must stay observable.
			require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
			require.Eventually(t, func() bool {
				reasons := s.subjectLifecycleDegraded()
				return len(reasons) == 1 && reasons[0] == "subject_ipc_unavailable"
			}, 3*time.Second, 10*time.Millisecond)
			require.True(t, s.actionStatusResponse().Ready)
			close(fault.release)
			waitLifecycleIdle(t, s, h, 2)
			waitSubjectKnown(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), false)
			require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) == 0 }, 3*time.Second, 10*time.Millisecond)
		})
	}
}

func TestFastSandboxSubjectDrainIgnoresZeroFence(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	subj := subject.FromSandboxUID(testUID)
	real := subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subjectDrain.fences[subj] = real
	// A retained rebind token whose registry entry is already gone carries a
	// zero old fence; it must not fabricate an Unload or touch the mirror.
	s.recordSubjectIntentLocked(subj, subject.Fencing{}, true)
	s.recordSubjectIntentLocked(subj, subject.Fencing{}, false)
	require.Empty(t, s.subjectDrain.queue)
	require.Equal(t, real, s.subjectDrain.fences[subj])
}

func TestFastSandboxSubjectDrainShutdownIsNotIPCOutage(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(child)
	h := (*configs)[0].SubjectRevisionIPC
	fault, path := startLifecycleFaultProxy(t, h, true)
	client, err := revision.NewSubjectLifecycleClient(path, h.SessionToken, h.ControlGeneration)
	require.NoError(t, err)
	s.startSubjectDrain()
	// A deadline far longer than this test: only shutdown cancels the
	// in-flight command.
	s.mu.Lock()
	s.subjectDrain.deadline = time.Minute
	s.mu.Unlock()
	s.publishSubjectClient(client, *h)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	select {
	case <-fault.consumed:
	case <-time.After(5 * time.Second):
		t.Fatal("command not consumed")
	}
	s.subjectDrain.cancel()
	<-s.subjectDrain.done
	require.Empty(t, s.subjectLifecycleDegraded())
}

func TestFastSandboxSubjectDrainConfirmedFailures(t *testing.T) {
	// The production receiver confirms capacity exhaustion and conflict through
	// lifecycle-status; these consume a sequence and must not auto-retry.
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	boundedLaunch := func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
		h := *cfg.SubjectRevisionIPC
		h.MaxSubjects = 1
		cfg.SubjectRevisionIPC = &h
		return launch(cfg)
	}
	child, err := o.launch(context.Background(), mitmproxy.Config{}, boundedLaunch)
	require.NoError(t, err)
	defer o.stop(child)
	h := (*configs)[0].SubjectRevisionIPC
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	s.publishSubjectClient(o.sessions[child].client, o.sessions[child].handoff)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
	waitSubjectKnown(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), true)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "overflow", "10.0.0.9", "r2", "a2", 1, nil)).Code)
	require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) == 1 }, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"subject_capacity_exhausted"}, s.subjectLifecycleDegraded())
	waitLifecycleIdle(t, s, h, 2)
	require.True(t, s.actionStatusResponse().Ready)
	// Bounded classifier never forwards arbitrary server or error strings.
	for _, outcome := range []string{"subject_conflict", "subject_failed", "subject_capacity_exhausted"} {
		require.Equal(t, outcome, subjectLifecycleReason(revision.LifecycleReceipt{Result: revision.LifecycleResult{Outcome: outcome}}))
	}
	require.Equal(t, "subject_ipc_unavailable", subjectLifecycleReason(revision.LifecycleReceipt{Result: revision.LifecycleResult{Outcome: "hostname.secret.example"}}))
}

func TestFastSandboxSubjectDrainDisabledAndStaleIntents(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
	require.Nil(t, s.subjectDrain)
	raw, err := json.Marshal(s.actionStatusResponse())
	require.NoError(t, err)
	require.NotContains(t, string(raw), "subjectLifecycle")
	// Enqueue before publishing a client, then replay only the current fence.
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(child)
	s.publishSubjectClient(o.sessions[child].client, o.sessions[child].handoff)
	h := (*configs)[0].SubjectRevisionIPC
	waitSubjectKnown(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), true)
	waitLifecycleIdle(t, s, h, 1)
	// A stale Register cannot revive an old generation; a stale Unload cannot
	// close the currently registered generation, even if deliberately queued.
	s.mu.Lock()
	stale := fastSandboxSubjectIntent{subj: subject.FromSandboxUID(testUID), fence: subject.Fencing{RuntimeInstanceID: "old", AttachmentID: "old"}, target: lifecycleTarget(t, testUID, "old", "old")}
	unload := fastSandboxSubjectIntent{subj: subject.FromSandboxUID(testUID), fence: subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}, target: lifecycleTarget(t, testUID, testFenceR, testFenceA), unload: true}
	s.subjectDrain.queue = append(s.subjectDrain.queue, stale, unload)
	s.subjectDrain.signal()
	s.mu.Unlock()
	waitLifecycleIdle(t, s, h, 1)
}

func TestFastSandboxSubjectDrainReplacePending(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	firstChild, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(firstChild)
	first := (*configs)[0].SubjectRevisionIPC
	fault, path := startLifecycleFaultProxy(t, first, false)
	oldClient, err := revision.NewSubjectLifecycleClient(path, first.SessionToken, first.ControlGeneration)
	require.NoError(t, err)
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	s.publishSubjectClient(oldClient, o.sessions[firstChild].handoff)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
	select {
	case <-fault.consumed:
	case <-time.After(5 * time.Second):
		t.Fatal("old command not consumed")
	}
	// The old send is still blocked. Switching clients cancels it and never
	// waits for its mutex or an acknowledgement before recording replay.
	nextChild, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(nextChild)
	next := (*configs)[1].SubjectRevisionIPC
	s.publishSubjectClient(o.sessions[nextChild].client, o.sessions[nextChild].handoff)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "later", "10.0.0.9", "r2", "a2", 1, nil)).Code)
	waitSubjectKnown(t, next, lifecycleTarget(t, testUID, testFenceR, testFenceA), true)
	waitSubjectKnown(t, next, lifecycleTarget(t, "later", "r2", "a2"), true)
	waitLifecycleIdle(t, s, next, 2)
	require.Empty(t, s.subjectLifecycleDegraded(), "old canceled result cannot degrade new session")
	fault.mu.Lock()
	defer fault.mu.Unlock()
	require.Len(t, fault.commands, 1, "replacement must abandon only the old process command")
}

// Fault-injected Unix peer confirms terminal receiver failures. This tests the
// worker/client boundary; the Python capacity test above exercises a real owner.
func TestFastSandboxSubjectDrainFailureReceiptsAreNotRetried(t *testing.T) {
	for _, outcome := range []string{"subject_conflict", "subject_failed"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, _ := fastSandboxTestServer(t)
			o := subjectTestOwner(t)
			path := filepath.Join(o.cfg.parent, "peer.sock")
			listener, err := net.Listen("unix", path)
			require.NoError(t, err)
			token, err := revision.NewSessionToken()
			require.NoError(t, err)
			var mu sync.Mutex
			var targets []revision.SubjectTarget
			var receipt revision.LifecycleReceipt
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(r.URL.Path, "/v2/revisions/") {
					var command struct {
						Revision revision.Identity `json:"revision"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&command))
					require.NoError(t, json.NewEncoder(w).Encode(command))
					return
				}
				require.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
				if strings.HasSuffix(r.URL.Path, "lifecycle-status") {
					raw, _ := json.Marshal(receipt)
					_, _ = w.Write([]byte(strings.Replace(string(raw), `"lifecycleSequence":`, `"lastSequence":`, 1)))
					return
				}
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				var command struct {
					Sequence int64                  `json:"lifecycleSequence"`
					Target   revision.SubjectTarget `json:"target"`
				}
				require.NoError(t, json.Unmarshal(raw, &command))
				targets = append(targets, command.Target)
				digest := sha256.Sum256(append([]byte("opensandbox.subject-lifecycle.v2\x00register\x00"), raw...))
				result := revision.LifecycleResult{HTTPStatus: 200, Outcome: "registered"}
				if len(targets) == 1 {
					result.HTTPStatus = 503
					result.Outcome = outcome
					if outcome == "subject_conflict" {
						result.HTTPStatus = 409
					}
				}
				receipt = revision.LifecycleReceipt{LifecycleSequence: command.Sequence, Operation: "register", CommandDigest: hex.EncodeToString(digest[:]), Result: result}
				w.WriteHeader(result.HTTPStatus)
				require.NoError(t, json.NewEncoder(w).Encode(receipt))
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			client, err := revision.NewSubjectLifecycleClient(path, token, "control-test")
			require.NoError(t, err)
			s.startSubjectDrain()
			defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
			s.publishSubjectClient(client, mitmproxy.SubjectRevisionIPCConfig{SocketPath: path, SessionToken: token, ControlGeneration: "control-test", MaxSnapshotBytes: defaultRevisionMaxSnapshotSize})
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
			require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) == 1 }, time.Second, 10*time.Millisecond)
			require.Equal(t, []string{outcome}, s.subjectLifecycleDegraded())
			require.Equal(t, 200, doAction(t, s, bindBody(t, "next", "10.0.0.9", "r2", "a2", 1, nil)).Code)
			require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return receipt.LifecycleSequence == 2 }, time.Second, 10*time.Millisecond)
			require.True(t, s.actionStatusResponse().Ready)
			mu.Lock()
			require.Len(t, targets, 2)
			require.Equal(t, "s-next", targets[1].SubjectKey, "next intent must run after a confirmed failure")
			mu.Unlock()
		})
	}
}

// Every snapshot test uses the production addon and actual Unix HTTP socket.
// The receiver validates canonical bytes/digest on Prepare before publishing.
func snapshotDrainHarness(t *testing.T) (*fastSandboxPolicyServer, *subject.MemoryRegistry, *fastSandboxSubjectLaunchOwner, func(mitmproxy.Config) (*mitmproxy.Running, error), *[]mitmproxy.Config) {
	t.Helper()
	s, reg, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	o.attach(s, child)
	t.Cleanup(o.stopDrain)
	return s, reg, o, launch, configs
}

func latestSubjectSnapshot(t *testing.T, s *fastSandboxPolicyServer, uid string) *fastSandboxSubjectSnapshot {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	frozen := s.subjectDrain.snapshots[subject.FromSandboxUID(uid)]
	require.NotNil(t, frozen)
	return frozen // immutable after publication
}

func waitSubjectInstalled(t *testing.T, h *mitmproxy.SubjectRevisionIPCConfig, target revision.SubjectTarget, frozen *fastSandboxSubjectSnapshot) revision.Identity {
	t.Helper()
	transport, err := revision.NewSubjectUnixTransport(h.SocketPath, h.SessionToken, h.ControlGeneration, target, h.MaxSnapshotBytes)
	require.NoError(t, err)
	digest := sha256.Sum256(frozen.payload)
	var active *revision.Identity
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		active, err = transport.Readback(ctx)
		return err == nil && active != nil && active.Digest == hex.EncodeToString(digest[:]) && active.VaultRevision == frozen.vaultRevision && active.PolicyEpoch == frozen.policyEpoch
	}, 5*time.Second, 10*time.Millisecond, "exact receiver active identity")
	return *active
}

func TestFastSandboxSubjectSnapshotBindingAndReplay(t *testing.T) {
	s, _, o, launch, configs := snapshotDrainHarness(t)
	h := (*configs)[0].SubjectRevisionIPC
	target := lifecycleTarget(t, testUID, testFenceR, testFenceA)
	set := func(uid, ip, r, a string, generation uint64) {
		require.Equal(t, 200, doAction(t, s, bindBody(t, uid, ip, r, a, generation, vaultAllowPolicy())).Code)
	}
	set(testUID, testIP, testFenceR, testFenceA, 1)
	empty := latestSubjectSnapshot(t, s, testUID)
	require.Contains(t, string(empty.payload), `"state":"active-empty"`)
	require.Equal(t, int64(0), empty.vaultRevision)
	require.Equal(t, int64(1), empty.policyEpoch, "binding policy committed before freezing")
	first := waitSubjectInstalled(t, h, target, empty)
	require.Equal(t, int64(1), first.DecisionEpoch)
	require.Equal(t, 201, doVault(t, s, http.MethodPost, testUID, vaultCredential(), "").Code)
	set(testUID, testIP, testFenceR, testFenceA, 2)
	bound := latestSubjectSnapshot(t, s, testUID)
	require.Contains(t, string(bound.payload), `"state":"active"`)
	require.Equal(t, int64(1), bound.vaultRevision)
	second := waitSubjectInstalled(t, h, target, bound)
	require.Greater(t, second.DecisionEpoch, first.DecisionEpoch)
	// The relay itself is a B2a-2 producer; SET is the only trigger here.
	require.Equal(t, 200, doPolicy(t, s, http.MethodPut, testUID, policyPutDoc(policyAllow(), 0, false, false), "").Code)
	set(testUID, testIP, testFenceR, testFenceA, 3)
	newPolicy := latestSubjectSnapshot(t, s, testUID)
	require.Greater(t, newPolicy.policyEpoch, bound.policyEpoch)
	third := waitSubjectInstalled(t, h, target, newPolicy)
	require.Greater(t, third.DecisionEpoch, second.DecisionEpoch)
	set("other", "10.0.0.9", "r2", "a2", 1)
	otherTarget := lifecycleTarget(t, "other", "r2", "a2")
	other := latestSubjectSnapshot(t, s, "other")
	otherActive := waitSubjectInstalled(t, h, otherTarget, other)
	require.NotEqual(t, third.Digest, otherActive.Digest, "subjects cannot inherit another target's bindings")
	// A deleted Vault freezes revision zero and empty bindings on the next SET.
	require.Equal(t, 201, doVault(t, s, http.MethodPost, "other", vaultCredential(), "").Code)
	require.Equal(t, 204, doVault(t, s, http.MethodDelete, "other", "", "").Code)
	set("other", "10.0.0.9", "r2", "a2", 2)
	other = latestSubjectSnapshot(t, s, "other")
	require.Equal(t, int64(0), other.vaultRevision)
	waitSubjectInstalled(t, h, otherTarget, other)
	// Publish a fresh production child through the same owner seam as restart.
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	o.published(child)
	next := (*configs)[1].SubjectRevisionIPC
	replayed := waitSubjectInstalled(t, next, target, newPolicy)
	require.NotEqual(t, third.ControlGeneration, replayed.ControlGeneration)
	require.Equal(t, int64(1), replayed.DecisionEpoch)
	waitSubjectInstalled(t, next, otherTarget, other)
	set(testUID, testIP, "new-r", "new-a", 4)
	newTarget := lifecycleTarget(t, testUID, "new-r", "new-a")
	rebound := waitSubjectInstalled(t, next, newTarget, latestSubjectSnapshot(t, s, testUID))
	require.Greater(t, rebound.DecisionEpoch, replayed.DecisionEpoch, "subject epochs stay monotonic across generations in one child")
	waitSubjectKnown(t, next, target, false)
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, "new-r", "new-a")).Code)
	waitSubjectKnown(t, next, newTarget, false)
	s.mu.Lock()
	require.Nil(t, s.subjectDrain.snapshots[subject.FromSandboxUID(testUID)])
	for _, intent := range s.subjectDrain.queue {
		require.NotEqual(t, newTarget, intent.target)
	}
	s.mu.Unlock()
	require.Empty(t, s.subjectLifecycleDegraded())
}

// A bounded Unix fault proxy records the exact protocol order. It can lose an
// applied Commit reply and hold Readback, or inject a confirmed revision error;
// lifecycle calls still reach the real production addon.
type snapshotWireEvent struct {
	Path     string
	Target   revision.SubjectTarget `json:"target"`
	Revision revision.Identity      `json:"revision"`
	Payload  []byte                 `json:"payload"`
}
type snapshotFault struct {
	mu               sync.Mutex
	events           []snapshotWireEvent
	once             sync.Once
	reached, release chan struct{}
}

func startSnapshotFault(t *testing.T, h *mitmproxy.SubjectRevisionIPCConfig, outcome, failOperation string) (*snapshotFault, mitmproxy.SubjectRevisionIPCConfig) {
	t.Helper()
	dir, err := os.MkdirTemp("", "si-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "p.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.SocketPath)
	}}
	client := &http.Client{Transport: transport}
	fault := &snapshotFault{reached: make(chan struct{}), release: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		var event snapshotWireEvent
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		event.Path = r.URL.Path
		fault.mu.Lock()
		fault.events = append(fault.events, event)
		fault.mu.Unlock()
		isTarget := event.Target.SubjectKey == "s-"+testUID
		if isTarget && strings.HasSuffix(r.URL.Path, "/"+failOperation) && outcome != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": outcome})
			fault.once.Do(func() { close(fault.reached) })
			return
		}
		if outcome == "" && isTarget && strings.HasSuffix(r.URL.Path, "/active") {
			select {
			case <-fault.release:
			case <-r.Context().Done():
				return
			}
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://addon"+r.URL.Path, strings.NewReader(string(raw)))
		if err != nil {
			return
		}
		request.Header = r.Header.Clone()
		response, err := client.Do(request)
		if err != nil {
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return
		}
		lose := false
		if (outcome == "" || failOperation == "active") && isTarget && strings.HasSuffix(r.URL.Path, "/commit") {
			fault.once.Do(func() { lose = true; close(fault.reached) })
		}
		if lose {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); transport.CloseIdleConnections() })
	proxy := *h
	proxy.SocketPath = path
	return fault, proxy
}

func snapshotFaultHarness(t *testing.T, outcome, operation string) (*fastSandboxPolicyServer, *mitmproxy.SubjectRevisionIPCConfig, *snapshotFault) {
	t.Helper()
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	_ = child
	h := (*configs)[0].SubjectRevisionIPC
	fault, proxy := startSnapshotFault(t, h, outcome, operation)
	client, err := revision.NewSubjectLifecycleClient(proxy.SocketPath, proxy.SessionToken, proxy.ControlGeneration)
	require.NoError(t, err)
	s.startSubjectDrain()
	s.mu.Lock()
	s.subjectDrain.deadline = 100 * time.Millisecond
	s.mu.Unlock()
	s.publishSubjectClient(client, proxy)
	t.Cleanup(func() { s.subjectDrain.cancel(); <-s.subjectDrain.done })
	return s, h, fault
}

func TestFastSandboxSubjectSnapshotReconcileCoalesceAndSlowIPC(t *testing.T) {
	s, h, fault := snapshotFaultHarness(t, "", "")
	set := func(uid, ip, r, a string, generation uint64) {
		require.Equal(t, 200, doAction(t, s, bindBody(t, uid, ip, r, a, generation, vaultAllowPolicy())).Code)
	}
	set(testUID, testIP, testFenceR, testFenceA, 1)
	select {
	case <-fault.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("Commit not reached")
	}
	require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) > 0 }, time.Second, 10*time.Millisecond)
	first := latestSubjectSnapshot(t, s, testUID)
	// All public work must finish while the unresolved revision owns the FIFO,
	// including same-subject hooks/relays and a second subject's actions.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, hook := range []string{constants.HookRuntimeReady, constants.HookDataPlaneReady} {
			require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, hook)).Code)
		}
		require.Equal(t, 201, doVault(t, s, http.MethodPost, testUID, vaultCredential(), "").Code)
		for i := uint64(2); i <= 4; i++ {
			require.Equal(t, 200, doPolicy(t, s, http.MethodPut, testUID, policyPutDoc(policyAllow(), 0, false, false), "").Code)
			set(testUID, testIP, testFenceR, testFenceA, i)
		}
		set("other", "10.0.0.9", "r2", "a2", 1)
		set("retired", "10.0.0.8", "old", "old", 1)
		set("retired", "10.0.0.8", "new", "new", 2)
		require.Equal(t, 200, doAction(t, s, removeBody(t, "other", "r2", "a2")).Code)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("action or relay blocked on revision IPC")
	}
	require.True(t, s.actionStatusResponse().Ready)
	latest := latestSubjectSnapshot(t, s, testUID)
	require.NotEqual(t, string(first.payload), string(latest.payload))
	s.mu.Lock()
	var installs []fastSandboxSubjectIntent
	for _, intent := range s.subjectDrain.queue {
		if intent.snapshot != nil {
			installs = append(installs, intent)
		}
	}
	require.Len(t, installs, 2, "only latest same-generation and new-generation payloads survive")
	require.Equal(t, latest, installs[0].snapshot)
	require.Equal(t, lifecycleTarget(t, "retired", "new", "new"), installs[1].target)
	s.mu.Unlock()
	close(fault.release)
	active := waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latest)
	require.Equal(t, int64(2), active.DecisionEpoch, "Readback confirms the original attempt without allocating another epoch")
	waitSubjectInstalled(t, h, lifecycleTarget(t, "retired", "new", "new"), latestSubjectSnapshot(t, s, "retired"))
	waitLifecycleIdle(t, s, h, 7)
	fault.mu.Lock()
	defer fault.mu.Unlock()
	prepares, commits, readbacks := 0, 0, 0
	registered := make(map[revision.SubjectTarget]bool)
	for _, event := range fault.events {
		if strings.HasSuffix(event.Path, "/register") {
			registered[event.Target] = true
		}
		if strings.HasSuffix(event.Path, "/unload") {
			delete(registered, event.Target)
		}
		if strings.HasSuffix(event.Path, "/prepare") {
			require.True(t, registered[event.Target], "every Prepare follows the exact generation's Register")
			require.NotEqual(t, "s-other", event.Target.SubjectKey, "REMOVE drops unsent install")
			require.NotEqual(t, lifecycleTarget(t, "retired", "old", "old"), event.Target)
		}
		if event.Target.SubjectKey != "s-"+testUID {
			continue
		}
		switch event.Path {
		case "/v2/revisions/prepare":
			prepares++
		case "/v2/revisions/commit":
			commits++
		case "/v2/revisions/active":
			readbacks++
		}
	}
	require.Equal(t, 2, prepares, "one original and one coalesced payload")
	require.Equal(t, 2, commits, "consumed Commit must never be retried after exact readback")
	require.Positive(t, readbacks)
}

func TestFastSandboxSubjectSnapshotConfirmedRejectionsContinueFIFO(t *testing.T) {
	for _, operation := range []string{"prepare", "commit", "active"} {
		for _, outcome := range []string{"subject_unknown", "subject_generation_mismatch", "subject_failed", "control_generation_mismatch", "revision_rejected", "not-a-public-reason"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				s, h, fault := snapshotFaultHarness(t, outcome, operation)
				require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
				select {
				case <-fault.reached:
				case <-time.After(5 * time.Second):
					t.Fatal("rejection not reached")
				}
				require.Equal(t, 200, doAction(t, s, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
				waitSubjectInstalled(t, h, lifecycleTarget(t, "other", "r2", "a2"), latestSubjectSnapshot(t, s, "other"))
				reason := outcome
				if outcome == "revision_rejected" {
					reason = "subject_prepare_rejected"
				}
				if outcome == "not-a-public-reason" {
					reason = "subject_install_rejected"
				}
				require.Equal(t, []string{reason}, s.subjectLifecycleDegraded())
				require.True(t, s.actionStatusResponse().Ready)
				fault.mu.Lock()
				defer fault.mu.Unlock()
				for _, event := range fault.events {
					if event.Target.SubjectKey == "s-"+testUID {
						require.NotEqual(t, "/v2/revisions/abort", event.Path, "confirmed failures do not send Abort")
						if operation != "active" {
							require.NotEqual(t, "/v2/revisions/active", event.Path)
						}
					}
				}
			})
		}
	}
}

func TestFastSandboxSubjectSnapshotCoalesceKeysAndDiscard(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	s.startSubjectDrain() // no child: keep all intents queued for exact order checks
	t.Cleanup(func() { s.subjectDrain.cancel(); <-s.subjectDrain.done })
	subj := subject.FromSandboxUID(testUID)
	oldFence := subject.Fencing{RuntimeInstanceID: "old", AttachmentID: "a"}
	newFence := subject.Fencing{RuntimeInstanceID: "new", AttachmentID: "a"}
	oldEpoch, newEpoch := &fastSandboxSubjectEpoch{}, &fastSandboxSubjectEpoch{}
	old := fastSandboxSubjectIntent{subj: subj, fence: oldFence, target: mustSubjectTarget(subj, oldFence), epoch: oldEpoch, snapshot: &fastSandboxSubjectSnapshot{payload: []byte("old")}}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.subjectDrain
	marker := fastSandboxSubjectIntent{subj: "s-other"}
	d.queue = []fastSandboxSubjectIntent{old, marker}
	same := old
	same.snapshot = &fastSandboxSubjectSnapshot{payload: []byte("latest")}
	d.enqueueInstallLocked(same)
	require.Equal(t, []fastSandboxSubjectIntent{same, marker}, d.queue)
	changedEpoch := same
	changedEpoch.epoch = newEpoch
	d.enqueueInstallLocked(changedEpoch)
	require.Equal(t, []fastSandboxSubjectIntent{marker, changedEpoch}, d.queue, "epoch change appends, never substitutes in place")
	d.fences[subj] = oldFence
	d.snapshots[subj] = same.snapshot
	s.recordSubjectIntentLocked(subj, newFence, false)
	next := old
	next.fence, next.target, next.epoch = newFence, mustSubjectTarget(subj, newFence), newEpoch
	d.enqueueInstallLocked(next)
	require.Equal(t, []fastSandboxSubjectIntent{marker, {subj: subj, fence: newFence, target: next.target}, next}, d.queue)
	s.recordSubjectIntentLocked(subj, newFence, true)
	for _, intent := range d.queue {
		require.Nil(t, intent.snapshot, "Unload drops all generation installs")
	}
	require.Nil(t, d.snapshots[subj])
}

func TestFastSandboxSubjectSnapshotOversizeDoesNotChangeSET(t *testing.T) {
	s, _, _, _, configs := snapshotDrainHarness(t)
	h := (*configs)[0].SubjectRevisionIPC
	body := bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())
	before := doAction(t, s, body)
	require.Equal(t, 200, before.Code)
	initial := latestSubjectSnapshot(t, s, testUID)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), initial)
	// A valid committed render can exceed the wire cap because its value is
	// present in both rendered headers and redactions. Build it through the
	// Store candidate API, avoiding HTTP body-size admission in this fixture.
	var request credentialvault.CreateRequest
	require.NoError(t, json.Unmarshal([]byte(strings.Replace(vaultCredential(), "sekret", strings.Repeat("x", 5<<20), 1)), &request))
	store := credentialvault.NewStore(nil, nil)
	pol, err := policy.ParsePolicy(*vaultAllowPolicy())
	require.NoError(t, err)
	candidate, err := store.PrepareCreate(request, pol)
	require.NoError(t, err)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = store.CommitCandidate(candidate)
	require.NoError(t, err)
	s.mu.Lock()
	s.vaults[subject.FromSandboxUID(testUID)] = vaultEntry{store: store, fence: subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}}
	s.mu.Unlock()
	after := doAction(t, s, body)
	require.Equal(t, before.Code, after.Code)
	require.Equal(t, before.Body.String(), after.Body.String())
	require.Equal(t, []string{"subject_snapshot_too_large"}, s.subjectLifecycleDegraded())
	require.True(t, s.actionStatusResponse().Ready)
	require.Same(t, initial, latestSubjectSnapshot(t, s, testUID), "failed construction must not replace replay payload")
}

func TestFastSandboxSubjectSnapshotFlagDifferential(t *testing.T) {
	baseline, _, _ := fastSandboxTestServer(t)
	candidate, _, _, _, _ := snapshotDrainHarness(t)
	type observation struct {
		code  int
		body  string
		ready bool
	}
	exercise := func(s *fastSandboxPolicyServer) []observation {
		var observations []observation
		bodies := []string{
			bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy()),
			hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady),
			hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady),
			bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy()),
			removeBody(t, testUID, testFenceR, testFenceA),
		}
		for _, body := range bodies {
			response := doAction(t, s, body)
			observations = append(observations, observation{response.Code, response.Body.String(), s.actionStatusResponse().Ready})
		}
		return observations
	}
	require.Equal(t, exercise(baseline), exercise(candidate))
	require.Nil(t, baseline.subjectDrain, "disabled path does not allocate intents or caches")
	raw, err := json.Marshal(baseline.actionStatusResponse())
	require.NoError(t, err)
	require.NotContains(t, string(raw), "subjectLifecycle")
}

func TestFastSandboxSubjectSnapshotRegisterFailureDropsInstall(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, func(cfg mitmproxy.Config) (*mitmproxy.Running, error) {
		h := *cfg.SubjectRevisionIPC
		h.MaxSubjects = 1
		cfg.SubjectRevisionIPC = &h
		return launch(cfg)
	})
	require.NoError(t, err)
	o.attach(s, child)
	t.Cleanup(o.stopDrain)
	h := (*configs)[0].SubjectRevisionIPC
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
	require.Equal(t, 200, doAction(t, s, bindBody(t, "overflow", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	waitLifecycleIdle(t, s, h, 2)
	require.Equal(t, []string{"subject_capacity_exhausted"}, s.subjectLifecycleDegraded())
	waitSubjectKnown(t, h, lifecycleTarget(t, "overflow", "r2", "a2"), false)
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "later", "10.0.0.8", "r3", "a3", 1, vaultAllowPolicy())).Code)
	waitSubjectInstalled(t, h, lifecycleTarget(t, "later", "r3", "a3"), latestSubjectSnapshot(t, s, "later"))
	require.True(t, s.actionStatusResponse().Ready)
}

func TestFastSandboxSubjectSnapshotReplacementCancelsPending(t *testing.T) {
	s, _, fault := snapshotFaultHarness(t, "", "")
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	select {
	case <-fault.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("Commit not reached")
	}
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	s.publishSubjectClient(o.sessions[child].client, o.sessions[child].handoff)
	h := (*configs)[0].SubjectRevisionIPC
	active := waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
	require.Equal(t, int64(1), active.DecisionEpoch)
	require.Empty(t, s.subjectLifecycleDegraded(), "canceled old child completion cannot degrade the replacement")
}

func TestFastSandboxSubjectSnapshotMirrorBudget(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	s.startSubjectDrain()
	t.Cleanup(func() { s.subjectDrain.cancel(); <-s.subjectDrain.done })
	s.mu.Lock()
	for i := 0; i < fastSandboxRevisionMaxSubjects; i++ {
		s.subjectDrain.snapshots[subject.Subject("s-budget-"+strconv.Itoa(i))] = &fastSandboxSubjectSnapshot{}
	}
	s.mu.Unlock()
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, []string{"subject_capacity_exhausted"}, s.subjectLifecycleDegraded())
	s.mu.Lock()
	require.Len(t, s.subjectDrain.snapshots, fastSandboxRevisionMaxSubjects)
	require.Nil(t, s.subjectDrain.snapshots[subject.FromSandboxUID(testUID)])
	s.mu.Unlock()
	require.True(t, s.actionStatusResponse().Ready)
}
