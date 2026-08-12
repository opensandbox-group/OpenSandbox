// Copyright 2026 Alibaba Group Holding Ltd.
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

package file

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"golang.org/x/sys/unix"
)

func openNoFollow(path string) (*os.File, error) {
	return openNoFollowWithFlags(path, unix.O_CREAT|unix.O_RDWR|unix.O_APPEND|unix.O_NONBLOCK, 0o640)
}

func openNoFollowRead(path string) (*os.File, error) {
	return openNoFollowWithFlags(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
}

func openNoFollowExisting(path string) (*os.File, error) {
	return openNoFollowWithFlags(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
}

func createNoFollowExclusive(path string) (*os.File, error) {
	return openNoFollowWithFlags(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NONBLOCK, 0o640)
}

func openNoFollowWithFlags(path string, flags int, mode uint32) (*os.File, error) {
	dirFD, name, clean, err := openParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dirFD, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	_ = unix.Close(dirFD)
	if err != nil {
		return nil, classifyPathError("open durable file", clean, err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, classifyPathError("stat durable file", clean, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, api.Permanent(fmt.Errorf("durable file target %q is not a regular file", clean))
	}
	return f, nil
}

func openParentNoFollow(path string) (int, string, string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return -1, "", clean, api.Permanent(fmt.Errorf("durable file path %q must be absolute", path))
	}
	name := filepath.Base(clean)
	if clean == string(filepath.Separator) || name == "" || name == "." || name == ".." {
		return -1, "", clean, api.Permanent(fmt.Errorf("durable file path %q has no filename", path))
	}
	dirFD, err := openDirNoFollowAs(filepath.Dir(clean), "open durable file directory", clean)
	if err != nil {
		return -1, "", clean, err
	}
	return dirFD, name, clean, nil
}

func openDirNoFollowAs(path, operation, errorPath string) (int, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return -1, api.Permanent(fmt.Errorf("durable directory path %q must be absolute", path))
	}
	dirFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, classifyPathError(operation, errorPath, err)
	}
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			_ = unix.Close(dirFD)
			return -1, api.Permanent(fmt.Errorf("durable directory path %q has an unsafe component", clean))
		}
		nextFD, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(dirFD)
		if openErr != nil {
			return -1, classifyPathError(operation, errorPath, openErr)
		}
		dirFD = nextFD
	}
	return dirFD, nil
}

func mkdirAllNoFollow(path string, mode os.FileMode) error {
	return mkdirAllNoFollowWithSync(path, mode, unix.Fsync)
}

func mkdirAllNoFollowWithSync(path string, mode os.FileMode, syncFD func(int) error) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return api.Permanent(fmt.Errorf("durable directory path %q must be absolute", path))
	}
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	dirFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return classifyPathError("open durable directory root", clean, err)
	}
	defer func() { _ = unix.Close(dirFD) }()
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return api.Permanent(fmt.Errorf("durable directory path %q has an unsafe component", clean))
		}
		created := false
		if err := unix.Mkdirat(dirFD, part, uint32(mode.Perm())); err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return classifyPathError("create durable directory", clean, err)
		}
		if created {
			if err := syncDirectoryFD(dirFD, clean, syncFD); err != nil {
				return err
			}
		}
		nextFD, err := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return classifyPathError("open durable directory", clean, err)
		}
		_ = unix.Close(dirFD)
		dirFD = nextFD
	}
	return nil
}

func syncData(f *os.File) error {
	for {
		err := unix.Fdatasync(int(f.Fd()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return classifyPathError("sync durable file", f.Name(), err)
		}
		return nil
	}
}

func renameNoReplace(oldPath, newPath string) error {
	oldDirFD, oldName, cleanOldPath, err := openParentNoFollow(oldPath)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(oldDirFD) }()
	newDirFD, newName, cleanNewPath, err := openParentNoFollow(newPath)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(newDirFD) }()

	err = unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EINVAL) {
		return classifyPathError("publish finalization marker", cleanNewPath, err)
	}
	if linkErr := unix.Linkat(oldDirFD, oldName, newDirFD, newName, 0); linkErr != nil {
		return classifyPathError("publish finalization marker", cleanNewPath, linkErr)
	}
	if removeErr := unix.Unlinkat(oldDirFD, oldName, 0); removeErr != nil {
		return classifyPathError("remove temporary finalization marker", cleanOldPath, removeErr)
	}
	return nil
}

func syncDir(path string) error {
	clean := filepath.Clean(path)
	fd, err := openDirNoFollowAs(clean, "open durable directory for sync", clean)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return syncDirectoryFD(fd, clean, unix.Fsync)
}

func syncDirectoryFD(fd int, path string, syncFD func(int) error) error {
	for {
		err := syncFD(fd)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return classifyPathError("sync durable directory", path, err)
		}
		return nil
	}
}

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, api.Permanent(errors.New("unsupported durable file identity"))
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func classifyPathError(operation, path string, err error) error {
	wrapped := fmt.Errorf("%s %q: %w", operation, path, err)
	if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.EPERM) || errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EROFS) ||
		errors.Is(err, unix.ENAMETOOLONG) || errors.Is(err, unix.EINVAL) {
		return api.Permanent(wrapped)
	}
	return wrapped
}
