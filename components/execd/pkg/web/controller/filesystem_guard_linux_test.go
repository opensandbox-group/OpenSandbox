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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestIsBlockedFSMagic(t *testing.T) {
	blocked := []uint64{
		uint64(unix.PROC_SUPER_MAGIC),
		uint64(unix.SYSFS_MAGIC),
		uint64(unix.DEVPTS_SUPER_MAGIC),
		uint64(unix.CGROUP_SUPER_MAGIC),
		uint64(unix.CGROUP2_SUPER_MAGIC),
		uint64(unix.BPF_FS_MAGIC),
		uint64(unix.SECURITYFS_MAGIC),
		uint64(unix.DEBUGFS_MAGIC),
		uint64(unix.TRACEFS_MAGIC),
	}
	for _, magic := range blocked {
		require.Truef(t, isBlockedFSMagic(magic), "expected magic 0x%x to be blocked", magic)
	}

	allowed := []uint64{
		uint64(unix.TMPFS_MAGIC),
		uint64(unix.EXT4_SUPER_MAGIC),
		uint64(unix.OVERLAYFS_SUPER_MAGIC),
	}
	for _, magic := range allowed {
		require.Falsef(t, isBlockedFSMagic(magic), "expected magic 0x%x to be allowed", magic)
	}
}

func TestOpenGuardedReadFileRejectsProcDescriptor(t *testing.T) {
	f, err := openGuardedReadFile("/proc/self/environ")
	if f != nil {
		_ = f.Close()
	}
	require.ErrorIs(t, err, errForbiddenPath)
}

func TestOpenLeafAtFallbackAndEXDEV(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "ok.txt")
	require.NoError(t, os.WriteFile(target, []byte("hello"), 0o644))

	orig := openat2Fn
	t.Cleanup(func() { openat2Fn = orig })

	t.Run("ENOSYS falls back to openat", func(t *testing.T) {
		openat2Fn = func(int, string, *unix.OpenHow) (int, error) {
			return -1, unix.ENOSYS
		}
		f, err := openGuardedReadFile(target)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	})

	t.Run("EXDEV maps to forbiddenPathError", func(t *testing.T) {
		openat2Fn = func(int, string, *unix.OpenHow) (int, error) {
			return -1, unix.EXDEV
		}
		f, err := openGuardedReadFile(target)
		if f != nil {
			_ = f.Close()
		}
		require.ErrorIs(t, err, errForbiddenPath)
	})
}
