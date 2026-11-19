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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readyWaitServer registers testUID up to runtime-ready with a subject drain
// but no receiver, so its data-plane-ready Hook cannot be acknowledged until
// the test publishes one.
func readyWaitServer(t *testing.T, wait time.Duration) *fastSandboxPolicyServer {
	t.Helper()
	s, _, _ := fastSandboxTestServer(t)
	mutationPersistence(t, s)
	readinessDrain(t, s)
	s.mu.Lock()
	s.subjectDrain.readyWait = wait
	s.mu.Unlock()
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady)).Code)
	return s
}

// startReadyHook sends one data-plane-ready Hook and returns once it is parked
// on the ACK wait, i.e. before it enters the action gate.
func startReadyHook(t *testing.T, s *fastSandboxPolicyServer, uid, r, a string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	out := make(chan *httptest.ResponseRecorder, 1)
	body := hookBody(t, uid, r, a, constants.HookDataPlaneReady) // build outside the goroutine: builders use require
	go func() { out <- doAction(t, s, body) }()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, waiting := s.subjectDrain.ackWaiters[subject.FromSandboxUID(uid)]
		return waiting
	}, 5*time.Second, time.Millisecond, "Hook must park on the ACK wait")
	return out
}

func requireReadyHookResult(t *testing.T, out <-chan *httptest.ResponseRecorder, code int) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-out:
		require.Equal(t, code, rec.Code, rec.Body.String())
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("data-plane-ready Hook did not return")
		return nil
	}
}

// An ACK that lands inside the wait releases the same Hook invocation, so the
// Fastlet never has to retry on its probe tick.
func TestFastSandboxReadyWaitACKWithinBound(t *testing.T) {
	s := readyWaitServer(t, 10*time.Second)
	out := startReadyHook(t, s, testUID, testFenceR, testFenceA)
	h := readinessReceiver(t, s)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
	requireReadyHookResult(t, out, 200)
	state, ok := s.reg.Get(subject.FromSandboxUID(testUID))
	require.True(t, ok)
	require.Equal(t, subject.StateActive, state)
	s.mu.Lock()
	require.Empty(t, s.subjectDrain.ackWaiters, "an ACK releases and forgets its waiters")
	s.mu.Unlock()
}

// The bound expires into the unchanged gate answer.
func TestFastSandboxReadyWaitTimeoutKeeps503(t *testing.T) {
	const wait = 40 * time.Millisecond
	s := readyWaitServer(t, wait)
	started := time.Now()
	rec := doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, 503, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "subject snapshot not acknowledged")
	require.GreaterOrEqual(t, time.Since(started), wait)
	state, _ := s.reg.Get(subject.FromSandboxUID(testUID))
	require.Equal(t, subject.StateDenying, state)
}

// The wait holds no action gate or business lock: another subject's SET,
// Hooks and Vault relay, and the waiting subject's own Vault relay, complete
// while the first Hook is parked.
func TestFastSandboxReadyWaitDoesNotBlockOtherActions(t *testing.T) {
	s := readyWaitServer(t, 10*time.Second)
	out := startReadyHook(t, s, testUID, testFenceR, testFenceA)
	bind := bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())
	runtimeReady := hookBody(t, "other", "r2", "a2", constants.HookRuntimeReady)
	credential := vaultCredential()
	done := make(chan struct{})
	go func() {
		// assert, not require: FailNow must not run off the test goroutine.
		defer close(done)
		assert.Equal(t, 200, doAction(t, s, bind).Code)
		assert.Equal(t, 200, doAction(t, s, runtimeReady).Code)
		assert.Equal(t, 201, doVault(t, s, "POST", "other", credential, "").Code)
		assert.Equal(t, 201, doVault(t, s, "POST", testUID, credential, "").Code)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("actions queued behind a parked data-plane-ready wait")
	}
	select {
	case rec := <-out:
		t.Fatalf("Hook returned before any ACK: %d %s", rec.Code, rec.Body.String())
	default:
	}
	h := readinessReceiver(t, s)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
	requireReadyHookResult(t, out, 200)
	waitReadiness(t, s, "other", "r2", "a2")
}

// A rebind while the Hook waits wakes it; the unchanged fence check under the
// gate then rejects the stale Hook instead of activating the successor.
func TestFastSandboxReadyWaitRebindReturns409(t *testing.T) {
	s := readyWaitServer(t, 10*time.Second)
	out := startReadyHook(t, s, testUID, testFenceR, testFenceA)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "next-r", "next-a", 2, vaultAllowPolicy())).Code)
	rec := requireReadyHookResult(t, out, 409)
	require.Contains(t, rec.Body.String(), "fence mismatch")
	current, ok := s.reg.Fence(subject.FromSandboxUID(testUID))
	require.True(t, ok)
	require.Equal(t, subject.Fencing{RuntimeInstanceID: "next-r", AttachmentID: "next-a"}, current)
}

// Removal while waiting wakes the Hook into the existing unregistered answer.
func TestFastSandboxReadyWaitRemovalWakes(t *testing.T) {
	s := readyWaitServer(t, 10*time.Second)
	out := startReadyHook(t, s, testUID, testFenceR, testFenceA)
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	requireReadyHookResult(t, out, 409)
}

// Caller cancellation ends the wait without consulting the gate.
func TestFastSandboxReadyWaitHonorsContext(t *testing.T) {
	s := readyWaitServer(t, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		s.awaitSubjectReadyACK(ctx, subject.FromSandboxUID(testUID), subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA})
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.subjectDrain.ackWaiters) == 1
	}, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled wait did not return")
	}
}

// States no in-flight install can resolve, and interception mode all (no
// subject drain), return at once and leave no waiter behind.
func TestFastSandboxReadyWaitSkipsUnresolvableStates(t *testing.T) {
	fence := subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}
	subj := subject.FromSandboxUID(testUID)
	quick := func(t *testing.T, s *fastSandboxPolicyServer, f subject.Fencing) {
		t.Helper()
		started := time.Now()
		s.awaitSubjectReadyACK(context.Background(), subj, f)
		require.Less(t, time.Since(started), time.Second)
	}
	t.Run("mode-all", func(t *testing.T) {
		s, _, _ := fastSandboxTestServer(t)
		require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
		require.Nil(t, s.subjectDrain)
		quick(t, s, fence)
		require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)
	})
	t.Run("stale-fence", func(t *testing.T) {
		s := readyWaitServer(t, 10*time.Second)
		quick(t, s, subject.Fencing{RuntimeInstanceID: "old-r", AttachmentID: "old-a"})
	})
	t.Run("snapshot-failure", func(t *testing.T) {
		s := readyWaitServer(t, 10*time.Second)
		s.mu.Lock()
		if s.subjectDrain.snapshotFailures == nil {
			s.subjectDrain.snapshotFailures = make(map[subject.Subject]bool)
		}
		s.subjectDrain.snapshotFailures[subj] = true
		s.mu.Unlock()
		quick(t, s, fence)
	})
	t.Run("unknown-vault", func(t *testing.T) {
		s := readyWaitServer(t, 10*time.Second)
		s.policyCommitMu.Lock()
		s.mu.Lock()
		entry := s.vaults[subj]
		entry.vaultStateUnknown = true
		s.vaults[subj] = entry
		s.mu.Unlock()
		s.policyCommitMu.Unlock()
		quick(t, s, fence)
	})
	t.Run("zero-bound", func(t *testing.T) {
		s := readyWaitServer(t, 0)
		quick(t, s, fence)
	})
}
