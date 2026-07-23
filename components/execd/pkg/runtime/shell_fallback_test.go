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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// useShOnlyPath limits PATH to a directory containing sh but not bash.
func useShOnlyPath(t *testing.T) {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	require.NoError(t, err, "sh is required to test the fallback")
	shPath, err = filepath.Abs(shPath)
	require.NoError(t, err)

	binDir := t.TempDir()
	require.NoError(t, os.Symlink(shPath, filepath.Join(binDir, "sh")))
	t.Setenv("PATH", binDir)
	require.Equal(t, "sh", getShell())
}
