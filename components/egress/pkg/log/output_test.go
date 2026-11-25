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

package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type testDestination func([]byte) (int, error)

func (f testDestination) Write(p []byte) (int, error) { return f(p) }
func (testDestination) Sync() error                   { panic("unexpected physical Sync") }

type outputGate struct {
	entered, release chan struct{}
	once             sync.Once
}

func newOutputGate() *outputGate {
	return &outputGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *outputGate) block() {
	close(g.entered)
	<-g.release
}

func (g *outputGate) open() { g.once.Do(func() { close(g.release) }) }

func awaitOutput(t *testing.T, ch <-chan struct{}, activity string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", activity)
	}
}

func closeTestOutput(o *Output) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return o.Close(ctx)
}

func testOutput(t *testing.T, out, errs testDestination, closeDestinations func() error, fields ...slogger.Field) *Output {
	t.Helper()
	o := newOutput(slogger.Config{Level: "info", OutputPaths: []string{"stdout"}}, outputDestinations{
		out: out, errors: errs, close: closeDestinations,
	}, fields...)
	t.Cleanup(func() {
		if err := closeTestOutput(o); errors.Is(err, context.DeadlineExceeded) {
			t.Error("output worker did not exit")
		}
	})
	return o
}

func closeDiscardedOutput() error { return nil }

func outputRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var record map[string]any
		if err := dec.Decode(&record); errors.Is(err, io.EOF) {
			return records
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
}

func requireOutputSync(t *testing.T, l slogger.Logger) {
	t.Helper()
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestOutputCountBoundsAreIndependentDuringStall(t *testing.T) {
	var physical bytes.Buffer
	gate := newOutputGate()
	o := testOutput(t, func(p []byte) (int, error) {
		if bytes.Contains(p, []byte(`"msg":"hold sink"`)) {
			gate.block()
		}
		return physical.Write(p)
	}, io.Discard.Write, closeDiscardedOutput, slogger.Field{Key: "sandbox_id", Value: "ordinary-and-l4"})
	defer gate.open()

	o.L4Logger().Warnf("hold sink")
	awaitOutput(t, gate.entered, "blocked physical L4 write")
	admitted := make(chan struct{})
	go func() {
		defer close(admitted)
		for i := range 4096 {
			o.L4Logger().Warnf("l4:%d", i)
		}
		o.L4Logger().Warnf("l4 overflow")
		for i := range 256 {
			o.OrdinaryLogger().Infof("ordinary:%d", i)
		}
		o.OrdinaryLogger().Warnf("ordinary overflow 1")
		o.OrdinaryLogger().Warnf("ordinary overflow 2")
	}()
	awaitOutput(t, admitted, "both producers with a blocked sink")
	gate.open()
	if err := closeTestOutput(o); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	gaps := map[string]float64{}
	for _, record := range outputRecords(t, physical.Bytes()) {
		if record["logger"] != "opensandbox.egress" || record["sandbox_id"] != "ordinary-and-l4" {
			t.Fatalf("missing common identity: %v", record)
		}
		message := record["msg"].(string)
		switch {
		case strings.HasPrefix(message, "ordinary:"):
			counts["ordinary"]++
		case strings.HasPrefix(message, "l4:"):
			counts["l4"]++
		case message == "hold sink":
			counts["in-flight"]++
		case record["opensandbox.event"] == "egress.log.output_gap":
			lane := record["egress.log.lane"].(string)
			delta := record["egress.log.dropped_records"].(float64)
			if record["level"] != "warn" || record["egress.log.dropped_records_total"] != delta || record["network.policy.decision"] != nil || gaps[lane] != 0 {
				t.Fatalf("wrong gap: %v", record)
			}
			gaps[lane] = delta
		default:
			t.Fatalf("unexpected output record: %v", record)
		}
	}
	if counts["ordinary"] != 256 || counts["l4"] != 4096 || counts["in-flight"] != 1 || gaps["ordinary"] != 2 || gaps["l4"] != 1 {
		t.Fatalf("delivered=%v admission drops=%v", counts, gaps)
	}
}

func TestOutputByteBoundsAndOwnedCopies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int
		lane  func(*Output) *outputLane
	}{
		{"ordinary", 1 << 20, func(o *Output) *outputLane { return &o.ordinary }},
		{"l4", 4 << 20, func(o *Output) *outputLane { return &o.l4 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var physical bytes.Buffer
			gate := newOutputGate()
			o := testOutput(t, func(p []byte) (int, error) {
				if bytes.Contains(p, []byte(`"msg":"hold sink"`)) {
					gate.block()
				}
				return physical.Write(p)
			}, io.Discard.Write, closeDiscardedOutput)
			defer gate.open()
			o.L4Logger().Warnf("hold sink")
			awaitOutput(t, gate.entered, "blocked write")

			// Drive the post-encoding adapter directly to hit the exact byte boundary
			// and mutate the writable buffer Zap would normally return to its pool.
			writer := laneWriter{owner: o, lane: tc.lane(o)}
			tooBig := make([]byte, tc.bytes+1)
			var n int
			var err error
			if allocs := testing.AllocsPerRun(10, func() { n, err = writer.Write(tooBig) }); allocs != 0 || n != len(tooBig) || err != nil {
				t.Fatalf("oversized admission: allocations=%v n=%d err=%v", allocs, n, err)
			}
			payload := strings.Repeat("x", tc.bytes/16-len("{\"msg\":\"\"}\n"))
			encoded := []byte("{\"msg\":\"" + payload + "\"}\n")
			// AllocsPerRun includes a warm-up call: 16 writes fill the budget exactly.
			if allocs := testing.AllocsPerRun(15, func() { n, err = writer.Write(encoded) }); allocs != 1 || n != len(encoded) || err != nil {
				t.Fatalf("accepted admission: allocations=%v n=%d err=%v", allocs, n, err)
			}
			clear(encoded)
			if _, err := writer.Write([]byte("{\"msg\":\"over byte bound\"}\n")); err != nil {
				t.Fatal(err)
			}

			gate.open()
			if err := closeTestOutput(o); err != nil {
				t.Fatal(err)
			}
			var kept, gaps int
			for _, r := range outputRecords(t, physical.Bytes()) {
				switch r["msg"] {
				case payload:
					kept++
				case "hold sink":
				default:
					gaps++
					if r["opensandbox.event"] != "egress.log.output_gap" || r["egress.log.lane"] != tc.name || r["egress.log.dropped_records"] != float64(12) {
						t.Fatalf("incorrect byte-bound loss diagnostic: %v", r)
					}
				}
			}
			if kept != 16 || gaps != 1 {
				t.Fatalf("preserved buffers=%d gap reports=%d", kept, gaps)
			}
		})
	}
}

func TestOutputEagerFieldEncoding(t *testing.T) {
	var physical bytes.Buffer
	gate := newOutputGate()
	startup := map[string]string{"value": "startup"}
	o := testOutput(t, func(p []byte) (int, error) {
		if bytes.Contains(p, []byte(`"msg":"hold sink"`)) {
			gate.block()
		}
		return physical.Write(p)
	}, io.Discard.Write, closeDiscardedOutput, slogger.Field{Key: "startup", Value: startup})
	defer gate.open()
	startup["value"] = "mutated startup"
	o.OrdinaryLogger().Warnf("hold sink")
	awaitOutput(t, gate.entered, "blocked write")

	with := map[string]string{"value": "at With"}
	l4 := o.L4Logger().With(slogger.Field{Key: "child", Value: with}).Named("child")
	with["value"] = "mutated With"
	call := map[string]string{"value": "at call"}
	zapLogger, ok := slogger.AsZapSugared(l4)
	if !ok {
		t.Fatal("expected zap logger")
	}
	zapLogger.Warnw("structured L4", "call", call)
	call["value"] = "mutated call"

	gate.open()
	if err := closeTestOutput(o); err != nil {
		t.Fatal(err)
	}
	records := outputRecords(t, physical.Bytes())
	if len(records) != 2 {
		t.Fatalf("records=%v", records)
	}
	for _, record := range records {
		if record["startup"].(map[string]any)["value"] != "startup" {
			t.Fatalf("startup fields were not captured: %v", record)
		}
	}
	r := records[1]
	if r["logger"] != "opensandbox.egress.child" || r["child"].(map[string]any)["value"] != "at With" || r["call"].(map[string]any)["value"] != "at call" {
		t.Fatalf("fields were not captured: %v", r)
	}
}

type fixedOutputClock struct{ zapcore.Clock }

func (fixedOutputClock) Now() time.Time { return time.Unix(1, 0) }

func TestOutputPreservesLevelsIdentityAndSampling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.json")
	o, err := NewOutput(slogger.Config{Level: "warn", OutputPaths: []string{path}}, slogger.Field{Key: "sandbox_id", Value: "sampling"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeTestOutput(o) })
	// Fix entry timestamps so the test never straddles a sampling window.
	clock := zap.WithClock(fixedOutputClock{zapcore.DefaultClock})
	for _, l := range []slogger.Logger{o.OrdinaryLogger(), o.L4Logger()} {
		sugar, ok := slogger.AsZapSugared(l)
		if !ok {
			t.Fatal("expected zap logger")
		}
		sugar = sugar.WithOptions(clock)
		sugar.Infof("filtered")
		for range 201 {
			sugar.Warnf("sample this")
		}
	}
	if err := closeTestOutput(o); err != nil {
		t.Fatal(err)
	}
	p, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records := outputRecords(t, p)
	if len(records) != 202 { // Each lane gets its first 100 and its 200th record.
		t.Fatalf("wrote %d records with the default sampler", len(records))
	}
	for _, r := range records {
		if r["msg"] != "sample this" || r["level"] != "warn" || r["logger"] != "opensandbox.egress" || r["sandbox_id"] != "sampling" {
			t.Fatalf("logging semantics changed: %v", r)
		}
	}
}

func TestOutputSyncSnapshotsOwnLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prior, future, l4 := newOutputGate(), newOutputGate(), newOutputGate()
		o := testOutput(t, func(p []byte) (int, error) {
			switch {
			case bytes.Contains(p, []byte(`"msg":"prior ordinary"`)):
				prior.block()
			case bytes.Contains(p, []byte(`"msg":"future ordinary"`)):
				future.block()
			case bytes.Contains(p, []byte(`"msg":"ongoing L4"`)):
				l4.block()
			}
			return len(p), nil
		}, io.Discard.Write, closeDiscardedOutput)
		defer prior.open()
		defer future.open()
		defer l4.open()

		ordinary := o.OrdinaryLogger()
		ordinary.Infof("prior ordinary")
		awaitOutput(t, prior.entered, "prior ordinary record")
		done := make(chan error, 1)
		go func() { done <- ordinary.Sync() }()
		synctest.Wait() // Sync has taken its snapshot and is now waiting for it.
		select {
		case err := <-done:
			t.Fatalf("Sync returned before its prior ordinary write completed: %v", err)
		default:
		}
		ordinary.Infof("future ordinary")
		prior.open()
		awaitOutput(t, future.entered, "later ordinary record")
		if err := <-done; err != nil {
			t.Fatalf("Sync waited for a later ordinary write: %v", err)
		}
		future.open()
		requireOutputSync(t, ordinary)

		o.L4Logger().Warnf("ongoing L4")
		awaitOutput(t, l4.entered, "blocked L4 write")
		requireOutputSync(t, ordinary)
		start := time.Now()
		if err := o.L4Logger().Sync(); !errors.Is(err, errOutputTimedOut) || time.Since(start) != 100*time.Millisecond {
			t.Fatalf("blocked L4 Sync took %s, result=%v", time.Since(start), err)
		}
	})
}

func TestOutputSyncIncludesPriorAdmissionDrops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := newOutputGate()
		o := testOutput(t, func(p []byte) (int, error) {
			if bytes.Contains(p, []byte(`"opensandbox.event":"egress.log.output_gap"`)) {
				gate.block()
			}
			return len(p), nil
		}, io.Discard.Write, closeDiscardedOutput)
		defer gate.open()

		o.L4Logger().Warnf("too large: %s", strings.Repeat("x", 4<<20))
		awaitOutput(t, gate.entered, "L4 gap diagnostic")
		requireOutputSync(t, o.OrdinaryLogger())
		done := make(chan error, 1)
		go func() { done <- o.L4Logger().Sync() }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Sync returned before the prior admission drop was handled: %v", err)
		default:
		}
		gate.open()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestOutputWriteFailuresAreLatchedAndReportedOnce(t *testing.T) {
	writeFailure := errors.New("destination full")
	for _, tc := range []struct {
		name       string
		lane       string
		writeErr   error
		want       error
		failReport bool
	}{
		{name: "l4 write failure", lane: "l4", writeErr: writeFailure, want: writeFailure},
		{name: "ordinary short write", lane: "ordinary", want: io.ErrShortWrite},
		{name: "nonrecursive failed report", lane: "ordinary", writeErr: writeFailure, want: writeFailure, failReport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var written, reported int
			var diagnostics bytes.Buffer
			o := testOutput(t, func(p []byte) (int, error) {
				written++
				return len(p) - 1, tc.writeErr
			}, func(p []byte) (int, error) {
				reported++
				if tc.failReport {
					return 0, errors.New("error destination full")
				}
				return diagnostics.Write(p)
			}, closeDiscardedOutput, slogger.Field{Key: "sandbox_id", Value: "failed-write"})
			l := o.OrdinaryLogger()
			if tc.lane == "l4" {
				l = o.L4Logger()
			}
			l.Warnf("failed write")
			if err := l.Sync(); !errors.Is(err, tc.want) {
				t.Fatalf("Sync error=%v; want %v", err, tc.want)
			}
			if err := closeTestOutput(o); !errors.Is(err, tc.want) {
				t.Fatalf("Close error=%v; want %v", err, tc.want)
			}
			if reported != 1 || written != 1 {
				t.Fatalf("writes=%d reports=%d; no retries or recursion allowed", written, reported)
			}
			if !tc.failReport {
				r := outputRecords(t, diagnostics.Bytes())[0]
				if r["opensandbox.event"] != "egress.log.output_error" || r["level"] != "error" || r["logger"] != "opensandbox.egress" || r["sandbox_id"] != "failed-write" || r["egress.log.lane"] != tc.lane || r["egress.log.output_operation"] != "record" || r["error"] != tc.want.Error() {
					t.Fatalf("wrong output failure diagnostic: %v", r)
				}
			}
		})
	}
}

type failCheckedCore struct {
	zapcore.Core
}

func (c failCheckedCore) Check(e zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return checked.AddCore(e, c)
}

func (failCheckedCore) Write(zapcore.Entry, []zapcore.Field) error {
	return errors.New("encoding failed")
}

func TestOutputZapErrorPathUsesSameWorkerWithoutImplicitFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := newOutputGate()
		var diagnostic bytes.Buffer
		var calls atomic.Int32
		errSink := errors.New("error output failed")
		o := testOutput(t, func(p []byte) (int, error) {
			gate.block()
			return len(p), nil
		}, func(p []byte) (int, error) {
			calls.Add(1)
			_, _ = diagnostic.Write(p)
			return 0, errSink
		}, closeDiscardedOutput)
		defer gate.open()
		o.L4Logger().Warnf("hold physical worker")
		awaitOutput(t, gate.entered, "blocked physical write")

		l, ok := slogger.AsZapSugared(o.L4Logger())
		if !ok {
			t.Fatal("expected zap logger")
		}
		start := time.Now()
		l.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core { return failCheckedCore{core} })).Warn("error reporting")
		if calls.Load() != 0 || !time.Now().Equal(start) {
			t.Fatal("Zap's error path either wrote concurrently or synchronously waited for the worker")
		}
		gate.open()
		// The L4 logger's internal error output was accepted into the ordinary lane.
		if err := o.OrdinaryLogger().Sync(); !errors.Is(err, errSink) {
			t.Fatalf("ordinary Sync did not report physical error-output failure: %v", err)
		}
		if err := closeTestOutput(o); !errors.Is(err, errSink) {
			t.Fatalf("Close error=%v", err)
		}
		if calls.Load() != 1 || !bytes.Contains(diagnostic.Bytes(), []byte("encoding failed")) {
			t.Fatalf("recursive or incorrect error output: writes=%d output=%q", calls.Load(), diagnostic.String())
		}
	})
}

func TestOutputConcurrentCloseLeavesBlockedResourcesWithWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writeGate, closeGate := newOutputGate(), newOutputGate()
		closeErr := errors.New("destination close failed")
		var written atomic.Int32
		var closed atomic.Int32
		o := testOutput(t, func(p []byte) (int, error) {
			if written.Add(1) == 1 {
				writeGate.block()
			}
			return len(p), nil
		}, io.Discard.Write, func() error {
			if closed.Add(1) == 1 {
				closeGate.block()
			}
			return closeErr
		})
		defer writeGate.open()
		defer closeGate.open()
		o.L4Logger().Warnf("blocked write")
		awaitOutput(t, writeGate.entered, "physical write")
		o.OrdinaryLogger().Warnf("accepted before close")

		results := make(chan error, 8)
		for range 8 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				results <- o.Close(ctx)
			}()
		}
		for range 8 {
			if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timed close error=%v", err)
			}
		}
		if closed.Load() != 0 || written.Load() != 1 {
			t.Fatalf("physical I/O overlapped blocked Write: closed=%d writes=%d", closed.Load(), written.Load())
		}
		for _, lane := range []*outputLane{&o.ordinary, &o.l4} {
			w := laneWriter{owner: o, lane: lane}
			if n, err := w.Write([]byte("rejected after close")); n != 0 || !errors.Is(err, errOutputClosed) {
				t.Fatalf("closed %s lane accepted bytes: n=%d err=%v", lane.name, n, err)
			}
		}

		writeGate.open()
		awaitOutput(t, closeGate.entered, "worker-owned close")
		if written.Load() != 2 {
			t.Fatal("destinations were closed before processing both accepted records")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := o.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("caller waited on a blocked destination Close: %v", err)
		}
		closeGate.open()
		if err := closeTestOutput(o); !errors.Is(err, closeErr) {
			t.Fatalf("Close did not latch the destination error: %v", err)
		}
		if err := o.L4Logger().Sync(); !errors.Is(err, closeErr) {
			t.Fatalf("Sync did not return the latched Close error: %v", err)
		}
		if closed.Load() != 1 {
			t.Fatalf("closed destinations %d times", closed.Load())
		}
	})
}

func TestOutputEarlyCloseWithoutAnyRecords(t *testing.T) {
	var closes atomic.Int32
	unexpectedWrite := func([]byte) (int, error) { return 0, errors.New("unexpected write on unused output") }
	o := testOutput(t, unexpectedWrite, unexpectedWrite, func() error { closes.Add(1); return nil })
	for range 2 {
		if err := closeTestOutput(o); err != nil {
			t.Fatal(err)
		}
	}
	if closes.Load() != 1 {
		t.Fatalf("closed unused destinations %d times", closes.Load())
	}
}
