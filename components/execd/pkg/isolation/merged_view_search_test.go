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
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergedView_SearchMergedVisibility(t *testing.T) {
	for _, mode := range []WorkspaceMode{WorkspaceOverlay, WorkspaceRW} {
		t.Run(string(mode), func(t *testing.T) {
			mv := newTestMergedView(t, mode)
			for _, layer := range []struct {
				root  string
				paths []string
			}{
				{mv.LowerDir, []string{"same.txt", "deleted.txt", "blocked/hidden.txt", "nested/lower.txt"}},
				{mv.UpperDir, []string{"same.txt", "a.txt", ".wh.deleted.txt", "blocked", "nested/upper.txt"}},
			} {
				for _, relativePath := range layer.paths {
					filePath := filepath.Join(layer.root, relativePath)
					require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o755))
					require.NoError(t, os.WriteFile(filePath, nil, 0o644))
				}
			}

			results, err := mv.Search(".", "*.txt")
			require.NoError(t, err)
			assert.Equal(t, []string{"a.txt", filepath.Join("nested", "lower.txt"), filepath.Join("nested", "upper.txt"), "same.txt"}, results)

			// A nonmatching upper file still hides the lower directory below it.
			results, err = mv.Search("blocked", "*.txt")
			require.NoError(t, err)
			assert.Empty(t, results)
			results, err = mv.Search(".", "*")
			require.NoError(t, err)
			assert.Equal(t, []string{"a.txt", "blocked", filepath.Join("nested", "lower.txt"), filepath.Join("nested", "upper.txt"), "same.txt"}, results)
		})
	}
}

func TestMergedView_SearchSameRootRW(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	mv.LowerDir = mv.UpperDir
	for _, relativePath := range []string{"z.txt", "a.txt", "nested/b.txt"} {
		filePath := filepath.Join(mv.UpperDir, relativePath)
		require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o755))
		require.NoError(t, os.WriteFile(filePath, nil, 0o644))
	}

	for _, scenario := range []struct {
		root string
		want []string
	}{
		{".", []string{"a.txt", filepath.Join("nested", "b.txt"), "z.txt"}},
		{"nested", []string{filepath.Join("nested", "b.txt")}},
		{"a.txt", []string{"a.txt"}},
		{"missing", nil},
	} {
		t.Run(scenario.root, func(t *testing.T) {
			results, err := mv.Search(scenario.root, "*.txt")
			require.NoError(t, err)
			assert.Equal(t, scenario.want, results)
		})
	}
}

func TestMergedView_SearchRWMarkerShapedFiles(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	mv.LowerDir = mv.UpperDir
	for _, name := range []string{"visible.txt", ".wh.deleted.txt", ".wh..wh.foo", ".wh.foo", "foo"} {
		require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, name), nil, 0o644))
	}

	// Preserve the existing visibility of marker-shaped files in a shared RW directory.
	results, err := mv.Search(".", "*")
	require.NoError(t, err)
	assert.Equal(t, []string{".wh..wh.foo", ".wh.deleted.txt", "foo", "visible.txt"}, results)
	results, err = mv.Search(".", "*.txt")
	require.NoError(t, err)
	assert.Equal(t, []string{".wh.deleted.txt", "visible.txt"}, results)
}

func TestMergedView_SearchLowerWithoutUpper(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRO)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "lower.txt"), nil, 0o644))
	for _, upper := range []string{"", filepath.Join(mv.UpperDir, "missing")} {
		mv.UpperDir = upper
		results, err := mv.Search(".", "*.txt")
		require.NoError(t, err)
		assert.Equal(t, []string{"lower.txt"}, results)
	}
}

func TestMergedView_SearchDoesNotFollowDirectorySymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on Windows")
	}
	mv := newTestMergedView(t, WorkspaceOverlay)
	external := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(external, "outside.txt"), nil, 0o644))
	for _, root := range []string{mv.LowerDir, mv.UpperDir} {
		require.NoError(t, os.Symlink(external, filepath.Join(root, "linked")))
	}

	results, err := mv.Search(".", "*")
	require.NoError(t, err)
	assert.Equal(t, []string{"linked"}, results)
}

func TestMergedView_SearchSkipsUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX directory permissions enforced for a non-root user")
	}
	for _, sameRoot := range []bool{false, true} {
		name := "separate_roots"
		if sameRoot {
			name = "same_root_rw"
		}
		t.Run(name, func(t *testing.T) {
			mv := newTestMergedView(t, WorkspaceRW)
			if sameRoot {
				mv.LowerDir = mv.UpperDir
			}
			blocked := filepath.Join(mv.UpperDir, "blocked")
			require.NoError(t, os.Mkdir(blocked, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(blocked, "hidden.txt"), nil, 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, "visible.txt"), nil, 0o644))
			require.NoError(t, os.Chmod(blocked, 0o000))
			t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

			results, err := mv.Search(".", "*.txt")
			require.NoError(t, err)
			assert.Equal(t, []string{"visible.txt"}, results)
			results, err = mv.Search("blocked", "*.txt")
			require.NoError(t, err)
			assert.Empty(t, results)
		})
	}
}
