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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadExtraEnvFromFileUnset(t *testing.T) {
	t.Setenv("EXECD_ENVS", "")
	require.Nil(t, loadExtraEnvFromFile(), "expected nil when EXECD_ENVS unset")
}

func TestLoadExtraEnvFromFileParsesAndExpands(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")

	t.Setenv("EXECD_ENVS", envFile)
	t.Setenv("BASE_DIR", "/opt/base")

	content := strings.Join([]string{
		"# comment",
		"FOO=bar",
		"PATH=$BASE_DIR/bin",
		"MALFORMED",
		"EMPTY=",
		"",
	}, "\n")

	require.NoError(t, os.WriteFile(envFile, []byte(content), 0o644))

	got := loadExtraEnvFromFile()
	require.Len(t, got, 3)
	require.Equal(t, "bar", got["FOO"])
	require.Equal(t, "/opt/base/bin", got["PATH"])
	val, ok := got["EMPTY"]
	require.True(t, ok)
	require.Equal(t, "", val)
}

func TestLoadExtraEnvFromFileMissingFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "does-not-exist")
	t.Setenv("EXECD_ENVS", envFile)

	require.Nil(t, loadExtraEnvFromFile(), "expected nil for missing file")
}

func TestLoadExtraEnvFromFileSupportsHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	envFile := filepath.Join(home, "extra.env")
	require.NoError(t, os.WriteFile(envFile, []byte("FOO=bar\n"), 0o644))
	t.Setenv("EXECD_ENVS", "~/extra.env")

	got := loadExtraEnvFromFile()
	require.Equal(t, "bar", got["FOO"])
}

func TestMergeEnvsOverlaysExtra(t *testing.T) {
	base := []string{"A=1", "B=2"}
	extra := map[string]string{"B": "override", "C": "3"}

	merged := mergeEnvs(base, extra)
	got := make(map[string]string)
	for _, kv := range merged {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			got[parts[0]] = parts[1]
		}
	}

	require.Len(t, got, 3)
	require.Equal(t, "1", got["A"])
	require.Equal(t, "override", got["B"])
	require.Equal(t, "3", got["C"])
}

func TestMergeExtraEnvsMergesAndOverrides(t *testing.T) {
	fromFile := map[string]string{"A": "1", "B": "2"}
	fromRequest := map[string]string{"B": "override", "C": "3"}

	got := mergeExtraEnvs(fromFile, fromRequest)

	require.Len(t, got, 3)
	require.Equal(t, "1", got["A"])
	require.Equal(t, "override", got["B"])
	require.Equal(t, "3", got["C"])
}

func TestMergeExtraEnvsHandlesNilFromFile(t *testing.T) {
	fromRequest := map[string]string{"ONLY": "request"}

	got := mergeExtraEnvs(nil, fromRequest)

	require.Len(t, got, 1)
	require.Equal(t, "request", got["ONLY"])
}
