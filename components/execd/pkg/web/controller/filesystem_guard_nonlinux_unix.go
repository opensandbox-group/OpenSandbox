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

//go:build unix && !linux

package controller

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const dirTraversalFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

const rootDirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC

const parentDirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC

func verifyDirFD(_ int, _ string) error {
	return nil
}

func isBlockedDirFS(_ string) bool {
	return false
}

func openLeafAt(dirfd int, name string, flags int, perm uint32) (int, error) {
	return unix.Openat(dirfd, name, flags, perm)
}

func chmodChownPinnedLeaf(parentFD int, base, absPath string, mode os.FileMode, hasMode bool, uid, gid int, hasOwner bool) error {
	if hasMode {
		if err := unix.Fchmodat(parentFD, base, uint32(mode), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return &os.PathError{Op: "chmod", Path: absPath, Err: err}
		}
	}
	if hasOwner {
		if err := unix.Fchownat(parentFD, base, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("failed to set owner/group for %s: %w", absPath, err)
		}
	}
	return nil
}

func renameNoReplaceAt(srcParentFD int, srcBase string, dstParentFD int, dstBase string) error {
	return unix.Renameat(srcParentFD, srcBase, dstParentFD, dstBase)
}

func statTFromUnix(st *unix.Stat_t) (*syscall.Stat_t, time.Time) {
	mtim := syscall.Timespec{Sec: st.Mtim.Sec, Nsec: st.Mtim.Nsec}
	ctim := syscall.Timespec{Sec: st.Ctim.Sec, Nsec: st.Ctim.Nsec}
	sys := &syscall.Stat_t{
		Dev:       st.Dev,
		Ino:       st.Ino,
		Mode:      st.Mode,
		Uid:       st.Uid,
		Gid:       st.Gid,
		Size:      st.Size,
		Mtimespec: mtim,
		Ctimespec: ctim,
	}
	return sys, time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
}
