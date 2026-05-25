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

//go:build windows
// +build windows

package controller

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/util/glob"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// FilesystemController handles file system operations.
type FilesystemController struct {
	*basicController
}

func NewFilesystemController(ctx *gin.Context) *FilesystemController {
	return &FilesystemController{basicController: newBasicController(ctx)}
}

func (c *FilesystemController) handleFileError(err error) {
	if os.IsNotExist(err) {
		c.RespondError(
			http.StatusNotFound,
			model.ErrorCodeFileNotFound,
			fmt.Sprintf("file not found. %v", err),
		)
	} else {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error accessing file: %v", err),
		)
	}
}

// GetFilesInfo retrieves metadata for specified file paths
func (c *FilesystemController) GetFilesInfo() {
	rec := beginFilesystemMetric("info")
	defer rec.Finish(c.basicController)

	paths := c.ctx.QueryArray("path")
	if len(paths) == 0 {
		rec.MarkSuccess()
		c.RespondSuccess(make(map[string]model.FileInfo))
		return
	}

	resp := make(map[string]model.FileInfo)
	for _, filePath := range paths {
		fileInfo, err := GetFileInfo(filePath)
		if err != nil {
			c.handleFileError(err)
			return
		}
		resp[filePath] = fileInfo
	}

	rec.MarkSuccess()
	c.RespondSuccess(resp)
}

// RemoveFiles deletes specified files
func (c *FilesystemController) RemoveFiles() {
	rec := beginFilesystemMetric("delete")
	defer rec.Finish(c.basicController)

	paths := c.ctx.QueryArray("path")
	for _, filePath := range paths {
		if err := DeleteFile(filePath); err != nil {
			c.RespondError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error removing file %s. %v", filePath, err),
			)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

// ChmodFiles changes file permissions for specified files
func (c *FilesystemController) ChmodFiles() {
	rec := beginFilesystemMetric("chmod")
	defer rec.Finish(c.basicController)

	var request map[string]model.Permission
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err),
		)
		return
	}

	for file, item := range request {
		err := ChmodFile(file, item)
		if err != nil {
			c.RespondError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error changing permissions for %s. %v", file, err),
			)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

// RenameFiles renames or moves files to new paths
func (c *FilesystemController) RenameFiles() {
	rec := beginFilesystemMetric("rename")
	defer rec.Finish(c.basicController)

	var request []model.RenameFileItem
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err),
		)
		return
	}

	for _, renameItem := range request {
		if err := RenameFile(renameItem); err != nil {
			c.handleFileError(err)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

// MakeDirs creates directories with specified permissions
func (c *FilesystemController) MakeDirs() {
	rec := beginFilesystemMetric("mkdir")
	defer rec.Finish(c.basicController)

	var request map[string]model.Permission
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err),
		)
		return
	}

	for dir, perm := range request {
		if err := MakeDir(dir, perm); err != nil {
			c.handleFileError(err)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

// RemoveDirs recursively removes directories
func (c *FilesystemController) RemoveDirs() {
	rec := beginFilesystemMetric("rmdir")
	defer rec.Finish(c.basicController)

	paths := c.ctx.QueryArray("path")
	for _, dir := range paths {
		resolvedDir, err := pathutil.ExpandPath(dir)
		if err != nil {
			c.RespondError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error resolving directory %s. %v", dir, err),
			)
			return
		}
		if err := os.RemoveAll(resolvedDir); err != nil {
			c.RespondError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error removing directory %s. %v", dir, err),
			)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

// SearchFiles searches for files matching a pattern in a directory
func (c *FilesystemController) SearchFiles() {
	rec := beginFilesystemMetric("search")
	defer rec.Finish(c.basicController)

	path := c.ctx.Query("path")
	if path == "" {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeMissingQuery,
			"missing query parameter 'path'",
		)
		return
	}

	path, err := pathutil.ExpandAbsPath(path)
	if err != nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error converting path %s to absolute. %v", path, err),
		)
		return
	}

	_, err = os.Stat(path)
	if err != nil {
		c.handleFileError(err)
		return
	}

	pattern := c.ctx.Query("pattern")
	if pattern == "" {
		pattern = "**"
	}

	files := make([]model.FileInfo, 0, 16)
	err = filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("error accessing path %s: %w", filePath, err)
		}
		if info.IsDir() {
			return nil
		}

		match, err := glob.PathMatch(pattern, info.Name())
		if err != nil {
			return fmt.Errorf("invalid pattern %s: %w", pattern, err)
		}

		if match {
			files = append(files, model.FileInfo{
				Path:       filePath,
				Size:       info.Size(),
				ModifiedAt: info.ModTime(),
				CreatedAt:  getFileCreateTime(info),
				Permission: model.Permission{
					Owner: "",
					Group: "",
					Mode: func() int {
						mode := strconv.FormatInt(int64(info.Mode().Perm()), 8)
						i, _ := strconv.Atoi(mode)
						return i
					}(),
				},
			})
		}

		return nil
	})

	if err != nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error searching files. %v", err),
		)
		return
	}

	rec.MarkSuccess()
	c.RespondSuccess(files)
}

// ReplaceContent replaces text content in specified files
func (c *FilesystemController) ReplaceContent() {
	rec := beginFilesystemMetric("replace")
	defer rec.Finish(c.basicController)

	var request map[string]model.ReplaceFileContentItem
	if err := c.bindJSON(&request); err != nil {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			fmt.Sprintf("error parsing request, MAYBE invalid body format. %v", err),
		)
		return
	}

	for file, item := range request {
		file, err := pathutil.ExpandAbsPath(file)
		if err != nil {
			c.handleFileError(err)
			return
		}

		if _, err = os.Stat(file); err != nil {
			c.handleFileError(err)
			return
		}

		content, err := os.ReadFile(file)
		if err != nil {
			c.handleFileError(err)
			return
		}

		fileInfo, err := os.Stat(file)
		if err != nil {
			c.handleFileError(err)
			return
		}
		mode := fileInfo.Mode()

		newContent := strings.ReplaceAll(string(content), item.Old, item.New)

		err = os.WriteFile(file, []byte(newContent), mode)
		if err != nil {
			c.handleFileError(err)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}
