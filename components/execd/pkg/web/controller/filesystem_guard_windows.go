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

//go:build windows

package controller

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func isBlockedDirFS(_ string) bool {
	return false
}

func checkRootDirFS(_ string) error {
	return nil
}

func renameStagedUpload(tmpName, absTarget string) error {
	return os.Rename(tmpName, absTarget)
}

func openGuardedReadFile(absPath string) (*os.File, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	if err := verifyWindowsHandle(f, absPath, true); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openGuardedWriteFile(absPath string, flags int) (*os.File, error) {
	// Defer truncation until after handle verification so a reparse-point
	// swap cannot truncate a blocked target file.
	openFlags := flags &^ os.O_TRUNC
	f, err := os.OpenFile(absPath, openFlags, 0)
	if err != nil {
		return nil, err
	}
	if err := verifyWindowsHandle(f, absPath, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	if flags&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

func verifyWindowsHandle(f *os.File, absPath string, allowDir bool) error {
	finalPath, err := finalPathByHandle(windows.Handle(f.Fd()))
	if err != nil {
		return err
	}
	if err := checkFilesystemPath(finalPath); err != nil {
		return err
	}
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

func finalPathByHandle(h windows.Handle) (string, error) {
	// FILE_NAME_NORMALIZED (0x0) | VOLUME_NAME_DOS (0x0).
	const flags = 0
	buf := make([]uint16, 260)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
	if err != nil {
		return "", err
	}
	if n > uint32(len(buf)) {
		buf = make([]uint16, n)
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil {
			return "", err
		}
	}
	return stripWindowsExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
}

func stripWindowsExtendedPrefix(p string) string {
	if len(p) >= 8 && strings.EqualFold(p[:8], `\\?\UNC\`) {
		return `\\` + p[8:]
	}
	if strings.HasPrefix(p, `\\?\`) {
		return p[4:]
	}
	return p
}
