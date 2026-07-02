// Copyright 2026 Alibaba Group Holding Ltd.
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

package opensandbox

import (
	"sync"
	"time"
)

// reconcileState tracks consecutive warmup failures and backoff.
type reconcileState struct {
	mu              sync.Mutex
	threshold       int
	failureCount    int
	lastError       string
	backoffUntil    time.Time
	backoffAttempts int
}

func newReconcileState(threshold int) *reconcileState {
	return &reconcileState{threshold: threshold}
}

func (r *reconcileState) recordSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failureCount = 0
	r.lastError = ""
	r.backoffUntil = time.Time{}
	r.backoffAttempts = 0
}

func (r *reconcileState) recordFailure(errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failureCount++
	r.lastError = errMsg
	if r.failureCount >= r.threshold {
		r.backoffAttempts++
		// Clamp shift to prevent overflow of time.Duration (max ~292 years)
		shift := r.backoffAttempts - 1
		if shift > 20 {
			shift = 20
		}
		backoff := time.Duration(1<<uint(shift)) * 30 * time.Second
		maxBackoff := 24 * time.Hour
		if backoff <= 0 || backoff > maxBackoff {
			backoff = maxBackoff
		}
		r.backoffUntil = time.Now().Add(backoff)
	}
}

func (r *reconcileState) isBackoffActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.backoffUntil.IsZero() {
		return false
	}
	return time.Now().Before(r.backoffUntil)
}

func (r *reconcileState) isDegraded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failureCount >= r.threshold
}

func (r *reconcileState) snapshot() (failureCount int, lastError string, backoffActive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failureCount, r.lastError, !r.backoffUntil.IsZero() && time.Now().Before(r.backoffUntil)
}
