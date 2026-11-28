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
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestFileRotationAcrossLogRoutes(t *testing.T) {
	const helper = "write-rotation-fixture"
	if flag.Arg(0) == helper {
		paths := []string{"events.log"}
		if flag.Arg(1) == "duplicate" {
			paths = append(paths, paths[0])
		}
		// A failing extra core makes Zap emit real internal diagnostics for
		// the two warnings without changing the ordinary file output.
		r, w := io.Pipe()
		_ = r.Close()
		defer w.Close()
		failWarn := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(w), zap.WarnLevel)
		l, err := NewWithExtraCores(Config{OutputPaths: paths, Rotate: &RotateConfig{MaxSize: 1}}, failWarn)
		if err != nil {
			t.Fatal(err)
		}
		l.Warnf("before rotation") // Open both routes before the file is renamed.
		payload := strings.Repeat("x", 600<<10)
		for range 2 / len(paths) {
			l.Infof("large record: %s", payload)
		}
		l.Warnf("after rotation")
		return
	}

	for i, name := range []string{"single", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// The logger has no Close API. Let its process release the files
			// before inspecting them and removing the temporary directory.
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFileRotationAcrossLogRoutes$", "--", helper, name)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("write fixture: %v\n%s", err, out)
			}
			backups, err := filepath.Glob(filepath.Join(dir, "events-*.log"))
			if err != nil || len(backups) != 1 {
				t.Fatalf("expected one rotation, got %v (err=%v)", backups, err)
			}
			for _, path := range []string{backups[0], filepath.Join(dir, "events.log")} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if n := bytes.Count(data, []byte("large record:")); n != 1 {
					t.Errorf("%s: got %d large records, want 1", filepath.Base(path), n)
				}
				if n := bytes.Count(data, []byte(io.ErrClosedPipe.Error())); n != i+1 {
					t.Errorf("%s: got %d internal diagnostics, want %d", filepath.Base(path), n, i+1)
				}
			}
		})
	}
}
