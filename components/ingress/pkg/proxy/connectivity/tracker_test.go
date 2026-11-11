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

package connectivity

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func validTrackerConfig() TrackerConfig {
	return TrackerConfig{
		Window:                   time.Minute,
		MaxDistinctTargets:       8,
		MinAttempts:              4,
		MinDistinctTargets:       2,
		MinDistinctSignalTargets: 2,
		DegradedFailureRatio:     0.5,
	}
}

func TestNewTrackerRejectsInvalidConfig(t *testing.T) {
	tests := []func(*TrackerConfig){
		func(config *TrackerConfig) { config.Window = 0 },
		func(config *TrackerConfig) { config.MaxDistinctTargets = 0 },
		func(config *TrackerConfig) { config.MinAttempts = 0 },
		func(config *TrackerConfig) { config.MinDistinctTargets = 9 },
		func(config *TrackerConfig) { config.MinDistinctSignalTargets = 9 },
		func(config *TrackerConfig) { config.DegradedFailureRatio = math.NaN() },
	}
	for i, mutate := range tests {
		config := validTrackerConfig()
		mutate(&config)
		if _, err := NewTracker(config); err == nil {
			t.Fatalf("case %d: NewTracker() returned nil error", i)
		}
	}
}

func TestTrackerUsesMostRecentCompletedWindow(t *testing.T) {
	tracker, err := NewTracker(validTrackerConfig())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	observations := []Observation{
		{At: start, Target: "10.0.0.1:80", Result: ResultTimeout},
		{At: start, Target: "10.0.0.2:80", Result: ResultUnreachable},
		{At: start, Target: "10.0.0.1:443", Result: ResultSuccess},
		{At: start, Target: "10.0.0.2:443", Result: ResultRefused},
	}
	for _, observation := range observations {
		tracker.Observe(observation)
	}

	if partial := tracker.Snapshot(start.Add(30 * time.Second)); partial.Attempts != 0 {
		t.Fatalf("partial window was exposed: %+v", partial)
	}
	completed := tracker.Snapshot(start.Add(time.Minute))
	if !completed.Qualified || !completed.Degraded || completed.Attempts != 4 || completed.SignalFailures != 2 || completed.DistinctTargets != 2 || completed.DistinctSignalTargets != 2 {
		t.Fatalf("unexpected completed snapshot: %+v", completed)
	}
}

func TestTrackerExcludesCanceledAttemptsFromAssessment(t *testing.T) {
	tracker, err := NewTracker(validTrackerConfig())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	tracker.Observe(Observation{At: start, Target: "10.0.0.1:80", Result: ResultCanceled})

	snapshot := tracker.Snapshot(start.Add(time.Minute))
	if snapshot.Attempts != 0 || snapshot.DistinctTargets != 0 {
		t.Fatalf("canceled attempt changed assessment: %+v", snapshot)
	}
}

func TestTrackerAcceptsLateObservationForCompletedWindow(t *testing.T) {
	tracker, err := NewTracker(validTrackerConfig())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	tracker.Observe(Observation{At: start.Add(time.Minute), Target: "10.0.0.2:80", Result: ResultSuccess})
	tracker.Observe(Observation{At: start.Add(time.Minute - time.Nanosecond), Target: "10.0.0.1:80", Result: ResultTimeout})

	snapshot := tracker.Snapshot(start.Add(time.Minute))
	if snapshot.Attempts != 1 || snapshot.SignalFailures != 1 || snapshot.DistinctTargets != 1 {
		t.Fatalf("late observation was not retained: %+v", snapshot)
	}
}

func TestTrackerAcceptsZeroObservationTime(t *testing.T) {
	tracker, err := NewTracker(validTrackerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(Observation{Target: "10.0.0.1:80", Result: ResultSuccess})
	tracker.Observe(Observation{Target: "10.0.0.2:80", Result: ResultSuccess})

	if tracker.current.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", tracker.current.attempts)
	}
}

func TestTrackerDoesNotQualifyOneTarget(t *testing.T) {
	tracker, err := NewTracker(validTrackerConfig())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	for range 4 {
		tracker.Observe(Observation{At: start, Target: "10.0.0.1:80", Result: ResultTimeout})
	}

	snapshot := tracker.Snapshot(start.Add(time.Minute))
	if snapshot.Qualified || snapshot.Degraded || snapshot.DistinctTargets != 1 {
		t.Fatalf("unexpected single-target snapshot: %+v", snapshot)
	}
}

func TestTrackerBoundsTargetsAndSupportsConcurrentObservation(t *testing.T) {
	config := validTrackerConfig()
	config.MaxDistinctTargets = 2
	config.MinDistinctTargets = 2
	config.MinDistinctSignalTargets = 2
	tracker, err := NewTracker(config)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	const workers = 8
	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(target int) {
			defer waitGroup.Done()
			tracker.Observe(Observation{At: start, Target: fmt.Sprintf("10.0.0.%d", target+1), Result: ResultTimeout})
		}(worker)
	}
	waitGroup.Wait()

	snapshot := tracker.Snapshot(start.Add(time.Minute))
	if snapshot.Attempts != workers || snapshot.SignalFailures != workers || snapshot.DistinctTargets != 2 || snapshot.DistinctSignalTargets != 2 {
		t.Fatalf("unexpected bounded snapshot: %+v", snapshot)
	}
}

func TestTrackerCanonicalizesEquivalentIPv6Targets(t *testing.T) {
	tracker, err := NewTracker(validTrackerConfig())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	tracker.Observe(Observation{At: start, Target: "[::1]:80", Result: ResultSuccess})
	tracker.Observe(Observation{At: start, Target: "[0:0:0:0:0:0:0:1]:443", Result: ResultTimeout})

	snapshot := tracker.Snapshot(start.Add(time.Minute))
	if snapshot.DistinctTargets != 1 || snapshot.DistinctSignalTargets != 1 {
		t.Fatalf("equivalent IPv6 targets were counted separately: %+v", snapshot)
	}
}
