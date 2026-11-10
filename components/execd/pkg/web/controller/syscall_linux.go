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

//go:build linux
// +build linux

package controller

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func getFileCreateTime(filePath string, fileInfo os.FileInfo) time.Time {
	var statx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, filePath, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &statx); err == nil && statx.Mask&unix.STATX_BTIME != 0 {
		return time.Unix(statx.Btime.Sec, int64(statx.Btime.Nsec))
	}

	// Preserve the legacy fallback on kernels/filesystems without birth time.
	// Inode change time is not creation time and can change after metadata updates.
	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileInfo.ModTime()
	}

	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
}
