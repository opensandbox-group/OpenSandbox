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

package isolation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMergedView(t *testing.T, mode WorkspaceMode) *MergedView {
	t.Helper()
	lower := filepath.Join(t.TempDir(), "lower")
	upper := filepath.Join(t.TempDir(), "upper")
	require.NoError(t, os.MkdirAll(lower, 0o755))
	require.NoError(t, os.MkdirAll(upper, 0o755))
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

	// Verify in upper.
	data, err := os.ReadFile(filepath.Join(mv.UpperDir, "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, []byte("session-data"), data)

	// Verify NOT in lower.
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
	assert.Error(t, err, "cannot remove lower-only file without whiteout")
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

	// Old gone.
	_, err := mv.Stat("old.txt")
	assert.True(t, os.IsNotExist(err))

	// New exists.
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

	results, err := mv.Search("*.txt")
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
