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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestFilesystem_ReadLower(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-lower"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "readme.txt"), []byte("hello-lower"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Read file from lower (workspace).
	data, err := mv.ReadFile("readme.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello-lower"), data)

	// Stat the file.
	info, err := mv.Stat("readme.txt")
	require.NoError(t, err)
	assert.Equal(t, "readme.txt", info.Name())
}

func TestFilesystem_WriteUpper(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-write"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Write a file via MergedView.
	require.NoError(t, mv.WriteFile("session-file.txt", []byte("from-session"), 0o644))

	// In rw mode, write goes directly to workspace (host view).
	data, err := os.ReadFile(filepath.Join(wsDir, "session-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, []byte("from-session"), data)
}

func TestFilesystem_WriteThenRead(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-rw"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	content := []byte("write-then-read")
	require.NoError(t, mv.WriteFile("data.txt", content, 0o644))

	data, err := mv.ReadFile("data.txt")
	require.NoError(t, err)
	assert.Equal(t, content, data)
}

func TestFilesystem_Delete(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-del"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Write then delete.
	require.NoError(t, mv.WriteFile("del.txt", []byte("tmp"), 0o644))
	require.NoError(t, mv.Remove("del.txt"))

	_, err = mv.Stat("del.txt")
	assert.True(t, os.IsNotExist(err))
}

func TestFilesystem_MkdirAndList(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-dir"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	require.NoError(t, mv.MkdirAll("a/b", 0o755))
	require.NoError(t, mv.WriteFile("a/b/f.txt", []byte("nested"), 0o644))

	entries, err := mv.ReadDir("a/b")
	require.NoError(t, err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "f.txt")
}

func TestFilesystem_Rename(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-rename"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	require.NoError(t, mv.WriteFile("old.txt", []byte("renamed"), 0o644))
	require.NoError(t, mv.Rename("old.txt", "new.txt"))

	_, err = mv.Stat("old.txt")
	assert.True(t, os.IsNotExist(err))

	data, err := mv.ReadFile("new.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("renamed"), data)
}

func TestFilesystem_Chmod(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-chmod"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	require.NoError(t, mv.WriteFile("perm.txt", []byte("x"), 0o600))
	require.NoError(t, mv.Chmod("perm.txt", 0o755))

	info, err := mv.Stat("perm.txt")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestFilesystem_ReplaceContent(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-replace"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	require.NoError(t, mv.WriteFile("text.txt", []byte("abc def abc"), 0o644))
	require.NoError(t, mv.ReplaceContent("text.txt", "abc", "xyz"))

	data, err := mv.ReadFile("text.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("xyz def xyz"), data)
}

func TestFilesystem_Search(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-search"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	require.NoError(t, mv.WriteFile("a.txt", []byte("a"), 0o644))
	require.NoError(t, mv.WriteFile("b.log", []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "c.txt"), []byte("c"), 0o644))

	results, err := mv.Search("*.txt")
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Contains(t, results, "a.txt")
	assert.Contains(t, results, "c.txt")
}

func TestFilesystem_OverlayWriteNotVisibleOnHost(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-overlay"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "lower.txt"), []byte("lower"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "overlay",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Write goes to upper, not lower.
	require.NoError(t, mv.WriteFile("upper-only.txt", []byte("secret"), 0o644))

	// NOT visible on host workspace.
	_, err = os.ReadFile(filepath.Join(wsDir, "upper-only.txt"))
	assert.True(t, os.IsNotExist(err))

	// Visible via MergedView.
	data, err := mv.ReadFile("upper-only.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), data)

	// Lower file still visible.
	data, err = mv.ReadFile("lower.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("lower"), data)
}

func TestFilesystem_ReadOnly(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-fs-ro"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "readme.txt"), []byte("ro-data"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "ro",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	mv, err := r.GetMergedView(id)
	require.NoError(t, err)

	// Read works.
	data, err := mv.ReadFile("readme.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("ro-data"), data)

	// Write denied.
	assert.Error(t, mv.WriteFile("new.txt", []byte("x"), 0o644))
	assert.Error(t, mv.Remove("readme.txt"))
	assert.Error(t, mv.MkdirAll("d", 0o755))
}

func TestFilesystem_GetMergedView_NotFound(t *testing.T) {
	r := newRunner(t)
	_, err := r.GetMergedView("nonexistent")
	assert.Error(t, err)
}
