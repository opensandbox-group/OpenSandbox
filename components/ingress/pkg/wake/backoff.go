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

// parkingLot bounds concurrently parked requests. It does not bound
// restore work: slots belong to requests and are released when a request
// ends, while a flight outlives it — restores are bounded by permits.
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

// maxBackoffStep caps growth for unbounded (post-budget) flights.
const maxBackoffStep = 2 * time.Second

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
	// Grow and clamp in float64: pow overflows int64 around attempt ~100
	// and the converted Duration can turn negative before any clamp.
	b.attempt++
	delay := float64(b.interval) * pow(b.factor, float64(b.attempt-1))
	if ceiling := float64(maxBackoffStep); delay > ceiling {
		delay = ceiling
	}
	if floor := float64(b.interval); delay < floor {
		delay = floor
	}
	if b.jitter > 0 {
		delay *= 1 + rand.Float64()*b.jitter //nolint:gosec // G404: jitter only, not security-sensitive
		if delay > float64(maxBackoffStep) {
			delay = float64(maxBackoffStep)
		}
	}
	return time.Duration(delay)
}

func pow(base float64, exp float64) float64 {
	result := 1.0
	for ; exp >= 1; exp-- {
		result *= base
	}
	return result
}

// sleepBackoff waits one backoff step; reports false only on ctx done.
func sleepBackoff(ctx context.Context, b *backoff) bool {
	timer := time.NewTimer(b.next())
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
