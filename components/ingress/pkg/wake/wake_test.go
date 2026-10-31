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

package wake

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/stretchr/testify/require"
)

var wakeTarget = sandbox.EndpointTarget{RouteKind: sandbox.RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "sb-1", Port: 8080}

type probeCall struct {
	probe sandbox.SandboxProbe
	err   error
}

// scriptedLifecycle scripts ProbeSandbox responses in order (the last one
// repeats) and records every ResumeSandbox call.
type scriptedLifecycle struct {
	mu          sync.Mutex
	probes      []probeCall
	probeCalls  int
	resumeOut   []sandbox.ResumeOutcome
	resumeErrs  []error
	resumeCalls int
	resumeIDs   []string
	lastReqID   string
	lastCkpt    string
	resumeHook  func() // optional; runs inside ResumeSandbox under lock
	probeHook   func() // optional; runs after each ProbeSandbox, outside the lock
}

func (s *scriptedLifecycle) ProbeSandbox(context.Context, sandbox.EndpointTarget) (sandbox.SandboxProbe, error) {
	s.mu.Lock()
	call := s.probeCalls
	s.probeCalls++
	if call >= len(s.probes) {
		call = len(s.probes) - 1
	}
	probe, probeErr := s.probes[call].probe, s.probes[call].err
	s.mu.Unlock()
	if s.probeHook != nil {
		s.probeHook()
	}
	return probe, probeErr
}

func (s *scriptedLifecycle) ResumeSandbox(_ context.Context, _ sandbox.EndpointTarget, checkpointID, requestID string) (sandbox.ResumeOutcome, error) {
	s.mu.Lock()
	s.resumeCalls++
	s.resumeIDs = append(s.resumeIDs, requestID)
	s.lastReqID = requestID
	s.lastCkpt = checkpointID
	call := s.resumeCalls - 1
	if call >= len(s.resumeOut) {
		call = len(s.resumeOut) - 1
	}
	outcome, err := s.resumeOut[call], s.resumeErrs[call]
	s.mu.Unlock()
	if s.resumeHook != nil {
		// Runs outside the lock so concurrent probes can proceed.
		s.resumeHook()
	}
	return outcome, err
}

func (s *scriptedLifecycle) snapshot() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumeCalls, s.lastCkpt
}

func pausedProbe(checkpoint string) probeCall {
	return probeCall{probe: sandbox.SandboxProbe{
		Phase:        sandbox.SandboxPhaseWakeable,
		RuntimeState: 10, // RUNTIME_STATE_PAUSED
		CheckpointID: checkpoint,
	}}
}

func readyProbe() probeCall {
	return probeCall{probe: sandbox.SandboxProbe{Phase: sandbox.SandboxPhaseReady}}
}

func newTestWaker(t *testing.T, cfg Config, lifecycle *scriptedLifecycle) (*Waker, *scriptedLifecycle) {
	t.Helper()
	// Tests shrink the parking lot; keep the permit pool inside it.
	if cfg.RestoreMax <= 0 {
		cfg.RestoreMax = cfg.ParkMax
	}
	return newTestWakerWithPolicy(t, cfg, lifecycle, AllowAll())
}

func newTestWakerWithPolicy(t *testing.T, cfg Config, lifecycle *scriptedLifecycle, policy PolicySource) (*Waker, *scriptedLifecycle) {
	t.Helper()
	if cfg.RestoreMax <= 0 {
		cfg.RestoreMax = cfg.ParkMax
	}
	waker, err := NewWaker(cfg, lifecycle, nil, policy)
	require.NoError(t, err)
	return waker, lifecycle
}

func TestWakeServesImmediatelyWhenSandboxReady(t *testing.T) {
	lifecycle := &scriptedLifecycle{probes: []probeCall{readyProbe()}}
	waker, scripted := newTestWaker(t, Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, _ := scripted.snapshot()
	require.Zero(t, resumeCalls)
}

func TestWakeFailsFastOnTerminalSandbox(t *testing.T) {
	lifecycle := &scriptedLifecycle{probes: []probeCall{{probe: sandbox.SandboxProbe{Phase: sandbox.SandboxPhaseTerminal}}}}
	waker, _ := newTestWaker(t, Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	err := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, err, sandbox.ErrSandboxNotFound)
}

func TestWakeKeepsNotReadyBehavior(t *testing.T) {
	lifecycle := &scriptedLifecycle{probes: []probeCall{{probe: sandbox.SandboxProbe{Phase: sandbox.SandboxPhaseNotReady}}}}
	waker, _ := newTestWaker(t, Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	err := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, err, sandbox.ErrSandboxNotReady)
}

// countingPolicy records every lookup and answers from a set.
type countingPolicy struct {
	mu      sync.Mutex
	allowed map[string]bool
	lookups []string
}

func (p *countingPolicy) WakeAllowed(_ context.Context, namespace, sandboxID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lookups = append(p.lookups, namespace+"/"+sandboxID)
	return p.allowed[namespace+"/"+sandboxID]
}

func (p *countingPolicy) lookupCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.lookups)
}

func TestWakeDeniesSandboxWithoutWakePolicy(t *testing.T) {
	lifecycle := &scriptedLifecycle{probes: []probeCall{pausedProbe("ckpt-1")}}
	policy := &countingPolicy{allowed: map[string]bool{}}
	waker, scripted := newTestWakerWithPolicy(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle, policy)

	err := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, err, sandbox.ErrSandboxNotReady)
	resumeCalls, _ := scripted.snapshot()
	require.Zero(t, resumeCalls)
	// Denied before admission: no slot, no permit consumed.
	require.Empty(t, waker.lot.slots)
	require.Empty(t, waker.permits)
}

func TestWakePolicyConsultedOnlyForWakeable(t *testing.T) {
	policy := &countingPolicy{allowed: map[string]bool{"tenant-a/sb-1": true}}
	lifecycle := &scriptedLifecycle{probes: []probeCall{readyProbe()}}
	waker, _ := newTestWakerWithPolicy(t, Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle, policy)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	require.Zero(t, policy.lookupCount()) // Ready needs no gate

	lifecycle.mu.Lock()
	lifecycle.probes = []probeCall{{probe: sandbox.SandboxProbe{Phase: sandbox.SandboxPhaseTerminal}}}
	lifecycle.mu.Unlock()
	require.ErrorIs(t, waker.Wake(context.Background(), wakeTarget), sandbox.ErrSandboxNotFound)
	require.Zero(t, policy.lookupCount()) // terminal fails fast before the gate

	// Repeated probes clamp to the last entry; place the paused entry at
	// the index the next probe lands on.
	lifecycle.mu.Lock()
	lifecycle.probes = []probeCall{readyProbe(), {probe: sandbox.SandboxProbe{Phase: sandbox.SandboxPhaseTerminal}}, pausedProbe("ckpt-1"), readyProbe()}
	lifecycle.resumeOut = []sandbox.ResumeOutcome{sandbox.ResumeAccepted}
	lifecycle.resumeErrs = []error{nil}
	lifecycle.mu.Unlock()
	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	require.Equal(t, 1, policy.lookupCount()) // exactly one lookup per wakeable wake
}

func TestWakeParksUntilReadyAndResumesWithCheckpointFence(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 2 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, checkpoint := scripted.snapshot()
	require.Equal(t, 1, resumeCalls)
	require.Equal(t, "ckpt-1", checkpoint)
	scripted.mu.Lock()
	require.Regexp(t, `^wake-tenant-a-sb-1-[0-9a-f]{16}-1-1$`, scripted.lastReqID)
	scripted.mu.Unlock()
}

func TestWakeSingleflightCollapsesConcurrentRequests(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 64, RetryInterval: time.Millisecond}, lifecycle)

	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = waker.Wake(context.Background(), wakeTarget)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		require.NoError(t, errs[i])
	}
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 1, resumeCalls)
}

func TestWakeBudgetExhaustionShedsWithoutCancelingResume(t *testing.T) {
	// The sandbox stays paused for the whole budget; the flight keeps
	// polling and is never canceled, then a later request finds it Ready.
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1")},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: 150 * time.Millisecond, ParkMax: 8, RetryInterval: 10 * time.Millisecond}, lifecycle)

	err := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, err, ErrBudgetExhausted)
	// Held to flight terminal, not to budget exhaustion.
	require.Equal(t, 1, len(waker.permits))

	// The restore converges in the background; the next request is served
	// on the fast path without a new resume.
	lifecycle.mu.Lock()
	lifecycle.probes = []probeCall{readyProbe()}
	lifecycle.mu.Unlock()
	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
}

func TestWakeParkingLotShedsBeforeResume(t *testing.T) {
	ownerReleased := make(chan struct{})
	lifecycle := &scriptedLifecycle{
		// owner detection, shed request detection, owner readiness poll.
		probes:     []probeCall{pausedProbe("ckpt-1"), pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	// Owner's resume blocks until the shed assertion ran.
	lifecycle.resumeHook = func() {
		<-ownerReleased
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 1, RetryInterval: time.Millisecond}, lifecycle)

	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- waker.Wake(context.Background(), wakeTarget)
	}()
	// Wait until the owner holds the only parking slot.
	time.Sleep(50 * time.Millisecond)

	shedErr := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, shedErr, ErrParkingLotFull)
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 1, resumeCalls) // only the owner's flight; the shed request never resumed

	close(ownerReleased)
	select {
	case err := <-ownerErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("owner wake did not finish")
	}
}

func TestWakeJoinerShedsWhenLotFull(t *testing.T) {
	ownerReleased := make(chan struct{})
	lifecycle := &scriptedLifecycle{
		// owner detection, joiner detection, owner readiness poll.
		probes:     []probeCall{pausedProbe("ckpt-1"), pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() {
		<-ownerReleased
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 1, RetryInterval: time.Millisecond}, lifecycle)

	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- waker.Wake(context.Background(), wakeTarget)
	}()
	time.Sleep(50 * time.Millisecond)

	// A joiner of the same sandbox still needs a parking slot.
	require.ErrorIs(t, waker.Wake(context.Background(), wakeTarget), ErrParkingLotFull)

	close(ownerReleased)
	require.NoError(t, <-ownerErr)
}

func TestWakeConflictRestartsWithFreshCheckpoint(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		probes: []probeCall{pausedProbe("ckpt-1"), pausedProbe("ckpt-2"), readyProbe()},
		// First resume hits the checkpoint fence; the retry is accepted.
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeConflict, sandbox.ResumeAccepted},
		resumeErrs: []error{nil, nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, checkpoint := scripted.snapshot()
	require.Equal(t, 2, resumeCalls)
	require.Equal(t, "ckpt-2", checkpoint)
}

func TestWakeEmptyFenceAlreadyRunningReprobesInsteadOfJoining(t *testing.T) {
	// A PAUSING sandbox carries no checkpoint yet, so the flight resumes
	// with an empty fence. A FailedPrecondition then cannot distinguish
	// "someone resumed" from "pause still in progress" — the flight must
	// re-probe and retry with the fresh checkpoint instead of joining a
	// resume that does not exist (which would spin parked requests until
	// budget exhaustion).
	lifecycle := &scriptedLifecycle{
		// owner detection (empty checkpoint), ambiguous-join re-probe
		// (fresh checkpoint), readiness poll.
		probes:     []probeCall{pausedProbe(""), pausedProbe("ckpt-2"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAlreadyRunning, sandbox.ResumeAccepted},
		resumeErrs: []error{nil, nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, checkpoint := scripted.snapshot()
	require.Equal(t, 2, resumeCalls)
	require.Equal(t, "ckpt-2", checkpoint)
}

func TestWakeEmptyFenceAlreadyRunningJoinsWhenReady(t *testing.T) {
	// Empty fence, but the re-probe shows the sandbox already Running: the
	// ambiguous join resolves as served.
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe(""), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAlreadyRunning},
		resumeErrs: []error{nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 1, resumeCalls)
}

func TestRequestIDsDifferAcrossReplicasAndSandboxes(t *testing.T) {
	wakerA, scriptedA := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	})
	wakerB, scriptedB := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	})

	require.NoError(t, wakerA.Wake(context.Background(), wakeTarget))
	require.NoError(t, wakerB.Wake(context.Background(), wakeTarget))
	// Two replicas on their first flight for the same sandbox must not mint
	// identical request ids.
	require.NotEqual(t, scriptedA.resumeIDs[0], scriptedB.resumeIDs[0])
	// The id carries the namespace so same-named sandboxes in different
	// namespaces cannot collide.
	require.Contains(t, scriptedA.resumeIDs[0], "tenant-a")
}

func TestWakeConflictWithTransientProbeRetriesInsteadOfFailing(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		// owner detection, conflict re-probe (transient failure), conflict
		// re-probe (fresh checkpoint), readiness poll.
		probes: []probeCall{
			pausedProbe("ckpt-1"),
			{probe: sandbox.SandboxProbe{Phase: sandbox.SandboxPhaseNotReady}, err: fmt.Errorf("%w: probe down", sandbox.ErrSandboxNotReady)},
			pausedProbe("ckpt-2"),
			readyProbe(),
		},
		// First resume hits the fence, the retry after the transient probe
		// conflicts again, the last one is accepted.
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeConflict, sandbox.ResumeConflict, sandbox.ResumeAccepted},
		resumeErrs: []error{nil, nil, nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, checkpoint := scripted.snapshot()
	require.Equal(t, 3, resumeCalls)
	require.Equal(t, "ckpt-2", checkpoint)
	// Each checkpoint intent carries a distinct request_id so FastPath-side
	// dedup cannot replay a rejected fence.
	scripted.mu.Lock()
	require.NotEqual(t, scripted.resumeIDs[0], scripted.resumeIDs[2])
	scripted.mu.Unlock()
}

func TestWakeTreatsAlreadyRunningAsJoined(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAlreadyRunning},
		resumeErrs: []error{nil},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 1, resumeCalls)
}

func TestWakeSurfacesResumeNotFound(t *testing.T) {
	// The lifecycle contract carries already-mapped provider errors.
	notFound := fmt.Errorf("%w: sandbox not found", sandbox.ErrSandboxNotFound)
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1")},
		resumeOut:  []sandbox.ResumeOutcome{0},
		resumeErrs: []error{notFound},
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	err := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, err, sandbox.ErrSandboxNotFound)
}

func TestWakeEndsImmediatelyOnPermanentRejection(t *testing.T) {
	// PermissionDenied-class failures cannot heal within a park budget; the
	// flight must end at once instead of retrying until exhaustion.
	rejected := fmt.Errorf("%w: PermissionDenied", sandbox.ErrSandboxLifecycleRejected)
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1")},
		resumeOut:  []sandbox.ResumeOutcome{0},
		resumeErrs: []error{rejected},
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 10 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	err := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, err, sandbox.ErrSandboxLifecycleRejected)
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 1, resumeCalls) // no retry churn
}

func TestWakeJoinerSharesRemainingBudget(t *testing.T) {
	ownerStart := time.Now()
	ownerReleased := make(chan struct{})
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1")},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() {
		<-ownerReleased
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: 400 * time.Millisecond, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- waker.Wake(context.Background(), wakeTarget)
	}()
	time.Sleep(200 * time.Millisecond) // joiner arrives mid-flight

	joinerErr := waker.Wake(context.Background(), wakeTarget)
	require.ErrorIs(t, joinerErr, ErrBudgetExhausted)
	// The joiner's wait is bounded by the OWNER's budget (~400ms from owner
	// start), not its own arrival plus the full budget.
	require.Less(t, time.Since(ownerStart), 600*time.Millisecond)

	close(ownerReleased)
	require.ErrorIs(t, <-ownerErr, ErrBudgetExhausted)
}

func TestWakeCancelsWhenClientLeaves(t *testing.T) {
	ownerReleased := make(chan struct{})
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1")},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() {
		<-ownerReleased
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := waker.Wake(ctx, wakeTarget)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrBudgetExhausted)

	close(ownerReleased)
}

func TestParkingLotReleaseIsIdempotent(t *testing.T) {
	lot := newParkingLot(1)
	require.True(t, lot.tryAcquire())
	require.False(t, lot.tryAcquire())
	lot.release()
	lot.release() // second release must not over-drain
	require.True(t, lot.tryAcquire())
	require.False(t, lot.tryAcquire())
}

func TestBackoffNeverOverflowsOrRegresses(t *testing.T) {
	b := newBackoff(Config{RetryInterval: 50 * time.Millisecond, RetryFactor: 1.3, RetryJitter: 0.1})
	for range 500 {
		next := b.next()
		require.GreaterOrEqual(t, next, 50*time.Millisecond)
		require.LessOrEqual(t, next, maxBackoffStep)
	}
}

func TestAcquireFlightJoinsWhileQueuedOnPermit(t *testing.T) {
	lifecycle := &scriptedLifecycle{probes: []probeCall{pausedProbe("ckpt-1")}}
	waker, _ := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RestoreMax: 1, RetryInterval: time.Millisecond}, lifecycle)

	// Saturate the permit pool directly: a starter for a sandbox with no
	// flight yet must queue on it.
	waker.permits <- struct{}{}

	acquired := make(chan struct{})
	var joined *flight
	var joinErr error
	go func() {
		defer close(acquired)
		joined, _, joinErr = waker.acquireFlight(context.Background(), wakeTarget, "ckpt-1")
	}()
	time.Sleep(20 * time.Millisecond) // the starter is now queued on the permit

	// A flight for the same sandbox appears while it waits.
	live := &flight{
		key:      flightKey{namespace: wakeTarget.Namespace, sandboxID: wakeTarget.SandboxID},
		deadline: time.Now().Add(5 * time.Second),
		done:     make(chan struct{}),
	}
	close(live.done)
	waker.flights.mu.Lock()
	waker.flights.flights[live.key] = live
	waker.flights.mu.Unlock()

	<-acquired
	require.NoError(t, joinErr)
	require.Same(t, live, joined)
	// Joined without consuming a permit: the pool is untouched.
	require.Equal(t, 1, len(waker.permits))
}

func TestRestoreGaugeIsPairedPerFlight(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	require.Eventually(t, func() bool { return len(waker.permits) == 0 }, 2*time.Second, 5*time.Millisecond)
}

func TestFlightLifetimeCapsPollingAndFreesPermit(t *testing.T) {
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1")},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	waker, _ := newTestWaker(t, Config{ParkBudget: 10 * time.Millisecond, ParkMax: 8, MaxFlightLifetime: 50 * time.Millisecond, RetryInterval: 10 * time.Millisecond}, lifecycle)

	_, _, err := waker.acquireFlight(context.Background(), wakeTarget, "ckpt-1")
	require.NoError(t, err)
	require.Equal(t, 1, len(waker.permits))

	// The lifetime expires, the flight finishes, and the permit is freed
	// even though the sandbox never became Ready.
	require.Eventually(t, func() bool { return len(waker.permits) == 0 }, 2*time.Second, 5*time.Millisecond)
}

func TestBackoffScheduleGrowsAndJitters(t *testing.T) {
	b := newBackoff(Config{RetryInterval: 50 * time.Millisecond, RetryFactor: 1.3, RetryJitter: 0.1})
	previous := time.Duration(0)
	for attempt := range 5 {
		next := b.next()
		base := time.Duration(float64(50*time.Millisecond) * pow(1.3, float64(attempt)))
		require.GreaterOrEqual(t, next, base)
		require.LessOrEqual(t, next, time.Duration(float64(base)*1.1)+time.Millisecond)
		require.Greater(t, next, previous)
		previous = next
	}
}

func TestNewWakerValidatesLifecycle(t *testing.T) {
	_, err := NewWaker(Config{}, nil, nil, nil)
	require.ErrorContains(t, err, "lifecycle")
}

func TestNewWakerRejectsRestoreMaxAboveParkMax(t *testing.T) {
	_, err := NewWaker(Config{ParkBudget: time.Second, ParkMax: 4, RestoreMax: 8, RetryInterval: time.Millisecond}, &scriptedLifecycle{}, nil, nil)
	require.ErrorContains(t, err, "restore permits")
}

func TestNewWakerRejectsParkBudgetNotBelowFlightLifetime(t *testing.T) {
	// A waiter outliving its flight would get the flight's own context error,
	// which the proxy reads as a client disconnect instead of a 503.
	for _, budget := range []time.Duration{time.Minute, 2 * time.Minute} {
		_, err := NewWaker(Config{ParkBudget: budget, MaxFlightLifetime: time.Minute}, &scriptedLifecycle{}, nil, nil)
		require.ErrorContains(t, err, "max flight lifetime")
	}
}

func TestRestorePermitQueueShedsWithoutResume(t *testing.T) {
	// One permit; A's flight blocks inside ResumeSandbox holding it. B
	// queues FIFO and takes the permit once freed.
	firstResumeHeld := make(chan struct{})
	releaseFirst := make(chan struct{})
	bProbed := make(chan struct{})
	var probeCount atomic.Int64
	var hookOnce sync.Once
	lifecycle := &scriptedLifecycle{
		// call0: A probe; call1: B probe; call2+: polls.
		probes:     []probeCall{pausedProbe("ckpt-1"), pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() {
		hookOnce.Do(func() {
			close(firstResumeHeld)
			<-releaseFirst
		})
	}
	lifecycle.probeHook = func() {
		if probeCount.Add(1) == 2 {
			close(bProbed) // B probed and is about to queue
		}
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RestoreMax: 1, RetryInterval: time.Millisecond}, lifecycle)
	targetB := wakeTarget
	targetB.SandboxID = "sb-2"

	ownerErr := make(chan error, 1)
	go func() { ownerErr <- waker.Wake(context.Background(), wakeTarget) }()
	<-firstResumeHeld

	queuedErr := make(chan error, 1)
	go func() { queuedErr <- waker.Wake(context.Background(), targetB) }()
	<-bProbed
	require.Equal(t, 1, len(waker.permits))

	close(releaseFirst)
	require.NoError(t, <-ownerErr)
	require.NoError(t, <-queuedErr)
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 2, resumeCalls)
}

func TestRestorePermitShedsWhenBudgetElapsesQueued(t *testing.T) {
	// The queued starter's own deadline is shorter than the permit hold: it
	// sheds without any ResumeSandbox issued.
	firstResumeHeld := make(chan struct{})
	bProbed := make(chan struct{})
	var probeCount atomic.Int64
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() { <-firstResumeHeld }
	lifecycle.probeHook = func() {
		if probeCount.Add(1) == 2 {
			close(bProbed)
		}
	}
	waker, scripted := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RestoreMax: 1, RetryInterval: time.Millisecond}, lifecycle)
	targetB := wakeTarget
	targetB.SandboxID = "sb-2"

	ownerErr := make(chan error, 1)
	go func() { ownerErr <- waker.Wake(context.Background(), wakeTarget) }()
	// A's flight holds the only permit (acquired before ResumeSandbox).
	require.Eventually(t, func() bool { return len(waker.permits) == 1 }, 2*time.Second, 5*time.Millisecond)

	queued := make(chan error, 1)
	go func() {
		// A queued starter is bounded by its own request budget.
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		queued <- waker.Wake(ctx, targetB)
	}()
	<-bProbed
	require.ErrorIs(t, <-queued, context.DeadlineExceeded)
	resumeCalls, _ := scripted.snapshot()
	require.Equal(t, 1, resumeCalls)

	close(firstResumeHeld)
	require.NoError(t, <-ownerErr)
}

func TestFlightJoinersAcquireNoRestorePermit(t *testing.T) {
	ownerReleased := make(chan struct{})
	lifecycle := &scriptedLifecycle{
		// call0: owner probe; call1..3: joiner probes; call4+: poll.
		probes: []probeCall{
			pausedProbe("ckpt-1"), pausedProbe("ckpt-1"), pausedProbe("ckpt-1"),
			pausedProbe("ckpt-1"), readyProbe(),
		},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() { <-ownerReleased }
	waker, _ := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RestoreMax: 2, RetryInterval: time.Millisecond}, lifecycle)

	ownerErr := make(chan error, 1)
	go func() { ownerErr <- waker.Wake(context.Background(), wakeTarget) }()
	require.Eventually(t, func() bool { return len(waker.permits) == 1 }, 2*time.Second, 5*time.Millisecond)

	joiners := make(chan error, 3)
	for range 3 {
		go func() { joiners <- waker.Wake(context.Background(), wakeTarget) }()
	}
	// Joiners park but send their outcome only when the flight resolves.
	require.Eventually(t, func() bool { return len(waker.lot.slots) == 4 }, 2*time.Second, 5*time.Millisecond)
	// The flight holds the single permit; joiners take none.
	require.Equal(t, 1, len(waker.permits))

	close(ownerReleased)
	require.NoError(t, <-ownerErr)
	require.NoError(t, <-joiners)
	require.NoError(t, <-joiners)
	require.NoError(t, <-joiners)
}

func TestPermitHeldAcrossClientDisconnect(t *testing.T) {
	ownerReleased := make(chan struct{})
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	lifecycle.resumeHook = func() { <-ownerReleased }
	waker, _ := newTestWaker(t, Config{ParkBudget: 5 * time.Second, ParkMax: 8, RestoreMax: 2, RetryInterval: time.Millisecond}, lifecycle)

	ctx, cancel := context.WithCancel(context.Background())
	wakeErr := make(chan error, 1)
	go func() { wakeErr <- waker.Wake(ctx, wakeTarget) }()
	require.Eventually(t, func() bool { return len(waker.permits) == 1 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-wakeErr, context.Canceled)
	// The client is gone; the permit is not.
	require.Equal(t, 1, len(waker.permits))

	close(ownerReleased)
	require.Eventually(t, func() bool { return len(waker.permits) == 0 }, 2*time.Second, 5*time.Millisecond)
}

func TestWakeActivityWrittenWhenFlightObservesReady(t *testing.T) {
	var recorded []string
	var mu sync.Mutex
	recorder := recorderFunc(func(namespace, sandboxID string) {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, namespace+"/"+sandboxID)
	})
	lifecycle := &scriptedLifecycle{
		probes:     []probeCall{pausedProbe("ckpt-1"), readyProbe()},
		resumeOut:  []sandbox.ResumeOutcome{sandbox.ResumeAccepted},
		resumeErrs: []error{nil},
	}
	waker, err := NewWaker(Config{ParkBudget: 5 * time.Second, ParkMax: 8, RestoreMax: 8, RetryInterval: time.Millisecond}, lifecycle, recorder, AllowAll())
	require.NoError(t, err)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"tenant-a/sb-1"}, recorded)
}

type recorderFunc func(namespace, sandboxID string)

func (f recorderFunc) Record(namespace, sandboxID string) { f(namespace, sandboxID) }
