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
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/util/glob"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
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
	parentFD, base, err := openGuardedTargetParentFD(absPath)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)

	return chmodChownPinnedLeaf(parentFD, base, absPath, mode, hasMode, uid, gid, hasOwner)
}

// openGuardedTargetParentFD resolves absPath (including final-component
// symlinks) via dirfds and returns a pinned parent directory fd and the
// non-symlink final entry name.
func openGuardedTargetParentFD(absPath string) (int, string, error) {
	g := getFilesystemGuard()
	cleaned := filepath.Clean(absPath)
	if g.matchesPath(cleaned) {
		return -1, "", &forbiddenPathError{path: absPath}
	}
	curPath, remaining, ok := splitAbsolutePath(cleaned)
	if !ok || len(remaining) == 0 {
		return -1, "", &forbiddenPathError{path: absPath}
	}

	dirfd, err := openRootDirFD()
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: absPath, Err: err}
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
		res, doneBase, err := stepTargetParent(g, dirfd, part, curPath, absPath, remaining, &hops)
		if err != nil {
			return -1, "", err
		}
		if doneBase != "" {
			okReturn = true
			return dirfd, doneBase, nil
		}
		if res.nextFD != dirfd {
			_ = unix.Close(dirfd)
			dirfd = res.nextFD
		}
		curPath, remaining = res.nextCur, res.nextRem
	}
	return -1, "", &forbiddenPathError{path: absPath}
}

func stepTargetParent(
	g *filesystemGuard,
	dirfd int,
	part, curPath, absPath string,
	remaining []string,
	hops *int,
) (unixStepResult, string, error) {
	isFinal := len(remaining) == 0
	if part == ".." {
		parentFD, nextCur, err := stepUnixParentDir(g, dirfd, curPath, absPath, isFinal)
		if err != nil {
			return unixStepResult{}, "", err
		}
		return unixStepResult{nextFD: parentFD, nextCur: nextCur, nextRem: remaining}, "", nil
	}

	nextPath := filepath.Join(curPath, part)
	if g.isStepBlocked(nextPath, isFinal) {
		return unixStepResult{}, "", &forbiddenPathError{path: absPath}
	}

	if isFinal {
		if !isSymlinkAt(dirfd, part) {
			return unixStepResult{}, part, nil
		}
	} else {
		nextFD, sym, err := openGuardedDirComponent(dirfd, part, absPath)
		if err != nil {
			return unixStepResult{}, "", err
		}
		if !sym {
			return unixStepResult{nextFD: nextFD, nextCur: nextPath, nextRem: remaining}, "", nil
		}
	}

	*hops++
	newFD, nextCur, nextRem, err := stepUnixSymlink(g, dirfd, part, curPath, absPath, remaining, *hops)
	if err != nil {
		return unixStepResult{}, "", err
	}
	return unixStepResult{nextFD: newFD, nextCur: nextCur, nextRem: nextRem}, "", nil
}

func renameGuardedUnixFile(srcPath, dstPath, origSrc, origDest string) error {
	srcParentFD, srcBase, srcResolvedParent, err := openGuardedParentDirFD(srcPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("source path not found: %s: %w", origSrc, err)
		}
		return err
	}
	defer unix.Close(srcParentFD)

	resolvedSrc := filepath.Join(srcResolvedParent, srcBase)
	if resolvedSrc != filepath.Clean(srcPath) {
		if err := checkNotBlockedOrAncestorForRename(resolvedSrc); err != nil {
			return err
		}
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
	if err := renameNoReplaceAt(srcParentFD, srcBase, dstParentFD, dstBase); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("destination path already exists: %s", origDest)
		}
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
		if chErr := chmodChownPinnedLeaf(dirfd, part, origPath, 0, false, uid, gid, true); chErr != nil {
			_ = unix.Close(fd)
			return -1, false, chErr
		}
	}
	return fd, false, nil
}

func removeAllGuardedUnix(absPath string) error {
	parentFD, base, resolvedParent, err := openGuardedParentDirFD(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer unix.Close(parentFD)

	resolvedEntry := filepath.Join(resolvedParent, base)
	if resolvedEntry != filepath.Clean(absPath) {
		if err := checkNotBlockedOrAncestorForDelete(resolvedEntry); err != nil {
			return err
		}
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
	defer dirFile.Close()
	g := getFilesystemGuard()
	for {
		entries, readErr := dirFile.ReadDir(256)
		for _, entry := range entries {
			child := entry.Name()
			childPath := filepath.Join(origPath, child)
			if g.matchesPath(childPath) {
				return &forbiddenPathError{path: childPath}
			}
			if entry.IsDir() {
				if err := removeDirTreeAt(dirfd, child, childPath); err != nil {
					return err
				}
			} else if err := unix.Unlinkat(dirfd, child, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
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
		fd, openErr := unix.Openat(parentFD, base, dirTraversalFlags, 0)
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
	return newUnixStatFileInfo(base, &st), nil
}

type unixStatFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	sys     *syscall.Stat_t
}

func newUnixStatFileInfo(name string, st *unix.Stat_t) os.FileInfo {
	sys, modTime := statTFromUnix(st)
	return &unixStatFileInfo{
		name:    name,
		size:    st.Size,
		mode:    fileModeFromUnix(uint32(st.Mode)),
		modTime: modTime,
		sys:     sys,
	}
}

func (fi *unixStatFileInfo) Name() string       { return fi.name }
func (fi *unixStatFileInfo) Size() int64        { return fi.size }
func (fi *unixStatFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *unixStatFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *unixStatFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *unixStatFileInfo) Sys() any           { return fi.sys }

func fileModeFromUnix(m uint32) os.FileMode {
	mode := os.FileMode(m & 0o777)
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	case unix.S_IFIFO:
		mode |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= os.ModeSocket
	case unix.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFBLK:
		mode |= os.ModeDevice
	}
	if m&unix.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if m&unix.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if m&unix.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

// openReadableGuardedDirFile opens dirPath via the guarded O_NOFOLLOW walk with
// O_RDONLY|O_DIRECTORY on the final component so ReadDir works even on
// read-only (0400) directories without requiring +x on dirPath itself.
func openReadableGuardedDirFile(dirPath string) (*os.File, error) {
	return openGuardedUnixFile(dirPath, unix.O_RDONLY|unix.O_DIRECTORY, true)
}

func readSortedDirEntries(dirFile *os.File) ([]os.DirEntry, error) {
	dirEntries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(dirEntries, func(a, b os.DirEntry) int {
		return cmp.Compare(a.Name(), b.Name())
	})
	return dirEntries, nil
}

func openReadableChildDirFD(parentFD int, name, childPath string) (int, bool, error) {
	childFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || isSymlinkComponentErr(err) {
			return -1, false, nil
		}
		return -1, false, &os.PathError{Op: "open", Path: childPath, Err: err}
	}
	if err := verifyDirFD(childFD, childPath); err != nil {
		_ = unix.Close(childFD)
		return -1, false, nil //nolint:nilerr // skip subdirectories backed by blocked pseudo-filesystems
	}
	return childFD, true, nil
}

func listDirectoryEntriesPinned(root string, maxDepth int) ([]model.FileInfo, error) {
	entries := make([]model.FileInfo, 0, 16)
	if maxDepth == 0 {
		return entries, nil
	}
	rootFile, err := openReadableGuardedDirFile(root)
	if err != nil {
		return nil, err
	}
	return entries, walkListPinned(rootFile, root, 0, maxDepth, &entries)
}

func walkListPinned(dirFile *os.File, dirPath string, depth, maxDepth int, out *[]model.FileInfo) error {
	defer dirFile.Close()
	dirFD := int(dirFile.Fd())

	dirEntries, err := readSortedDirEntries(dirFile)
	if err != nil {
		return err
	}
	for _, entry := range dirEntries {
		name := entry.Name()
		if isAnyStagingName(name) {
			continue
		}
		entryPath := filepath.Join(dirPath, name)
		if isWalkEntryBlocked(entryPath, entry) {
			continue
		}
		info, err := statEntryAt(dirFD, name, entryPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		entryInfo, err := buildFileInfo(entryPath, info)
		if err != nil {
			return err
		}
		*out = append(*out, entryInfo)

		if info.IsDir() && depth+1 < maxDepth {
			childFD, ok, err := openReadableChildDirFD(dirFD, name, entryPath)
			if err != nil {
				return err
			}
			if ok {
				if err := walkListPinned(os.NewFile(uintptr(childFD), entryPath), entryPath, depth+1, maxDepth, out); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func searchFilesInDirPinned(root, pattern string) ([]model.FileInfo, error) {
	files := make([]model.FileInfo, 0, 16)
	rootInfo, err := lstatGuardedUnixFile(root)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return searchSingleFile(root, rootInfo, pattern)
	}
	rootFile, err := openReadableGuardedDirFile(root)
	if err != nil {
		return nil, err
	}
	return files, walkSearchPinned(rootFile, root, pattern, &files)
}

func searchSingleFile(path string, info os.FileInfo, pattern string) ([]model.FileInfo, error) {
	files := make([]model.FileInfo, 0, 1)
	if isAnyStagingName(info.Name()) {
		return files, nil
	}
	match, err := glob.PathMatch(pattern, info.Name())
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %s: %w", pattern, err)
	}
	if !match {
		return files, nil
	}
	fi, err := buildFileInfo(path, info)
	if err != nil {
		return nil, err
	}
	return append(files, fi), nil
}

func walkSearchPinned(dirFile *os.File, dirPath, pattern string, out *[]model.FileInfo) error {
	defer dirFile.Close()
	dirFD := int(dirFile.Fd())

	dirEntries, err := readSortedDirEntries(dirFile)
	if err != nil {
		return fmt.Errorf("error accessing path %s: %w", dirPath, err)
	}
	for _, entry := range dirEntries {
		name := entry.Name()
		entryPath := filepath.Join(dirPath, name)
		if isWalkEntryBlocked(entryPath, entry) {
			continue
		}
		if entry.IsDir() {
			childFD, ok, err := openReadableChildDirFD(dirFD, name, entryPath)
			if err != nil {
				return fmt.Errorf("error accessing path %s: %w", entryPath, err)
			}
			if ok {
				if err := walkSearchPinned(os.NewFile(uintptr(childFD), entryPath), entryPath, pattern, out); err != nil {
					return err
				}
			}
			continue
		}
		if err := matchAndAppendPinned(dirFD, name, entryPath, pattern, out); err != nil {
			return err
		}
	}
	return nil
}

func matchAndAppendPinned(dirFD int, name, entryPath, pattern string, out *[]model.FileInfo) error {
	if isAnyStagingName(name) {
		return nil
	}
	match, err := glob.PathMatch(pattern, name)
	if err != nil {
		return fmt.Errorf("invalid pattern %s: %w", pattern, err)
	}
	if !match {
		return nil
	}
	info, err := statEntryAt(dirFD, name, entryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("error accessing path %s: %w", entryPath, err)
	}
	if info.IsDir() {
		return nil
	}
	fileInfo, err := buildFileInfo(entryPath, info)
	if err != nil {
		return err
	}
	*out = append(*out, fileInfo)
	return nil
}

func statEntryAt(dirFD int, name, fullPath string) (os.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &os.PathError{Op: "lstat", Path: fullPath, Err: err}
	}
	return newUnixStatFileInfo(name, &st), nil
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
