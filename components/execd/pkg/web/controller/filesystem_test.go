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
	require.Equal(t, "file", info.Type)
	require.NotZero(t, info.Size)
}

func TestFilesystemControllerGetFilesInfoReportsSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target.txt")
	linkPath := filepath.Join(tmpDir, "link.txt")
	require.NoError(t, os.WriteFile(target, []byte("demo"), 0o644))
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	query := fmt.Sprintf("/files/info?path=%s", url.QueryEscape(linkPath))
	ctrl, rec := newFilesystemController(t, http.MethodGet, query, nil)

	ctrl.GetFilesInfo()

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	info, ok := resp[linkPath]
	require.True(t, ok, "response missing entry for %s", linkPath)
	require.Equal(t, "symlink", info.Type)
}

func TestFilesystemControllerGetFilesInfoReturnsNotFoundForMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-does-not-exist.txt")
	query := fmt.Sprintf("/files/info?path=%s", url.QueryEscape(missing))
	ctrl, rec := newFilesystemController(t, http.MethodGet, query, nil)

	ctrl.GetFilesInfo()

	require.Equal(t, http.StatusNotFound, rec.Code)
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeFileNotFound, resp.Code)
}

func TestFilesystemControllerRenameFilesReturnsNotFoundForMissingSource(t *testing.T) {
	tmpDir := t.TempDir()
	missingSrc := filepath.Join(tmpDir, "missing.txt")
	dst := filepath.Join(tmpDir, "dest.txt")
	payload, err := json.Marshal([]model.RenameFileItem{{Src: missingSrc, Dest: dst}})
	require.NoError(t, err)
	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/mv", payload)

	ctrl.RenameFiles()

	require.Equal(t, http.StatusNotFound, rec.Code)
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeFileNotFound, resp.Code)
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
	require.Equal(t, "file", files[0].Type)
}

func TestFilesystemControllerListDirectoryDefaultDepth(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "alpha.txt")
	dirPath := filepath.Join(tmpDir, "nested")
	deepFile := filepath.Join(dirPath, "deep.txt")
	require.NoError(t, os.MkdirAll(dirPath, 0o755))
	require.NoError(t, os.WriteFile(filePath, []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(deepFile, []byte("deep"), 0o644))

	rawURL := fmt.Sprintf("/directories/list?path=%s", url.QueryEscape(tmpDir))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.ListDirectory()

	require.Equal(t, http.StatusOK, rec.Code)
	var entries []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
	require.Len(t, entries, 2)

	byPath := map[string]model.FileInfo{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	require.Equal(t, "file", byPath[filePath].Type)
	require.Equal(t, "directory", byPath[dirPath].Type)
	require.NotContains(t, byPath, deepFile)
}

func TestFilesystemControllerListDirectoryDepth(t *testing.T) {
	tmpDir := t.TempDir()
	nested := filepath.Join(tmpDir, "nested")
	deeper := filepath.Join(nested, "deeper")
	deepFile := filepath.Join(nested, "deep.txt")
	tooDeep := filepath.Join(deeper, "too-deep.txt")
	require.NoError(t, os.MkdirAll(deeper, 0o755))
	require.NoError(t, os.WriteFile(deepFile, []byte("deep"), 0o644))
	require.NoError(t, os.WriteFile(tooDeep, []byte("too deep"), 0o644))

	rawURL := fmt.Sprintf("/directories/list?path=%s&depth=2", url.QueryEscape(tmpDir))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.ListDirectory()

	require.Equal(t, http.StatusOK, rec.Code)
	var entries []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))

	byPath := map[string]model.FileInfo{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	require.Equal(t, "directory", byPath[nested].Type)
	require.Equal(t, "directory", byPath[deeper].Type)
	require.Equal(t, "file", byPath[deepFile].Type)
	require.NotContains(t, byPath, tooDeep)
}

func TestFilesystemControllerListDirectoryDepthZero(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "alpha.txt"), []byte("alpha"), 0o644))

	rawURL := fmt.Sprintf("/directories/list?path=%s&depth=0", url.QueryEscape(tmpDir))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.ListDirectory()

	require.Equal(t, http.StatusOK, rec.Code)
	var entries []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
	require.Empty(t, entries)
}

func TestFilesystemControllerListDirectoryReportsSymlinkWithoutRecursing(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "target")
	linkPath := filepath.Join(tmpDir, "link")
	targetFile := filepath.Join(targetDir, "target.txt")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(targetFile, []byte("target"), 0o644))
	if err := os.Symlink(targetDir, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	rawURL := fmt.Sprintf("/directories/list?path=%s&depth=2", url.QueryEscape(tmpDir))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.ListDirectory()

	require.Equal(t, http.StatusOK, rec.Code)
	var entries []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))

	byPath := map[string]model.FileInfo{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	require.Equal(t, "symlink", byPath[linkPath].Type)
	require.NotContains(t, byPath, filepath.Join(linkPath, "target.txt"))
	require.Contains(t, byPath, targetFile)
}

func TestFilesystemControllerListDirectoryReturnsLexicalOrder(t *testing.T) {
	tmpDir := t.TempDir()
	// Create entries in non-lexical creation order to make sure the response
	// reflects the contracted lexical-by-name ordering rather than insertion
	// order or os-specific listing order.
	for _, name := range []string{"charlie.txt", "alpha", "bravo.txt"} {
		full := filepath.Join(tmpDir, name)
		if name == "alpha" {
			require.NoError(t, os.MkdirAll(full, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(full, "y.txt"), []byte("y"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(full, "x.txt"), []byte("x"), 0o644))
			continue
		}
		require.NoError(t, os.WriteFile(full, []byte(name), 0o644))
	}

	rawURL := fmt.Sprintf("/directories/list?path=%s&depth=2", url.QueryEscape(tmpDir))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.ListDirectory()

	require.Equal(t, http.StatusOK, rec.Code)
	var entries []model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	require.Equal(t, []string{
		filepath.Join(tmpDir, "alpha"),
		filepath.Join(tmpDir, "alpha", "x.txt"),
		filepath.Join(tmpDir, "alpha", "y.txt"),
		filepath.Join(tmpDir, "bravo.txt"),
		filepath.Join(tmpDir, "charlie.txt"),
	}, paths)
}

func TestFilesystemControllerListDirectoryRejectsSymlinkRoot(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "real")
	linkPath := filepath.Join(tmpDir, "link-to-real")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "leak.txt"), []byte("leak"), 0o644))
	if err := os.Symlink(targetDir, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	rawURL := fmt.Sprintf("/directories/list?path=%s", url.QueryEscape(linkPath))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.ListDirectory()

	require.Equal(t, http.StatusBadRequest, rec.Code, "symlink as root should be rejected per spec")
	// Make sure the response body does not leak the target directory contents.
	require.NotContains(t, rec.Body.String(), "leak.txt")
}

func TestFilesystemControllerListDirectoryRejectsInvalidRequests(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "file.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("file"), 0o644))

	tests := []struct {
		name   string
		rawURL string
		want   int
	}{
		{name: "missing path", rawURL: "/directories/list", want: http.StatusBadRequest},
		{name: "missing directory", rawURL: "/directories/list?path=/not/exists", want: http.StatusNotFound},
		{name: "file path", rawURL: fmt.Sprintf("/directories/list?path=%s", url.QueryEscape(filePath)), want: http.StatusBadRequest},
		{name: "invalid depth", rawURL: fmt.Sprintf("/directories/list?path=%s&depth=abc", url.QueryEscape(tmpDir)), want: http.StatusBadRequest},
		{name: "negative depth", rawURL: fmt.Sprintf("/directories/list?path=%s&depth=-1", url.QueryEscape(tmpDir)), want: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl, rec := newFilesystemController(t, http.MethodGet, tt.rawURL, nil)
			ctrl.ListDirectory()
			require.Equal(t, tt.want, rec.Code)
		})
	}
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

	require.Equal(t, http.StatusNotFound, rec.Code)
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeFileNotFound, resp.Code)
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

func writeTestFileWithLines(t *testing.T, lines []string) string {
	t.Helper()
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "lines.txt")
	content := ""
	for i, l := range lines {
		if i > 0 {
			content += "\n"
		}
		content += l
	}
	require.NoError(t, os.WriteFile(target, []byte(content), 0o644))
	return target
}

func TestDownloadFileLineBasedReading(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1", "line2", "line3", "line4", "line5"})
	rawURL := fmt.Sprintf("/files/download?path=%s&offset=2&limit=2", url.QueryEscape(target))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.DownloadFile()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "line2\nline3", rec.Body.String())
	require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestDownloadFileLineBasedOffsetOnly(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1", "line2", "line3"})
	rawURL := fmt.Sprintf("/files/download?path=%s&offset=2", url.QueryEscape(target))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.DownloadFile()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "line2\nline3", rec.Body.String())
}

func TestDownloadFileLimitOnly(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1", "line2", "line3"})
	rawURL := fmt.Sprintf("/files/download?path=%s&limit=2", url.QueryEscape(target))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.DownloadFile()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "line1\nline2", rec.Body.String())
}

func TestDownloadFileOffsetBeyondEOF(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1", "line2"})
	rawURL := fmt.Sprintf("/files/download?path=%s&offset=100", url.QueryEscape(target))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.DownloadFile()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.String())
}

func TestDownloadFileLimitBeyondRemaining(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1", "line2", "line3"})
	rawURL := fmt.Sprintf("/files/download?path=%s&offset=2&limit=100", url.QueryEscape(target))
	ctrl, rec := newFilesystemController(t, http.MethodGet, rawURL, nil)

	ctrl.DownloadFile()

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "line2\nline3", rec.Body.String())
}

func TestDownloadFileLineRangeConflict(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1"})
	rawURL := fmt.Sprintf("/files/download?path=%s&offset=1&limit=1", url.QueryEscape(target))
	ctx, rec := newTestContext(http.MethodGet, rawURL, nil)
	ctx.Request.Header.Set("Range", "bytes=0-10")
	ctrl := NewFilesystemController(ctx)

	ctrl.DownloadFile()

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeInvalidRequest, resp.Code)
}

func TestDownloadFileInvalidOffset(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1"})
	tests := []struct {
		name   string
		rawURL string
	}{
		{name: "non-numeric", rawURL: fmt.Sprintf("/files/download?path=%s&offset=abc", url.QueryEscape(target))},
		{name: "zero", rawURL: fmt.Sprintf("/files/download?path=%s&offset=0", url.QueryEscape(target))},
		{name: "negative", rawURL: fmt.Sprintf("/files/download?path=%s&offset=-1", url.QueryEscape(target))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl, rec := newFilesystemController(t, http.MethodGet, tt.rawURL, nil)
			ctrl.DownloadFile()
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestDownloadFileInvalidLimit(t *testing.T) {
	target := writeTestFileWithLines(t, []string{"line1"})
	tests := []struct {
		name   string
		rawURL string
	}{
		{name: "non-numeric", rawURL: fmt.Sprintf("/files/download?path=%s&limit=abc", url.QueryEscape(target))},
		{name: "zero", rawURL: fmt.Sprintf("/files/download?path=%s&limit=0", url.QueryEscape(target))},
		{name: "negative", rawURL: fmt.Sprintf("/files/download?path=%s&limit=-1", url.QueryEscape(target))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl, rec := newFilesystemController(t, http.MethodGet, tt.rawURL, nil)
			ctrl.DownloadFile()
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}
