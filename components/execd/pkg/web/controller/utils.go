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
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

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

func SetFileOwnership(absPath string, owner string, group string) error {
	if owner == "" && group == "" {
		return nil
	}

	uid := -1
	if owner != "" {
		userInfo, err := user.Lookup(owner)
		if err != nil {
			return fmt.Errorf("failed to lookup user %s: %w", owner, err)
		}
		uid, err = strconv.Atoi(userInfo.Uid)
		if err != nil {
			return fmt.Errorf("failed to convert uid for user %s: %w", owner, err)
		}
	}

	gid := -1
	if group != "" {
		groupInfo, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("failed to lookup group %s: %w", group, err)
		}
		gid, err = strconv.Atoi(groupInfo.Gid)
		if err != nil {
			return fmt.Errorf("failed to convert gid for group %s: %w", group, err)
		}
	}

	if err := os.Chown(absPath, uid, gid); err != nil {
		return fmt.Errorf("failed to set owner/group for %s: %w", absPath, err)
	}

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
	// Walk up to find the first directory that needs to be created.
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

	// Apply ownership to every newly created directory from firstNew down to targetDir.
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
		CreatedAt:  getFileCreateTime(fileInfo),
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

	fileInfo, err := os.Lstat(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return model.FileInfo{}, fmt.Errorf("file not found: %s: %w", filePath, err)
		}
		return model.FileInfo{}, fmt.Errorf("error accessing file %s: %w", filePath, err)
	}

	return buildFileInfo(absPath, fileInfo)
}

// uploadOwnerOf reports the uid/gid an atomic upload must preserve when it
// replaces an existing destination. -1/-1 leaves the staged file as-is.
func uploadOwnerOf(fi os.FileInfo) (int, int) {
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid)
	}
	return -1, -1
}

// uploadLinkCount reports how many directory entries share the destination's
// inode, so an upload can keep the aliases of a hard-linked file in sync
// instead of repointing just the requested name. 0 means "unknown".
func uploadLinkCount(fi os.FileInfo) uint64 {
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 0
}

// copyUploadAttrs copies the destination's extended attributes - the POSIX
// access ACL lives in one - onto the staging file, so publishing a new inode
// does not silently drop access control the old in-place write kept. It
// reports whether attributes survived the copy: false means the caller must
// publish in place, which preserves them by construction.
func copyUploadAttrs(dest, stagedName string) bool {
	names, err := listUploadXattrs(dest)
	if err != nil {
		return false
	}
	for _, name := range names {
		value, err := getUploadXattr(dest, name)
		if err != nil {
			return false
		}
		if err := unix.Setxattr(stagedName, name, value, 0); err != nil {
			// security.* and trusted.* need a privilege the upload does not
			// have; leave the inode in place so the attribute survives.
			return false
		}
	}
	return true
}

func listUploadXattrs(path string) ([]string, error) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	size, err = unix.Listxattr(path, buf)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(string(buf[:size]), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func getUploadXattr(path, name string) ([]byte, error) {
	size, err := unix.Getxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	size, err = unix.Getxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:size], nil
}
