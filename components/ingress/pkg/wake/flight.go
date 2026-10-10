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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
)

type flightKey struct {
	namespace string
	sandboxID string
}

// flight is one per-sandbox resume attempt: at most one concurrent
// ResumeSandbox per replica per sandbox, with all parked requests sharing
// its outcome and its remaining budget.
type flight struct {
	key      flightKey
	target   sandbox.EndpointTarget
	epoch    uint64
	deadline time.Time
	done     chan struct{}
	err      error

	waker *Waker

	removeOnce sync.Once
}

func (f *flight) requestID(attempt int) string {
	// Unique across attempts, replicas, and sandboxes: the fence attempt
	// counter separates checkpoint intents, the per-process random prefix
	// separates replicas (the epoch counter is process-local), and the
	// namespace disambiguates same-named sandboxes so FastPath-side request
	// dedup can never replay one caller's outcome to another.
	return fmt.Sprintf("wake-%s-%s-%s-%d-%d",
		f.target.Namespace, f.target.SandboxID, f.waker.processUnique, f.epoch, attempt)
}

// wait blocks until the flight finishes, the caller's context is done, or
// the flight's shared budget elapses. The flight itself keeps running on its
// background context in every case.
func (f *flight) wait(ctx context.Context) error {
	remaining := time.Until(f.deadline)
	if remaining <= 0 {
		return ErrBudgetExhausted
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		// The client is gone; the flight keeps running on its background
		// context and is distinct from budget exhaustion.
		return ctx.Err()
	case <-timer.C:
		return ErrBudgetExhausted
	}
}

// finish records the outcome and detaches the flight from the registry so
// later requests start a fresh one.
func (f *flight) finish(err error) {
	f.err = err
	close(f.done)
	f.removeOnce.Do(func() {
		f.waker.flights.remove(f)
	})
}

// run executes the resume flight on a background context. The desired-state
// resume is never canceled: when the budget expires the flight simply stops
// polling and lets fast-sandbox converge on its own.
func (f *flight) run(checkpointID string) {
	ctx := context.Background()
	w := f.waker
	budget := time.NewTimer(time.Until(f.deadline))
	defer budget.Stop()
	backoff := newBackoff(w.cfg)

	if !f.establishResume(ctx, budget, backoff, &checkpointID) {
		return
	}
	f.pollUntilReady(ctx, budget, backoff)
}

// establishResume submits the resume intent, restarting on the checkpoint
// fence until it is accepted, joined, or the flight ends. It reports false
// when the flight finished without reaching the poll phase.
func (f *flight) establishResume(ctx context.Context, budget *time.Timer, backoff *backoff, checkpointID *string) bool {
	w := f.waker
	attempt := 0
	for {
		attempt++
		outcome, err := w.lifecycle.ResumeSandbox(ctx, f.target, *checkpointID, f.requestID(attempt))

		// "Not paused" (FailedPrecondition) is an unambiguous join — someone
		// else already resumed past our fence — only when the fence
		// referenced a real checkpoint. With an empty fence the same outcome
		// also fires when the pause is still in progress, in which case
		// joining a resume that does not exist would leave every parked
		// request spinning until the budget: re-probe instead and decide
		// from the current phase.
		needsReprobe := outcome == sandbox.ResumeConflict ||
			(outcome == sandbox.ResumeAlreadyRunning && *checkpointID == "")

		if err == nil && !needsReprobe {
			return true
		}
		if err != nil {
			switch {
			case errors.Is(err, sandbox.ErrSandboxNotFound),
				errors.Is(err, sandbox.ErrSandboxLifecycleRejected):
				// Terminal: the sandbox is gone, or FastPath rejected the
				// call permanently (auth/validation/server fault) — retrying
				// within the budget cannot succeed and would only burn it.
				f.finish(err)
				return false
			default:
				// Transient RPC failure: retry within the remaining budget.
			}
		} else {
			// Fence conflict or ambiguous join: re-probe and restart with
			// the fresh checkpoint; a sandbox that is no longer wakeable
			// resolves the flight from its current phase.
			probe, probeErr := w.lifecycle.ProbeSandbox(ctx, f.target)
			switch {
			case probeErr == nil && probe.Phase != sandbox.SandboxPhaseWakeable:
				f.finish(phaseOutcome(probe.Phase))
				return false
			case probeErr == nil:
				*checkpointID = probe.CheckpointID
			case errors.Is(probeErr, sandbox.ErrSandboxNotFound):
				f.finish(probeErr)
				return false
			default:
				// Transient probe failure: fall through to the backoff and
				// retry the resume with the previous checkpoint; the fence
				// fires again and the re-probe repeats.
			}
		}
		if !sleepBackoff(ctx, budget, backoff) {
			f.finish(ErrBudgetExhausted)
			return false
		}
	}
}

// pollUntilReady polls until runtime and data plane are Ready, the sandbox
// turns terminal, or the budget elapses.
func (f *flight) pollUntilReady(ctx context.Context, budget *time.Timer, backoff *backoff) {
	w := f.waker
	for {
		probe, err := w.lifecycle.ProbeSandbox(ctx, f.target)
		if err == nil {
			switch probe.Phase {
			case sandbox.SandboxPhaseReady:
				if w.activity != nil {
					w.activity.Record(f.target.Namespace, f.target.SandboxID)
				}
				f.finish(nil)
				return
			case sandbox.SandboxPhaseTerminal:
				f.finish(fmt.Errorf("%w: sandbox became terminal while resuming", sandbox.ErrSandboxNotFound))
				return
			}
		} else if errors.Is(err, sandbox.ErrSandboxNotFound) || errors.Is(err, sandbox.ErrSandboxLifecycleRejected) {
			f.finish(err)
			return
		}
		if !sleepBackoff(ctx, budget, backoff) {
			f.finish(ErrBudgetExhausted)
			return
		}
	}
}

func phaseOutcome(phase sandbox.SandboxPhase) error {
	switch phase {
	case sandbox.SandboxPhaseReady:
		return nil
	case sandbox.SandboxPhaseTerminal:
		return fmt.Errorf("%w: sandbox is stopped or expired", sandbox.ErrSandboxNotFound)
	default:
		return fmt.Errorf("%w: sandbox runtime is %s", sandbox.ErrSandboxNotReady, phase)
	}
}

// flightRegistry is the per-replica singleflight registry keyed by
// (namespace, sandbox_id). Duplicates across replicas are harmless by
// construction: ResumeSandbox is an idempotent compare-and-set patch and the
// restore itself is executed once by the fast-sandbox controller.
type flightRegistry struct {
	mu      sync.Mutex
	flights map[flightKey]*flight
}

func (r *flightRegistry) joinOrCreate(key flightKey, create func() *flight) (*flight, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flights == nil {
		r.flights = make(map[flightKey]*flight)
	}
	if existing, ok := r.flights[key]; ok && time.Until(existing.deadline) > 0 {
		return existing, true
	}
	f := create()
	r.flights[key] = f
	return f, false
}

func (r *flightRegistry) remove(f *flight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.flights[f.key]; ok && current == f {
		delete(r.flights, f.key)
	}
}
