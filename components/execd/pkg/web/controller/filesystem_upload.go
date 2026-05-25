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
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type uploadError struct {
	status  int
	code    model.ErrorCode
	message string
}

func newUploadError(status int, code model.ErrorCode, message string) *uploadError {
	return &uploadError{status: status, code: code, message: message}
}

// UploadFile uploads files with metadata to specified paths
func (c *FilesystemController) UploadFile() {
	rec := beginFilesystemMetric("upload")
	defer rec.Finish(c.basicController)

	metadataParts, fileParts, uerr := c.parseUploadForm()
	if uerr != nil {
		c.RespondError(uerr.status, uerr.code, uerr.message)
		return
	}

	for i := range metadataParts {
		if uerr := c.processUploadPair(metadataParts[i], fileParts[i]); uerr != nil {
			c.RespondError(uerr.status, uerr.code, uerr.message)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

func (c *FilesystemController) parseUploadForm() ([]*multipart.FileHeader, []*multipart.FileHeader, *uploadError) {
	form, err := c.ctx.MultipartForm()
	if err != nil || form == nil {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFile, "multipart form is empty")
	}

	metadataParts := form.File["metadata"]
	fileParts := form.File["file"]

	if len(metadataParts) == 0 {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileMetadata, "metadata file is missing")
	}
	if len(fileParts) == 0 {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileContent, "file is missing")
	}
	if len(metadataParts) != len(fileParts) {
		return nil, nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFile,
			fmt.Sprintf("metadata and file count mismatch: %d vs %d", len(metadataParts), len(fileParts)),
		)
	}
	return metadataParts, fileParts, nil
}

func (c *FilesystemController) processUploadPair(metadataHeader, fileHeader *multipart.FileHeader) *uploadError {
	meta, uerr := parseUploadMetadata(metadataHeader)
	if uerr != nil {
		return uerr
	}

	resolvedPath, uerr := resolveUploadTarget(meta.Path)
	if uerr != nil {
		return uerr
	}

	if uerr := writeUploadFile(resolvedPath, fileHeader); uerr != nil {
		return uerr
	}

	return applyUploadPermission(resolvedPath, meta.Permission)
}

func parseUploadMetadata(header *multipart.FileHeader) (*model.FileMetadata, *uploadError) {
	metadataFile, err := header.Open()
	if err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("error opening metadata file. %v", err),
		)
	}
	metaBytes, err := io.ReadAll(metadataFile)
	metadataFile.Close()
	if err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("error reading metadata content. %v", err),
		)
	}

	var meta model.FileMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("invalid metadata format. %v", err),
		)
	}
	if meta.Path == "" {
		return nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileMetadata, "metadata path is empty")
	}
	return &meta, nil
}

func resolveUploadTarget(targetPath string) (string, *uploadError) {
	resolvedPath, err := pathutil.ExpandPath(targetPath)
	if err != nil {
		return "", newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error resolving target path %s. %v", targetPath, err),
		)
	}
	targetDir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(targetDir, os.ModePerm); err != nil {
		return "", newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error creating target directory %s. %v", targetDir, err),
		)
	}
	return resolvedPath, nil
}

func writeUploadFile(resolvedPath string, fileHeader *multipart.FileHeader) *uploadError {
	file, err := fileHeader.Open()
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error opening file %s. %v", fileHeader.Filename, err),
		)
	}
	defer file.Close()

	dst, err := os.OpenFile(resolvedPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error opening destination file %s. %v", resolvedPath, err),
		)
	}

	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error copying file %s. %v", resolvedPath, err),
		)
	}

	if err := dst.Sync(); err != nil {
		log.Error("failed to sync target file: %v", err)
	}
	if err := dst.Close(); err != nil {
		log.Error("failed to close target file: %v", err)
	}

	// fsync parent directory so the new dirent is durable and visible on
	// weakly-coherent filesystems (virtio-fs, 9pfs, etc.). Best-effort:
	// some filesystems return ENOTSUP for directory fsync.
	targetDir := filepath.Dir(resolvedPath)
	if d, err := os.Open(targetDir); err == nil {
		if err := d.Sync(); err != nil {
			log.Warning("failed to sync parent dir %s: %v", targetDir, err)
		}
		_ = d.Close()
	}
	return nil
}

// applyUploadPermission applies the metadata permission with one retry to
// absorb metadata-propagation delay on weakly-coherent filesystems
// (virtio-fs, 9pfs). ChmodFile always invokes chown under the hood, so a
// freshly-created dirent that has not yet propagated will surface as ENOENT
// here even though the file is fully written and synced.
func applyUploadPermission(resolvedPath string, permission model.Permission) *uploadError {
	chmodErr := ChmodFile(resolvedPath, permission)
	if chmodErr != nil {
		time.Sleep(20 * time.Millisecond)
		chmodErr = ChmodFile(resolvedPath, permission)
	}
	if chmodErr != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error chmoding file %s. %v", resolvedPath, chmodErr),
		)
	}
	return nil
}
