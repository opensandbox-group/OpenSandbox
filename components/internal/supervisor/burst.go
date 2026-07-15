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

package supervisor

import (
	"time"
)

// burstTracker counts launches within a sliding window. The ring is sized
// to BurstMax so we discard older entries automatically; exceeded() asks
// "are the BurstMax most-recent launches all within BurstWindow?".
type burstTracker struct {
	max    int
	window time.Duration
	now    func() time.Time
	ring   []time.Time
	idx    int
	filled int
}

func newBurstTracker(max int, window time.Duration, now func() time.Time) *burstTracker {
	if max < 1 {
		max = 1
	}
	return &burstTracker{
		max:    max,
		window: window,
		now:    now,
		ring:   make([]time.Time, max),
	}
}

func (b *burstTracker) record() {
	b.ring[b.idx] = b.now()
	b.idx = (b.idx + 1) % b.max
	if b.filled < b.max {
		b.filled++
	}
}

// exceeded reports whether the oldest of the last BurstMax launches falls
// inside BurstWindow. With BurstMax=10 and window=5m, this triggers once 10
// launches have all occurred within a 5-minute span.
func (b *burstTracker) exceeded() bool {
	if b.filled < b.max {
		return false
	}
	oldest := b.ring[b.idx] // next slot to overwrite = oldest entry
	return b.now().Sub(oldest) <= b.window
}

func (b *burstTracker) count() int {
	cutoff := b.now().Add(-b.window)
	n := 0
	for i := 0; i < b.filled; i++ {
		if !b.ring[i].Before(cutoff) {
			n++
		}
	}
	return n
}
