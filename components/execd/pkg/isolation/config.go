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

package isolation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// Config holds all isolation-related settings, loaded from a TOML file.
// Missing fields fall back to DefaultConfig values.
type Config struct {
	UpperRoot       string   `toml:"upper_root"`
	UpperMaxBytes   int64    `toml:"upper_max_bytes"`
	DiffMaxBytes    int64    `toml:"diff_max_bytes"`
	AllowedWritable []string `toml:"allowed_writable"`

	// Seccomp overrides the built-in syscall denylist. When nil (i.e. the
	// [seccomp] section is absent), the built-in denylist is used. When
	// present, Deny completely replaces the built-in list — no merging.
	Seccomp *SeccompOverride `toml:"seccomp"`
}

// SeccompOverride specifies a custom syscall denylist that replaces the
// built-in default when present.
type SeccompOverride struct {
	Deny []string `toml:"deny"`
}

// DefaultConfig returns the built-in defaults used when no config file is
// provided or when individual fields are missing from the file.
func DefaultConfig() Config {
	return Config{
		UpperRoot:       "/var/lib/execd/isolation",
		UpperMaxBytes:   8 * 1024 * 1024 * 1024, // 8 GiB
		DiffMaxBytes:    4 * 1024 * 1024 * 1024, // 4 GiB
		AllowedWritable: []string{"/workspace", "/mnt", "/media", "/data"},
		Seccomp:         nil, // use built-in denylist
	}
}

// LoadConfig reads isolation configuration from a TOML file at path.
//
//   - Empty path or file-not-found → DefaultConfig(), nil.
//   - Existing file with invalid TOML → error.
//   - Existing file → parsed values override defaults; missing fields keep
//     their default values.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("isolation config: read %s: %w", path, err)
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("isolation config: parse %s: %w", path, err)
	}

	return cfg, nil
}
