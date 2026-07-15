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

package controller

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/vfs"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func (c *IsolatedSessionController) getMergedView() (vfs.FS, error) {
	if isolatedRunner == nil || !isolatedRunner.Available() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return nil, fmt.Errorf("isolation unavailable")
	}
	sessionID := c.ctx.Param("sessionId")
	mv, err := isolatedRunner.GetMergedView(sessionID)
	if err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return nil, err
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return nil, err
	}
	if mv == nil {
		c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
		return nil, fmt.Errorf("no merged view")
	}
	return mv, nil
}

func (c *IsolatedSessionController) GetFilesInfo() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	paths := c.ctx.QueryArray("path")
	if len(paths) == 0 {
		c.RespondSuccess(make(map[string]model.FileInfo))
		return
	}

	resp := make(map[string]model.FileInfo)
	for _, filePath := range paths {
		cleaned := filepath.Clean(filePath)
		info, err := mv.Stat(cleaned)
		if err != nil {
			if os.IsNotExist(err) {
				c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			} else {
				c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			}
			return
		}
		resp[filePath] = buildIsolatedFileInfo(filePath, info)
	}
	c.RespondSuccess(resp)
}

func (c *IsolatedSessionController) SearchFiles() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	path := c.ctx.Query("path")
	if path == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing query parameter 'path'")
		return
	}

	pattern := c.ctx.Query("pattern")
	if pattern == "" {
		pattern = "**"
	}

	results, err := mv.Search(path, pattern)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	files := make([]model.FileInfo, 0, len(results))
	for _, rel := range results {
		info, err := mv.Stat(rel)
		if err != nil {
			continue
		}
		files = append(files, buildIsolatedFileInfo(rel, info))
	}
	c.RespondSuccess(files)
}

func (c *IsolatedSessionController) DownloadFile() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	filePath := c.ctx.Query("path")
	if filePath == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "path is required")
		return
	}

	rawOffset := c.ctx.Query("offset")
	rawLimit := c.ctx.Query("limit")
	hasLineParams := rawOffset != "" || rawLimit != ""
	rangeHeader := c.ctx.GetHeader("Range")

	if hasLineParams && rangeHeader != "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
			"line-based reading (offset/limit) and byte range (Range header) are mutually exclusive")
		return
	}

	f, err := mv.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	defer f.Close()

	if hasLineParams {
		c.serveIsolatedLineRange(f, rawOffset, rawLimit)
		return
	}

	fileInfo, err := f.Stat()
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.ctx.Header("Content-Type", "application/octet-stream")
	c.ctx.Header("Content-Disposition", formatContentDisposition(filepath.Base(filePath)))
	c.ctx.Header("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

	if rangeHeader != "" {
		ranges, err := ParseRange(rangeHeader, fileInfo.Size())
		if err != nil {
			c.RespondError(http.StatusRequestedRangeNotSatisfiable, model.ErrorCodeUnknown)
			return
		}
		if len(ranges) > 0 {
			r := ranges[0]
			c.ctx.Status(http.StatusPartialContent)
			c.ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, fileInfo.Size()))
			c.ctx.Header("Content-Length", strconv.FormatInt(r.length, 10))

			_, _ = f.Seek(r.start, io.SeekStart)
			_, _ = io.CopyN(c.ctx.Writer, f, r.length)
			return
		}
	}

	http.ServeContent(c.ctx.Writer, c.ctx.Request, filepath.Base(filePath), fileInfo.ModTime(), f)
}

func (c *IsolatedSessionController) serveIsolatedLineRange(file *os.File, rawOffset, rawLimit string) {
	offset := int64(1)
	if rawOffset != "" {
		parsed, err := strconv.ParseInt(rawOffset, 10, 64)
		if err != nil || parsed < 1 {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
				fmt.Sprintf("invalid query parameter 'offset': %s", rawOffset))
			return
		}
		offset = parsed
	}

	limit := int64(-1)
	if rawLimit != "" {
		parsed, err := strconv.ParseInt(rawLimit, 10, 64)
		if err != nil || parsed < 1 {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
				fmt.Sprintf("invalid query parameter 'limit': %s", rawLimit))
			return
		}
		limit = parsed
	}

	c.ctx.Header("Content-Type", "text/plain; charset=utf-8")
	c.ctx.Status(http.StatusOK)

	reader := bufio.NewReader(file)
	var lineNum int64
	var written int64
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			lineNum++
			if lineNum >= offset {
				if written > 0 {
					_, _ = c.ctx.Writer.Write([]byte("\n"))
				}
				_, _ = c.ctx.Writer.Write(line)
				written++
				if limit >= 0 && written >= limit {
					break
				}
			}
		}
		if err != nil {
			break
		}
	}
}

func (c *IsolatedSessionController) UploadFile() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	metadataParts, fileParts, uerr := parseUploadForm(c.ctx)
	if uerr != nil {
		c.RespondError(uerr.status, uerr.code, uerr.message)
		return
	}

	for i := range metadataParts {
		meta, uerr := parseUploadMetadata(metadataParts[i])
		if uerr != nil {
			c.RespondError(uerr.status, uerr.code, uerr.message)
			return
		}

		file, err := fileParts[i].Open()
		if err != nil {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidFile, err.Error())
			return
		}

		filePath := filepath.Clean(meta.Path)
		dir := filepath.Dir(filePath)
		if dir != "." && dir != "/" {
			if err := mv.MkdirAll(dir, 0o755); err != nil {
				log.Warn("isolated upload: mkdir %s: %v", dir, err)
			}
		}

		perm := os.FileMode(0o644)
		if meta.Permission.Mode != 0 {
			mode, convErr := strconv.ParseUint(strconv.Itoa(meta.Permission.Mode), 8, 32)
			if convErr == nil {
				perm = os.FileMode(mode)
			}
		}

		if _, err := mv.WriteFileReader(filePath, file, perm); err != nil {
			file.Close()
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			return
		}
		file.Close()
	}

	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) RemoveFiles() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	paths := c.ctx.QueryArray("path")
	for _, p := range paths {
		if err := mv.Remove(p); err != nil {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			return
		}
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) RenameFiles() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	var request []model.RenameFileItem
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err))
		return
	}

	for _, item := range request {
		if err := mv.Rename(item.Src, item.Dest); err != nil {
			if os.IsNotExist(err) {
				c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			} else {
				c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			}
			return
		}
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) ChmodFiles() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	var request map[string]model.Permission
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err))
		return
	}

	for file, item := range request {
		if item.Mode != 0 {
			mode, err := strconv.ParseUint(strconv.Itoa(item.Mode), 8, 32)
			if err != nil {
				c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
					fmt.Sprintf("invalid mode for %s: %v", file, err))
				return
			}
			if err := mv.Chmod(file, os.FileMode(mode)); err != nil {
				c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
				return
			}
		}
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) ReplaceContent() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	verbose := c.ctx.Query("verbose") == "true"

	var request map[string]model.ReplaceFileContentItem
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err))
		return
	}

	var results map[string]model.ReplaceFileContentResult
	if verbose {
		results = make(map[string]model.ReplaceFileContentResult)
	}

	for file, item := range request {
		if item.Old == "" {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "old content must not be empty")
			return
		}

		data, err := mv.ReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			} else {
				c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			}
			return
		}

		contentStr := string(data)
		if verbose {
			results[file] = model.ReplaceFileContentResult{
				ReplacedCount: strings.Count(contentStr, item.Old),
			}
		}

		newContent := strings.ReplaceAll(contentStr, item.Old, item.New)
		origInfo, _ := mv.Stat(file)
		perm := os.FileMode(0o644)
		if origInfo != nil {
			perm = origInfo.Mode().Perm()
		}
		if err := mv.WriteFile(file, []byte(newContent), perm); err != nil {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			return
		}
	}

	if verbose {
		c.RespondSuccess(results)
	} else {
		c.RespondSuccess(nil)
	}
}

func (c *IsolatedSessionController) MakeDirs() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	var request map[string]model.Permission
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err))
		return
	}

	for dir, perm := range request {
		mode := os.FileMode(0o755)
		if perm.Mode != 0 {
			parsed, err := strconv.ParseUint(strconv.Itoa(perm.Mode), 8, 32)
			if err == nil {
				mode = os.FileMode(parsed)
			}
		}
		if err := mv.MkdirAll(dir, mode); err != nil {
			if os.IsNotExist(err) {
				c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			} else {
				c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			}
			return
		}
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) RemoveDirs() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	paths := c.ctx.QueryArray("path")
	for _, p := range paths {
		if err := mv.RemoveAll(p); err != nil {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			return
		}
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) ListDirectory() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	path := c.ctx.Query("path")
	if path == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing query parameter 'path'")
		return
	}

	depth := 1
	if rawDepth := c.ctx.Query("depth"); rawDepth != "" {
		parsedDepth, err := strconv.Atoi(rawDepth)
		if err != nil || parsedDepth < 0 {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest,
				fmt.Sprintf("invalid query parameter 'depth': %s", rawDepth))
			return
		}
		depth = parsedDepth
	}

	entries, err := c.listIsolatedDir(mv, path, depth)
	if err != nil {
		if os.IsNotExist(err) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
		} else {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		}
		return
	}
	c.RespondSuccess(entries)
}

func (c *IsolatedSessionController) listIsolatedDir(mv vfs.FS, root string, maxDepth int) ([]model.FileInfo, error) {
	entries := make([]model.FileInfo, 0, 16)
	if maxDepth == 0 {
		return entries, nil
	}

	var walk func(string, int) error
	walk = func(dir string, currentDepth int) error {
		dirEntries, err := mv.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range dirEntries {
			entryPath := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return err
			}
			entries = append(entries, buildIsolatedFileInfo(entryPath, info))
			if entry.IsDir() && currentDepth+1 < maxDepth {
				if err := walk(entryPath, currentDepth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}

	return entries, walk(root, 0)
}

func buildIsolatedFileInfo(path string, info os.FileInfo) model.FileInfo {
	ft := "file"
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		ft = "symlink"
	} else if info.IsDir() {
		ft = "directory"
	}

	modeStr := strconv.FormatInt(int64(mode.Perm()), 8)
	modeInt, _ := strconv.Atoi(modeStr)

	return model.FileInfo{
		Path:       path,
		Type:       ft,
		Size:       info.Size(),
		ModifiedAt: info.ModTime(),
		Permission: model.Permission{
			Mode: modeInt,
		},
	}
}
