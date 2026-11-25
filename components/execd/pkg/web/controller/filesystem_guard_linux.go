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

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

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
