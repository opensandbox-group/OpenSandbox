// Copyright 2025 The OpenSandbox Authors
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
	"io/fs"
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
		if errors.Is(err, fs.ErrNotExist) {
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

// lookupOwnership is a placeholder on Windows, where owner and group are ignored.
func lookupOwnership(_ string, _ string) (int, int, error) {
	return -1, -1, nil
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
		return fmt.Errorf("source path not found: %s: %w", item.Src, err)
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

// MkdirAllWithOwnership creates targetDir and any missing parents, then applies
// owner/group only to the directories that were actually created (not pre-existing ones).
func MkdirAllWithOwnership(targetDir string, dirPerm os.FileMode, owner, group string) error {
	firstNew := ""
	cur := targetDir
	for {
		if _, err := os.Stat(cur); err == nil {
			break
		}
		firstNew = cur
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	if err := os.MkdirAll(targetDir, dirPerm); err != nil {
		return err
	}

	if firstNew == "" || (owner == "" && group == "") {
		return nil
	}

	rel, err := filepath.Rel(firstNew, targetDir)
	if err != nil {
		return err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur = firstNew
	if err := SetFileOwnership(cur, owner, group); err != nil {
		return err
	}
	for _, p := range parts {
		if p == "." {
			continue
		}
		cur = filepath.Join(cur, p)
		if err := SetFileOwnership(cur, owner, group); err != nil {
			return err
		}
	}
	return nil
}

func MakeDir(dir string, perm model.Permission) error {
	abs, err := pathutil.ExpandAbsPath(dir)
	if err != nil {
		return err
	}

	_, statErr := os.Stat(abs)
	existed := statErr == nil

	if err := MkdirAllWithOwnership(abs, os.ModePerm, perm.Owner, perm.Group); err != nil {
		return err
	}

	if !existed && perm.Mode != 0 {
		mode, err := strconv.ParseUint(strconv.Itoa(perm.Mode), 8, 32)
		if err != nil {
			return err
		}
		return os.Chmod(abs, os.FileMode(mode))
	}
	return nil
}

func fileType(fileInfo os.FileInfo) string {
	mode := fileInfo.Mode()
	if mode&os.ModeSymlink != 0 {
		return "symlink"
	}
	if fileInfo.IsDir() {
		return "directory"
	}
	if mode.IsRegular() {
		return "file"
	}
	return "other"
}

func buildFileInfo(absPath string, fileInfo os.FileInfo) (model.FileInfo, error) {
	createdAt := getFileCreateTime(fileInfo)
	if data, ok := fileInfo.Sys().(*syscall.Win32FileAttributeData); ok && data != nil {
		createdAt = time.Unix(0, data.CreationTime.Nanoseconds())
	}

	mode := strconv.FormatInt(int64(fileInfo.Mode().Perm()), 8)

	return model.FileInfo{
		Path:       absPath,
		Type:       fileType(fileInfo),
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

func GetFileInfo(filePath string) (model.FileInfo, error) {
	absPath, err := pathutil.ExpandAbsPath(filePath)
	if err != nil {
		return model.FileInfo{}, fmt.Errorf("invalid path %s: %w", filePath, err)
	}

	fileInfo, err := os.Lstat(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return model.FileInfo{}, fmt.Errorf("file not found: %s: %w", filePath, err)
		}
		return model.FileInfo{}, fmt.Errorf("error accessing file %s: %w", filePath, err)
	}

	return buildFileInfo(absPath, fileInfo)
}
