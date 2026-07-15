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

package pathutil

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandPath_HomeAndEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASE_DIR", "project")

	got, err := ExpandPath("~/docs/$BASE_DIR")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "docs", "project"), got)
}

func TestExpandPath_Empty(t *testing.T) {
	got, err := ExpandPath("")
	require.NoError(t, err)
	require.Equal(t, "", got)
}

func TestExpandPath_EnvVarInMiddle(t *testing.T) {
	t.Setenv("MID", "segment")

	got, err := ExpandPath("prefix/$MID/suffix")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("prefix", "segment", "suffix"), got)
}

func TestExpandPathWithEnv_RequestOverrideHasHigherPriority(t *testing.T) {
	t.Setenv("WORKDIR", "from-process")

	got, err := ExpandPathWithEnv("base/$WORKDIR", map[string]string{
		"WORKDIR": "from-request",
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join("base", "from-request"), got)
}

func TestExpandPathWithEnv_CanResolveVarOnlyInOverride(t *testing.T) {
	got, err := ExpandPathWithEnv("$WORKDIR/tmp", map[string]string{
		"WORKDIR": "/tmp/ws",
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join("/tmp/ws", "tmp"), got)
}

func TestExpandPath_UndefinedEnvVarInMiddleReturnsError(t *testing.T) {
	got, err := ExpandPath("prefix/$NOT_SET/suffix")
	require.Error(t, err)
	require.Contains(t, err.Error(), "NOT_SET")
	require.Equal(t, "", got)
}

func TestExpandPath_UndefinedEnvVarReturnsError(t *testing.T) {
	got, err := ExpandPath("$NOT_SET/tmp")
	require.Error(t, err)
	require.Contains(t, err.Error(), "NOT_SET")
	require.Equal(t, "", got)
}

func TestExpandPath_UndefinedEnvVarOnlyReturnsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := ExpandPath("$NOT_SET")
	require.Error(t, err)
	require.Contains(t, err.Error(), "NOT_SET")
	require.Equal(t, "", got)
}
