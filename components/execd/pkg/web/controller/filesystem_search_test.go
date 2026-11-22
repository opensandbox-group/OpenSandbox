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

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func TestFilesystemControllerSearchFilesRecursiveMetadata(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "alpha.txt"),
		filepath.Join(root, "branch", "beta.txt"),
		filepath.Join(root, "branch", "nested", "gamma.txt"),
		filepath.Join(root, "directory.txt", "inside.txt"),
		filepath.Join(root, "zeta.txt"),
	}
	for _, path := range append([]string{filepath.Join(root, "ignored.log")}, paths...) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(filepath.Base(path)), 0o640))
	}

	for _, test := range []struct {
		name    string
		pattern string
		paths   []string
	}{
		{name: "recursive_basename_matches", pattern: "*.txt", paths: paths},
		{name: "parent_path_does_not_match_basename", pattern: "branch/*.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rawURL := fmt.Sprintf("/files/search?path=%s&pattern=%s", url.QueryEscape(root), url.QueryEscape(test.pattern))
			ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
			ctrl.SearchFiles()

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var files []model.FileInfo
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
			require.Len(t, files, len(test.paths))
			for index, path := range test.paths {
				require.Equal(t, path, files[index].Path, "results must retain lexical traversal order")
				require.Equal(t, "file", files[index].Type)
				require.Equal(t, int64(len(filepath.Base(path))), files[index].Size)
				query := fmt.Sprintf("/files/info?path=%s", url.QueryEscape(path))
				infoCtrl, infoRec := newFilesystemController(t, http.MethodGet, query, nil)
				infoCtrl.GetFilesInfo()
				require.Equal(t, http.StatusOK, infoRec.Code, infoRec.Body.String())
				var metadata map[string]model.FileInfo
				require.NoError(t, json.Unmarshal(infoRec.Body.Bytes(), &metadata))
				require.Equal(t, metadata[path].Permission, files[index].Permission)
				require.Equal(t, metadata[path].ModifiedAt, files[index].ModifiedAt)
				if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
					require.Equal(t, metadata[path].CreatedAt, files[index].CreatedAt)
				} else {
					// Other platforms currently return the lookup time as created_at.
					require.NotZero(t, files[index].CreatedAt)
				}
			}
		})
	}
}

func TestFilesystemControllerSearchFilesDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	target := filepath.Join(external, "outside.txt")
	require.NoError(t, os.WriteFile(target, []byte("outside"), 0o644))
	fileLink := filepath.Join(root, "file-link")
	directoryLink := filepath.Join(root, "directory-link")
	danglingLink := filepath.Join(root, "missing-link")
	for _, link := range []struct{ target, path string }{
		{target, fileLink},
		{external, directoryLink},
		{filepath.Join(external, "missing.txt"), danglingLink},
	} {
		if err := os.Symlink(link.target, link.path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}

	rawURL := fmt.Sprintf("/files/search?path=%s", url.QueryEscape(root))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
	ctrl.SearchFiles()

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var files []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
	require.Len(t, files, 3)
	require.Equal(t, directoryLink, files[0].Path)
	require.Equal(t, fileLink, files[1].Path)
	require.Equal(t, danglingLink, files[2].Path)
	for _, file := range files {
		require.Equal(t, "symlink", file.Type)
	}
}

func TestFilesystemControllerSearchFilesConcurrentRemoval(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 512)
	for index := range paths {
		paths[index] = filepath.Join(root, fmt.Sprintf("file-%04d.txt", index))
		require.NoError(t, os.WriteFile(paths[index], []byte("content"), 0o644))
	}
	removed := make(chan error, 1)
	go func() {
		for index := len(paths) - 1; index >= 0; index-- {
			if err := os.Remove(paths[index]); err != nil {
				removed <- err
				return
			}
		}
		removed <- nil
	}()

	rawURL := fmt.Sprintf("/files/search?path=%s&pattern=*.txt", url.QueryEscape(root))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
	ctrl.SearchFiles()
	require.NoError(t, <-removed)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var files []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
	for _, file := range files {
		require.Contains(t, paths, file.Path)
		require.Equal(t, "file", file.Type)
	}
}

func TestFilesystemControllerSearchFilesRootFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.txt")
	require.NoError(t, os.WriteFile(path, []byte("root"), 0o644))
	for _, pattern := range []string{"*.txt", "*.log"} {
		t.Run(pattern, func(t *testing.T) {
			rawURL := fmt.Sprintf("/files/search?path=%s&pattern=%s", url.QueryEscape(path), url.QueryEscape(pattern))
			ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
			ctrl.SearchFiles()

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var files []model.FileInfo
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
			if pattern == "*.log" {
				require.Empty(t, files)
				return
			}
			require.Len(t, files, 1)
			require.Equal(t, path, files[0].Path)
			require.Equal(t, "file", files[0].Type)
		})
	}
}

func TestFilesystemControllerSearchFilesRootSymlink(t *testing.T) {
	external := t.TempDir()
	file := filepath.Join(external, "outside.txt")
	require.NoError(t, os.WriteFile(file, []byte("outside"), 0o644))
	for _, target := range []string{file, external} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			link := filepath.Join(t.TempDir(), "root-link")
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			rawURL := fmt.Sprintf("/files/search?path=%s", url.QueryEscape(link))
			ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
			ctrl.SearchFiles()

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var files []model.FileInfo
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
			require.Len(t, files, 1)
			require.Equal(t, link, files[0].Path)
			require.Equal(t, "symlink", files[0].Type)
		})
	}
}

func TestFilesystemControllerSearchFilesReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory read/execute permissions are unavailable")
	}
	root := t.TempDir()
	path := filepath.Join(root, "unmatched.bin")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o644))
	require.NoError(t, os.Chmod(root, 0o400))
	t.Cleanup(func() { require.NoError(t, os.Chmod(root, 0o700)) })

	_, err := os.Lstat(path)
	if err == nil {
		t.Skip("process can stat entries without directory execute permission")
	}
	require.ErrorIs(t, err, fs.ErrPermission)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrPermission) {
		t.Skip("filesystem cannot enumerate entry types without directory execute permission")
	}
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, filepath.Base(path), entries[0].Name())

	for _, test := range []struct {
		name    string
		pattern string
		status  int
	}{
		{name: "unmatched_entries_do_not_require_metadata", pattern: "*.txt", status: http.StatusOK},
		{name: "matched_entries_still_require_metadata", pattern: "*.bin", status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			rawURL := fmt.Sprintf("/files/search?path=%s&pattern=%s", url.QueryEscape(root), url.QueryEscape(test.pattern))
			ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
			ctrl.SearchFiles()

			require.Equal(t, test.status, rec.Code, rec.Body.String())
			if test.status == http.StatusOK {
				require.JSONEq(t, "[]", rec.Body.String())
				return
			}
			var response model.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Equal(t, model.ErrorCodeRuntimeError, response.Code)
		})
	}
}

func TestFilesystemControllerSearchFilesReportsUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory read permissions are unavailable")
	}
	root := t.TempDir()
	unreadable := filepath.Join(root, "unmatched-directory")
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(unreadable, 0o700)) })
	_, err := os.ReadDir(unreadable)
	if err == nil {
		t.Skip("process can read directories without read permission")
	}
	require.ErrorIs(t, err, fs.ErrPermission)

	for _, path := range []string{root, unreadable} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			rawURL := fmt.Sprintf("/files/search?path=%s&pattern=%s", url.QueryEscape(path), url.QueryEscape("*.txt"))
			ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)
			ctrl.SearchFiles()

			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			var response model.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Equal(t, model.ErrorCodeRuntimeError, response.Code)
		})
	}
}
