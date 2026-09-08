// Copyright 2025 Alibaba Group Holding Ltd.
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
