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

package isolation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, "/var/lib/execd/isolation", cfg.UpperRoot)
	assert.Equal(t, int64(8*1024*1024*1024), cfg.UpperMaxBytes)
	assert.Equal(t, int64(4*1024*1024*1024), cfg.DiffMaxBytes)
	assert.Equal(t, []string{"/workspace", "/mnt", "/media", "/data"}, cfg.AllowedWritable)
	assert.Nil(t, cfg.Seccomp)
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	cfg, err := LoadConfig("")
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}

func TestLoadConfig_FileNotExist(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/path/isolation.toml")
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}

func TestLoadConfig_Valid(t *testing.T) {
	content := `
upper_root = "/data/isolation"
upper_max_bytes = 1073741824
diff_max_bytes = 536870912
allowed_writable = ["/tmp", "/var/run"]

[seccomp]
deny = ["mount", "ptrace"]
`
	path := writeTempTOML(t, content)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "/data/isolation", cfg.UpperRoot)
	assert.Equal(t, int64(1073741824), cfg.UpperMaxBytes)
	assert.Equal(t, int64(536870912), cfg.DiffMaxBytes)
	assert.Equal(t, []string{"/tmp", "/var/run"}, cfg.AllowedWritable)
	require.NotNil(t, cfg.Seccomp)
	assert.Equal(t, []string{"mount", "ptrace"}, cfg.Seccomp.Deny)
}

func TestLoadConfig_InvalidTOML(t *testing.T) {
	path := writeTempTOML(t, `upper_root = [invalid`)

	_, err := LoadConfig(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestLoadConfig_PartialFields(t *testing.T) {
	content := `upper_root = "/custom/path"`
	path := writeTempTOML(t, content)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "/custom/path", cfg.UpperRoot)
	// Missing fields keep defaults.
	assert.Equal(t, int64(8*1024*1024*1024), cfg.UpperMaxBytes)
	assert.Equal(t, int64(4*1024*1024*1024), cfg.DiffMaxBytes)
	// Missing allowed_writable keeps the built-in default allowlist.
	assert.Equal(t, []string{"/workspace", "/mnt", "/media", "/data"}, cfg.AllowedWritable)
	assert.Nil(t, cfg.Seccomp)
}

func TestLoadConfig_SeccompReplace(t *testing.T) {
	content := `
[seccomp]
deny = ["socket", "connect"]
`
	path := writeTempTOML(t, content)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Seccomp)
	assert.Equal(t, []string{"socket", "connect"}, cfg.Seccomp.Deny)
}

func TestLoadConfig_NoSeccomp(t *testing.T) {
	content := `upper_root = "/data/iso"`
	path := writeTempTOML(t, content)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Nil(t, cfg.Seccomp, "absent [seccomp] section should leave Seccomp nil")
}

func TestLoadConfig_EmptySeccompDeny(t *testing.T) {
	content := `
[seccomp]
deny = []
`
	path := writeTempTOML(t, content)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Seccomp, "[seccomp] present → non-nil even if deny is empty")
	assert.Empty(t, cfg.Seccomp.Deny)
}

func TestLoadConfigQoS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    QoSConfig
	}{
		{"absent", "", QoSConfig{false, "standard", 19, "other", 500}},
		{"empty", "[qos]", QoSConfig{false, "standard", 19, "other", 500}},
		{"enabled", "[qos]\nenabled = true", QoSConfig{true, "standard", 19, "other", 500}},
		{"disabled", "[qos]\nenabled = false", QoSConfig{false, "standard", 19, "other", 500}},
		{"no implicit enable", "[qos]\nentrypoint = 'high'", QoSConfig{false, "high", 19, "other", 500}},
		{"zero disables OOM", "[qos]\nenabled = true\noom_score_adj = 0", QoSConfig{true, "standard", 19, "other", 0}},
		{"custom", "[qos]\nenabled = true\nentrypoint = 'high'\nnice = 1\nscheduler = 'idle'\noom_score_adj = 1000", QoSConfig{true, "high", 1, "idle", 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeTempTOML(t, tc.content))
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.QoS)
			assert.Nil(t, cfg.Hardening)
		})
	}
}

func TestLoadConfigQoSInvalid(t *testing.T) {
	for _, value := range []string{
		"entrypoint = 'urgent'", "entrypoint = ''", "entrypoint = 1",
		"scheduler = 'fifo'", "scheduler = ''", "scheduler = true",
		"nice = 0", "nice = -1", "nice = 20", "nice = 1.5", "nice = '19'",
		"oom_score_adj = -1", "oom_score_adj = 1001", "oom_score_adj = '500'",
		"enabled = 'true'",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadConfig(writeTempTOML(t, "[qos]\n"+value))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "isolation config:")
		})
	}
}

func writeTempTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "isolation.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
