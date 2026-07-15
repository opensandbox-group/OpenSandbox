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
	"testing"
	"time"
)

// fixedRNG returns a constant value to make jitter deterministic.
func fixedRNG(v float64) func() float64 { return func() float64 { return v } }

func TestNextBackoff_NoJitterDoublesAndClamps(t *testing.T) {
	min := 1 * time.Second
	max := 8 * time.Second
	noJitter := fixedRNG(0.5) // would be zero delta anyway with jitter=0

	cases := []struct {
		prev time.Duration
		want time.Duration
	}{
		{0, min},                      // initial
		{-1 * time.Second, min},       // negative
		{500 * time.Millisecond, min}, // below min after doubling -> clamp up
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, max}, // would be 16s -> clamp to max
		{100 * time.Second, max},
	}
	for _, c := range cases {
		got := nextBackoff(c.prev, min, max, 0, noJitter)
		if got != c.want {
			t.Errorf("nextBackoff(prev=%s) = %s, want %s", c.prev, got, c.want)
		}
	}
}

func TestNextBackoff_JitterWithinBounds(t *testing.T) {
	min := 1 * time.Second
	max := 10 * time.Second
	jitter := 0.5

	// rng=0   -> delta = -1 * 0.5 * d = -50%
	// rng=0.5 -> delta = 0
	// rng=1-  -> delta ≈ +50%
	// Approximate by checking the two extremes plus the midpoint.
	for _, v := range []float64{0.0, 0.5, 0.9999} {
		got := nextBackoff(2*time.Second, min, max, jitter, fixedRNG(v))
		// Base after doubling = 4s. jitter range ±2s. So [2s, 6s].
		if got < 2*time.Second || got > 6*time.Second {
			t.Errorf("rng=%v: got %s, want in [2s, 6s]", v, got)
		}
	}
}

func TestNextBackoff_JitterClampsToMax(t *testing.T) {
	min := 1 * time.Second
	max := 5 * time.Second
	// Base = max after doubling. Positive jitter must not push above max.
	got := nextBackoff(max, min, max, 0.5, fixedRNG(0.9999))
	if got > max {
		t.Errorf("got %s > max %s", got, max)
	}
}

func TestNextBackoff_JitterClampsToMin(t *testing.T) {
	min := 2 * time.Second
	max := 10 * time.Second
	// Base = min. Negative jitter must not push below min.
	got := nextBackoff(0, min, max, 0.5, fixedRNG(0.0))
	if got < min {
		t.Errorf("got %s < min %s", got, min)
	}
}

// Spec.BackoffJitter is *float64 specifically so callers can pass &zero to
// disable jitter; verify the underlying nextBackoff respects jitter=0.
func TestNextBackoff_JitterZeroDisablesJitter(t *testing.T) {
	min := 1 * time.Second
	max := 16 * time.Second
	// With jitter=0, output must be the exact doubled value regardless of rng.
	for _, rng := range []float64{0.0, 0.5, 0.9999} {
		got := nextBackoff(2*time.Second, min, max, 0, fixedRNG(rng))
		if got != 4*time.Second {
			t.Errorf("rng=%v: got %s, want exactly 4s", rng, got)
		}
	}
}

func TestApplyDefaults_BackoffJitter(t *testing.T) {
	cases := []struct {
		name string
		in   *float64
		want float64
	}{
		{"nil applies default", nil, defaultBackoffJitter},
		{"zero stays zero", floatPtr(0), 0},
		{"explicit value preserved", floatPtr(0.25), 0.25},
		{"negative clamped to zero", floatPtr(-0.5), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Spec{Cmd: "/bin/true", BackoffJitter: c.in}
			s.applyDefaults()
			if s.BackoffJitter == nil {
				t.Fatal("BackoffJitter still nil after applyDefaults")
			}
			if *s.BackoffJitter != c.want {
				t.Errorf("BackoffJitter = %v, want %v", *s.BackoffJitter, c.want)
			}
		})
	}
}

func floatPtr(v float64) *float64 { return &v }
