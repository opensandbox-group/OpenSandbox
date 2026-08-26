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

package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigMaterializesEnvironmentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "lifecycle.toml")
	t.Setenv(ConfigPathEnv, path)
	t.Setenv(ConfigEnv, `{
  "version": 1,
  "preStart": {"command": ["sh", "-c", "echo ready"], "timeoutSeconds": 5},
  "periodic": [{"name": "sync", "schedule": "@every 5m", "command": ["sync"]}]
}`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"sh", "-c", "echo ready"}, cfg.PreStart.Command)
	require.FileExists(t, path)

	t.Setenv(ConfigEnv, "")
	reloaded, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	assert.Equal(t, "sync", reloaded.Periodic[0].Name)
}

func TestLoadConfigMaterializesEnvironmentConfigUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(ConfigPathEnv, "")
	t.Setenv(ConfigEnv, `{"preStart":{"command":["true"]}}`)

	cfg, err := LoadConfig()

	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.FileExists(t, filepath.Join(home, ".execd", "lifecycle.toml"))
}

func TestLoadConfigRejectsUnwritableDefaultPath(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".execd"), []byte("not a directory"), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(ConfigPathEnv, "")
	t.Setenv(ConfigEnv, `{"preStart":{"command":["true"]}}`)

	cfg, err := LoadConfig()

	assert.Nil(t, cfg)
	require.ErrorContains(t, err, "create lifecycle config directory")
}

func TestLoadConfigRejectsUnwritableExplicitPath(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(parent, []byte("file"), 0o600))
	t.Setenv(ConfigPathEnv, filepath.Join(parent, "lifecycle.toml"))
	t.Setenv(ConfigEnv, `{"preStart":{"command":["true"]}}`)

	cfg, err := LoadConfig()

	assert.Nil(t, cfg)
	require.ErrorContains(t, err, "create lifecycle config directory")
}

func TestLoadConfigEnvironmentOverridesPersistedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.toml")
	require.NoError(t, os.WriteFile(path, []byte(`version = 1
[preStart]
command = ["old"]
`), 0o600))
	t.Setenv(ConfigPathEnv, path)
	t.Setenv(ConfigEnv, `{"preStart":{"command":["new"]}}`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"new"}, cfg.PreStart.Command)

	t.Setenv(ConfigEnv, "")
	reloaded, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	assert.Equal(t, []string{"new"}, reloaded.PreStart.Command)
}

func TestDecodeConfigRejectsDuplicatePeriodicNames(t *testing.T) {
	_, err := decodeConfig([]byte(`{
  "periodic": [
    {"name": "sync", "schedule": "@hourly", "command": ["true"]},
    {"name": "sync", "schedule": "@daily", "command": ["true"]}
  ]
}`))

	require.ErrorContains(t, err, `duplicate periodic hook name "sync"`)
}

func TestDecodeConfigRejectsInvalidPeriodicSchedule(t *testing.T) {
	_, err := decodeConfig([]byte(`{
  "periodic": [{"name": "sync", "schedule": "61 * * * *", "command": ["true"]}]
}`))

	require.ErrorContains(t, err, `periodic hook "sync" has invalid schedule`)
}

func TestDecodeConfigNormalizesPeriodicIdentityAndSchedule(t *testing.T) {
	cfg, err := decodeConfig([]byte(`{
  "periodic": [{"name": " sync ", "schedule": " @every 1m ", "command": ["true"]}]
}`))

	require.NoError(t, err)
	assert.Equal(t, "sync", cfg.Periodic[0].Name)
	assert.Equal(t, "@every 1m", cfg.Periodic[0].Schedule)
}

func TestDecodeConfigRejectsNonWholeSecondEveryInterval(t *testing.T) {
	for _, schedule := range []string{
		"@every 500ms",
		"TZ=UTC @every 500ms",
		"CRON_TZ=UTC @every 1500ms",
	} {
		t.Run(schedule, func(t *testing.T) {
			_, err := decodeConfig([]byte(`{
  "periodic": [{"name": "sync", "schedule": "` + schedule + `", "command": ["true"]}]
}`))

			require.ErrorContains(t, err, `periodic hook "sync" @every interval must be a whole number of seconds`)
		})
	}
}

func TestDecodeConfigRejectsTimezoneWithoutSchedule(t *testing.T) {
	_, err := decodeConfig([]byte(`{
  "periodic": [{"name": "sync", "schedule": "TZ=UTC", "command": ["true"]}]
}`))

	require.ErrorContains(t, err, `periodic hook "sync" has invalid schedule`)
}

func TestDecodeConfigEnforcesMaximumHookTimeout(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "preStart", raw: `{"preStart":{"command":["true"],"timeoutSeconds":301}}`},
		{name: "periodic", raw: `{"periodic":[{"name":"sync","schedule":"@hourly","command":["true"],"timeoutSeconds":301}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeConfig([]byte(test.raw))
			require.ErrorContains(t, err, "timeoutSeconds must not exceed 300")
		})
	}

	_, err := decodeConfig([]byte(`{
  "preStart": {"command": ["true"], "timeoutSeconds": 300},
  "periodic": [{"name": "sync", "schedule": "@hourly", "command": ["true"], "timeoutSeconds": 300}]
}`))
	require.NoError(t, err)
}

func TestLoadConfigRejectsInvalidPersistedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.toml")
	require.NoError(t, os.WriteFile(path, []byte("not valid TOML ="), 0o600))
	t.Setenv(ConfigPathEnv, path)
	t.Setenv(ConfigEnv, "")

	cfg, err := LoadConfig()
	require.ErrorContains(t, err, "invalid persisted lifecycle config")
	assert.Nil(t, cfg)
}

func TestLoadConfigReturnsNilWhenNotConfigured(t *testing.T) {
	t.Setenv(ConfigPathEnv, filepath.Join(t.TempDir(), "lifecycle.toml"))
	t.Setenv(ConfigEnv, "")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg)
	_, statErr := os.Stat(os.Getenv(ConfigPathEnv))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestLoadConfigWithoutHome(t *testing.T) {
	t.Setenv(ConfigPathEnv, "")
	t.Setenv(ConfigEnv, "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.Nil(t, cfg)

	t.Setenv(ConfigEnv, `{"preStart":{"command":["true"]}}`)
	cfg, err = LoadConfig()

	assert.Nil(t, cfg)
	require.ErrorContains(t, err, "resolve lifecycle config home directory")
}
