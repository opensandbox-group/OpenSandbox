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

//go:build linux

package controller

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// dirTraversalFlags uses O_PATH on Linux so non-root identity workers can
// traverse execute-only (0733/0111) ancestor directories without requiring
// read permission, while still supporting openat/fstatat/readlinkat/fstatfs.
const dirTraversalFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

const rootDirOpenFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC

const parentDirOpenFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC

// openat2Fn allows tests to exercise the fallback and EXDEV error paths.
var openat2Fn = unix.Openat2

// isBlockedFSMagic reports whether fsType belongs to a Linux pseudo-filesystem
// that must never be served or mutated via /files/* or /directories/*.
func isBlockedFSMagic(fsType uint64) bool {
	switch fsType {
	case uint64(unix.PROC_SUPER_MAGIC),
		uint64(unix.SYSFS_MAGIC),
		uint64(unix.DEVPTS_SUPER_MAGIC),
		uint64(unix.CGROUP_SUPER_MAGIC),
		uint64(unix.CGROUP2_SUPER_MAGIC),
		uint64(unix.BPF_FS_MAGIC),
		uint64(unix.SECURITYFS_MAGIC),
		uint64(unix.DEBUGFS_MAGIC),
		uint64(unix.TRACEFS_MAGIC):
		return true
	default:
		return false
	}
}

// verifyDirFD fails closed if fstatfs fails or reports a blocked pseudo-fs.
func verifyDirFD(fd int, absPath string) error {
	var stfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &stfs); err != nil {
		log.Warn("filesystem guard: fstatfs failed on %s: %v (rejecting fail-closed)", absPath, err)
		return &forbiddenPathError{path: absPath}
	}
	if isBlockedFSMagic(uint64(stfs.Type)) {
		return &forbiddenPathError{path: absPath}
	}
	return nil
}

// isBlockedDirFS reports whether dirPath resides on a blocked pseudo-filesystem
// (such as a bind-mounted /proc or /sys alias under an allowed directory).
func isBlockedDirFS(dirPath string) bool {
	fd, err := unix.Open(dirPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var stfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &stfs); err != nil {
		log.Warn("filesystem guard: fstatfs failed on directory %s: %v (treating as blocked)", dirPath, err)
		return true
	}
	return isBlockedFSMagic(uint64(stfs.Type))
}

func openLeafAt(dirfd int, name string, flags int, perm uint32) (int, error) {
	how := &unix.OpenHow{
		Flags:   uint64(flags),
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := openat2Fn(dirfd, name, how)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
		return unix.Openat(dirfd, name, flags, perm)
	}
	if errors.Is(err, unix.EXDEV) {
		return -1, &forbiddenPathError{path: name}
	}
	return fd, err
}

// chmodChownPinnedLeaf applies mode and/or ownership to an already-verified
// non-symlink leaf under parentFD using an O_PATH descriptor, so non-root
// workers can chmod owned 0200 files / 0300 dirs and metadata ops on FIFOs or
// sockets still work without following swapped symlinks.
func chmodChownPinnedLeaf(parentFD int, base, absPath string, mode os.FileMode, hasMode bool, uid, gid int, hasOwner bool) error {
	leafFD, err := openLeafAt(parentFD, base, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, errForbiddenPath) {
			return err
		}
		return &os.PathError{Op: "open", Path: absPath, Err: err}
	}
	defer unix.Close(leafFD)

	if err := verifyDirFD(leafFD, absPath); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(leafFD, &st); err != nil {
		return &os.PathError{Op: "fstat", Path: absPath, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return &forbiddenPathError{path: absPath}
	}
	if hasMode {
		// /proc/self/fd/<fd> addresses the pinned inode directly without
		// following user symlinks and works on every Linux kernel for O_PATH fds.
		procPath := "/proc/self/fd/" + strconv.Itoa(leafFD)
		if err := unix.Chmod(procPath, uint32(mode)); err != nil {
			return &os.PathError{Op: "chmod", Path: absPath, Err: err}
		}
	}
	if hasOwner {
		if err := unix.Fchownat(leafFD, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
			return fmt.Errorf("failed to set owner/group for %s: %w", absPath, err)
		}
	}
	return nil
}

func renameNoReplaceAt(srcParentFD int, srcBase string, dstParentFD int, dstBase string) error {
	err := unix.Renameat2(srcParentFD, srcBase, dstParentFD, dstBase, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) {
		return unix.Renameat(srcParentFD, srcBase, dstParentFD, dstBase)
	}
	return err
}

func statTFromUnix(st *unix.Stat_t) (*syscall.Stat_t, time.Time) {
	mtim := syscall.Timespec{Sec: st.Mtim.Sec, Nsec: st.Mtim.Nsec}
	ctim := syscall.Timespec{Sec: st.Ctim.Sec, Nsec: st.Ctim.Nsec}
	sys := &syscall.Stat_t{
		Dev:  st.Dev,
		Ino:  st.Ino,
		Mode: st.Mode,
		Uid:  st.Uid,
		Gid:  st.Gid,
		Size: st.Size,
		Mtim: mtim,
		Ctim: ctim,
	}
	return sys, time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
}
