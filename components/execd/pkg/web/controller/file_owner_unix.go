//go:build unix

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

package controller

import (
	"fmt"
	"os"
	"syscall"
)

// copyFileOwnership records the owner and group of info on file, so staging an
// upload and renaming it over an existing one keeps the ownership an in-place
// write preserved. The open descriptor is used rather than the staging path:
// the staging directory is writable, so the name could be swapped between
// creation and this call.
func copyFileOwnership(file *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := file.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
		return fmt.Errorf("chown to %d:%d: %w", stat.Uid, stat.Gid, err)
	}
	return nil
}
