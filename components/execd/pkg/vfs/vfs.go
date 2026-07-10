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

// Package vfs defines a virtual filesystem interface used by file handlers.
package vfs

import (
	"io"
	"os"
)

// FS abstracts filesystem operations so that both the host OS and
// overlay (MergedView) can be used interchangeably by file handlers.
type FS interface {
	Stat(path string) (os.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error)
	Remove(path string) error
	RemoveAll(path string) error
	MkdirAll(path string, perm os.FileMode) error
	Rename(oldPath, newPath string) error
	Chmod(path string, mode os.FileMode) error
	ReadDir(path string) ([]os.DirEntry, error)
	Open(path string) (*os.File, error)
	Search(root, pattern string) ([]string, error)
	ReplaceContent(path, old, newStr string) error
}
