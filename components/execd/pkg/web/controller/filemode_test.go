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
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func TestChmodFileAppliesSetuidAndReportsIt(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("setuid is a unix file mode")
	}

	path := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644))

	require.NoError(t, ChmodFile(path, model.Permission{Mode: 4755}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSetuid)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	got, err := GetFileInfo(path)
	require.NoError(t, err)
	require.Equal(t, 4755, got.Permission.Mode)
}

func TestMakeDirAppliesStickyBit(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("sticky is a unix file mode")
	}

	dir := filepath.Join(t.TempDir(), "drop")
	require.NoError(t, MakeDir(dir, model.Permission{Mode: 1755}))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSticky)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	got, err := GetFileInfo(dir)
	require.NoError(t, err)
	require.Equal(t, 1755, got.Permission.Mode)
}

func TestChmodFileModeZeroLeavesPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	require.NoError(t, ChmodFile(path, model.Permission{Mode: 0}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
