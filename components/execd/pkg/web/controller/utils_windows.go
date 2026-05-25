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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func DeleteFile(filePath string) error {
	absPath, err := pathutil.ExpandAbsPath(filePath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	fileInfo, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if fileInfo.IsDir() {
		return fmt.Errorf("path is a directory: %s", filePath)
	}

	if err := os.Remove(absPath); err != nil {
		return fmt.Errorf("failed to remove file: %w", err)
	}

	return nil
}

func ChmodFile(file string, perms model.Permission) error {
	abs, err := pathutil.ExpandAbsPath(file)
	if err != nil {
		return err
	}

	if perms.Mode != 0 {
		mode, err := strconv.ParseUint(strconv.Itoa(perms.Mode), 8, 32)
		if err != nil {
			return err
		}
		err = os.Chmod(abs, os.FileMode(mode))
		if err != nil {
			return err
		}
	}
	return SetFileOwnership(abs, perms.Owner, perms.Group)
}

// SetFileOwnership is a placeholder on Windows where POSIX ownership is not supported.
func SetFileOwnership(_ string, _ string, _ string) error {
	// TODO: add Windows ACL support if needed.
	return nil
}

func RenameFile(item model.RenameFileItem) error {
	srcPath, err := pathutil.ExpandAbsPath(item.Src)
	if err != nil {
		return fmt.Errorf("invalid source path: %w", err)
	}

	dstPath, err := pathutil.ExpandAbsPath(item.Dest)
	if err != nil {
		return fmt.Errorf("invalid destination path: %w", err)
	}

	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("source path not found: %s", item.Src)
	}

	dstDir := filepath.Dir(dstPath)

	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("destination path already exists: %s", item.Dest)
	}

	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	return nil
}

func MakeDir(dir string, perm model.Permission) error {
	abs, err := pathutil.ExpandAbsPath(dir)
	if err != nil {
		return err
	}
	err = os.MkdirAll(abs, os.ModePerm)
	if err != nil {
		return err
	}

	return ChmodFile(abs, perm)
}

func GetFileInfo(filePath string) (model.FileInfo, error) {
	absPath, err := pathutil.ExpandAbsPath(filePath)
	if err != nil {
		return model.FileInfo{}, fmt.Errorf("invalid path %s: %w", filePath, err)
	}

	fileInfo, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return model.FileInfo{}, fmt.Errorf("file not found: %s", filePath)
		}
		return model.FileInfo{}, fmt.Errorf("error accessing file %s: %w", filePath, err)
	}

	createdAt := getFileCreateTime(fileInfo)
	if data, ok := fileInfo.Sys().(*syscall.Win32FileAttributeData); ok && data != nil {
		createdAt = time.Unix(0, data.CreationTime.Nanoseconds())
	}

	mode := strconv.FormatInt(int64(fileInfo.Mode().Perm()), 8)

	return model.FileInfo{
		Path:       absPath,
		Size:       fileInfo.Size(),
		ModifiedAt: fileInfo.ModTime(),
		CreatedAt:  createdAt,
		Permission: model.Permission{
			Owner: "",
			Group: "",
			Mode: func() int {
				i, _ := strconv.Atoi(mode)
				return i
			}(),
		},
	}, nil
}

func SearchFileMetadata(metadata map[string]model.FileMetadata, filePath string) (string, model.FileMetadata, bool) {
	base := filepath.Base(filePath)
	for path, info := range metadata {
		if filepath.Base(path) == base {
			return path, info, true
		}
	}

	return "", model.FileMetadata{}, false
}

type httpRange struct {
	start, length int64
}

func ParseRange(s string, size int64) ([]httpRange, error) {
	if !strings.HasPrefix(s, "bytes=") {
		return nil, errors.New("invalid range")
	}

	ranges := strings.Split(s[6:], ",")
	result := make([]httpRange, 0, len(ranges))

	for _, ra := range ranges {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}
		i := strings.Index(ra, "-")
		if i < 0 {
			return nil, errors.New("invalid range")
		}
		start, end := strings.TrimSpace(ra[:i]), strings.TrimSpace(ra[i+1:])
		var r httpRange

		if start == "" {
			// suffix-length
			n, err := strconv.ParseInt(end, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New("invalid range")
			}
			if n > size {
				n = size
			}
			r.start = size - n
			r.length = size - r.start
		} else {
			// start-end
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i < 0 {
				return nil, errors.New("invalid range")
			}
			if end == "" {
				// start-
				r.start = i
				r.length = size - i
			} else {
				// start-end
				j, err := strconv.ParseInt(end, 10, 64)
				if err != nil || j < i {
					return nil, errors.New("invalid range")
				}
				r.start = i
				r.length = j - i + 1
			}
		}
		if r.start >= size {
			continue
		}
		if r.start+r.length > size {
			r.length = size - r.start
		}
		result = append(result, r)
	}
	return result, nil
}
