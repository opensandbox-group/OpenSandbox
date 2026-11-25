// Copyright 2026 The OpenSandbox Authors
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

//go:build unix

package controller

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openGuardedDirFD walks dirPath component-by-component with O_NOFOLLOW and
// verifyDirFD, returning a pinned directory fd and its resolved canonical path.
func openGuardedDirFD(dirPath, origPath string) (int, string, error) {
	g := getFilesystemGuard()
	cleaned := filepath.Clean(dirPath)
	if g.matchesPath(cleaned) {
		return -1, "", &forbiddenPathError{path: origPath}
	}
	curPath, remaining, ok := splitAbsolutePath(cleaned)
	if !ok {
		return -1, "", &os.PathError{Op: "open", Path: origPath, Err: unix.EINVAL}
	}

	dirfd, err := openRootDirFD()
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: origPath, Err: err}
	}
	okReturn := false
	defer func() {
		if !okReturn && dirfd >= 0 {
			_ = unix.Close(dirfd)
		}
	}()

	hops := 0
	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			parentFD, nextCur, err := stepUnixParentDir(g, dirfd, curPath, origPath, len(remaining) == 0)
			if err != nil {
				return -1, "", err
			}
			_ = unix.Close(dirfd)
			dirfd, curPath = parentFD, nextCur
			continue
		}

		nextPath := filepath.Join(curPath, part)
		if g.isStepBlocked(nextPath, len(remaining) == 0) {
			return -1, "", &forbiddenPathError{path: origPath}
		}

		nextFD, sym, err := openGuardedDirComponent(dirfd, part, origPath)
		if err != nil {
			return -1, "", err
		}
		if !sym {
			_ = unix.Close(dirfd)
			dirfd, curPath = nextFD, nextPath
			continue
		}

		hops++
		newFD, nextCur, nextRem, err := stepUnixSymlink(g, dirfd, part, curPath, origPath, remaining, hops)
		if err != nil {
			return -1, "", err
		}
		if newFD != dirfd {
			_ = unix.Close(dirfd)
			dirfd = newFD
		}
		curPath, remaining = nextCur, nextRem
	}

	if err := verifyDirFD(dirfd, origPath); err != nil {
		return -1, "", err
	}
	okReturn = true
	return dirfd, curPath, nil
}

// openGuardedParentDirFD opens the verified parent directory of absPath and
// validates that the final entry name under that parent is not blocked.
func openGuardedParentDirFD(absPath string) (int, string, string, error) {
	cleaned := filepath.Clean(absPath)
	g := getFilesystemGuard()
	if g.matchesPath(cleaned) {
		return -1, "", "", &forbiddenPathError{path: absPath}
	}
	parent := filepath.Dir(cleaned)
	base := filepath.Base(cleaned)
	if parent == cleaned {
		return -1, "", "", &forbiddenPathError{path: absPath}
	}
	parentFD, resolvedParent, err := openGuardedDirFD(parent, absPath)
	if err != nil {
		return -1, "", "", err
	}
	if g.matchesPath(filepath.Join(resolvedParent, base)) {
		_ = unix.Close(parentFD)
		return -1, "", "", &forbiddenPathError{path: absPath}
	}
	return parentFD, base, resolvedParent, nil
}

func deleteGuardedUnixFile(absPath, origPath string) error {
	parentFD, base, resolvedParent, err := openGuardedParentDirFD(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer unix.Close(parentFD)

	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "lstat", Path: absPath, Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return fmt.Errorf("path is a directory: %s", origPath)
	case unix.S_IFLNK:
		if err := checkFilesystemPath(filepath.Join(resolvedParent, base)); err != nil {
			return err
		}
		var targetSt unix.Stat_t
		if err := unix.Fstatat(parentFD, base, &targetSt, 0); err == nil && targetSt.Mode&unix.S_IFMT == unix.S_IFDIR {
			return fmt.Errorf("path is a directory: %s", origPath)
		}
	}
	if err := unix.Unlinkat(parentFD, base, 0); err != nil {
		return fmt.Errorf("failed to remove file: %w", err)
	}
	return nil
}

func chmodGuardedUnixFile(absPath string, mode os.FileMode, hasMode bool, uid, gid int, hasOwner bool) error {
	f, err := openGuardedUnixFile(absPath, unix.O_RDONLY, true)
	if err != nil {
		return err
	}
	defer f.Close()

	fd := int(f.Fd())
	if hasMode {
		if err := unix.Fchmod(fd, uint32(mode)); err != nil {
			return &os.PathError{Op: "chmod", Path: absPath, Err: err}
		}
	}
	if hasOwner {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return fmt.Errorf("failed to set owner/group for %s: %w", absPath, err)
		}
	}
	return nil
}

func renameGuardedUnixFile(srcPath, dstPath, origSrc, origDest string) error {
	if err := checkNotBlockedOrAncestor(srcPath); err != nil {
		return err
	}
	srcParentFD, srcBase, srcResolvedParent, err := openGuardedParentDirFD(srcPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("source path not found: %s: %w", origSrc, err)
		}
		return err
	}
	defer unix.Close(srcParentFD)

	resolvedSrc := filepath.Join(srcResolvedParent, srcBase)
	if err := checkNotBlockedOrAncestor(resolvedSrc); err != nil {
		return err
	}

	var srcSt unix.Stat_t
	if err := unix.Fstatat(srcParentFD, srcBase, &srcSt, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("source path not found: %s: %w", origSrc, fs.ErrNotExist)
		}
		return &os.PathError{Op: "lstat", Path: srcPath, Err: err}
	}
	if srcSt.Mode&unix.S_IFMT == unix.S_IFLNK {
		if err := checkFilesystemPath(resolvedSrc); err != nil {
			return err
		}
	}

	if err := mkdirAllGuardedUnix(filepath.Dir(dstPath), 0o755, -1, -1); err != nil {
		if errors.Is(err, errForbiddenPath) {
			return err
		}
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	dstParentFD, dstBase, _, err := openGuardedParentDirFD(dstPath)
	if err != nil {
		return err
	}
	defer unix.Close(dstParentFD)

	var dstSt unix.Stat_t
	if err := unix.Fstatat(dstParentFD, dstBase, &dstSt, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return fmt.Errorf("destination path already exists: %s", origDest)
	}
	if err := unix.Renameat(srcParentFD, srcBase, dstParentFD, dstBase); err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}
	return nil
}

// mkdirAllGuardedUnix walks each component from / using dirfds with O_NOFOLLOW,
// creating missing directories with mkdirat and applying uid/gid only to newly
// created directories.
func mkdirAllGuardedUnix(absPath string, perm os.FileMode, uid, gid int) error {
	g := getFilesystemGuard()
	cleaned := filepath.Clean(absPath)
	if g.matchesPath(cleaned) {
		return &forbiddenPathError{path: absPath}
	}
	curPath, remaining, ok := splitAbsolutePath(cleaned)
	if !ok {
		return &os.PathError{Op: "mkdir", Path: absPath, Err: unix.EINVAL}
	}

	dirfd, err := openRootDirFD()
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: absPath, Err: err}
	}
	defer func() {
		if dirfd >= 0 {
			_ = unix.Close(dirfd)
		}
	}()

	hops := 0
	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			parentFD, nextCur, err := stepUnixParentDir(g, dirfd, curPath, absPath, len(remaining) == 0)
			if err != nil {
				return err
			}
			_ = unix.Close(dirfd)
			dirfd, curPath = parentFD, nextCur
			continue
		}

		nextPath := filepath.Join(curPath, part)
		if g.isStepBlocked(nextPath, len(remaining) == 0) {
			return &forbiddenPathError{path: absPath}
		}

		nextFD, sym, err := openOrCreateGuardedDir(dirfd, part, absPath, perm, uid, gid)
		if err != nil {
			return err
		}
		if !sym {
			_ = unix.Close(dirfd)
			dirfd, curPath = nextFD, nextPath
			continue
		}

		hops++
		newFD, nextCur, nextRem, err := stepUnixSymlink(g, dirfd, part, curPath, absPath, remaining, hops)
		if err != nil {
			return err
		}
		if newFD != dirfd {
			_ = unix.Close(dirfd)
			dirfd = newFD
		}
		curPath, remaining = nextCur, nextRem
	}
	return nil
}

func openOrCreateGuardedDir(dirfd int, part, origPath string, perm os.FileMode, uid, gid int) (int, bool, error) {
	fd, sym, err := openGuardedDirComponent(dirfd, part, origPath)
	if err == nil || sym || !errors.Is(err, fs.ErrNotExist) {
		return fd, sym, err
	}
	created := true
	if mkErr := unix.Mkdirat(dirfd, part, uint32(perm.Perm())); mkErr != nil {
		if !errors.Is(mkErr, unix.EEXIST) {
			return -1, false, &os.PathError{Op: "mkdir", Path: origPath, Err: mkErr}
		}
		created = false
	}
	fd, sym, err = openGuardedDirComponent(dirfd, part, origPath)
	if err != nil || sym {
		return fd, sym, err
	}
	if created && (uid != -1 || gid != -1) {
		if chErr := unix.Fchown(fd, uid, gid); chErr != nil {
			_ = unix.Close(fd)
			return -1, false, fmt.Errorf("failed to set owner/group for %s: %w", origPath, chErr)
		}
	}
	return fd, false, nil
}

func removeAllGuardedUnix(absPath string) error {
	if err := checkNotBlockedOrAncestor(absPath); err != nil {
		return err
	}
	parentFD, base, resolvedParent, err := openGuardedParentDirFD(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer unix.Close(parentFD)

	resolvedEntry := filepath.Join(resolvedParent, base)
	if err := checkNotBlockedOrAncestor(resolvedEntry); err != nil {
		return err
	}

	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "lstat", Path: absPath, Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if err := checkFilesystemPath(resolvedEntry); err != nil {
			return err
		}
		return unix.Unlinkat(parentFD, base, 0)
	case unix.S_IFDIR:
		return removeDirTreeAt(parentFD, base, absPath)
	default:
		return unix.Unlinkat(parentFD, base, 0)
	}
}

func removeDirTreeAt(parentFD int, name, origPath string) error {
	dirfd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "open", Path: origPath, Err: err}
	}
	if err := verifyDirFD(dirfd, origPath); err != nil {
		_ = unix.Close(dirfd)
		return err
	}
	dirFile := os.NewFile(uintptr(dirfd), origPath)
	entries, readErr := dirFile.ReadDir(-1)
	if readErr != nil {
		_ = dirFile.Close()
		return readErr
	}
	for _, entry := range entries {
		child := entry.Name()
		if entry.IsDir() {
			if err := removeDirTreeAt(dirfd, child, filepath.Join(origPath, child)); err != nil {
				_ = dirFile.Close()
				return err
			}
		} else if err := unix.Unlinkat(dirfd, child, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			_ = dirFile.Close()
			return err
		}
	}
	_ = dirFile.Close()
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func lstatGuardedUnixFile(absPath string) (os.FileInfo, error) {
	cleaned := filepath.Clean(absPath)
	if cleaned == "/" {
		if err := checkRootDirFS(cleaned); err != nil {
			return nil, err
		}
		return os.Lstat(cleaned)
	}
	parentFD, base, resolvedParent, err := openGuardedParentDirFD(cleaned)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)

	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &os.PathError{Op: "lstat", Path: absPath, Err: err}
	}
	resolvedEntry := filepath.Join(resolvedParent, base)
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		fd, openErr := unix.Openat(parentFD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr == nil {
			verifyErr := verifyDirFD(fd, absPath)
			_ = unix.Close(fd)
			if verifyErr != nil {
				return nil, verifyErr
			}
		}
	case unix.S_IFLNK:
		if err := checkFilesystemPath(resolvedEntry); err != nil {
			return nil, err
		}
	}
	return os.Lstat(resolvedEntry)
}

func renameStagedUpload(tmpName, absTarget string) error {
	tmpParentFD, tmpBase, _, err := openGuardedParentDirFD(tmpName)
	if err != nil {
		return err
	}
	defer unix.Close(tmpParentFD)

	dstParentFD, dstBase, _, err := openGuardedParentDirFD(absTarget)
	if err != nil {
		return err
	}
	defer unix.Close(dstParentFD)

	return unix.Renameat(tmpParentFD, tmpBase, dstParentFD, dstBase)
}
