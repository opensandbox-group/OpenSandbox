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

//go:build !windows

package runtime

import (
	"io"
	"os"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

var _ vfs.FS = (*isolatedSessionFS)(nil)

// isolatedSessionFS wraps every VFS call in a session operation lease. Delete
// first closes admission and then waits for existing leases before removing an
// overlay upper, preventing an in-flight upload from recreating an untracked
// upper directory after cleanup.
type isolatedSessionFS struct {
	session  *isolatedSession
	delegate vfs.FS
}

func (f *isolatedSessionFS) withLease(operation func() error) error {
	if err := f.session.beginOperation(); err != nil {
		return err
	}
	defer f.session.endOperation()
	return operation()
}

func withSessionFSLease[T any](
	f *isolatedSessionFS,
	operation func() (T, error),
) (T, error) {
	var zero T
	if err := f.session.beginOperation(); err != nil {
		return zero, err
	}
	defer f.session.endOperation()
	return operation()
}

func (f *isolatedSessionFS) Stat(path string) (os.FileInfo, error) {
	return withSessionFSLease(f, func() (os.FileInfo, error) {
		return f.delegate.Stat(path)
	})
}

func (f *isolatedSessionFS) ReadFile(path string) ([]byte, error) {
	return withSessionFSLease(f, func() ([]byte, error) {
		return f.delegate.ReadFile(path)
	})
}

func (f *isolatedSessionFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return f.withLease(func() error {
		return f.delegate.WriteFile(path, data, perm)
	})
}

func (f *isolatedSessionFS) WriteFileReader(
	path string,
	reader io.Reader,
	perm os.FileMode,
) (int64, error) {
	return withSessionFSLease(f, func() (int64, error) {
		return f.delegate.WriteFileReader(path, reader, perm)
	})
}

func (f *isolatedSessionFS) Remove(path string) error {
	return f.withLease(func() error {
		return f.delegate.Remove(path)
	})
}

func (f *isolatedSessionFS) RemoveAll(path string) error {
	return f.withLease(func() error {
		return f.delegate.RemoveAll(path)
	})
}

func (f *isolatedSessionFS) MkdirAll(path string, perm os.FileMode) error {
	return f.withLease(func() error {
		return f.delegate.MkdirAll(path, perm)
	})
}

func (f *isolatedSessionFS) Rename(oldPath, newPath string) error {
	return f.withLease(func() error {
		return f.delegate.Rename(oldPath, newPath)
	})
}

func (f *isolatedSessionFS) Chmod(path string, mode os.FileMode) error {
	return f.withLease(func() error {
		return f.delegate.Chmod(path, mode)
	})
}

func (f *isolatedSessionFS) ReadDir(path string) ([]os.DirEntry, error) {
	return withSessionFSLease(f, func() ([]os.DirEntry, error) {
		return f.delegate.ReadDir(path)
	})
}

func (f *isolatedSessionFS) Open(path string) (*os.File, error) {
	return withSessionFSLease(f, func() (*os.File, error) {
		return f.delegate.Open(path)
	})
}

func (f *isolatedSessionFS) Search(root, pattern string) ([]string, error) {
	return withSessionFSLease(f, func() ([]string, error) {
		return f.delegate.Search(root, pattern)
	})
}

func (f *isolatedSessionFS) ReplaceContent(path, old, newString string) error {
	return f.withLease(func() error {
		return f.delegate.ReplaceContent(path, old, newString)
	})
}
