//go:build linux

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

package isolation

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// isKernelWhiteout reports whether info is the 0/0 character device kernel
// overlayfs leaves in upper when a process deletes a lower entry.
func isKernelWhiteout(info fs.FileInfo) bool {
	if info.Mode().Type() != fs.ModeDevice|fs.ModeCharDevice {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Rdev == 0
}

// isOpaqueDir reports whether kernel overlayfs marked the upper directory at
// path opaque, which it does when a process replaces a deleted lower
// directory. bwrap always mounts the overlay with userxattr, with or without
// a user namespace, so the marker is user.overlay.opaque.
func isOpaqueDir(path string) bool {
	buf := make([]byte, 1)
	n, err := unix.Lgetxattr(path, "user.overlay.opaque", buf)
	return err == nil && n == 1 && buf[0] == 'y'
}
