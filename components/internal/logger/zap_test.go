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

package logger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestLoggerWriterPreservesEncodingLevelsAndSampling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	cfg := Config{Level: "warn", OutputPaths: []string{path}}
	legacy, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	injected := NewWithWriter(cfg, zapcore.AddSync(&out), zapcore.AddSync(&errs))
	for _, l := range []Logger{legacy, injected} {
		log := withFixedClock(t, l.Named("egress").With(Field{Key: "component", Value: "test"}))
		log.Info("filtered")
		for range 300 {
			log.Warn("sampled")
		}
		log.Error("error record")
	}
	if out.String() != readOutput(t, path) || errs.Len() != 0 {
		t.Fatalf("injected log differs from legacy output; error route = %q", errs.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 103 {
		t.Fatalf("got %d entries, want 102 sampled warnings and one error", len(lines))
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		level, msg := "warn", "sampled"
		if i == len(lines)-1 {
			level, msg = "error", "error record"
		}
		if row["level"] != level || row["msg"] != msg || row["logger"] != "egress" || row["component"] != "test" ||
			row["ts"] != "2026-01-02T03:04:05.000Z" || len(row) != 5 {
			t.Fatalf("unexpected record: %v", row)
		}
	}
}

func TestLoggerWriterMatchesEmptyRouteFallback(t *testing.T) {
	t.Setenv(envLogOutput, " , , ")
	stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	previous := os.Stdout
	os.Stdout = stdout
	defer func() { os.Stdout = previous }()
	cfg := Config{Level: "error"}
	legacy, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	outputs := openTestOutputs(t, cfg)
	injected := NewWithWriter(cfg, outputs.Out, outputs.Errors)
	for _, l := range []Logger{legacy, injected} {
		log := withFixedClock(t, l)
		log.Debug("filtered")
		log.Info("stdout fallback")
	}
	writeOutput(t, outputs.Errors, "no implicit error destination\n")
	if err := outputs.Close(); err != nil {
		t.Fatal(err)
	}
	writeOutput(t, stdout, "borrowed stdout remains open\n")
	lines := strings.Split(strings.TrimSuffix(readOutput(t, stdout.Name()), "\n"), "\n")
	if len(lines) != 3 || lines[0] != lines[1] || lines[2] != "borrowed stdout remains open" {
		t.Fatalf("unexpected fallback output: %v", lines)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatal(err)
	}
	if row["level"] != "info" || row["msg"] != "stdout fallback" || row["ts"] != float64(logTestTime.Unix()) {
		t.Fatalf("fallback should retain the legacy info level and epoch encoder: %v", row)
	}
}

func TestLoggerFactoriesHonorConfiguredErrorRoute(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	for _, injected := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "errors.log")
		cfg := Config{OutputPaths: []string{"/dev/full"}, ErrorOutputPaths: []string{path, path}}
		var l Logger
		if injected {
			outputs := openTestOutputs(t, cfg)
			l = NewWithWriter(cfg, outputs.Out, outputs.Errors)
		} else {
			var err error
			l, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
		}
		l.Warnf("failed ordinary write")
		data := readOutput(t, path)
		if strings.Count(data, "write error: write /dev/full:") != 2 {
			t.Fatalf("injected=%t: configured error destination got %q", injected, data)
		}
	}
}

func TestLegacyExtraCoresPreserveSynchronousOutputMultiplicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.log")
	cfg := Config{Level: "warn", OutputPaths: []string{path, path}}
	observed, logs := observer.New(zapcore.DebugLevel)
	l, err := NewWithExtraCores(cfg, observed)
	if err != nil {
		t.Fatal(err)
	}
	l.Infof("extra core only")
	l.Warnf("synchronous duplicate")
	data := readOutput(t, path)
	if strings.Count(data, "synchronous duplicate") != 2 || strings.Contains(data, "extra core only") {
		t.Fatalf("legacy ordinary route got %q", data)
	}
	if logs.Len() != 2 {
		t.Fatalf("extra core got %d entries, want 2", logs.Len())
	}
}

var logTestTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

type logTestClock struct{ zapcore.Clock }

func (logTestClock) Now() time.Time { return logTestTime }

func withFixedClock(t *testing.T, l Logger) *zap.SugaredLogger {
	t.Helper()
	sugar, ok := AsZapSugared(l)
	if !ok {
		t.Fatal("expected a zap logger")
	}
	return sugar.Desugar().WithOptions(zap.WithClock(logTestClock{zapcore.DefaultClock})).Sugar()
}
