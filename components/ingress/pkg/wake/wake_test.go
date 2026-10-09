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
	lastReqID   string
	lastCkpt    string
	resumeHook  func() // optional; runs inside ResumeSandbox under lock
}

func (s *scriptedLifecycle) ProbeSandbox(context.Context, sandbox.EndpointTarget) (sandbox.SandboxProbe, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := s.probeCalls
	s.probeCalls++
	if call >= len(s.probes) {
		call = len(s.probes) - 1
	}
	return s.probes[call].probe, s.probes[call].err
}

func (s *scriptedLifecycle) ResumeSandbox(_ context.Context, _ sandbox.EndpointTarget, checkpointID, requestID string) (sandbox.ResumeOutcome, error) {
	s.mu.Lock()
	s.resumeCalls++
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
	waker, err := NewWaker(cfg, lifecycle, nil)
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
	require.Regexp(t, `^wake-sb-1-\d+$`, scripted.lastReqID)
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

func TestBackoffScheduleGrowsAndJitters(t *testing.T) {
	b := newBackoff(Config{RetryInterval: 50 * time.Millisecond, RetryFactor: 1.3, RetryJitter: 0.1})
	previous := time.Duration(0)
	for attempt := range 5 {
		next := b.next()
		base := time.Duration(float64(50*time.Millisecond) * pow(1.3, attempt))
		require.GreaterOrEqual(t, next, base)
		require.LessOrEqual(t, next, time.Duration(float64(base)*1.1)+time.Millisecond)
		require.Greater(t, next, previous)
		previous = next
	}
}

func TestNewWakerValidatesLifecycle(t *testing.T) {
	_, err := NewWaker(Config{}, nil, nil)
	require.ErrorContains(t, err, "lifecycle")
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
	waker, err := NewWaker(Config{ParkBudget: 5 * time.Second, ParkMax: 8, RetryInterval: time.Millisecond}, lifecycle, recorder)
	require.NoError(t, err)

	require.NoError(t, waker.Wake(context.Background(), wakeTarget))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"tenant-a/sb-1"}, recorded)
}

type recorderFunc func(namespace, sandboxID string)

func (f recorderFunc) Record(namespace, sandboxID string) { f(namespace, sandboxID) }
