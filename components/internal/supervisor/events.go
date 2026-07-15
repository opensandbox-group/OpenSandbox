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
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Event kinds. Stable string values; downstream log pipelines may filter on them.
const (
	EventStart     = "start"
	EventExit      = "exit"
	EventPreStart  = "prestart"
	EventPostExit  = "postexit"
	EventBackoff   = "backoff"
	EventStable    = "stable"
	EventBurstExit = "burst_exit"
	EventShutdown  = "shutdown"
)

// Event is one structured record in the supervisor's event log. Only set
// fields are emitted (omitempty everywhere) so different kinds share one type.
type Event struct {
	TS           time.Time `json:"ts"`
	Name         string    `json:"name,omitempty"`
	Event        string    `json:"event"`
	PID          int       `json:"pid,omitempty"`
	Gen          uint64    `json:"gen,omitempty"`
	Attempt      int       `json:"attempt,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	Signal       string    `json:"signal,omitempty"`
	DurationMS   int64     `json:"duration_ms,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	SleepMS      int64     `json:"sleep_ms,omitempty"`
	NextAttempt  int       `json:"next_attempt,omitempty"`
	Hook         string    `json:"hook,omitempty"`
	Attempts     int       `json:"attempts,omitempty"`
	Window       string    `json:"window,omitempty"`
	Error        string    `json:"error,omitempty"`
	ResetBackoff bool      `json:"reset_backoff,omitempty"`
}

// eventWriter serializes Event writes through a mutex; concurrent writers
// will not interleave bytes mid-line.
type eventWriter struct {
	mu   sync.Mutex
	w    io.Writer
	name string
	now  func() time.Time
}

func newEventWriter(w io.Writer, name string, now func() time.Time) *eventWriter {
	return &eventWriter{w: w, name: name, now: now}
}

// emit fills TS/Name and writes the event followed by a newline. Errors are
// returned to the caller so the supervisor can surface them; callers may
// choose to ignore (event logging must not abort the main loop).
func (ew *eventWriter) emit(e Event) error {
	if ew == nil || ew.w == nil {
		return nil
	}
	if e.TS.IsZero() {
		e.TS = ew.now()
	}
	if e.Name == "" {
		e.Name = ew.name
	}
	buf, err := json.Marshal(e)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	ew.mu.Lock()
	defer ew.mu.Unlock()
	_, err = ew.w.Write(buf)
	return err
}

// intPtr is a small helper for Event.ExitCode (which is *int so 0 vs unset
// is distinguishable in JSON output).
func intPtr(v int) *int { return &v }
