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
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

var (
	// ErrBudgetExhausted means the park budget elapsed before the restore
	// converged; the resume continues, only the wait ends.
	ErrBudgetExhausted = errors.New("wake park budget exhausted")

	// ErrParkingLotFull means the replica's park bound is reached; the
	// request is shed before any ResumeSandbox is issued.
	ErrParkingLotFull = errors.New("wake parking lot full")
)

const (
	DefaultParkBudget    = 5 * time.Second
	DefaultParkMax       = 1024
	DefaultRetryInterval = 50 * time.Millisecond
	DefaultRetryFactor   = 1.3
	DefaultRetryJitter   = 0.1
)

// Config bounds the wake path; it maps onto the --wake-* flags.
type Config struct {
	// ParkBudget is the per-flight wait budget; late joiners share what is
	// left of it.
	ParkBudget time.Duration
	// ParkMax bounds concurrently parked requests (and resume flights).
	ParkMax int
	// RetryInterval is the first poll interval, grown by RetryFactor with
	// up to RetryJitter relative jitter.
	RetryInterval time.Duration
	RetryFactor   float64
	RetryJitter   float64
}

// Lifecycle is the fast-sandbox control surface the wake path needs,
// satisfied by sandbox.FastSandboxProvider.
type Lifecycle interface {
	ProbeSandbox(ctx context.Context, target sandbox.EndpointTarget) (sandbox.SandboxProbe, error)
	ResumeSandbox(ctx context.Context, target sandbox.EndpointTarget, expectedCheckpointID, requestID string) (sandbox.ResumeOutcome, error)
}

// ActivityWriter records a last-activity observation; flights write one on
// Ready so the post-resume idle window starts cleanly.
type ActivityWriter interface {
	Record(namespace, sandboxID string)
}

// Waker is the per-replica wake orchestrator, safe for concurrent use.
type Waker struct {
	cfg       Config
	lifecycle Lifecycle
	activity  ActivityWriter
	lot       *parkingLot
	flights   flightRegistry
	epoch     atomic.Uint64
	// processUnique keeps request_ids collision-free across replicas; the
	// epoch counter alone is process-local.
	processUnique string
	now           func() time.Time
}

func NewWaker(cfg Config, lifecycle Lifecycle, activity ActivityWriter) (*Waker, error) {
	if lifecycle == nil {
		return nil, errors.New("wake: lifecycle client is required")
	}
	unique := make([]byte, 8)
	if _, err := cryptorand.Read(unique); err != nil {
		return nil, fmt.Errorf("wake: generate process-unique request id prefix: %w", err)
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
		cfg:           cfg,
		lifecycle:     lifecycle,
		activity:      activity,
		lot:           newParkingLot(cfg.ParkMax),
		processUnique: hex.EncodeToString(unique),
		now:           time.Now,
	}, nil
}

// Wake probes the target sandbox and, when paused, parks the caller until
// the restore converges or the budget elapses. A nil return means the
// sandbox is ready to serve: re-resolve the route and forward. Error
// mapping happens at the call site: ErrSandboxNotFound -> 404,
// ErrBudgetExhausted / ErrParkingLotFull -> 503 + Retry-After, else the
// usual not-ready handling.
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

	// Admission precedes flight creation: the lot bounds concurrent
	// restores, not just held connections.
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
		// Background context: budget exhaustion or client disconnect must
		// not cancel the resume.
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
	telemetry.RecordParkWait(parkOutcome(waitErr), w.now().Sub(start))
	return waitErr
}

func parkOutcome(waitErr error) string {
	switch {
	case waitErr == nil:
		return "served"
	case errors.Is(waitErr, ErrBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(waitErr, context.Canceled), errors.Is(waitErr, context.DeadlineExceeded):
		return "canceled"
	default:
		return "error"
	}
}
