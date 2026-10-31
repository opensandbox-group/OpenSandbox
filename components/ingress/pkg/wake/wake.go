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
	slogger "github.com/alibaba/opensandbox/internal/logger"
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
	DefaultParkBudget     = 5 * time.Second
	DefaultParkMax        = 1024
	DefaultRestoreMax     = 256
	DefaultFlightLifetime = 5 * time.Minute
	DefaultRetryInterval  = 50 * time.Millisecond
	DefaultRetryFactor    = 1.3
	DefaultRetryJitter    = 0.1
)

// Config bounds the wake path; it maps onto the --wake-* flags.
type Config struct {
	// ParkBudget is the per-flight wait budget; late joiners share what is
	// left of it.
	ParkBudget time.Duration
	// ParkMax bounds concurrently parked requests per replica.
	ParkMax int
	// RestoreMax bounds concurrent resume flights (permits are held to
	// flight terminal). Must not exceed ParkMax.
	RestoreMax int
	// MaxFlightLifetime bounds one flight's establish+poll phase; a stuck
	// restore releases its permit at expiry instead of holding it forever.
	MaxFlightLifetime time.Duration
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

// PolicySource answers the per-sandbox wake gate; must fail safe — every
// error resolves to false.
type PolicySource interface {
	WakeAllowed(ctx context.Context, namespace, sandboxID string) bool
}

// AllowAll permits every sandbox; for tests and callers that gate elsewhere.
func AllowAll() PolicySource { return allowAllPolicy{} }

type allowAllPolicy struct{}

func (allowAllPolicy) WakeAllowed(context.Context, string, string) bool { return true }

// Logger defaults to stderr and is replaced by the process-wide
// "opensandbox.ingress" logger at startup.
var Logger = slogger.MustNew(slogger.Config{Level: "info"})

// Waker is the per-replica wake orchestrator, safe for concurrent use.
type Waker struct {
	cfg       Config
	lifecycle Lifecycle
	activity  ActivityWriter
	policy    PolicySource
	lot       *parkingLot
	permits   chan struct{}
	flights   flightRegistry
	epoch     atomic.Uint64
	// processUnique keeps request_ids collision-free across replicas; the
	// epoch counter alone is process-local.
	processUnique string
	now           func() time.Time
}

func logFor(target sandbox.EndpointTarget) slogger.Logger {
	return Logger.With(
		slogger.Field{Key: "namespace", Value: target.Namespace},
		slogger.Field{Key: "sandbox_id", Value: target.SandboxID},
	)
}

func NewWaker(cfg Config, lifecycle Lifecycle, activity ActivityWriter, policy PolicySource) (*Waker, error) {
	if lifecycle == nil {
		return nil, errors.New("wake: lifecycle client is required")
	}
	if policy == nil {
		// No policy source means no sandbox is opted in: deny everything.
		policy = denyAllPolicy{}
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
	if cfg.RestoreMax <= 0 {
		cfg.RestoreMax = DefaultRestoreMax
	}
	// An unset RestoreMax defers to an explicit smaller park bound instead
	// of failing startup; an explicitly conflicting pair is still an error.
	if cfg.RestoreMax == DefaultRestoreMax && cfg.RestoreMax > cfg.ParkMax {
		cfg.RestoreMax = cfg.ParkMax
	}
	if cfg.RestoreMax > cfg.ParkMax {
		return nil, fmt.Errorf("wake: restore permits (%d) cannot exceed the parking lot capacity (%d)", cfg.RestoreMax, cfg.ParkMax)
	}
	if cfg.MaxFlightLifetime <= 0 {
		cfg.MaxFlightLifetime = DefaultFlightLifetime
	}
	// Waiters must give up before their flight does: a flight that outlives
	// its own lifetime finishes with a context error, which the proxy cannot
	// tell apart from a client disconnect.
	if cfg.ParkBudget >= cfg.MaxFlightLifetime {
		return nil, fmt.Errorf("wake: park budget (%v) must be shorter than the max flight lifetime (%v)", cfg.ParkBudget, cfg.MaxFlightLifetime)
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
		policy:        policy,
		lot:           newParkingLot(cfg.ParkMax),
		permits:       make(chan struct{}, cfg.RestoreMax),
		flights:       flightRegistry{flights: make(map[flightKey]*flight)},
		processUnique: hex.EncodeToString(unique),
		now:           time.Now,
	}, nil
}

// denyAllPolicy is the nil-PolicySource default.
type denyAllPolicy struct{}

func (denyAllPolicy) WakeAllowed(context.Context, string, string) bool { return false }

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

	// Opt-in gate: no policy keeps today's permanent 503. Runs before
	// admission so denial consumes no slot and no permit.
	if !w.policy.WakeAllowed(ctx, target.Namespace, target.SandboxID) {
		logFor(target).Debugf("wake denied: paused sandbox has no wake-on-access policy")
		return fmt.Errorf("%w: sandbox is paused without a wake-on-access policy", sandbox.ErrSandboxNotReady)
	}

	// Admission precedes flight creation.
	if !w.lot.tryAcquire() {
		telemetry.RecordParkShed()
		logFor(target).Debugf("wake shed: parking lot full")
		return ErrParkingLotFull
	}
	defer w.lot.release()

	flight, joined, err := w.acquireFlight(ctx, target, probe.CheckpointID)
	if err != nil {
		return err
	}
	if joined {
		telemetry.RecordWakeFlight("joined")
		logFor(target).Debugf("wake flight joined")
	} else {
		telemetry.RecordWakeFlight("triggered")
		logFor(target).With(
			slogger.Field{Key: "checkpoint_id", Value: probe.CheckpointID},
			slogger.Field{Key: "epoch", Value: flight.epoch},
		).Infof("wake flight started")
	}

	start := w.now()
	telemetry.RecordParkDelta(1)
	waitErr := flight.wait(ctx)
	telemetry.RecordParkDelta(-1)
	telemetry.RecordParkWait(parkOutcome(waitErr), w.now().Sub(start))
	return waitErr
}

// acquireFlight returns the active flight, creating it under a restore
// permit when none is running; a queued starter is bounded by its own
// budget, and joiners take no permit.
func (w *Waker) acquireFlight(ctx context.Context, target sandbox.EndpointTarget, checkpointID string) (*flight, bool, error) {
	key := flightKey{namespace: target.Namespace, sandboxID: target.SandboxID}
	if existing := w.flights.lookup(key); existing != nil {
		return existing, true, nil
	}

	timer := time.NewTimer(w.cfg.ParkBudget)
	defer timer.Stop()
	// Re-check the registry while queued: with permits saturated (the very
	// case they exist for), a joinable flight may appear at any moment and
	// joining it beats queueing for a spare permit.
	recheck := time.NewTicker(w.cfg.RetryInterval)
	defer recheck.Stop()

	for {
		select {
		case w.permits <- struct{}{}:
			// Another starter may have won while we queued; join and hand back.
			w.flights.mu.Lock()
			if existing := w.flights.active(key); existing != nil {
				w.flights.mu.Unlock()
				w.releasePermit()
				return existing, true, nil
			}
			f := &flight{
				key:      key,
				target:   target,
				epoch:    w.epoch.Add(1),
				deadline: w.now().Add(w.cfg.ParkBudget),
				started:  w.now(),
				done:     make(chan struct{}),
				waker:    w,
			}
			w.flights.flights[key] = f
			w.flights.mu.Unlock()
			telemetry.RecordRestoreDelta(1)
			// Background context: budget exhaustion or client disconnect must
			// not cancel the resume.
			go f.run(checkpointID)
			return f, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-timer.C:
			return nil, false, ErrBudgetExhausted
		case <-recheck.C:
			if existing := w.flights.lookup(key); existing != nil {
				return existing, true, nil
			}
		}
	}
}

// releasePermit hands one permit back; a double release cannot over-credit.
func (w *Waker) releasePermit() {
	select {
	case <-w.permits:
	default:
	}
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
