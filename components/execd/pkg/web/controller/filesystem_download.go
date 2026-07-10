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
	"bufio"
	"bytes"
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

// DownloadFile serves a file for download with support for range requests
// and line-based reading via offset/limit query parameters.
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

	rawOffset := c.ctx.Query("offset")
	rawLimit := c.ctx.Query("limit")
	hasLineParams := rawOffset != "" || rawLimit != ""
	rangeHeader := c.ctx.GetHeader("Range")

	if hasLineParams && rangeHeader != "" {
		c.RespondError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidRequest,
			"line-based reading (offset/limit) and byte range (Range header) are mutually exclusive",
		)
		return
	}

	file, err := os.Open(resolvedFilePath)
	if err != nil {
		c.handleFileError(err)
		return
	}
	defer file.Close()

	if hasLineParams {
		c.serveLineRange(file, rawOffset, rawLimit)
		rec.MarkSuccess()
		return
	}

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

	if rangeHeader != "" {
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

// serveLineRange reads lines from file starting at a 1-based offset and
// returns up to limit lines as text/plain.
func (c *FilesystemController) serveLineRange(file *os.File, rawOffset, rawLimit string) {
	offset := int64(1)
	if rawOffset != "" {
		parsed, err := strconv.ParseInt(rawOffset, 10, 64)
		if err != nil || parsed < 1 {
			c.RespondError(
				http.StatusBadRequest,
				model.ErrorCodeInvalidRequest,
				fmt.Sprintf("invalid query parameter 'offset': %s", rawOffset),
			)
			return
		}
		offset = parsed
	}

	limit := int64(-1)
	if rawLimit != "" {
		parsed, err := strconv.ParseInt(rawLimit, 10, 64)
		if err != nil || parsed < 1 {
			c.RespondError(
				http.StatusBadRequest,
				model.ErrorCodeInvalidRequest,
				fmt.Sprintf("invalid query parameter 'limit': %s", rawLimit),
			)
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
