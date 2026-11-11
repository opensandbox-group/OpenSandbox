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

//go:build !windows

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func ensurePrivateCommandOutputDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create command output parent directory: %w", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create command output directory %s: %w", path, err)
	}

	// Open the directory without following the final path component. This makes
	// the type, owner, and mode checks refer to the object that execd will use,
	// rather than to a symlink target selected by the sandbox workload.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open command output directory %s without following symlinks: %w", path, err)
	}
	defer unix.Close(fd) //nolint:errcheck

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat command output directory %s: %w", path, err)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("command output directory %s is owned by uid %d, want %d", path, stat.Uid, os.Geteuid())
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("command output directory %s has unsafe permissions %#o", path, stat.Mode&0o777)
	}
	return nil
}

func openNewCommandOutput(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create command output file %s: %w", path, err)
	}
	return file, nil
}

// truncateCommandOutputFile releases the file's data blocks through a
// descriptor opened without following symlinks: cleanup runs on paths a
// sandbox workload can race with, and a truncate that follows a symlink swap
// would zero an arbitrary file where os.Remove alone would only remove the
// link. O_NONBLOCK keeps a FIFO swap from blocking the open, and the
// descriptor is verified to be a regular file before truncating; anything
// else (symlink, FIFO, socket) is skipped, leaving the entry to the unlink.
// Hard links are skipped for the same reason: with Nlink > 1 the inode's
// content is still visible through other names, and truncating would clear
// data this name's deletion was never meant to touch. An fd held open by a
// forked child does not raise Nlink, so the reclaim behavior for ordinary
// command output files is unaffected.
func truncateCommandOutputFile(path string) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) {
			return nil
		}
		return err
	}
	defer unix.Close(fd) //nolint:errcheck

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	if stat.Nlink > 1 {
		return nil
	}
	return unix.Ftruncate(fd, 0)
}

func openCommandOutputForRead(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open command output file %s", path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("command output file %s is not a regular file", path)
	}
	return file, nil
}
