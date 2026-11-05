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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// mkKernelWhiteout creates the 0/0 character device kernel overlayfs leaves
// in upper when a process deletes a lower entry. Linux 5.8+ lets any user
// create it.
func mkKernelWhiteout(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mknod(path, unix.S_IFCHR, 0); err != nil {
		t.Skipf("cannot create a whiteout device: %v", err)
	}
}

func TestMergedView_KernelWhiteoutHidesLowerFile(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "f.txt"), []byte("deleted"), 0o644))
	mkKernelWhiteout(t, filepath.Join(mv.UpperDir, "f.txt"))

	assertNotInView(t, mv, "f.txt")
}

func TestMergedView_KernelWhiteoutHidesLowerDirectory(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "x.txt"), []byte("deleted"), 0o644))
	mkKernelWhiteout(t, filepath.Join(mv.UpperDir, "d"))

	assertNotInView(t, mv, "d")
	assertNotInView(t, mv, filepath.Join("d", "x.txt"))
	_, err := mv.ReadDir("d")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestMergedView_OpaqueDirectoryHidesLowerContents(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	require.NoError(t, os.MkdirAll(filepath.Join(mv.LowerDir, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mv.LowerDir, "d", "old.txt"), []byte("old"), 0o644))
	upperDir := filepath.Join(mv.UpperDir, "d")
	require.NoError(t, os.MkdirAll(upperDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(upperDir, "new.txt"), []byte("new"), 0o644))
	// What a process's `rm -r d && mkdir d` leaves behind; bwrap always
	// mounts the overlay with userxattr.
	if err := unix.Setxattr(upperDir, "user.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("cannot set user xattrs here: %v", err)
	}

	assertNotInView(t, mv, filepath.Join("d", "old.txt"))
	entries, err := mv.ReadDir("d")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "new.txt", entries[0].Name())
}
