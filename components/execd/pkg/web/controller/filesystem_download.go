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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// DownloadFile serves a file for download with support for range requests.
func (c *FilesystemController) DownloadFile() {
	rec := beginFilesystemMetric("download")
	defer rec.Finish(c.basicController)

	filePath := c.ctx.Query("path")
	if filePath == "" {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeMissingQuery,
			"missing query parameter 'path'",
		)
		return
	}
	resolvedFilePath, err := pathutil.ExpandPath(filePath)
	if err != nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error resolving file path: %s. %v", filePath, err),
		)
		return
	}

	file, err := os.Open(resolvedFilePath)
	if err != nil {
		c.handleFileError(err)
		return
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error getting file stat info: %s. %v", resolvedFilePath, err),
		)
		return
	}

	c.ctx.Header("Content-Type", "application/octet-stream")
	c.ctx.Header("Content-Disposition", formatContentDisposition(filepath.Base(resolvedFilePath)))
	c.ctx.Header("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

	if rangeHeader := c.ctx.GetHeader("Range"); rangeHeader != "" {
		ranges, err := ParseRange(rangeHeader, fileInfo.Size())
		if err != nil {
			c.RespondError(
				http.StatusRequestedRangeNotSatisfiable,
				model.ErrorCodeUnknown,
			)
			return
		}
		if len(ranges) > 0 {
			r := ranges[0]
			c.ctx.Status(http.StatusPartialContent)
			c.ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, fileInfo.Size()))
			c.ctx.Header("Content-Length", strconv.FormatInt(r.length, 10))

			_, _ = file.Seek(r.start, io.SeekStart)
			_, _ = io.CopyN(c.ctx.Writer, file, r.length)
			rec.MarkSuccess()
			return
		}
	}

	rec.MarkSuccess()
	http.ServeContent(c.ctx.Writer, c.ctx.Request, filepath.Base(resolvedFilePath), fileInfo.ModTime(), file)
}

// formatContentDisposition formats the Content-Disposition header value with proper
// encoding for non-ASCII filenames according to RFC 6266 and RFC 5987.
func formatContentDisposition(filename string) string {
	// Check if filename contains non-ASCII characters
	needsEncoding := false
	for _, r := range filename {
		if r > 127 {
			needsEncoding = true
			break
		}
	}

	if !needsEncoding {
		return "attachment; filename=\"" + filename + "\""
	}

	// Use RFC 5987 encoding for non-ASCII filenames
	// Format: attachment; filename="fallback"; filename*=UTF-8''encoded_name
	encodedFilename := url.PathEscape(filename)
	return "attachment; filename=\"" + encodedFilename + "\"; filename*=UTF-8''" + encodedFilename
}
