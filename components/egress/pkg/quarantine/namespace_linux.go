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

package quarantine

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// NamespaceTarget pins this process's original network namespace until Close.
// Keeping its nsfs descriptor open prevents that namespace's inode from being
// recycled, even if the process later leaves the namespace. It is only a live
// process handle, not an identity that can be saved or resumed after a restart.
// The caller must prevent namespace changes between Check and its operation.
// NamespaceTarget must not be copied after first use.
type NamespaceTarget struct {
	mu       sync.Mutex
	file     *os.File
	identity networkNamespaceIdentity
}

type networkNamespaceIdentity struct{ dev, inode uint64 }

// PinNamespace acquires an nsfs descriptor for /proc/self/ns/net. It does not
// switch namespaces, install a fence, or read or write persistent state.
func PinNamespace() (*NamespaceTarget, error) {
	file, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return nil, fmt.Errorf("pin network namespace: %w", err)
	}
	identity, err := readNetworkNamespaceIdentity(file)
	if err != nil {
		return nil, fmt.Errorf("pin network namespace: %w", errors.Join(err, file.Close()))
	}
	return &NamespaceTarget{file: file, identity: identity}, nil
}

// Check verifies the still-open pin and the current process namespace against
// the original kernel device/inode identity. An error never rebinds the target.
func (n *NamespaceTarget) Check() (err error) {
	if n == nil {
		return errors.New("network namespace target is unavailable")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.file == nil {
		return errors.New("network namespace target is closed")
	}
	pinned, err := readNetworkNamespaceIdentity(n.file)
	if err != nil {
		return fmt.Errorf("check pinned network namespace: %w", err)
	}
	if pinned != n.identity {
		return errors.New("pinned network namespace identity changed")
	}
	current, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open current network namespace: %w", err)
	}
	defer func() { err = errors.Join(err, current.Close()) }()
	identity, err := readNetworkNamespaceIdentity(current)
	if err != nil {
		return fmt.Errorf("check current network namespace: %w", err)
	}
	if identity != n.identity {
		return errors.New("current network namespace does not match pinned target")
	}
	return nil
}

// Close releases the live namespace pin. It is idempotent; Check fails after
// Close, and neither closing nor reopening a handle changes any packet rules.
func (n *NamespaceTarget) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.file == nil {
		return nil
	}
	file := n.file
	n.file = nil
	return file.Close()
}

func readNetworkNamespaceIdentity(file *os.File) (networkNamespaceIdentity, error) {
	var identity networkNamespaceIdentity
	fd := int(file.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return identity, err
	}
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return identity, err
	}
	if filesystem.Type != unix.NSFS_MAGIC {
		return identity, errors.New("network namespace descriptor is not nsfs")
	}
	kind, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil {
		return identity, fmt.Errorf("read namespace type: %w", err)
	}
	if kind != unix.CLONE_NEWNET {
		return identity, errors.New("namespace descriptor is not a network namespace")
	}
	if stat.Ino == 0 {
		return identity, errors.New("network namespace inode is invalid")
	}
	return networkNamespaceIdentity{dev: uint64(stat.Dev), inode: stat.Ino}, nil
}
