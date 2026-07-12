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

//go:build linux && bwrap

package bwrap_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// TestWorkflow_RunFileRunFile exercises the full lifecycle:
//
//	Run (generate file) → API read → API write → Run (read API-written file)
func TestWorkflow_RunFileRunFile(t *testing.T) {
	r := newRunner(t)
	ws := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Step 1: Run generates a file in workspace.
	err = r.RunInIsolatedSession(ctx, id,
		`echo "generated-by-run" > `+ws+`/step1.txt`, nil, nil)
	require.NoError(t, err)

	// Step 2: API reads the file Run just created.
	data, err := mv.ReadFile("step1.txt")
	require.NoError(t, err)
	assert.Equal(t, "generated-by-run\n", string(data))

	// Step 3: API writes a new file into workspace.
	require.NoError(t, mv.WriteFile("step3.txt", []byte("written-by-api"), 0o644))

	// Step 4: Run reads the file API just wrote.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`cat `+ws+`/step3.txt`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "written-by-api", lines[0])
}

// TestWorkflow_RunModifyRunVerify does:
//
//	Run (create file) → API modify (replace content) → Run (verify modification)
func TestWorkflow_RunModifyRunVerify(t *testing.T) {
	r := newRunner(t)
	ws := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Run: create a config file.
	err = r.RunInIsolatedSession(ctx, id,
		`echo 'host=localhost' > `+ws+`/config.txt && echo 'port=8080' >> `+ws+`/config.txt`, nil, nil)
	require.NoError(t, err)

	// API: replace host value.
	require.NoError(t, mv.ReplaceContent("config.txt", "localhost", "10.0.0.1"))

	// Run: verify the replacement took effect.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`grep host `+ws+`/config.txt`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	assert.Equal(t, "host=10.0.0.1", lines[0])
}

// TestWorkflow_MultiStepBuild simulates a multi-step build workflow:
//
//	API write source → Run compile → API read output → Run cleanup → API verify clean
func TestWorkflow_MultiStepBuild(t *testing.T) {
	r := newRunner(t)
	ws := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Step 1: API writes a "source file".
	source := `#!/bin/sh
echo "build output: $(date +%s)"
`
	require.NoError(t, mv.WriteFile("build.sh", []byte(source), 0o755))

	// Step 2: Run executes the build script, captures output to a file.
	err = r.RunInIsolatedSession(ctx, id,
		`sh `+ws+`/build.sh > `+ws+`/output.txt`, nil, nil)
	require.NoError(t, err)

	// Step 3: API reads the build output.
	data, err := mv.ReadFile("output.txt")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), "build output: "))

	// Step 4: Run cleans up.
	err = r.RunInIsolatedSession(ctx, id,
		`rm `+ws+`/output.txt`, nil, nil)
	require.NoError(t, err)

	// Step 5: API verifies cleanup.
	_, err = mv.Stat("output.txt")
	assert.True(t, os.IsNotExist(err))
}

// TestWorkflow_OverlayRunWriteAPIRead tests overlay mode:
// Run writes inside namespace (goes to upper) → API reads via MergedView
// → verify lower (host workspace) stays clean.
//
// Note: API writes to upper dir on host are NOT visible inside bwrap's
// live overlayfs mount (kernel VFS cache doesn't see direct upper changes).
// So we only test Run→API direction, not API→Run.
func TestWorkflow_OverlayRunWriteAPIRead(t *testing.T) {
	r := newRunner(t)
	ws := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("original"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "overlay",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Step 1: Run reads seed file from lower layer.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`cat `+ws+`/seed.txt`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "original", lines[0])

	// Step 2: Run writes a new file inside namespace (goes to upper).
	err = r.RunInIsolatedSession(ctx, id,
		`echo "from-run" > `+ws+`/run-file.txt`, nil, nil)
	require.NoError(t, err)

	// Step 3: API reads the Run-written file via MergedView.
	data, err := mv.ReadFile("run-file.txt")
	require.NoError(t, err)
	assert.Equal(t, "from-run\n", string(data))

	// Step 4: Verify lower (host workspace) is untouched.
	_, err = os.Stat(filepath.Join(ws, "run-file.txt"))
	assert.True(t, os.IsNotExist(err), "overlay write should not touch host workspace")

	data, err = os.ReadFile(filepath.Join(ws, "seed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
}

// TestWorkflow_EnvAndFileInteraction tests env vars set by Run persist
// across file operations.
func TestWorkflow_EnvAndFileInteraction(t *testing.T) {
	r := newRunner(t)
	ws := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Run 1: set env var.
	err = r.RunInIsolatedSession(ctx, id, `export APP_VERSION=1.2.3`, nil, nil)
	require.NoError(t, err)

	// API: write a template file.
	require.NoError(t, mv.WriteFile("version.txt", []byte("VERSION=__PLACEHOLDER__"), 0o644))

	// Run 2: use env var to fill the template, write result.
	err = r.RunInIsolatedSession(ctx, id,
		`sed "s/__PLACEHOLDER__/$APP_VERSION/" `+ws+`/version.txt > `+ws+`/version_final.txt`, nil, nil)
	require.NoError(t, err)

	// API: read the final file.
	data, err := mv.ReadFile("version_final.txt")
	require.NoError(t, err)
	assert.Equal(t, "VERSION=1.2.3", strings.TrimSpace(string(data)))
}

// TestWorkflow_MkdirUploadRunProcess tests:
//
//	API mkdir → API upload files → Run process all files → API read results
func TestWorkflow_MkdirUploadRunProcess(t *testing.T) {
	r := newRunner(t)
	ws := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// API: create input directory and upload data files.
	require.NoError(t, mv.MkdirAll("input", 0o755))
	require.NoError(t, mv.WriteFile("input/a.csv", []byte("1,2,3\n4,5,6\n"), 0o644))
	require.NoError(t, mv.WriteFile("input/b.csv", []byte("7,8,9\n"), 0o644))

	// Run: count total lines across all CSV files, write result.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`wc -l `+ws+`/input/*.csv | tail -1`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[0], "3", "should count 3 total lines (2+1)")

	// Run: concatenate all CSVs into output.
	err = r.RunInIsolatedSession(ctx, id,
		`cat `+ws+`/input/*.csv > `+ws+`/merged.csv`, nil, nil)
	require.NoError(t, err)

	// API: read merged result.
	data, err := mv.ReadFile("merged.csv")
	require.NoError(t, err)
	assert.Equal(t, "1,2,3\n4,5,6\n7,8,9\n", string(data))
}
