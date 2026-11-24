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

//go:build !windows

package controller

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilesystemControllerChmodFilesEmptyPathLeavesWorkingDirectory(t *testing.T) {
	t.Setenv("EXECD_CHMOD_EMPTY_PATH", "")
	for _, path := range []string{"", "$EXECD_CHMOD_EMPTY_PATH"} {
		t.Run(path, func(t *testing.T) {
			workDir := filepath.Join(t.TempDir(), "cwd")
			require.NoError(t, os.Mkdir(workDir, 0o755))
			t.Chdir(workDir)
			payload := []byte(`{"` + path + `": {"mode": 700}}`)
			ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/permissions", payload)

			ctrl.ChmodFiles()

			info, err := os.Stat(workDir)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the daemon's working directory was chmodded")
			require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}
