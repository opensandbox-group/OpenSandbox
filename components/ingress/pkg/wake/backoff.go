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
	"math/rand/v2"
	"time"
)

// parkingLot bounds the number of concurrently parked requests (and, because
// admission precedes flight creation, concurrent resume flights) per replica.
// Acquisition is non-blocking: overflow is shed immediately with 503 instead
// of queueing.
type parkingLot struct {
	slots chan struct{}
}

func newParkingLot(capacity int) *parkingLot {
	return &parkingLot{slots: make(chan struct{}, capacity)}
}

func (l *parkingLot) tryAcquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *parkingLot) release() {
	select {
	case <-l.slots:
	default:
	}
}

// backoff produces the OSEP-0024 retry schedule: exponential growth from the
// base interval by the given factor, with relative jitter applied on top
// (k8s wait.Backoff semantics: duration * (1 + rand*jitter)). Jitter is not
// security-sensitive; the rand/v2 global source is goroutine-safe.
type backoff struct {
	interval time.Duration
	factor   float64
	jitter   float64
	attempt  int
}

func newBackoff(cfg Config) *backoff {
	return &backoff{
		interval: cfg.RetryInterval,
		factor:   cfg.RetryFactor,
		jitter:   cfg.RetryJitter,
	}
}

func (b *backoff) next() time.Duration {
	delay := time.Duration(float64(b.interval) * pow(b.factor, b.attempt))
	b.attempt++
	if b.jitter > 0 {
		delay = time.Duration(float64(delay) * (1 + rand.Float64()*b.jitter)) //nolint:gosec // G404: jitter only, not security-sensitive
	}
	if delay < b.interval {
		delay = b.interval
	}
	return delay
}

func pow(base float64, exp int) float64 {
	result := 1.0
	for range exp {
		result *= base
	}
	return result
}

// sleepBackoff waits for one backoff step, bounded by the flight's remaining
// budget and cancelable through ctx. The budget timer is shared across steps:
// once it fires the flight is over. It reports false when the budget (or the
// context) ended first.
func sleepBackoff(ctx context.Context, budget *time.Timer, b *backoff) bool {
	timer := time.NewTimer(b.next())
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-budget.C:
		return false
	case <-ctx.Done():
		return false
	}
}
