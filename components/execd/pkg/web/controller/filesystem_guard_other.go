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

//go:build !linux

package controller

import (
	"os"
)

func openGuardedReadFile(absPath string) (*os.File, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	if err := verifyOpenedFile(f, absPath, true); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openGuardedWriteFile(absPath string, flags int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(absPath, flags, perm)
	if err != nil {
		return nil, err
	}
	if err := verifyOpenedFile(f, absPath, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func verifyOpenedFile(f *os.File, absPath string, allowDir bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return nil
	}
	if info.IsDir() && allowDir {
		return nil
	}
	return &forbiddenPathError{path: absPath}
}
