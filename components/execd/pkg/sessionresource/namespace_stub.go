//go:build !linux

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
	"io/fs"
)

type platformNamespaceOps struct{}

func (platformNamespaceOps) supported() bool {
	return false
}

func (platformNamespaceOps) effectiveUID() uint32 {
	return 0
}

func (platformNamespaceOps) mkdirAll(string, fs.FileMode) error {
	return ErrUnsupported
}

func (platformNamespaceOps) mkdir(string, fs.FileMode) error {
	return ErrUnsupported
}

func (platformNamespaceOps) createFile(string, fs.FileMode) (bool, error) {
	return false, ErrUnsupported
}

func (platformNamespaceOps) statPath(string) (pathInfo, error) {
	return pathInfo{}, ErrUnsupported
}

func (platformNamespaceOps) readFile(string) ([]byte, error) {
	return nil, ErrUnsupported
}

func (platformNamespaceOps) openNamespace(string) (int, error) {
	return -1, ErrUnsupported
}

func (platformNamespaceOps) statFD(int) (pathInfo, error) {
	return pathInfo{}, ErrUnsupported
}

func (platformNamespaceOps) fileSystemType(int) (int64, error) {
	return 0, ErrUnsupported
}

func (platformNamespaceOps) namespaceType(int) (int, error) {
	return 0, ErrUnsupported
}

func (platformNamespaceOps) namespaceOwner(int) (int, error) {
	return -1, ErrUnsupported
}

func (platformNamespaceOps) namespaceOwnerUID(int) (uint32, error) {
	return 0, ErrUnsupported
}

func (platformNamespaceOps) bindMountFD(int, string) error {
	return ErrUnsupported
}

func (platformNamespaceOps) unmount(string) error {
	return ErrUnsupported
}

func (platformNamespaceOps) remove(string) error {
	return ErrUnsupported
}

func (platformNamespaceOps) closeFD(int) error {
	return ErrUnsupported
}
