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

//go:build linux && bwrap

package bwrap_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// newBindsSession creates a session with a rw workspace plus one bind and
// returns the runner, session id, and the bind source and dest directories.
// The bind source is pre-created so callers can seed host-side content.
func newBindsSession(t *testing.T, readOnly bool) (*runtime.IsolatedRunner, string, string, string) {
	t.Helper()

	srcDir := t.TempDir()
	destDir := t.TempDir()

	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		Binds: []isolation.BindMount{
			{Source: srcDir, Dest: destDir, ReadOnly: readOnly},
		},
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.DeleteIsolatedSession(id) })
	return r, id, srcDir, destDir
}

func runInSession(t *testing.T, r *runtime.IsolatedRunner, id, code string, onStdout runtime.StdoutCallback) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, r.RunInIsolatedSession(ctx, id, code, nil, onStdout))
}

// TestBindsFiles_RunWriteVisibleViaFilesAPI is the reported scenario: a
// process inside the session writes into the bind dest with shell
// redirection, and the session files API must read the same data back
// through the bind dest path.
func TestBindsFiles_RunWriteVisibleViaFilesAPI(t *testing.T) {
	r, id, _, destDir := newBindsSession(t, false)

	runInSession(t, r, id, "echo run-data > "+destDir+"/run.txt", nil)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)
	data, err := mv.ReadFile(filepath.Join(destDir, "run.txt"))
	require.NoError(t, err, "files API must read data written by run into the bind")
	assert.Equal(t, "run-data\n", string(data))
}

// TestBindsFiles_APIWriteVisibleToRun is the reverse direction: the files
// API writes through the bind dest, and a shell command inside the session
// observes it in the namespace.
func TestBindsFiles_APIWriteVisibleToRun(t *testing.T) {
	r, id, srcDir, destDir := newBindsSession(t, false)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)
	require.NoError(t, mv.WriteFile(filepath.Join(destDir, "api.txt"), []byte("api-data"), 0o644))

	// Host side: the write must have gone to the bind source.
	host, err := os.ReadFile(filepath.Join(srcDir, "api.txt"))
	require.NoError(t, err)
	assert.Equal(t, "api-data", string(host))

	// Namespace side: run must see it at the bind dest.
	var lines []string
	runInSession(t, r, id, "cat "+destDir+"/api.txt", func(line string) { lines = append(lines, line) })
	assert.Equal(t, []string{"api-data"}, lines)
}

// TestBindsFiles_ReadAndListSeededSource covers read/list of host content
// seeded before session creation.
func TestBindsFiles_ReadAndListSeededSource(t *testing.T) {
	r, id, srcDir, destDir := newBindsSession(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "seed.txt"), []byte("seeded"), 0o644))

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	data, err := mv.ReadFile(filepath.Join(destDir, "seed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "seeded", string(data))

	entries, err := mv.ReadDir(destDir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "seed.txt")

	// Shell agrees with the files API.
	var lines []string
	runInSession(t, r, id, "ls "+destDir, func(line string) { lines = append(lines, line) })
	assert.Contains(t, lines, "seed.txt")
}

// TestBindsFiles_ReadOnlyBind: ro binds are readable but every files-API
// mutation is denied, matching --ro-bind inside the namespace.
func TestBindsFiles_ReadOnlyBind(t *testing.T) {
	r, id, srcDir, destDir := newBindsSession(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "ro.txt"), []byte("readonly"), 0o644))

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	data, err := mv.ReadFile(filepath.Join(destDir, "ro.txt"))
	require.NoError(t, err)
	assert.Equal(t, "readonly", string(data))

	err = mv.WriteFile(filepath.Join(destDir, "denied.txt"), []byte("x"), 0o644)
	require.Error(t, err, "files-API write to a read-only bind must be denied")
	assert.Contains(t, err.Error(), "read-only")

	// Shell cannot write either; the two views stay consistent.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = r.RunInIsolatedSession(ctx, id, "echo x > "+destDir+"/denied.txt", nil, nil)
	require.Error(t, err, "shell write to a read-only bind must fail too")
	_, hostErr := os.Stat(filepath.Join(srcDir, "denied.txt"))
	assert.True(t, os.IsNotExist(hostErr), "ro bind must not gain files from either side")
}

// TestBindsFiles_PathOutsideMountsRejected: paths outside every overlay and
// bind keep failing with ErrPathOutsideOverlays.
func TestBindsFiles_PathOutsideMountsRejected(t *testing.T) {
	r, id, _, _ := newBindsSession(t, false)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	_, err = mv.ReadFile("/etc/passwd")
	require.Error(t, err)
	assert.True(t, errors.Is(err, isolation.ErrPathOutsideOverlays))

	require.Error(t, mv.WriteFile("/etc/evil", []byte("x"), 0o644))
}

// TestBindsFiles_ReplaceAndDelete: content replacement and deletion through
// the bind dest land on the host source, in both views.
func TestBindsFiles_ReplaceAndDelete(t *testing.T) {
	r, id, srcDir, destDir := newBindsSession(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "f.txt"), []byte("hello-world"), 0o644))

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	require.NoError(t, mv.ReplaceContent(filepath.Join(destDir, "f.txt"), "world", "files-api"))
	host, err := os.ReadFile(filepath.Join(srcDir, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello-files-api", string(host))

	require.NoError(t, mv.Remove(filepath.Join(destDir, "f.txt")))
	_, err = os.Stat(filepath.Join(srcDir, "f.txt"))
	assert.True(t, os.IsNotExist(err), "files-API delete must remove the bind source file")
}

// TestBindsFiles_PassthroughSemantics: the bind view mirrors the namespace —
// symlinks resolve on the host and ".wh." names are real files, and search
// results come back in namespace space.
func TestBindsFiles_PassthroughSemantics(t *testing.T) {
	r, id, srcDir, destDir := newBindsSession(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "plain.txt"), []byte("P"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".wh.plain.txt"), []byte(""), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(srcDir, "plain.txt"), filepath.Join(srcDir, "alias.txt")))

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// The namespace follows the symlink; so must the files API.
	var lines []string
	runInSession(t, r, id, "cat "+destDir+"/alias.txt", func(line string) { lines = append(lines, line) })
	require.Equal(t, []string{"P"}, lines)
	data, err := mv.ReadFile(filepath.Join(destDir, "alias.txt"))
	require.NoError(t, err)
	assert.Equal(t, "P", string(data))

	// ".wh."-prefixed names are plain files through both views.
	runInSession(t, r, id, "cat "+destDir+"/.wh.plain.txt", nil)
	_, err = mv.ReadFile(filepath.Join(destDir, ".wh.plain.txt"))
	require.NoError(t, err)

	// Search rebases results onto the dest so follow-up stats route back.
	results, err := mv.Search(destDir, "**.txt")
	require.NoError(t, err)
	assert.Contains(t, results, filepath.Join(destDir, "plain.txt"))
	for _, p := range results {
		_, err := mv.Stat(p)
		require.NoError(t, err, "rebased search result %s must stat", p)
	}
}

// TestBindsFiles_OverlappingMountsRejected: create-time validation keeps the
// files view and the namespace in agreement for configs the router cannot
// represent.
func TestBindsFiles_OverlappingMountsRejected(t *testing.T) {
	r := newRunner(t)

	srcDir := t.TempDir()
	destDir := t.TempDir()
	wsDir := t.TempDir()
	_ = destDir

	t.Run("duplicate_bind_dests", func(t *testing.T) {
		_, err := r.CreateIsolatedSession(&runtime.IsolatedSessionOptions{
			Profile:       "balanced",
			WorkspacePath: wsDir,
			WorkspaceMode: "rw",
			Binds: []isolation.BindMount{
				{Source: srcDir, Dest: "/mnt/x"},
				{Source: t.TempDir(), Dest: "/mnt/x"},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate bind destination")
	})

	t.Run("bind_ancestor_of_workspace_rejected", func(t *testing.T) {
		_, err := r.CreateIsolatedSession(&runtime.IsolatedSessionOptions{
			Profile:       "balanced",
			WorkspacePath: "/tmp/bwrap-ancestor-ws",
			WorkspaceMode: "rw",
			Binds:         []isolation.BindMount{{Source: srcDir, Dest: "/tmp"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shadows mount")
	})

	t.Run("bind_under_trusted_base_rejected", func(t *testing.T) {
		_, err := r.CreateIsolatedSession(&runtime.IsolatedSessionOptions{
			Profile:       "balanced",
			WorkspacePath: wsDir,
			WorkspaceMode: "rw",
			Binds:         []isolation.BindMount{{Source: srcDir, Dest: "/run/secrets"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "trusted base mount")
	})
}
