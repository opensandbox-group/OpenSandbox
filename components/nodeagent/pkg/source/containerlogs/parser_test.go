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

package containerlogs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAndAssembleCRI(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(1024, time.Second)
	first, err := parseCRILine([]byte("2026-07-23T10:00:01.000000000Z stdout P hello \n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := assembler.consume(first, sourceSpan{FileID: "f", Path: "0.log", EndOffset: 10}, now); got != nil {
		t.Fatalf("partial emitted early: %+v", got)
	}
	last, err := parseCRILine([]byte("2026-07-23T10:00:01.000000005Z stdout F world\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := assembler.consume(last, sourceSpan{FileID: "f", Path: "0.log", StartOffset: 10, EndOffset: 20}, now)
	if got == nil || string(got.body) != "hello world" || got.stream != "stdout" || got.spans[len(got.spans)-1].EndOffset != 20 {
		t.Fatalf("unexpected assembled record: %+v", got)
	}
}

func TestPartialAssemblerSeparatesContainerRestarts(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(1024, time.Second)
	partialZero, err := parseCRILine([]byte("2026-07-23T10:00:00Z stdout P zero-"))
	if err != nil {
		t.Fatal(err)
	}
	partialOne, err := parseCRILine([]byte("2026-07-23T10:00:01Z stdout P one-"))
	if err != nil {
		t.Fatal(err)
	}
	final, err := parseCRILine([]byte("2026-07-23T10:00:02Z stdout F done"))
	if err != nil {
		t.Fatal(err)
	}
	if record := assembler.consume(partialZero, sourceSpan{FileID: "zero-rotated", Path: "0.log.20260723", EndOffset: 10, restart: "0"}, now); record != nil {
		t.Fatalf("restart zero partial emitted early: %+v", record)
	}
	if record := assembler.consume(partialOne, sourceSpan{FileID: "one", Path: "1.log", EndOffset: 10, restart: "1"}, now); record != nil {
		t.Fatalf("restart one partial emitted early: %+v", record)
	}
	if got := assembler.consume(final, sourceSpan{FileID: "one", Path: "1.log", StartOffset: 10, EndOffset: 20, restart: "1"}, now); got == nil || string(got.body) != "one-done" {
		t.Fatalf("restart one record=%+v", got)
	}
	if got := assembler.consume(final, sourceSpan{FileID: "zero-base", Path: "0.log", StartOffset: 10, EndOffset: 20, restart: "0"}, now); got == nil || string(got.body) != "zero-done" {
		t.Fatalf("restart zero record=%+v", got)
	}
}

func TestPartialAssemblerSeparatesGapRepairEpoch(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(1024, time.Second)
	partial := criLine{Timestamp: now, Stream: "stdout", Partial: true, Body: []byte("original-")}
	if record := assembler.consume(partial, sourceSpan{FileID: "file", Path: "0.log", EndOffset: 10}, now); record != nil {
		t.Fatalf("original partial emitted early: %+v", record)
	}
	repaired := criLine{Timestamp: now, Stream: "stdout", Partial: true, Body: []byte("repaired-")}
	repairSpan := sourceSpan{FileID: "file", Path: "0.log", EndOffset: 10, RepairGapID: "gap"}
	if record := assembler.consume(repaired, repairSpan, now); record != nil {
		t.Fatalf("repair partial emitted early: %+v", record)
	}
	final := criLine{Timestamp: now, Stream: "stdout", Body: []byte("done")}
	repairSpan.StartOffset = 10
	repairSpan.EndOffset = 20
	record := assembler.consume(final, repairSpan, now)
	if record == nil || string(record.body) != "repaired-done" || len(record.spans) != 1 || record.spans[0].RepairGapID != "gap" {
		t.Fatalf("repair record=%+v", record)
	}
	remaining := assembler.finish()
	if len(remaining) != 1 || string(remaining[0].body) != "original-[opensandbox: incomplete-partial]" {
		t.Fatalf("original partials=%+v", remaining)
	}
}

func TestParseCRIAcceptsExtendedTags(t *testing.T) {
	line, err := parseCRILine([]byte("2026-07-23T10:00:01Z stdout F:runtime-attribute hello\n"))
	if err != nil || line.Partial || string(line.Body) != "hello" {
		t.Fatalf("line=%+v err=%v", line, err)
	}
}

func TestPartialTimeout(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(1024, time.Second)
	line, err := parseCRILine([]byte("2026-07-23T10:00:01Z stderr P partial\n"))
	if err != nil {
		t.Fatal(err)
	}
	assembler.consume(line, sourceSpan{FileID: "f", Path: "0.log", EndOffset: 7}, now)
	got := assembler.expired(now.Add(2 * time.Second))
	if len(got) != 1 || string(got[0].body) != "partial[opensandbox: partial-timeout]" {
		t.Fatalf("unexpected timeout output: %+v", got)
	}
	continued, err := parseCRILine([]byte("2026-07-23T10:00:02Z stderr F rest\n"))
	if err != nil {
		t.Fatal(err)
	}
	record := assembler.consume(continued, sourceSpan{FileID: "f", Path: "0.log", StartOffset: 7, EndOffset: 14}, now.Add(3*time.Second))
	if record == nil || string(record.body) != "[opensandbox: continuation-after-timeout]rest" {
		t.Fatalf("continuation=%+v", record)
	}
}

func TestContinuationMarkersAreBounded(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(1024, time.Second)
	line, err := parseCRILine([]byte("2026-07-23T10:00:01Z stdout P partial\n"))
	if err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < maxContinuationMarkers+32; restart++ {
		assembler.consume(line, sourceSpan{FileID: fmt.Sprintf("file-%d", restart), Path: fmt.Sprintf("%d.log", restart), EndOffset: 1, restart: fmt.Sprintf("%d", restart)}, now)
	}
	assembler.expired(now.Add(2 * time.Second))
	if len(assembler.continuation) != maxContinuationMarkers {
		t.Fatalf("continuation markers=%d, want %d", len(assembler.continuation), maxContinuationMarkers)
	}
	oldest := partialKey{restart: "oldest", stream: "stdout"}
	ordered := newAssembler(1024, time.Second)
	ordered.setContinuation(oldest, "oldest")
	for index := 0; index < maxContinuationMarkers; index++ {
		ordered.setContinuation(partialKey{restart: fmt.Sprintf("new-%d", index), stream: "stdout"}, "new")
	}
	if _, found := ordered.continuation[oldest]; found {
		t.Fatal("oldest continuation marker was not evicted")
	}
	assembler.finish()
	if len(assembler.continuation) != maxContinuationMarkers {
		t.Fatalf("finish retained %d continuation markers, want %d", len(assembler.continuation), maxContinuationMarkers)
	}
}

func TestPartialAssemblerRetainsContinuationForLateData(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(1024, time.Second)
	key := partialKey{restart: "restart", stream: "stdout"}
	assembler.setContinuation(key, "[opensandbox: continuation-after-timeout]")
	if records := assembler.finish(); len(records) != 0 {
		t.Fatalf("finish records=%+v, want none", records)
	}
	line := criLine{Timestamp: now, Stream: "stdout", Body: []byte("late")}
	record := assembler.consume(line, sourceSpan{FileID: "file", Path: "0.log", EndOffset: 1, restart: key.restart}, now)
	if record == nil || string(record.body) != "[opensandbox: continuation-after-timeout]late" {
		t.Fatalf("late continuation=%+v", record)
	}
}

func TestPartialAssemblerEmitsInCreationOrder(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name string
		emit func(*assembler) []assembled
	}{
		{name: "expired", emit: func(a *assembler) []assembled { return a.expired(now.Add(2 * time.Second)) }},
		{name: "finish", emit: func(a *assembler) []assembled { return a.finish() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assembler := newAssembler(1024, time.Second)
			for index, body := range []string{"first", "second", "third"} {
				line := criLine{Timestamp: now.Add(time.Duration(2-index) * time.Second), Stream: "stdout", Partial: true, Body: []byte(body)}
				span := sourceSpan{FileID: body, Path: fmt.Sprintf("%d.log", index), EndOffset: 1, restart: body}
				if record := assembler.consume(line, span, now); record != nil {
					t.Fatalf("partial %q emitted early: %+v", body, record)
				}
			}
			records := tt.emit(assembler)
			if len(records) != 3 {
				t.Fatalf("records=%d, want 3", len(records))
			}
			for index, want := range []string{"first", "second", "third"} {
				if got := string(records[index].body); !strings.HasPrefix(got, want) {
					t.Fatalf("record %d body=%q, want prefix %q", index, got, want)
				}
			}
		})
	}
}

func TestTruncationProducesDropReason(t *testing.T) {
	line, err := parseCRILine([]byte("2026-07-23T10:00:00Z stdout F abcdef\n"))
	if err != nil {
		t.Fatal(err)
	}
	record := newAssembler(3, time.Second).consume(line, sourceSpan{FileID: "f", Path: "0.log", EndOffset: 10}, time.Now())
	if record == nil || string(record.body) != "abc[opensandbox: truncated]" || len(record.dropReasons) != 1 || record.dropReasons[0] != "line-truncated" {
		t.Fatalf("record=%+v", record)
	}
}

func TestTruncatedPartialKeepsTerminalMarker(t *testing.T) {
	now := time.Now()
	assembler := newAssembler(3, time.Second)
	line, err := parseCRILine([]byte("2026-07-23T10:00:00Z stdout P abcdef\n"))
	if err != nil {
		t.Fatal(err)
	}
	assembler.consume(line, sourceSpan{FileID: "f", Path: "0.log", EndOffset: 10}, now)
	records := assembler.expired(now.Add(2 * time.Second))
	if len(records) != 1 || string(records[0].body) != "abc[opensandbox: truncated][opensandbox: partial-timeout]" {
		t.Fatalf("records=%+v", records)
	}
}

func TestSpanLimitIncludesFinalCoveredFragment(t *testing.T) {
	assembler := newAssembler(8192, time.Second)
	line, err := parseCRILine([]byte("2026-07-23T10:00:00Z stdout P x\n"))
	if err != nil {
		t.Fatal(err)
	}
	var record *assembled
	for index := 0; index < 4097; index++ {
		record = assembler.consume(line, sourceSpan{FileID: "f", Path: "0.log", StartOffset: int64(index), EndOffset: int64(index + 1)}, time.Now())
	}
	if record == nil || len(record.spans) != 1 || record.spans[0].EndOffset != 4097 || len(record.body) < 4097 || record.body[4096] != 'x' {
		t.Fatalf("record body=%d spans=%d", len(record.body), len(record.spans))
	}
	next := assembler.consume(line, sourceSpan{FileID: "f", Path: "0.log", StartOffset: 4097, EndOffset: 4098}, time.Now())
	if next != nil {
		t.Fatalf("continuation emitted early: %+v", next)
	}
	final, err := parseCRILine([]byte("2026-07-23T10:00:00Z stdout F y\n"))
	if err != nil {
		t.Fatal(err)
	}
	next = assembler.consume(final, sourceSpan{FileID: "f", Path: "0.log", StartOffset: 4098, EndOffset: 4099}, time.Now())
	if next == nil || string(next.body) != "[opensandbox: continuation-after-span-limit]xy" {
		t.Fatalf("continuation=%+v", next)
	}
}

func TestDiscoverFilesOrdersRotationsBeforeBase(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"1.log", "0.log", "0.log.20260723", "0.log.20260722", "0.log.20260721.gz", "noise"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := discoverDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0.log.20260722", "0.log.20260723", "0.log", "1.log"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFingerprintRejectsInvalidPersistedHashLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0.log")
	if err := os.WriteFile(path, []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, hashBytes := range []int{-2, maxFingerprintHashBytes + 1} {
		if _, err := fingerprintFile(file, hashBytes); err == nil {
			t.Fatalf("fingerprint accepted hash length %d", hashBytes)
		}
	}
}
