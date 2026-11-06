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

package isolation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMergedView(t *testing.T, mode WorkspaceMode) *MergedView {
	t.Helper()
	// Resolve symlinks: MergedView rejects symlinked path components, and
	// t.TempDir() sits under the /var symlink on macOS.
	lower := filepath.Join(t.TempDir(), "lower")
	upper := filepath.Join(t.TempDir(), "upper")
	require.NoError(t, os.MkdirAll(lower, 0o755))
	require.NoError(t, os.MkdirAll(upper, 0o755))
	var err error
	if lower, err = filepath.EvalSymlinks(lower); err != nil {
		t.Fatal(err)
	}
	if upper, err = filepath.EvalSymlinks(upper); err != nil {
		t.Fatal(err)
	}
	return NewMergedView(lower, upper, mode, uint32(os.Getuid()), uint32(os.Getgid()))
}

func TestMergedView_Stat_Lower(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "test.txt"), []byte("hello"), 0o644))

	info, err := mv.Stat("test.txt")
	require.NoError(t, err)
	assert.Equal(t, "test.txt", info.Name())
	assert.Equal(t, int64(5), info.Size())
}

func TestMergedView_Stat_UpperOverrides(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "same.txt"), []byte("lower"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, "same.txt"), []byte("upper"), 0o644))

	info, err := mv.Stat("same.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(5), info.Size(), "upper should override lower")
}

func TestMergedView_ReadFile(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "data.txt"), []byte("lower-data"), 0o644))

	data, err := mv.ReadFile("data.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("lower-data"), data)
}

func TestMergedView_WriteFile(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	err := mv.WriteFile("new.txt", []byte("session-data"), 0o644)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(mv.UpperDir, "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, []byte("session-data"), data)

	_, err = os.ReadFile(filepath.Join(mv.LowerDir, "new.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestMergedView_WriteFile_DenyReadOnly(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRO)

	err := mv.WriteFile("new.txt", []byte("data"), 0o644)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
}

func TestMergedView_Remove(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	require.NoError(t, mv.WriteFile("del.txt", []byte("tmp"), 0o644))
	require.NoError(t, mv.Remove("del.txt"))

	_, err := os.Stat(filepath.Join(mv.UpperDir, "del.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestMergedView_Remove_LowerOnly(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "lower-only.txt"), []byte("x"), 0o644))

	err := mv.Remove("lower-only.txt")
	require.NoError(t, err)

	whPath := filepath.Join(mv.UpperDir, ".wh.lower-only.txt")
	_, err = os.Stat(whPath)
	assert.NoError(t, err, "whiteout marker should exist")

	_, err = mv.Stat("lower-only.txt")
	assert.True(t, os.IsNotExist(err))
}

func TestMergedView_RemoveAll(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, mv.MkdirAll("subdir", 0o755))
	require.NoError(t, mv.WriteFile("subdir/f.txt", []byte("x"), 0o644))

	require.NoError(t, mv.RemoveAll("subdir"))

	_, err := os.Stat(filepath.Join(mv.UpperDir, "subdir"))
	assert.True(t, os.IsNotExist(err))
}

func TestMergedView_MkdirAll(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	require.NoError(t, mv.MkdirAll("a/b/c", 0o755))

	info, err := os.Stat(filepath.Join(mv.UpperDir, "a", "b", "c"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestMergedView_Rename(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, mv.WriteFile("old.txt", []byte("renamed"), 0o644))

	require.NoError(t, mv.Rename("old.txt", "new.txt"))

	_, err := mv.Stat("old.txt")
	assert.True(t, os.IsNotExist(err))

	data, err := mv.ReadFile("new.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("renamed"), data)
}

func TestMergedView_Rename_LowerToUpper(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "src.txt"), []byte("from-lower"), 0o644))

	require.NoError(t, mv.Rename("src.txt", "dst.txt"))

	data, err := mv.ReadFile("dst.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("from-lower"), data)
}

func TestMergedView_Rename_LowerFileKeepsMode(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "run.sh"), []byte("#!/bin/sh"), 0o755))

	require.NoError(t, mv.Rename("run.sh", "start.sh"))

	info, err := mv.Stat("start.sh")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestMergedView_Rename_LowerDirectory(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d", "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "sub", "b.txt"), []byte("b"), 0o600))

	require.NoError(t, mv.Rename("d", "e"))

	assertNotInView(t, mv, "d")
	info, err := mv.Stat("e")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())
	data, err := mv.ReadFile(filepath.Join("e", "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "a", string(data))
	info, err = mv.Stat(filepath.Join("e", "sub", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// A directory present in both layers moves with the lower entries it shows,
// but not with the ones that were deleted from it.
func TestMergedView_Rename_MergedDirectoryKeepsLowerEntries(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "lower.txt"), []byte("lower"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "gone.txt"), []byte("gone"), 0o644))
	require.NoError(t, mv.WriteFile(filepath.Join("d", "upper.txt"), []byte("upper"), 0o644))
	require.NoError(t, mv.Remove(filepath.Join("d", "gone.txt")))

	require.NoError(t, mv.Rename("d", "e"))

	assertNotInView(t, mv, "d")
	assertNotInView(t, mv, filepath.Join("e", "gone.txt"))
	entries, err := mv.ReadDir("e")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"lower.txt", "upper.txt"}, names)
	data, err := mv.ReadFile(filepath.Join("e", "lower.txt"))
	require.NoError(t, err)
	assert.Equal(t, "lower", string(data))
}

func TestMergedView_Rename_ReadOnlyLowerDirectory(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	dir := filepath.Join(mv.LowerDir, "ro")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(filepath.Join(mv.UpperDir, "moved"), 0o755)
	})

	require.NoError(t, mv.Rename("ro", "moved"))

	info, err := mv.Stat("moved")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o555), info.Mode().Perm())
	data, err := mv.ReadFile(filepath.Join("moved", "x.txt"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
}

func TestMergedView_Rename_LowerSymlinkStaysSymlink(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(mv.LowerDir, "link")))

	require.NoError(t, mv.Rename("link", "moved"))

	target, err := os.Readlink(filepath.Join(mv.UpperDir, "moved"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/hostname", target)
}

func TestMergedView_Chmod_LowerSymlinkDenied(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	target := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(target, filepath.Join(mv.LowerDir, "link")))

	assert.Error(t, mv.Chmod("link", 0o777))

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestMergedView_DeletedLowerEntryCannotBeRenamedOrChmodded(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "f.txt"), []byte("f"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, mv.Remove("f.txt"))
	require.NoError(t, mv.RemoveAll("d"))

	for _, rel := range []string{"f.txt", filepath.Join("d", "x.txt")} {
		assert.ErrorIs(t, mv.Rename(rel, "moved.txt"), fs.ErrNotExist, "Rename(%s)", rel)
		assert.ErrorIs(t, mv.Chmod(rel, 0o600), fs.ErrNotExist, "Chmod(%s)", rel)
		assertNotInView(t, mv, rel)
	}
	assertNotInView(t, mv, "moved.txt")
}

// A directory recreated under a deleted one shows none of the lower entries
// that were deleted with it, so renaming it must not bring them back.
func TestMergedView_Rename_UnderDeletedDirectoryLeavesLowerBehind(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "sub", "old.txt"), []byte("old"), 0o644))
	require.NoError(t, mv.RemoveAll("d"))
	require.NoError(t, mv.WriteFile(filepath.Join("d", "sub", "new.txt"), []byte("new"), 0o644))

	require.NoError(t, mv.Rename(filepath.Join("d", "sub"), "e"))

	assertNotInView(t, mv, filepath.Join("e", "old.txt"))
	data, err := mv.ReadFile(filepath.Join("e", "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

// A copy-up that fails leaves nothing in upper: the lower file stays visible
// instead of an unfinished copy shadowing it.
func TestMergedView_FailedCopyUpLeavesLowerVisible(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can chown to any UID")
	}
	mv := newTestMergedView(t, WorkspaceRW)
	mv.Uid = uint32(os.Getuid()) + 1
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "f.txt"), []byte("lower"), 0o644))

	assert.Error(t, mv.Rename("f.txt", "g.txt"))
	assert.Error(t, mv.Chmod("f.txt", 0o600))
	assert.Error(t, mv.Rename("d", "e"))

	for _, rel := range []string{"f.txt", "d", "g.txt", "e"} {
		_, err := os.Lstat(filepath.Join(mv.UpperDir, rel))
		assert.ErrorIs(t, err, fs.ErrNotExist, "upper %s", rel)
	}
	entries, err := os.ReadDir(mv.UpperDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	data, err := mv.ReadFile("f.txt")
	require.NoError(t, err)
	assert.Equal(t, "lower", string(data))
}

func TestMergedView_Chmod_LowerDirectory(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "x.txt"), []byte("x"), 0o644))

	require.NoError(t, mv.Chmod("d", 0o700))

	info, err := mv.Stat("d")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	data, err := mv.ReadFile(filepath.Join("d", "x.txt"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
	lowerInfo, err := os.Stat(filepath.Join(mv.LowerDir, "d"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), lowerInfo.Mode().Perm())
}

func TestMergedView_Chmod(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, mv.WriteFile("perm.txt", []byte("x"), 0o600))

	require.NoError(t, mv.Chmod("perm.txt", 0o755))

	info, err := os.Stat(filepath.Join(mv.UpperDir, "perm.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestMergedView_ReplaceContent(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, mv.WriteFile("replace.txt", []byte("hello world"), 0o644))

	require.NoError(t, mv.ReplaceContent("replace.txt", "world", "gopher"))

	data, err := mv.ReadFile("replace.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello gopher"), data)
}

func TestMergedView_Search(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, mv.WriteFile("a.txt", []byte("a"), 0o644))
	require.NoError(t, mv.WriteFile("b.log", []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "c.txt"), []byte("c"), 0o644))

	results, err := mv.Search(".", "*.txt")
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Contains(t, results, "a.txt")
	assert.Contains(t, results, "c.txt")
}

func TestMergedView_ReadDir(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, mv.WriteFile("upper.txt", []byte("u"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "lower.txt"), []byte("l"), 0o644))

	entries, err := mv.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	assert.Contains(t, names, "upper.txt")
	assert.Contains(t, names, "lower.txt")
}

func TestMergedView_ReadDir_Whiteout(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "hidden.txt"), []byte("secret"), 0o644))
	// Create whiteout file in upper to hide lower entry.
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, ".wh.hidden.txt"), nil, 0o644))

	entries, err := mv.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, "hidden.txt", e.Name(), "whiteout should hide lower entry")
	}
}

func TestMergedView_WriteFileReader(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	n, err := mv.WriteFileReader("stream.txt", strings.NewReader("streamed-data"), 0o644)
	require.NoError(t, err)
	assert.Equal(t, int64(13), n)

	data, err := mv.ReadFile("stream.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("streamed-data"), data)
}

func TestMergedView_PathTraversal(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	_, err := mv.Stat("../etc/passwd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")

	_, err = mv.ReadFile("../../host")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")

	err = mv.WriteFile("../escape", []byte("x"), 0o644)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")
}

func TestMergedView_ReadOnly_AllWritesDenied(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRO)

	assert.Error(t, mv.WriteFile("x.txt", []byte("x"), 0o644))
	assert.Error(t, mv.Remove("x.txt"))
	assert.Error(t, mv.MkdirAll("d", 0o755))
	assert.Error(t, mv.Rename("a", "b"))
	assert.Error(t, mv.Chmod("x.txt", 0o755))
}

func TestMergedView_AbsolutePathOutsideLowerRejected(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceOverlay)

	// Every operation rejects an absolute path outside LowerDir instead of
	// silently resolving it against the workspace.
	outside := []string{"/tmp/escape.txt", "/etc/passwd", "/etc", "/"}
	for _, p := range outside {
		_, err := mv.Stat(p)
		assert.True(t, errors.Is(err, ErrPathOutsideOverlays), "Stat %s = %v", p, err)

		_, err = mv.ReadFile(p)
		assert.True(t, errors.Is(err, ErrPathOutsideOverlays), "ReadFile %s = %v", p, err)

		_, err = mv.Open(p)
		assert.True(t, errors.Is(err, ErrPathOutsideOverlays), "Open %s = %v", p, err)

		_, err = mv.ReadDir(p)
		assert.True(t, errors.Is(err, ErrPathOutsideOverlays), "ReadDir %s = %v", p, err)

		assert.True(t, errors.Is(mv.WriteFile(p, []byte("x"), 0o644), ErrPathOutsideOverlays), "WriteFile %s", p)
		assert.True(t, errors.Is(mv.MkdirAll(p, 0o755), ErrPathOutsideOverlays), "MkdirAll %s", p)
		assert.True(t, errors.Is(mv.Remove(p), ErrPathOutsideOverlays), "Remove %s", p)
		assert.True(t, errors.Is(mv.RemoveAll(p), ErrPathOutsideOverlays), "RemoveAll %s", p)
		assert.True(t, errors.Is(mv.Chmod(p, 0o644), ErrPathOutsideOverlays), "Chmod %s", p)
		assert.True(t, errors.Is(mv.ReplaceContent(p, "a", "b"), ErrPathOutsideOverlays), "ReplaceContent %s", p)
	}
	_, err := mv.Search("/tmp", "*")
	assert.True(t, errors.Is(err, ErrPathOutsideOverlays), "Search /tmp = %v", err)
	assert.True(t, errors.Is(mv.Rename("a.txt", "/tmp/b.txt"), ErrPathOutsideOverlays), "Rename to /tmp/b.txt")
	assert.True(t, errors.Is(mv.Rename("/tmp/a.txt", "b.txt"), ErrPathOutsideOverlays), "Rename from /tmp/a.txt")

	// Nothing leaked into the upper directory.
	entries, err := os.ReadDir(mv.UpperDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "rejected paths must not land in the upper dir")
}

func TestMergedView_AbsolutePathUnderLowerStillWorks(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceOverlay)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "ws.txt"), []byte("lower"), 0o644))

	// Absolute path under LowerDir is stripped and served normally.
	info, err := mv.Stat(filepath.Join(mv.LowerDir, "ws.txt"))
	require.NoError(t, err)
	assert.Equal(t, "ws.txt", info.Name())

	require.NoError(t, mv.WriteFile(filepath.Join(mv.LowerDir, "abs.txt"), []byte("via-abs"), 0o644))
	data, err := os.ReadFile(filepath.Join(mv.UpperDir, "abs.txt"))
	require.NoError(t, err)
	assert.Equal(t, "via-abs", string(data))

	require.NoError(t, mv.Remove(filepath.Join(mv.LowerDir, "abs.txt")))
	_, err = os.Stat(filepath.Join(mv.UpperDir, "abs.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestMergedView_RootLowerServesAllAbsolutePaths(t *testing.T) {
	// A "/" overlay is the namespace root: every absolute path belongs to it.
	upper := filepath.Join(t.TempDir(), "upper")
	require.NoError(t, os.MkdirAll(upper, 0o755))
	upper, err := filepath.EvalSymlinks(upper)
	require.NoError(t, err)
	mv := NewMergedView("/", upper, WorkspaceOverlay, 0, 0)

	_, statErr := mv.Stat("/definitely-not-a-real-path-87953550")
	assert.False(t, errors.Is(statErr, ErrPathOutsideOverlays), "root view must not reject absolute paths")
	assert.True(t, os.IsNotExist(statErr))

	// The returned path is workspace-relative, including for the root view.
	rel, err := mv.safePath("/etc/passwd")
	require.NoError(t, err)
	assert.Equal(t, "etc/passwd", rel)

	rel, err = mv.safePath("/")
	require.NoError(t, err)
	assert.Equal(t, ".", rel)
}

func TestMergedView_EmptyLowerRejectsAbsolutePaths(t *testing.T) {
	// Defensive: a view without a lower root has no namespace scope to
	// resolve absolute paths against.
	mv := NewMergedView("", t.TempDir(), WorkspaceOverlay, 0, 0)

	_, err := mv.Stat("/tmp/x")
	assert.True(t, errors.Is(err, ErrPathOutsideOverlays))
}

func TestMergedView_TraversalDenialUnchanged(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	_, err := mv.Stat("../etc/passwd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")

	// A path that escapes LowerDir via .. cleans to an absolute path
	// outside the workspace and is rejected as out-of-scope.
	_, err = mv.Stat(filepath.Join(mv.LowerDir, "..", "etc", "passwd"))
	assert.True(t, errors.Is(err, ErrPathOutsideOverlays))
}

// assertNotInView checks that every read path of mv treats rel as absent.
func assertNotInView(t *testing.T, mv *MergedView, rel string) {
	t.Helper()
	_, err := mv.Stat(rel)
	assert.ErrorIs(t, err, fs.ErrNotExist, "Stat(%s)", rel)
	_, err = mv.ReadFile(rel)
	assert.ErrorIs(t, err, fs.ErrNotExist, "ReadFile(%s)", rel)
	f, err := mv.Open(rel)
	if err == nil {
		f.Close()
	}
	assert.ErrorIs(t, err, fs.ErrNotExist, "Open(%s)", rel)

	entries, err := mv.ReadDir(filepath.Dir(rel))
	if err == nil {
		for _, e := range entries {
			assert.NotEqual(t, filepath.Base(rel), e.Name(), "ReadDir(%s)", filepath.Dir(rel))
		}
	}
	results, err := mv.Search(".", "*")
	require.NoError(t, err)
	assert.NotContains(t, results, rel, "Search")
}

func TestMergedView_DeletedDirectoryHidesLowerContents(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "sub", "y.txt"), []byte("y"), 0o644))

	require.NoError(t, mv.RemoveAll("d"))

	assertNotInView(t, mv, "d")
	assertNotInView(t, mv, filepath.Join("d", "x.txt"))
	assertNotInView(t, mv, filepath.Join("d", "sub", "y.txt"))
	_, err := mv.ReadDir("d")
	assert.ErrorIs(t, err, fs.ErrNotExist)
	_, err = mv.ReadDir(filepath.Join("d", "sub"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
	results, err := mv.Search("d", "*")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestMergedView_RecreatedDirectoryHidesOldLowerContents(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "old.txt"), []byte("old"), 0o644))

	require.NoError(t, mv.RemoveAll("d"))
	require.NoError(t, mv.WriteFile(filepath.Join("d", "new.txt"), []byte("new"), 0o644))

	assertNotInView(t, mv, filepath.Join("d", "old.txt"))
	entries, err := mv.ReadDir("d")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "new.txt", entries[0].Name())
	results, err := mv.Search(".", "*.txt")
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join("d", "new.txt")}, results)
}

func TestMergedView_UpperFileHidesLowerDirectory(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, "d"), []byte("file"), 0o644))

	assertNotInView(t, mv, filepath.Join("d", "x.txt"))
	data, err := mv.ReadFile("d")
	require.NoError(t, err)
	assert.Equal(t, "file", string(data))
}

// In rw mode the runtime passes the workspace as both lower and upper, so
// every file "hides" itself in lower; reads must not change.
func TestMergedView_RWModeSameUpperAndLower(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	mv := NewMergedView(dir, dir, WorkspaceRW, uint32(os.Getuid()), uint32(os.Getgid()))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d", "x.txt"), []byte("x"), 0o644))

	data, err := mv.ReadFile(filepath.Join("d", "x.txt"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
	entries, err := mv.ReadDir("d")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "x.txt", entries[0].Name())
	results, err := mv.Search(".", "*.txt")
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join("d", "x.txt")}, results)

	_, err = mv.ReadDir(filepath.Join("d", "x.txt"))
	assert.False(t, errors.Is(err, fs.ErrNotExist), "an existing file is not missing: %v", err)
}

// An upper file that exists but cannot be read keeps its error: it is
// neither missing nor a reason to serve the lower copy.
func TestMergedView_UnreadableUpperFileKeepsItsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode 000 files")
	}
	for _, sameDir := range []bool{false, true} {
		mv := newTestMergedView(t, WorkspaceRW)
		if sameDir {
			// rw mode as the runtime builds it.
			mv = NewMergedView(mv.UpperDir, mv.UpperDir, WorkspaceRW, mv.Uid, mv.Gid)
		} else {
			require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "f.txt"), []byte("stale"), 0o644))
		}
		require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, "f.txt"), []byte("new"), 0o000))

		_, err := mv.ReadFile("f.txt")
		assert.ErrorIs(t, err, fs.ErrPermission, "ReadFile, same dir %v", sameDir)
		f, err := mv.Open("f.txt")
		if err == nil {
			f.Close()
		}
		assert.ErrorIs(t, err, fs.ErrPermission, "Open, same dir %v", sameDir)
	}
}
