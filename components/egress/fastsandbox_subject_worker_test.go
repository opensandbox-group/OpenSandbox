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
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
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
		snapshot, err := transport.Readback(ctx)
		if known {
			return err == nil && snapshot == nil
		}
		return err == revision.ErrTransportRejected
	}, 5*time.Second, 10*time.Millisecond, "receiver known/uninstalled=%v", known)
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

func startLifecycleFaultProxy(t *testing.T, h *mitmproxy.SubjectRevisionIPCConfig) (*lifecycleFaultProxy, string) {
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
	s, reg, _ := fastSandboxTestServer(t)
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	// Start the real addon without attaching the owner: the fault proxy is the
	// only transport given to the production worker.
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	defer o.stop(child)
	h := (*configs)[0].SubjectRevisionIPC
	fault, path := startLifecycleFaultProxy(t, h)
	client, err := revision.NewSubjectLifecycleClient(path, h.SessionToken, h.ControlGeneration)
	require.NoError(t, err)
	s.startSubjectDrain()
	defer func() { s.subjectDrain.cancel(); <-s.subjectDrain.done }()
	s.mu.Lock()
	s.subjectDrain.deadline = 100 * time.Millisecond
	s.mu.Unlock()
	s.publishSubjectClient(client)
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
	require.Equal(t, 2, queued, "Unload and Register must remain queued during Reconcile")
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
	require.Len(t, fault.commands, 3, "lost acknowledgement must not allocate a new sequence or repeat consumed command")
	for i, raw := range fault.commands {
		var command struct {
			Sequence int `json:"lifecycleSequence"`
		}
		require.NoError(t, json.Unmarshal(raw, &command))
		require.Equal(t, i+1, command.Sequence)
	}
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
	s.publishSubjectClient(o.sessions[child].client)
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
	s.publishSubjectClient(o.sessions[child].client)
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
