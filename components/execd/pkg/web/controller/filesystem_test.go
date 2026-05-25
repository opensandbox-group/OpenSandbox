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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func newFilesystemController(t *testing.T, method, rawURL string, body []byte) (*FilesystemController, *httptest.ResponseRecorder) {
	t.Helper()
	ctx, rec := newTestContext(method, rawURL, body)
	ctrl := NewFilesystemController(ctx)
	return ctrl, rec
}

func TestFilesystemControllerGetFilesInfo(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "foo.txt")
	require.NoError(t, os.WriteFile(target, []byte("demo"), 0o644))

	query := fmt.Sprintf("/files/info?path=%s", url.QueryEscape(target))
	ctrl, rec := newFilesystemController(t, http.MethodGet, query, nil)

	ctrl.GetFilesInfo()

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	info, ok := resp[target]
	require.True(t, ok, "response missing entry for %s", target)
	require.NotEmpty(t, info.Path)
	require.NotZero(t, info.Size)
}

func TestFilesystemControllerSearchFiles(t *testing.T) {
	tmpDir := t.TempDir()
	a := filepath.Join(tmpDir, "alpha.txt")
	b := filepath.Join(tmpDir, "beta.log")
	require.NoError(t, os.WriteFile(a, []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("beta"), 0o644))

	rawURL := fmt.Sprintf("/files/search?path=%s&pattern=%s", url.QueryEscape(tmpDir), url.QueryEscape("*.txt"))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.SearchFiles()

	require.Equal(t, http.StatusOK, rec.Code)
	var files []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
	require.Len(t, files, 1)
	require.Equal(t, a, files[0].Path)
}

func TestFilesystemControllerReplaceContent(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "content.txt")
	require.NoError(t, os.WriteFile(target, []byte("hello world"), 0o644))

	body, err := json.Marshal(map[string]model.ReplaceFileContentItem{
		target: {
			Old: "world",
			New: "universe",
		},
	})
	require.NoError(t, err)

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/replace", body)

	ctrl.ReplaceContent()

	require.Equal(t, http.StatusOK, rec.Code)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "hello universe", string(data))
}

func TestFilesystemControllerReplaceContentSupportsHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	target := filepath.Join(home, "content.txt")
	require.NoError(t, os.WriteFile(target, []byte("hello world"), 0o644))

	body, err := json.Marshal(map[string]model.ReplaceFileContentItem{
		"~/content.txt": {
			Old: "world",
			New: "home",
		},
	})
	require.NoError(t, err)

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/replace", body)
	ctrl.ReplaceContent()

	require.Equal(t, http.StatusOK, rec.Code)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "hello home", string(data))
}

func TestFilesystemControllerSearchFilesHandlesAbsentDir(t *testing.T) {
	rawURL := "/files/search?path=/not/exists"
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.SearchFiles()

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestReplaceContentFailsUnknownFile(t *testing.T) {
	payload, _ := json.Marshal(map[string]model.ReplaceFileContentItem{
		filepath.Join(t.TempDir(), "missing.txt"): {
			Old: "old",
			New: "new",
		},
	})
	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/replace", payload)

	ctrl.ReplaceContent()

	require.Contains(t, []int{http.StatusNotFound, http.StatusInternalServerError}, rec.Code, "expected failure status")
}

func TestFormatContentDisposition(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "ASCII filename",
			filename: "test.txt",
			want:     "attachment; filename=\"test.txt\"",
		},
		{
			name:     "Chinese filename",
			filename: "测试文件.txt",
			want:     "attachment; filename=\"%E6%B5%8B%E8%AF%95%E6%96%87%E4%BB%B6.txt\"; filename*=UTF-8''%E6%B5%8B%E8%AF%95%E6%96%87%E4%BB%B6.txt",
		},
		{
			name:     "Japanese filename",
			filename: "テスト.txt",
			want:     "attachment; filename=\"%E3%83%86%E3%82%B9%E3%83%88.txt\"; filename*=UTF-8''%E3%83%86%E3%82%B9%E3%83%88.txt",
		},
		{
			name:     "Special characters in filename",
			filename: "file with spaces.txt",
			want:     "attachment; filename=\"file with spaces.txt\"",
		},
		{
			name:     "Mixed ASCII and non-ASCII",
			filename: "report-报告.pdf",
			want:     "attachment; filename=\"report-%E6%8A%A5%E5%91%8A.pdf\"; filename*=UTF-8''report-%E6%8A%A5%E5%91%8A.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatContentDisposition(tt.filename)
			require.Equal(t, tt.want, got)
		})
	}
}
