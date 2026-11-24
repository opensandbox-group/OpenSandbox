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
	"os"

	"golang.org/x/sys/unix"
)

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

func openGuardedReadFile(absPath string) (*os.File, error) {
	return openGuardedFile(absPath, unix.O_RDONLY, 0, true)
}

func openGuardedWriteFile(absPath string, flags int, perm os.FileMode) (*os.File, error) {
	return openGuardedFile(absPath, flags, perm, false)
}

// openGuardedFile opens absPath with openat2(RESOLVE_NO_MAGICLINKS) when
// supported (falling back to open on older kernels or restricted seccomp
// profiles), then verifies the opened descriptor with fstatfs and fstat to
// prevent TOCTOU symlink swaps onto pseudo-filesystems or blocking on devices
// and FIFOs.
func openGuardedFile(absPath string, flags int, perm os.FileMode, allowDir bool) (*os.File, error) {
	openFlags := flags | unix.O_CLOEXEC | unix.O_NONBLOCK
	var mode uint64
	if flags&(unix.O_CREAT|unix.O_TMPFILE) != 0 {
		mode = uint64(perm.Perm())
	}

	how := &unix.OpenHow{
		Flags:   uint64(openFlags),
		Mode:    mode,
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, absPath, how)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
		fd, err = unix.Open(absPath, openFlags, uint32(perm.Perm()))
	}
	if err != nil {
		if errors.Is(err, unix.EXDEV) {
			return nil, &forbiddenPathError{path: absPath}
		}
		return nil, &os.PathError{Op: "open", Path: absPath, Err: err}
	}

	if err := verifyOpenedFD(fd, absPath, allowDir); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	// Clear O_NONBLOCK after verifying the descriptor is not a FIFO/device.
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK)
	return os.NewFile(uintptr(fd), absPath), nil
}

func verifyOpenedFD(fd int, absPath string, allowDir bool) error {
	var stfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &stfs); err == nil {
		if isBlockedFSMagic(uint64(stfs.Type)) {
			return &forbiddenPathError{path: absPath}
		}
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
