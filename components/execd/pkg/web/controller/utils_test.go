// Copyright 2025 Alibaba Group Holding Ltd.
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

package controller

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func TestDeleteFile(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "sample.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o644))

	require.NoError(t, DeleteFile(file))
	_, err := os.Stat(file)
	require.True(t, os.IsNotExist(err), "expected file removed, got err=%v", err)

	// removing a non-existent file should be a no-op
	require.NoError(t, DeleteFile(file), "expected no error deleting missing file")
}

func TestRenameFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o644))

	dst := filepath.Join(tmp, "nested", "renamed.txt")
	require.NoError(t, RenameFile(model.RenameFileItem{Src: src, Dest: dst}))

	_, err := os.Stat(dst)
	require.NoError(t, err)
	_, err = os.Stat(src)
	require.True(t, os.IsNotExist(err), "expected source removed, got err=%v", err)

	// destination exists -> expect error
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o644))
	require.Error(t, RenameFile(model.RenameFileItem{Src: src, Dest: dst}), "expected error when destination already exists")
}

func TestMakeDir_PreExistingDir(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "existing")
	require.NoError(t, os.Mkdir(existing, 0o755))

	origInfo, err := os.Stat(existing)
	require.NoError(t, err)
	origMode := origInfo.Mode().Perm()

	err = MakeDir(existing, model.Permission{Mode: 700})
	require.NoError(t, err, "MakeDir on pre-existing dir should not fail")

	afterInfo, err := os.Stat(existing)
	require.NoError(t, err)
	require.Equal(t, origMode, afterInfo.Mode().Perm(), "permissions of pre-existing dir should be unchanged")
}

func TestMakeDir_NewDir(t *testing.T) {
	tmp := t.TempDir()
	newDir := filepath.Join(tmp, "brand-new")

	err := MakeDir(newDir, model.Permission{Mode: 755})
	require.NoError(t, err)

	info, err := os.Stat(newDir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	}
}

func TestSetFileOwnership_EmptyOwnerGroup(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "test.txt")
	require.NoError(t, os.WriteFile(file, []byte("data"), 0o644))

	err := SetFileOwnership(file, "", "")
	require.NoError(t, err, "empty owner and group should be a no-op")
}

func TestSetFileOwnership_MissingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SetFileOwnership is a no-op on Windows")
	}
	err := SetFileOwnership("/nonexistent/path/file.txt", "root", "")
	require.Error(t, err, "chown on missing file should return error")
}

func TestSetFileOwnership_InvalidOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SetFileOwnership is a no-op on Windows")
	}
	tmp := t.TempDir()
	file := filepath.Join(tmp, "test.txt")
	require.NoError(t, os.WriteFile(file, []byte("data"), 0o644))

	err := SetFileOwnership(file, "nonexistent_user_xyz", "")
	require.Error(t, err, "invalid owner should return error")
}

func TestSearchFileMetadata(t *testing.T) {
	metadata := map[string]model.FileMetadata{
		"/tmp/a/notes.txt": {Path: "/tmp/a/notes.txt"},
		"/tmp/b/readme.md": {Path: "/tmp/b/readme.md"},
	}

	path, info, ok := SearchFileMetadata(metadata, "/any/notes.txt")
	require.True(t, ok, "expected metadata entry")
	require.Equal(t, "/tmp/a/notes.txt", path)
	require.Equal(t, "/tmp/a/notes.txt", info.Path)

	_, _, ok = SearchFileMetadata(metadata, "/foo/unknown.txt")
	require.False(t, ok, "expected no match")
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		size      int64
		want      []httpRange
		expectErr bool
	}{
		{
			name:   "start-end",
			header: "bytes=0-9",
			size:   20,
			want:   []httpRange{{start: 0, length: 10}},
		},
		{
			name:   "suffix",
			header: "bytes=-5",
			size:   10,
			want:   []httpRange{{start: 5, length: 5}},
		},
		{
			name:      "invalid",
			header:    "bytes=foo",
			size:      10,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRange(tt.header, tt.size)
			if tt.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.True(t, reflect.DeepEqual(got, tt.want), "got %+v want %+v", got, tt.want)
		})
	}
}
