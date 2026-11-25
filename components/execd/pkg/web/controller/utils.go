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

//go:build !windows
// +build !windows

package controller

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func DeleteFile(filePath string) error {
	absPath, err := pathutil.ExpandAbsPath(filePath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	if err := checkFilesystemPath(absPath); err != nil {
		return err
	}
	return deleteGuardedUnixFile(absPath, filePath)
}

func ChmodFile(file string, perms model.Permission) error {
	abs, err := pathutil.ExpandAbsPath(file)
	if err != nil {
		return err
	}
	if err := checkFilesystemPath(abs); err != nil {
		return err
	}

	hasMode := perms.Mode != 0
	var mode uint64
	if hasMode {
		mode, err = strconv.ParseUint(strconv.Itoa(perms.Mode), 8, 32)
		if err != nil {
			return err
		}
	}
	hasOwner := perms.Owner != "" || perms.Group != ""
	if !hasMode && !hasOwner {
		return nil
	}
	uid, gid, err := lookupOwnerGroupIDs(perms.Owner, perms.Group)
	if err != nil {
		return err
	}
	return chmodGuardedUnixFile(abs, os.FileMode(mode), hasMode, uid, gid, hasOwner)
}

func lookupOwnerGroupIDs(owner, group string) (int, int, error) {
	uid := -1
	if owner != "" {
		userInfo, err := user.Lookup(owner)
		if err != nil {
			return -1, -1, fmt.Errorf("failed to lookup user %s: %w", owner, err)
		}
		uid, err = strconv.Atoi(userInfo.Uid)
		if err != nil {
			return -1, -1, fmt.Errorf("failed to convert uid for user %s: %w", owner, err)
		}
	}

	gid := -1
	if group != "" {
		groupInfo, err := user.LookupGroup(group)
		if err != nil {
			return -1, -1, fmt.Errorf("failed to lookup group %s: %w", group, err)
		}
		gid, err = strconv.Atoi(groupInfo.Gid)
		if err != nil {
			return -1, -1, fmt.Errorf("failed to convert gid for group %s: %w", group, err)
		}
	}
	return uid, gid, nil
}

func SetFileOwnership(absPath string, owner string, group string) error {
	if owner == "" && group == "" {
		return nil
	}
	uid, gid, err := lookupOwnerGroupIDs(owner, group)
	if err != nil {
		return err
	}
	return chmodGuardedUnixFile(absPath, 0, false, uid, gid, true)
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
	if err := checkNotBlockedOrAncestorForRename(srcPath); err != nil {
		return err
	}
	if err := checkFilesystemPath(dstPath); err != nil {
		return err
	}

	return renameGuardedUnixFile(srcPath, dstPath, item.Src, item.Dest)
}

// MkdirAllWithOwnership creates targetDir and any missing parents, then applies
// owner/group only to the directories that were actually created (not pre-existing ones).
func MkdirAllWithOwnership(targetDir string, dirPerm os.FileMode, owner, group string) error {
	absDir, err := filepath.Abs(targetDir)
	if err != nil {
		return err
	}
	uid, gid, err := lookupOwnerGroupIDs(owner, group)
	if err != nil {
		return err
	}
	return mkdirAllGuardedUnix(absDir, dirPerm, uid, gid)
}

func MakeDir(dir string, perm model.Permission) error {
	abs, err := pathutil.ExpandAbsPath(dir)
	if err != nil {
		return err
	}
	if err := rejectStagingPath(abs); err != nil {
		return err
	}
	if err := checkFilesystemPath(abs); err != nil {
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
		return chmodGuardedUnixFile(abs, os.FileMode(mode), true, -1, -1, false)
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
	stat := fileInfo.Sys().(*syscall.Stat_t)

	owner := strconv.FormatUint(uint64(stat.Uid), 10)
	if ownerUser, err := user.LookupId(owner); err == nil {
		owner = ownerUser.Username
	}

	group := strconv.FormatUint(uint64(stat.Gid), 10)
	if groupInfo, err := user.LookupGroupId(group); err == nil {
		group = groupInfo.Name
	}

	mode := strconv.FormatInt(int64(fileInfo.Mode().Perm()), 8)

	return model.FileInfo{
		Path:       absPath,
		Type:       fileType(fileInfo),
		Size:       fileInfo.Size(),
		ModifiedAt: fileInfo.ModTime(),
		CreatedAt:  getFileCreateTime(absPath, fileInfo),
		Permission: model.Permission{
			Owner: owner,
			Group: group,
			Mode:  func() int { i, _ := strconv.Atoi(mode); return i }(),
		},
	}, nil
}

func GetFileInfo(filePath string) (model.FileInfo, error) {
	absPath, err := pathutil.ExpandAbsPath(filePath)
	if err != nil {
		return model.FileInfo{}, fmt.Errorf("invalid path %s: %w", filePath, err)
	}
	if err := checkFilesystemPath(absPath); err != nil {
		return model.FileInfo{}, err
	}

	fileInfo, err := lstatGuardedUnixFile(absPath)
	if err != nil {
		if errors.Is(err, errForbiddenPath) {
			return model.FileInfo{}, err
		}
		if errors.Is(err, fs.ErrNotExist) {
			return model.FileInfo{}, fmt.Errorf("file not found: %s: %w", filePath, err)
		}
		return model.FileInfo{}, fmt.Errorf("error accessing file %s: %w", filePath, err)
	}

	return buildFileInfo(absPath, fileInfo)
}
