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
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openGuardedReadFile(absPath string) (*os.File, error) {
	return openGuardedUnixFile(absPath, unix.O_RDONLY, true)
}

func openGuardedWriteFile(absPath string, flags int) (*os.File, error) {
	return openGuardedUnixFile(absPath, flags, false)
}

// openGuardedUnixFile resolves absPath component-by-component using dirfds and
// O_NOFOLLOW so a concurrent symlink swap between check and open cannot
// redirect the open onto a blocked regular file or pseudo-filesystem.
func openGuardedUnixFile(absPath string, flags int, allowDir bool) (*os.File, error) {
	g := getFilesystemGuard()
	cleaned := filepath.Clean(absPath)
	if g.matchesPath(cleaned) {
		return nil, &forbiddenPathError{path: absPath}
	}

	curPath, remaining, ok := splitAbsolutePath(cleaned)
	if !ok {
		return nil, &os.PathError{Op: "open", Path: absPath, Err: unix.EINVAL}
	}

	dirfd, err := openRootDirFD()
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: absPath, Err: err}
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

		res, err := stepGuardedUnixPath(g, dirfd, part, curPath, absPath, remaining, flags, allowDir, &hops)
		if err != nil {
			return nil, err
		}
		if res.leaf != nil {
			return res.leaf, nil
		}
		if res.nextFD != dirfd {
			_ = unix.Close(dirfd)
			dirfd = res.nextFD
		}
		curPath, remaining = res.nextCur, res.nextRem
	}

	if err := verifyOpenedFD(dirfd, absPath, allowDir); err != nil {
		return nil, err
	}
	fd := dirfd
	dirfd = -1
	return os.NewFile(uintptr(fd), absPath), nil
}

type unixStepResult struct {
	leaf    *os.File
	nextFD  int
	nextCur string
	nextRem []string
}

func stepGuardedUnixPath(
	g *filesystemGuard,
	dirfd int,
	part, curPath, absPath string,
	remaining []string,
	flags int,
	allowDir bool,
	hops *int,
) (unixStepResult, error) {
	isFinal := len(remaining) == 0
	if part == ".." {
		parentFD, nextCur, err := stepUnixParentDir(g, dirfd, curPath, absPath, isFinal)
		if err != nil {
			return unixStepResult{}, err
		}
		return unixStepResult{nextFD: parentFD, nextCur: nextCur, nextRem: remaining}, nil
	}

	nextPath := filepath.Join(curPath, part)
	if g.isStepBlocked(nextPath, isFinal) {
		return unixStepResult{}, &forbiddenPathError{path: absPath}
	}

	if isFinal {
		fd, sym, err := openGuardedLeaf(dirfd, part, absPath, flags, allowDir)
		if err != nil {
			return unixStepResult{}, err
		}
		if !sym {
			return unixStepResult{leaf: finishGuardedFile(fd, absPath, flags)}, nil
		}
	} else {
		nextFD, sym, err := openGuardedDirComponent(dirfd, part, absPath)
		if err != nil {
			return unixStepResult{}, err
		}
		if !sym {
			return unixStepResult{nextFD: nextFD, nextCur: nextPath, nextRem: remaining}, nil
		}
	}

	*hops++
	newFD, nextCur, nextRem, err := stepUnixSymlink(g, dirfd, part, curPath, absPath, remaining, *hops)
	if err != nil {
		return unixStepResult{}, err
	}
	return unixStepResult{nextFD: newFD, nextCur: nextCur, nextRem: nextRem}, nil
}

func stepUnixParentDir(g *filesystemGuard, dirfd int, curPath, absPath string, isFinal bool) (int, string, error) {
	parentFD, err := unix.Openat(dirfd, "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: absPath, Err: err}
	}
	if err := verifyDirFD(parentFD, absPath); err != nil {
		_ = unix.Close(parentFD)
		return -1, "", err
	}
	nextCur := filepath.Dir(curPath)
	if g.isStepBlocked(nextCur, isFinal) {
		_ = unix.Close(parentFD)
		return -1, "", &forbiddenPathError{path: absPath}
	}
	return parentFD, nextCur, nil
}

func stepUnixSymlink(
	g *filesystemGuard,
	dirfd int,
	part, curPath, absPath string,
	remaining []string,
	hops int,
) (int, string, []string, error) {
	if hops > maxGuardSymlinkDepth {
		return -1, "", nil, &forbiddenPathError{path: absPath}
	}
	target, err := readlinkAt(dirfd, part)
	if err != nil {
		return -1, "", nil, &os.PathError{Op: "readlink", Path: absPath, Err: err}
	}
	if !strings.Contains(target, "..") {
		resolvedTarget := resolveSymlinkTarget(curPath, target)
		if g.isStepBlocked(resolvedTarget, len(remaining) == 0) ||
			g.matchesPath(joinRemaining(resolvedTarget, remaining)) {
			return -1, "", nil, &forbiddenPathError{path: absPath}
		}
	}
	nextCur, targetSegs, ok := expandRawSymlinkTarget(curPath, target)
	if !ok {
		return -1, "", nil, &forbiddenPathError{path: absPath}
	}
	if filepath.IsAbs(target) {
		newRoot, err := openRootDirFD()
		if err != nil {
			return -1, "", nil, &os.PathError{Op: "open", Path: absPath, Err: err}
		}
		return newRoot, nextCur, append(targetSegs, remaining...), nil
	}
	return dirfd, nextCur, append(targetSegs, remaining...), nil
}

func openRootDirFD() (int, error) {
	return unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

func openGuardedDirComponent(dirfd int, part, absPath string) (int, bool, error) {
	fd, err := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if isSymlinkComponentErr(err) && isSymlinkAt(dirfd, part) {
			return -1, true, nil
		}
		return -1, false, &os.PathError{Op: "open", Path: absPath, Err: err}
	}
	if err := verifyDirFD(fd, absPath); err != nil {
		_ = unix.Close(fd)
		return -1, false, err
	}
	return fd, false, nil
}

func openGuardedLeaf(dirfd int, part, absPath string, flags int, allowDir bool) (int, bool, error) {
	openFlags := flags | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	fd, err := openLeafAt(dirfd, part, openFlags, 0)
	if err != nil {
		if errors.Is(err, errForbiddenPath) {
			return -1, false, err
		}
		if isSymlinkComponentErr(err) && isSymlinkAt(dirfd, part) {
			return -1, true, nil
		}
		return -1, false, &os.PathError{Op: "open", Path: absPath, Err: err}
	}
	if err := verifyOpenedFD(fd, absPath, allowDir); err != nil {
		_ = unix.Close(fd)
		return -1, false, err
	}
	return fd, false, nil
}

func finishGuardedFile(fd int, absPath string, flags int) *os.File {
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK)
	return os.NewFile(uintptr(fd), absPath)
}

func isSymlinkComponentErr(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EMLINK)
}

func isSymlinkAt(dirfd int, name string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFLNK
}

func readlinkAt(dirfd int, name string) (string, error) {
	for size := 256; size <= 65536; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(dirfd, name, buf)
		if err != nil {
			return "", err
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
	return "", unix.ENAMETOOLONG
}

func verifyOpenedFD(fd int, absPath string, allowDir bool) error {
	if err := verifyDirFD(fd, absPath); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "fstat", Path: absPath, Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return nil
	case unix.S_IFDIR:
		if allowDir {
			return nil
		}
		return &os.PathError{Op: "open", Path: absPath, Err: unix.EISDIR}
	default:
		// Reject char/block devices, FIFOs, and sockets to avoid hangs and
		// device-node reads/writes.
		return &forbiddenPathError{path: absPath}
	}
}

// checkRootDirFS verifies that a directory root passed to ListDirectory or
// SearchFiles does not live on a blocked pseudo-filesystem mount alias.
func checkRootDirFS(absPath string) error {
	if isBlockedDirFS(absPath) {
		return &forbiddenPathError{path: absPath}
	}
	return nil
}
