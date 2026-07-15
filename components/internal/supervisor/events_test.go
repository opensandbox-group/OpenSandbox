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
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventWriter_Roundtrip(t *testing.T) {
	var buf bytes.Buffer
	fixed := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	ew := newEventWriter(&buf, "egress", func() time.Time { return fixed })

	if err := ew.emit(Event{Event: EventStart, PID: 1234, Attempt: 1, Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := ew.emit(Event{Event: EventExit, PID: 1234, Gen: 1, ExitCode: intPtr(0), DurationMS: 1500}); err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(strings.NewReader(buf.String()))

	if !sc.Scan() {
		t.Fatal("expected first line")
	}
	var e1 Event
	if err := json.Unmarshal(sc.Bytes(), &e1); err != nil {
		t.Fatal(err)
	}
	if e1.Event != EventStart || e1.PID != 1234 || e1.Name != "egress" || !e1.TS.Equal(fixed) {
		t.Fatalf("e1 unexpected: %+v", e1)
	}

	if !sc.Scan() {
		t.Fatal("expected second line")
	}
	var e2 Event
	if err := json.Unmarshal(sc.Bytes(), &e2); err != nil {
		t.Fatal(err)
	}
	if e2.Event != EventExit || e2.ExitCode == nil || *e2.ExitCode != 0 || e2.DurationMS != 1500 {
		t.Fatalf("e2 unexpected: %+v", e2)
	}
}

func TestEventWriter_ConcurrentWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	ew := newEventWriter(&buf, "n", time.Now)

	const goroutines = 16
	const perGoroutine = 64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := ew.emit(Event{Event: EventBackoff, SleepMS: 1}); err != nil {
					t.Errorf("emit: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	count := 0
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d not valid JSON: %v\nline=%q", count, err, sc.Text())
		}
		count++
	}
	if count != goroutines*perGoroutine {
		t.Fatalf("got %d events, want %d", count, goroutines*perGoroutine)
	}
}

func TestEventWriter_NilSafe(t *testing.T) {
	var ew *eventWriter
	if err := ew.emit(Event{Event: EventStart}); err != nil {
		t.Fatal(err)
	}
}
