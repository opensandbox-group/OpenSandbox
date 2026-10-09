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

// Package wake implements ingress wake-on-access for paused fast-sandbox
// sandboxes (OSEP-0024): detect a paused sandbox on route resolution,
// trigger ResumeSandbox through a per-sandbox singleflight, and park the
// request for up to a bounded budget while the restore converges.
package wake

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

var (
	// ErrBudgetExhausted means the park budget elapsed before the restore
	// converged. The resume itself is never canceled; clients retry.
	ErrBudgetExhausted = errors.New("wake park budget exhausted")

	// ErrParkingLotFull means this replica already holds the bounded number
	// of parked requests and concurrent resume flights; the request is shed
	// before any ResumeSandbox is issued.
	ErrParkingLotFull = errors.New("wake parking lot full")
)

const (
	DefaultParkBudget    = 5 * time.Second
	DefaultParkMax       = 1024
	DefaultRetryInterval = 50 * time.Millisecond
	DefaultRetryFactor   = 1.3
	DefaultRetryJitter   = 0.1
)

// Config bounds the wake path. It maps one-to-one onto the ingress
// --wake-* flags.
type Config struct {
	// ParkBudget is the per-flight wait budget Y. Late joiners share the
	// flight's remaining budget.
	ParkBudget time.Duration
	// ParkMax is the parking lot capacity: the bound on concurrently parked
	// requests (and therefore on concurrent resume flights) per replica.
	ParkMax int
	// RetryInterval is the first poll interval; each retry multiplies by
	// RetryFactor with up to RetryJitter relative jitter.
	RetryInterval time.Duration
	RetryFactor   float64
	RetryJitter   float64
}

// Lifecycle is the fast-sandbox control surface the wake path needs. It is
// satisfied by sandbox.FastSandboxProvider.
type Lifecycle interface {
	ProbeSandbox(ctx context.Context, target sandbox.EndpointTarget) (sandbox.SandboxProbe, error)
	ResumeSandbox(ctx context.Context, target sandbox.EndpointTarget, expectedCheckpointID, requestID string) (sandbox.ResumeOutcome, error)
}

// ActivityWriter records a last-activity observation. Every flight that
// observes Ready writes one so the post-resume idle window starts cleanly
// regardless of which replica won.
type ActivityWriter interface {
	Record(namespace, sandboxID string)
}

// Waker is the per-replica wake orchestrator. It is safe for concurrent use.
type Waker struct {
	cfg       Config
	lifecycle Lifecycle
	activity  ActivityWriter
	lot       *parkingLot
	flights   flightRegistry
	epoch     atomic.Uint64
	now       func() time.Time
}

func NewWaker(cfg Config, lifecycle Lifecycle, activity ActivityWriter) (*Waker, error) {
	if lifecycle == nil {
		return nil, errors.New("wake: lifecycle client is required")
	}
	if cfg.ParkBudget <= 0 {
		cfg.ParkBudget = DefaultParkBudget
	}
	if cfg.ParkMax <= 0 {
		cfg.ParkMax = DefaultParkMax
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.RetryFactor <= 1 {
		cfg.RetryFactor = DefaultRetryFactor
	}
	if cfg.RetryJitter < 0 {
		cfg.RetryJitter = DefaultRetryJitter
	}
	return &Waker{
		cfg:       cfg,
		lifecycle: lifecycle,
		activity:  activity,
		lot:       newParkingLot(cfg.ParkMax),
		now:       time.Now,
	}, nil
}

// Wake inspects the sandbox for the request's target and, when it is paused,
// parks the caller until the restore converges or the budget elapses.
//
// A nil return means the sandbox is (now) ready to serve: the caller
// re-resolves the route and forwards exactly like any other request.
// Returned errors map onto HTTP semantics at the call site:
// sandbox.ErrSandboxNotFound -> 404; ErrBudgetExhausted and
// ErrParkingLotFull -> 503 with Retry-After; other errors -> today's
// not-ready handling.
func (w *Waker) Wake(ctx context.Context, target sandbox.EndpointTarget) error {
	probe, err := w.lifecycle.ProbeSandbox(ctx, target)
	if err != nil {
		return err
	}
	switch probe.Phase {
	case sandbox.SandboxPhaseReady:
		telemetry.RecordWakeFlight("none")
		return nil
	case sandbox.SandboxPhaseTerminal:
		return fmt.Errorf("%w: sandbox is stopped or expired", sandbox.ErrSandboxNotFound)
	case sandbox.SandboxPhaseNotReady:
		return fmt.Errorf("%w: sandbox runtime is %s", sandbox.ErrSandboxNotReady, probe.RuntimeState)
	}

	// Admission precedes flight creation so the lot bounds concurrent
	// restores, not just held connections. Every parked request — flight
	// owner and joiner alike — holds a slot; requests served without
	// pausing never reach this line.
	if !w.lot.tryAcquire() {
		telemetry.RecordParkShed()
		return ErrParkingLotFull
	}
	defer w.lot.release()

	flight, joined := w.flights.joinOrCreate(flightKey{namespace: target.Namespace, sandboxID: target.SandboxID}, func() *flight {
		f := &flight{
			key:      flightKey{namespace: target.Namespace, sandboxID: target.SandboxID},
			target:   target,
			epoch:    w.epoch.Add(1),
			deadline: w.now().Add(w.cfg.ParkBudget),
			done:     make(chan struct{}),
			waker:    w,
		}
		// The flight runs on a background context: budget exhaustion or
		// client disconnect must not cancel the resume.
		go f.run(probe.CheckpointID)
		return f
	})
	if joined {
		telemetry.RecordWakeFlight("joined")
	} else {
		telemetry.RecordWakeFlight("triggered")
	}

	start := w.now()
	telemetry.RecordParkDelta(1)
	waitErr := flight.wait(ctx)
	telemetry.RecordParkDelta(-1)
	telemetry.RecordParkWait(parkOutcome(waitErr, ctx), w.now().Sub(start))
	return waitErr
}

func parkOutcome(waitErr error, ctx context.Context) string {
	switch {
	case waitErr == nil:
		return "served"
	case errors.Is(waitErr, ErrBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) || ctx.Err() != nil:
		return "canceled"
	default:
		return "error"
	}
}
