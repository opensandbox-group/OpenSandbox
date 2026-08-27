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

package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateWorkingDir_empty(t *testing.T) {
	require.NoError(t, ValidateWorkingDir(""))
}

func TestValidateWorkingDir_notExist(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "definitely-missing-subdir")
	err := ValidateWorkingDir(missing)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
	require.Contains(t, err.Error(), missing)
}

func TestValidateWorkingDir_notDir(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
	err := ValidateWorkingDir(f)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}

func TestValidateWorkingDir_ok(t *testing.T) {
	require.NoError(t, ValidateWorkingDir(t.TempDir()))
}

func TestValidateWorkingDir_ExpandsHome(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(target, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	require.NoError(t, ValidateWorkingDir("~/workspace"))
}

func TestValidateWorkingDirWithEnv(t *testing.T) {
	const (
		requestOnlyKey = "OPENSANDBOX_TEST_REQUEST_ONLY_CWD"
		overrideKey    = "OPENSANDBOX_TEST_OVERRIDE_CWD"
		missingKey     = "OPENSANDBOX_TEST_MISSING_CWD"
	)

	unsetEnvForTest(t, requestOnlyKey)
	requestDir := t.TempDir()
	require.NoError(t, ValidateWorkingDirWithEnv("$"+requestOnlyKey, map[string]string{
		requestOnlyKey: requestDir,
	}))

	processFile := filepath.Join(t.TempDir(), "process-file")
	require.NoError(t, os.WriteFile(processFile, []byte("x"), 0o600))
	t.Setenv(overrideKey, processFile)
	require.NoError(t, ValidateWorkingDirWithEnv("$"+overrideKey, map[string]string{
		overrideKey: t.TempDir(),
	}))

	unsetEnvForTest(t, missingKey)
	err := ValidateWorkingDirWithEnv("$"+missingKey, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "undefined environment variables")

	nonexistent := filepath.Join(t.TempDir(), "missing")
	err = ValidateWorkingDirWithEnv("$TARGET", map[string]string{"TARGET": nonexistent})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")

	err = ValidateWorkingDirWithEnv("$TARGET", map[string]string{"TARGET": processFile})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if existed {
			require.NoError(t, os.Setenv(key, previous))
			return
		}
		require.NoError(t, os.Unsetenv(key))
	})
}
