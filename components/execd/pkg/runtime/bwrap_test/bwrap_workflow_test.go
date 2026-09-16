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

	err = r.RunInIsolatedSession(ctx, id,
		`echo "generated-by-run" > `+ws+`/step1.txt`, nil, nil)
	require.NoError(t, err)

	data, err := mv.ReadFile("step1.txt")
	require.NoError(t, err)
	assert.Equal(t, "generated-by-run\n", string(data))

	require.NoError(t, mv.WriteFile("step3.txt", []byte("written-by-api"), 0o644))

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

	err = r.RunInIsolatedSession(ctx, id,
		`echo 'host=localhost' > `+ws+`/config.txt && echo 'port=8080' >> `+ws+`/config.txt`, nil, nil)
	require.NoError(t, err)

	require.NoError(t, mv.ReplaceContent("config.txt", "localhost", "10.0.0.1"))

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

	source := `#!/bin/sh
echo "build output: $(date +%s)"
`
	require.NoError(t, mv.WriteFile("build.sh", []byte(source), 0o755))

	err = r.RunInIsolatedSession(ctx, id,
		`sh `+ws+`/build.sh > `+ws+`/output.txt`, nil, nil)
	require.NoError(t, err)

	data, err := mv.ReadFile("output.txt")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), "build output: "))

	err = r.RunInIsolatedSession(ctx, id,
		`rm `+ws+`/output.txt`, nil, nil)
	require.NoError(t, err)

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

	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`cat `+ws+`/seed.txt`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "original", lines[0])

	err = r.RunInIsolatedSession(ctx, id,
		`echo "from-run" > `+ws+`/run-file.txt`, nil, nil)
	require.NoError(t, err)

	data, err := mv.ReadFile("run-file.txt")
	require.NoError(t, err)
	assert.Equal(t, "from-run\n", string(data))

	_, err = os.Stat(filepath.Join(ws, "run-file.txt"))
	assert.True(t, os.IsNotExist(err), "overlay write should not touch host workspace")

	data, err = os.ReadFile(filepath.Join(ws, "seed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
}

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

	err = r.RunInIsolatedSession(ctx, id, `export APP_VERSION=1.2.3`, nil, nil)
	require.NoError(t, err)

	require.NoError(t, mv.WriteFile("version.txt", []byte("VERSION=__PLACEHOLDER__"), 0o644))

	err = r.RunInIsolatedSession(ctx, id,
		`sed "s/__PLACEHOLDER__/$APP_VERSION/" `+ws+`/version.txt > `+ws+`/version_final.txt`, nil, nil)
	require.NoError(t, err)

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

	require.NoError(t, mv.MkdirAll("input", 0o755))
	require.NoError(t, mv.WriteFile("input/a.csv", []byte("1,2,3\n4,5,6\n"), 0o644))
	require.NoError(t, mv.WriteFile("input/b.csv", []byte("7,8,9\n"), 0o644))

	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`wc -l `+ws+`/input/*.csv | tail -1`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[0], "3", "should count 3 total lines (2+1)")

	err = r.RunInIsolatedSession(ctx, id,
		`cat `+ws+`/input/*.csv > `+ws+`/merged.csv`, nil, nil)
	require.NoError(t, err)

	data, err := mv.ReadFile("merged.csv")
	require.NoError(t, err)
	assert.Equal(t, "1,2,3\n4,5,6\n7,8,9\n", string(data))
}
