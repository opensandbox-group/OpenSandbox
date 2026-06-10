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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func (c *IsolatedSessionController) getMergedView() (*isolation.MergedView, error) {
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
		resp[filePath] = fileInfoToModel(info)
	}
	c.RespondSuccess(resp)
}

func (c *IsolatedSessionController) SearchFiles() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	pattern := c.ctx.Query("pattern")
	if pattern == "" {
		pattern = "*"
	}

	results, err := mv.Search(pattern)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	c.RespondSuccess(results)
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

	data, err := mv.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.ctx.Writer.Header().Set("Content-Type", "application/octet-stream")
	c.ctx.Writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(filePath)))
	c.ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if _, writeErr := c.ctx.Writer.Write(data); writeErr != nil {
		log.Error("download write: %v", writeErr)
	}
}

func (c *IsolatedSessionController) UploadFile() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	filePath := c.ctx.Query("path")
	if filePath == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "path is required")
		return
	}

	file, _, err := c.ctx.Request.FormFile("file")
	if err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidFile, err.Error())
		return
	}
	defer file.Close()

	n, err := mv.WriteFileReader(filePath, file, 0o644)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.RespondSuccess(map[string]interface{}{
		"path":      filePath,
		"bytes":     n,
		"overwrite": true,
	})
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

	oldPath := c.ctx.Query("old_path")
	newPath := c.ctx.Query("new_path")
	if oldPath == "" || newPath == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "old_path and new_path are required")
		return
	}

	if err := mv.Rename(oldPath, newPath); err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
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
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	for file, item := range request {
		if chmodErr := mv.Chmod(file, os.FileMode(item.Mode)); chmodErr != nil {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, chmodErr.Error())
			return
		}
		_ = item
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) ReplaceContent() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	filePath := c.ctx.Query("path")
	oldStr := c.ctx.Query("old")
	newStr := c.ctx.Query("new")
	if filePath == "" || oldStr == "" {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeMissingQuery, "path and old are required")
		return
	}

	if err := mv.ReplaceContent(filePath, oldStr, newStr); err != nil {
		if os.IsNotExist(err) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeFileNotFound, err.Error())
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	c.RespondSuccess(nil)
}

func (c *IsolatedSessionController) MakeDirs() {
	mv, _ := c.getMergedView()
	if mv == nil {
		return
	}

	paths := c.ctx.QueryArray("path")
	for _, p := range paths {
		if err := mv.MkdirAll(p, 0o755); err != nil {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
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

func fileInfoToModel(info os.FileInfo) model.FileInfo {
	return model.FileInfo{
		Path:       info.Name(),
		Size:       info.Size(),
		ModifiedAt: info.ModTime(),
		Permission: model.Permission{
			Mode: int(info.Mode()),
		},
	}
}
