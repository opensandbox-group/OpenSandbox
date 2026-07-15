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
	"math/rand/v2"
	"time"
)

// nextBackoff returns the next sleep duration given the previous one.
//
// prev <= 0 returns min. Otherwise the previous value is doubled, clamped to
// [min, max], and perturbed by ±jitter*value. The result is clamped a second
// time so jitter cannot exceed max or go below 1ns.
func nextBackoff(prev, min, max time.Duration, jitter float64, rng func() float64) time.Duration {
	if prev <= 0 {
		return clampJitter(min, min, max, jitter, rng)
	}
	d := prev * 2
	if d < min {
		d = min
	}
	if d > max {
		d = max
	}
	return clampJitter(d, min, max, jitter, rng)
}

func clampJitter(d, min, max time.Duration, jitter float64, rng func() float64) time.Duration {
	if jitter <= 0 {
		return d
	}
	span := float64(d) * jitter
	// rng returns [0,1); shift to [-1,1).
	delta := time.Duration((rng()*2 - 1) * span)
	out := d + delta
	if out < min {
		out = min
	}
	if out > max {
		out = max
	}
	if out < time.Nanosecond {
		out = time.Nanosecond
	}
	return out
}

// defaultRNG wraps math/rand/v2 for production use.
func defaultRNG() float64 { return rand.Float64() }
