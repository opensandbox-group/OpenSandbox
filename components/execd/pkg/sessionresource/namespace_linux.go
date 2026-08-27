//go:build linux

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

package sessionresource

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type platformNamespaceOps struct{}

func (platformNamespaceOps) supported() bool {
	return true
}

func (platformNamespaceOps) effectiveUID() uint32 {
	return uint32(os.Geteuid())
}

func (platformNamespaceOps) mkdirAll(path string, mode fs.FileMode) error {
	return os.MkdirAll(path, mode)
}

func (platformNamespaceOps) mkdir(path string, mode fs.FileMode) error {
	return os.Mkdir(path, mode)
}

func (platformNamespaceOps) createFile(
	path string,
	mode fs.FileMode,
) (bool, error) {
	fd, err := unix.Open(
		path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC,
		uint32(mode.Perm()),
	)
	if err != nil {
		return false, err
	}
	return true, unix.Close(fd)
}

func (platformNamespaceOps) statPath(path string) (pathInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return pathInfo{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pathInfo{}, errorsUnsupportedStat(path)
	}
	return pathInfo{
		dev:   uint64(stat.Dev),
		inode: stat.Ino,
		mode:  info.Mode(),
		uid:   stat.Uid,
	}, nil
}

func errorsUnsupportedStat(path string) error {
	return fmt.Errorf("stat %s: unsupported platform stat payload", path)
}

func (platformNamespaceOps) readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (platformNamespaceOps) openNamespace(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
}

func (platformNamespaceOps) statFD(fd int) (pathInfo, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return pathInfo{}, err
	}
	return pathInfo{
		dev:   uint64(stat.Dev),
		inode: stat.Ino,
		mode:  unixFileMode(stat.Mode),
		uid:   stat.Uid,
	}, nil
}

func unixFileMode(mode uint32) fs.FileMode {
	result := fs.FileMode(mode & 0o7777)
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		result |= fs.ModeDir
	case unix.S_IFLNK:
		result |= fs.ModeSymlink
	case unix.S_IFSOCK:
		result |= fs.ModeSocket
	case unix.S_IFIFO:
		result |= fs.ModeNamedPipe
	case unix.S_IFCHR:
		result |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		result |= fs.ModeDevice
	}
	return result
}

func (platformNamespaceOps) fileSystemType(fd int) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Type), nil
}

func (platformNamespaceOps) namespaceType(fd int) (int, error) {
	return unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
}

func (platformNamespaceOps) namespaceOwner(fd int) (int, error) {
	ownerFD, err := unix.IoctlRetInt(fd, unix.NS_GET_USERNS)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(ownerFD)
	return ownerFD, nil
}

func (platformNamespaceOps) namespaceOwnerUID(fd int) (uint32, error) {
	return unix.IoctlGetUint32(fd, unix.NS_GET_OWNER_UID)
}

func (platformNamespaceOps) bindMountFD(fd int, target string) error {
	return unix.Mount(
		fmt.Sprintf("/proc/self/fd/%d", fd),
		target,
		"",
		unix.MS_BIND,
		"",
	)
}

func (platformNamespaceOps) unmount(path string) error {
	return unix.Unmount(path, 0)
}

func (platformNamespaceOps) remove(path string) error {
	return os.Remove(path)
}

func (platformNamespaceOps) closeFD(fd int) error {
	return unix.Close(fd)
}
