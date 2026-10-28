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
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

type flightKey struct {
	namespace string
	sandboxID string
}

// flight is one per-sandbox resume attempt: while its budget is live it is
// the only flight for the sandbox on this replica, and all parked requests
// share its outcome and remaining budget. A flight past its budget may
// still be polling toward terminal (permit held) alongside a newer one.
type flight struct {
	key      flightKey
	target   sandbox.EndpointTarget
	epoch    uint64
	deadline time.Time
	started  time.Time
	done     chan struct{}
	err      error

	waker *Waker

	removeOnce sync.Once
}

func (f *flight) requestID(attempt int) string {
	// Namespace + process-unique prefix + attempt: request dedup on the
	// FastPath side must never replay one caller's outcome to another.
	return fmt.Sprintf("wake-%s-%s-%s-%d-%d",
		f.target.Namespace, f.target.SandboxID, f.waker.processUnique, f.epoch, attempt)
}

// wait blocks until the flight finishes, the caller's context is done, or
// the shared budget elapses. The flight keeps running in every case.
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
		// The client is gone; distinct from budget exhaustion.
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

// run executes the resume flight to a terminal outcome on a background
// context: never canceled by budget, disconnect, or slot release, so the
// permit it holds bounds actual restores. MaxFlightLifetime caps the whole
// phase so a stuck restore frees its permit instead of holding it forever.
func (f *flight) run(checkpointID string) {
	defer func() {
		Logger.With(
			slogger.Field{Key: "namespace", Value: f.target.Namespace},
			slogger.Field{Key: "sandbox_id", Value: f.target.SandboxID},
			slogger.Field{Key: "epoch", Value: f.epoch},
			slogger.Field{Key: "outcome", Value: flightOutcome(f.err)},
			slogger.Field{Key: "waited_seconds", Value: time.Since(f.started).Seconds()},
		).Infof("wake flight finished")
		f.waker.releasePermit()
		telemetry.RecordRestoreDelta(-1)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), f.waker.cfg.MaxFlightLifetime)
	defer cancel()
	w := f.waker
	backoff := newBackoff(w.cfg)

	if !f.establishResume(ctx, backoff, &checkpointID) {
		return
	}
	f.pollUntilReady(ctx, backoff)
}

func flightOutcome(err error) string {
	switch {
	case err == nil:
		return "ready"
	case errors.Is(err, ErrBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, sandbox.ErrSandboxNotFound):
		return "not_found"
	default:
		return "error"
	}
}

// establishResume submits the resume intent, restarting on the checkpoint
// fence until it is accepted, joined, or the flight ends. It reports false
// when the flight finished without reaching the poll phase.
func (f *flight) establishResume(ctx context.Context, backoff *backoff, checkpointID *string) bool {
	w := f.waker
	attempt := 0
	for {
		attempt++
		requestID := f.requestID(attempt)
		outcome, err := w.lifecycle.ResumeSandbox(ctx, f.target, *checkpointID, requestID)

		// With an empty fence, "not paused" may also mean the pause is
		// still in progress — joining a resume that does not exist would
		// spin parked requests until the budget. Re-probe instead.
		needsReprobe := outcome == sandbox.ResumeConflict ||
			(outcome == sandbox.ResumeAlreadyRunning && *checkpointID == "")

		if err == nil && !needsReprobe {
			Logger.With(
				slogger.Field{Key: "namespace", Value: f.target.Namespace},
				slogger.Field{Key: "sandbox_id", Value: f.target.SandboxID},
				slogger.Field{Key: "epoch", Value: f.epoch},
				slogger.Field{Key: "attempt", Value: attempt},
				slogger.Field{Key: "request_id", Value: requestID},
			).Infof("ResumeSandbox accepted")
			return true
		}
		if needsReprobe {
			Logger.With(
				slogger.Field{Key: "namespace", Value: f.target.Namespace},
				slogger.Field{Key: "sandbox_id", Value: f.target.SandboxID},
				slogger.Field{Key: "epoch", Value: f.epoch},
				slogger.Field{Key: "attempt", Value: attempt},
			).Warnf("resume fence conflict or ambiguous join, re-probing")
		}
		if err != nil {
			switch {
			case errors.Is(err, sandbox.ErrSandboxNotFound),
				errors.Is(err, sandbox.ErrSandboxLifecycleRejected):
				// Nothing a retry within the budget can fix.
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
				// Transient: retry the resume with the previous checkpoint;
				// the fence fires again and the re-probe repeats.
			}
		}
		if !sleepBackoff(ctx, backoff) {
			f.finish(ctx.Err())
			return false
		}
	}
}

// pollUntilReady polls until Ready or terminal; the park budget bounds the
// waiters, never this loop.
func (f *flight) pollUntilReady(ctx context.Context, backoff *backoff) {
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
		if !sleepBackoff(ctx, backoff) {
			f.finish(ctx.Err())
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
// (namespace, sandbox_id). Cross-replica duplicates are harmless:
// ResumeSandbox is an idempotent CAS patch, executed once by the controller.
type flightRegistry struct {
	mu      sync.Mutex
	flights map[flightKey]*flight
}

// lookup returns the flight that may absorb a new arrival, or nil: a
// flight whose budget elapsed no longer absorbs joiners.
func (r *flightRegistry) lookup(key flightKey) *flight {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active(key)
}

// active returns the flight for key if it still has wait budget left.
// Callers must hold r.mu.
func (r *flightRegistry) active(key flightKey) *flight {
	if existing, ok := r.flights[key]; ok && time.Until(existing.deadline) > 0 {
		return existing
	}
	return nil
}

func (r *flightRegistry) remove(f *flight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.flights[f.key]; ok && current == f {
		delete(r.flights, f.key)
	}
}
