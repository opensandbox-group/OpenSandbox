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
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

// TrackerConfig controls a fixed, wall-clock-aligned observation window.
// Thresholds only produce a shadow assessment.
type TrackerConfig struct {
	Window                   time.Duration
	MaxDistinctTargets       int
	MinAttempts              uint64
	MinDistinctTargets       int
	MinDistinctSignalTargets int
	DegradedFailureRatio     float64
}

// Snapshot is a bounded aggregate for the most recent fully elapsed window.
type Snapshot struct {
	WindowStart           time.Time
	Attempts              uint64
	SignalFailures        uint64
	DistinctTargets       int
	DistinctSignalTargets int
	Qualified             bool
	Degraded              bool
}

// Tracker keeps bounded, concurrency-safe TCP connection aggregates.
type Tracker struct {
	mu sync.RWMutex

	initialized bool
	config      TrackerConfig
	current     bucket
	completed   bucket
}

type bucket struct {
	start          time.Time
	attempts       uint64
	signalFailures uint64
	targets        map[string]struct{}
	signalTargets  map[string]struct{}
}

// NewTracker validates config and constructs an empty tracker.
func NewTracker(config TrackerConfig) (*Tracker, error) {
	switch {
	case config.Window <= 0:
		return nil, errors.New("connectivity tracker window must be positive")
	case config.MaxDistinctTargets <= 0:
		return nil, errors.New("connectivity tracker max distinct targets must be positive")
	case config.MinAttempts == 0:
		return nil, errors.New("connectivity tracker minimum attempts must be positive")
	case config.MinDistinctTargets <= 0:
		return nil, errors.New("connectivity tracker minimum distinct targets must be positive")
	case config.MinDistinctTargets > config.MaxDistinctTargets:
		return nil, errors.New("connectivity tracker minimum distinct targets exceeds cap")
	case config.MinDistinctSignalTargets <= 0:
		return nil, errors.New("connectivity tracker minimum distinct signal targets must be positive")
	case config.MinDistinctSignalTargets > config.MaxDistinctTargets:
		return nil, errors.New("connectivity tracker minimum distinct signal targets exceeds cap")
	case math.IsNaN(config.DegradedFailureRatio) || config.DegradedFailureRatio <= 0 || config.DegradedFailureRatio > 1:
		return nil, errors.New("connectivity tracker degraded failure ratio must be in (0,1]")
	}

	return &Tracker{config: config}, nil
}

// Observe records an observation in its fixed, wall-clock-aligned window.
func (t *Tracker) Observe(observation Observation) {
	// Caller cancellation does not establish whether the network path was
	// healthy or degraded, so it must not dilute the assessed failure ratio.
	if observation.Result == ResultCanceled {
		return
	}

	windowStart := observation.At.Truncate(t.config.Window)
	target := normalizeTarget(observation.Target)

	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.initialized {
		t.initialized = true
		t.current = newBucket(windowStart)
		t.completed = newBucket(windowStart.Add(-t.config.Window))
	} else if windowStart.After(t.current.start) {
		completedStart := windowStart.Add(-t.config.Window)
		if t.current.start.Equal(completedStart) {
			t.completed = t.current
		} else {
			t.completed = newBucket(completedStart)
		}
		t.current = newBucket(windowStart)
	}
	if windowStart.Before(t.current.start) {
		if windowStart.Equal(t.completed.start) {
			t.addObservation(&t.completed, target, observation.Result)
		}
		return
	}

	t.addObservation(&t.current, target, observation.Result)
}

// Snapshot returns the most recent fully elapsed fixed window without changing
// tracker state.
func (t *Tracker) Snapshot(now time.Time) Snapshot {
	completedStart := now.Truncate(t.config.Window).Add(-t.config.Window)

	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.current.start.Equal(completedStart) {
		return t.snapshot(t.current)
	}
	if t.completed.start.Equal(completedStart) {
		return t.snapshot(t.completed)
	}
	return Snapshot{WindowStart: completedStart}
}

func (t *Tracker) snapshot(source bucket) Snapshot {
	snapshot := Snapshot{
		WindowStart:           source.start,
		Attempts:              source.attempts,
		SignalFailures:        source.signalFailures,
		DistinctTargets:       len(source.targets),
		DistinctSignalTargets: len(source.signalTargets),
	}
	snapshot.Qualified = snapshot.Attempts >= t.config.MinAttempts &&
		snapshot.DistinctTargets >= t.config.MinDistinctTargets
	if snapshot.Qualified {
		snapshot.Degraded = snapshot.DistinctSignalTargets >= t.config.MinDistinctSignalTargets &&
			float64(snapshot.SignalFailures)/float64(snapshot.Attempts) >= t.config.DegradedFailureRatio
	}
	return snapshot
}

func (t *Tracker) addObservation(destination *bucket, target string, result Result) {
	destination.attempts++
	if isDegradationSignal(result) {
		destination.signalFailures++
		addTarget(destination.signalTargets, target, t.config.MaxDistinctTargets)
	}
	addTarget(destination.targets, target, t.config.MaxDistinctTargets)
}

func newBucket(start time.Time) bucket {
	return bucket{
		start:         start,
		targets:       make(map[string]struct{}),
		signalTargets: make(map[string]struct{}),
	}
}

func addTarget(targets map[string]struct{}, target string, limit int) {
	if target != "" && len(targets) < limit {
		targets[target] = struct{}{}
	}
}

func normalizeTarget(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		// Distinct targets represent network hosts. Multiple ports on one
		// Sandbox Pod must not satisfy the cross-target qualification guard.
		return canonicalHost(host)
	}
	return canonicalHost(strings.Trim(address, "[]"))
}

func canonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
