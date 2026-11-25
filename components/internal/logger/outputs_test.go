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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenOutputsPreservesLogicalRoutes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ordinary.log")
	alias := filepath.Join(dir, "ordinary-alias.log")
	errs := filepath.Join(dir, "errors.log")
	if err := os.Symlink(out, alias); err != nil {
		t.Fatal(err)
	}
	outputs := openTestOutputs(t, Config{
		OutputPaths:      []string{alias, out, out},
		ErrorOutputPaths: []string{errs, alias, errs},
	})
	writeOutput(t, outputs.Out, "ordinary\n")
	writeOutput(t, outputs.Errors, "diagnostic\n")
	if got := readOutput(t, out); got != "ordinary\nordinary\nordinary\ndiagnostic\n" {
		t.Fatalf("ordinary destination got %q", got)
	}
	if got := readOutput(t, errs); got != "diagnostic\ndiagnostic\n" {
		t.Fatalf("error destination got %q", got)
	}
}

func TestOpenOutputsAliasesShareRotation(t *testing.T) {
	for name, link := range map[string]func(string, string) error{"symlink": os.Symlink, "hardlink": os.Link} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path, alias := filepath.Join(dir, "out.log"), filepath.Join(dir, "alias.log")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := link(path, alias); err != nil {
				t.Fatal(err)
			}
			first, second := path, alias
			if name == "symlink" {
				first, second = alias, path
			}
			outputs := openTestOutputs(t, Config{
				OutputPaths:      []string{first},
				ErrorOutputPaths: []string{second},
				Rotate:           &RotateConfig{MaxSize: 1},
			})
			before := strings.Repeat("o", 700<<10) + "\n"
			after := strings.Repeat("d", 700<<10) + "\n"
			writeOutput(t, outputs.Out, before)
			writeOutput(t, outputs.Errors, after)
			writeOutput(t, outputs.Out, "post-rotation\n")
			if got := readOutput(t, path); got != after+"post-rotation\n" {
				t.Fatalf("ordinary and diagnostic writes did not follow one rotation: current file has %d bytes", len(got))
			}
			backups, err := filepath.Glob(filepath.Join(dir, "out-*.log"))
			if err != nil || len(backups) != 1 {
				t.Fatalf("rotated files = %v, err = %v", backups, err)
			}
			if got := readOutput(t, backups[0]); got != before {
				t.Fatalf("rotated file has %d bytes, want %d", len(got), len(before))
			}
		})
	}
}

func TestOpenOutputsPipeAliasesCloseOnlyOwnedDescriptor(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	fd := procOutputPath(t, writer)
	alias := filepath.Join(t.TempDir(), "pipe")
	if err := os.Symlink(fd, alias); err != nil {
		t.Fatal(err)
	}
	outputs := openTestOutputs(t, Config{
		OutputPaths:      []string{fd, alias},
		ErrorOutputPaths: []string{alias},
		Rotate:           &RotateConfig{MaxSize: 1},
	})
	if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		data []byte
		err  error
	}
	drained := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(reader)
		drained <- readResult{data, err}
		_ = reader.Close()
	}()
	// A record larger than the rotation limit must still go to the pipe.
	ordinary := strings.Repeat("o", 1<<20) + "\n"
	writeOutput(t, outputs.Out, ordinary)
	writeOutput(t, outputs.Errors, "diagnostic\n")
	if err := outputs.Close(); err != nil {
		t.Fatal(err)
	}
	writeOutput(t, writer, "original descriptor still open\n")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-drained
	if want := ordinary + ordinary + "diagnostic\noriginal descriptor still open\n"; got.err != nil || string(got.data) != want {
		t.Fatalf("pipe got %d/%d expected bytes, err = %v", len(got.data), len(want), got.err)
	}
}

func TestOpenOutputsUnlinkedDescriptorDoesNotRotateOrLeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unlinked.log")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd := procOutputPath(t, file)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Procfs's displayed target is not a usable rotation path, even if an
	// unrelated file really exists with that spelling.
	wrongPath := path + " (deleted)"
	if err := os.WriteFile(wrongPath, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	before := matchingOutputDescriptors(t, info)
	invalid := filepath.Join(t.TempDir(), "missing", "errors.log")
	outputs, err := OpenOutputs(Config{OutputPaths: []string{fd}, ErrorOutputPaths: []string{invalid}})
	if outputs != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid error route: outputs = %v, err = %v", outputs, err)
	}
	if got := matchingOutputDescriptors(t, info); got != before {
		t.Fatalf("setup failure left %d extra owned descriptors", got-before)
	}

	outputs = openTestOutputs(t, Config{OutputPaths: []string{fd, fd}, Rotate: &RotateConfig{MaxSize: 1}})
	if got := matchingOutputDescriptors(t, info); got != before+1 {
		t.Fatalf("repeated path acquired %d owned descriptors; want one", got-before)
	}
	large := strings.Repeat("x", 1<<20+1)
	writeOutput(t, outputs.Out, large)
	if err := outputs.Close(); err != nil {
		t.Fatal(err)
	}
	if got := matchingOutputDescriptors(t, info); got != before {
		t.Fatalf("close left %d extra owned descriptors", got-before)
	}
	got, err := io.ReadAll(file)
	if err != nil || len(got) != 2*len(large) || bytes.Count(got, []byte("x")) != len(got) {
		t.Fatalf("opaque underlying file got %d bytes, err = %v", len(got), err)
	}
	if got := readOutput(t, wrongPath); got != "untouched" {
		t.Fatalf("wrote to displayed procfs target, got %q", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recreated unlinked path: %v", err)
	}
}

func openTestOutputs(t *testing.T, cfg Config) *Outputs {
	t.Helper()
	outputs, err := OpenOutputs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := outputs.Close(); err != nil {
			t.Error(err)
		}
	})
	return outputs
}

func writeOutput(t *testing.T, out io.Writer, record string) {
	t.Helper()
	if n, err := io.WriteString(out, record); err != nil || n != len(record) {
		t.Fatalf("write = %d/%d bytes, err = %v", n, len(record), err)
	}
}

func readOutput(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func procOutputPath(t *testing.T, file *os.File) string {
	t.Helper()
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("requires procfs descriptors")
	}
	return fmt.Sprintf("/proc/self/fd/%d", file.Fd())
}

func matchingOutputDescriptors(t *testing.T, original os.FileInfo) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, entry := range entries {
		if info, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name())); err == nil && os.SameFile(info, original) {
			matches++
		}
	}
	return matches
}
