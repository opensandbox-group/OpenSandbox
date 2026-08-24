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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/robfig/cron/v3"
)

const (
	ConfigEnv     = "OPENSANDBOX_LIFECYCLE"
	ConfigPathEnv = "EXECD_LIFECYCLE_CONFIG"

	defaultTimeout        = 60 * time.Second
	maxHookTimeoutSeconds = 300
	configVersion         = 1
)

// Config is the creation-time sandbox lifecycle configuration consumed by
// bootstrap.sh and execd. JSON is used for environment transport while the
// persisted in-sandbox representation is TOML.
type Config struct {
	Version  int            `json:"version" toml:"version"`
	PreStart *Hook          `json:"preStart,omitempty" toml:"preStart,omitempty"`
	Periodic []PeriodicHook `json:"periodic,omitempty" toml:"periodic,omitempty"`
}

type Hook struct {
	Command        []string `json:"command" toml:"command"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty" toml:"timeout_seconds,omitempty"`
}

type PeriodicHook struct {
	Name           string   `json:"name" toml:"name"`
	Schedule       string   `json:"schedule" toml:"schedule"`
	Command        []string `json:"command" toml:"command"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty" toml:"timeout_seconds,omitempty"`
}

func (h Hook) timeout() time.Duration {
	if h.TimeoutSeconds == 0 {
		return defaultTimeout
	}
	return time.Duration(h.TimeoutSeconds) * time.Second
}

// PreStartTimeout returns the effective timeout for the configured preStart
// hook, or zero when no preStart hook is configured.
func (c *Config) PreStartTimeout() time.Duration {
	if c == nil || c.PreStart == nil {
		return 0
	}
	return c.PreStart.timeout()
}

func (h PeriodicHook) hook() Hook {
	return Hook{Command: h.Command, TimeoutSeconds: h.TimeoutSeconds}
}

// LoadConfig prefers and atomically persists the injected environment config.
// When the transport is absent, it reads the persisted config instead.
func LoadConfig() (*Config, error) {
	raw := strings.TrimSpace(os.Getenv(ConfigEnv))
	if raw != "" {
		cfg, err := decodeConfig([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", ConfigEnv, err)
		}
		path, err := resolveConfigPath()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create lifecycle config directory: %w", err)
		}
		if err := persistConfig(path, cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	path, err := resolveConfigPath()
	if err != nil {
		return nil, nil //nolint:nilerr,nilnil // no transport and no home means hooks are optional
	}
	if raw, err := os.ReadFile(path); err == nil {
		cfg, decodeErr := decodeConfig(raw)
		if decodeErr != nil {
			return nil, fmt.Errorf("invalid persisted lifecycle config %s: %w", path, decodeErr)
		}
		return cfg, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read lifecycle config %s: %w", path, err)
	}

	return nil, nil //nolint:nilnil // lifecycle hooks are optional
}

func decodeConfig(raw []byte) (*Config, error) {
	var cfg Config
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty lifecycle config")
	}
	var err error
	if trimmed[0] == '{' {
		err = json.Unmarshal(trimmed, &cfg)
	} else {
		err = toml.Unmarshal(trimmed, &cfg)
	}
	if err != nil {
		return nil, err
	}
	if cfg.Version == 0 {
		cfg.Version = configVersion
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Version != configVersion {
		return fmt.Errorf("unsupported lifecycle config version %d", c.Version)
	}
	if c.PreStart != nil {
		if err := validateHook("preStart", *c.PreStart); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(c.Periodic))
	for index := range c.Periodic {
		periodic := &c.Periodic[index]
		periodic.Name = strings.TrimSpace(periodic.Name)
		periodic.Schedule = strings.TrimSpace(periodic.Schedule)
		if periodic.Name == "" {
			return errors.New("periodic hook name must not be blank")
		}
		if _, ok := seen[periodic.Name]; ok {
			return fmt.Errorf("duplicate periodic hook name %q", periodic.Name)
		}
		seen[periodic.Name] = struct{}{}
		if periodic.Schedule == "" {
			return fmt.Errorf("periodic hook %q schedule must not be blank", periodic.Name)
		}
		schedule := periodic.Schedule
		descriptor := schedule
		if strings.HasPrefix(descriptor, "TZ=") || strings.HasPrefix(descriptor, "CRON_TZ=") {
			space := strings.IndexByte(descriptor, ' ')
			if space < 0 {
				return fmt.Errorf("periodic hook %q has invalid schedule", periodic.Name)
			}
			descriptor = strings.TrimSpace(descriptor[space+1:])
		}
		if _, err := cron.ParseStandard(schedule); err != nil {
			return fmt.Errorf("periodic hook %q has invalid schedule: %w", periodic.Name, err)
		}
		if strings.HasPrefix(descriptor, "@every ") {
			interval, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(descriptor, "@every ")))
			if err != nil || interval < time.Second || interval%time.Second != 0 {
				return fmt.Errorf("periodic hook %q @every interval must be a whole number of seconds", periodic.Name)
			}
		}
		if err := validateHook("periodic "+periodic.Name, periodic.hook()); err != nil {
			return err
		}
	}
	return nil
}

func validateHook(name string, hook Hook) error {
	if len(hook.Command) == 0 || strings.TrimSpace(hook.Command[0]) == "" {
		return fmt.Errorf("%s command must not be empty", name)
	}
	if hook.TimeoutSeconds < 0 {
		return fmt.Errorf("%s timeoutSeconds must not be negative", name)
	}
	if hook.TimeoutSeconds > maxHookTimeoutSeconds {
		return fmt.Errorf("%s timeoutSeconds must not exceed %d", name, maxHookTimeoutSeconds)
	}
	return nil
}

func resolveConfigPath() (string, error) {
	if configuredPath := os.Getenv(ConfigPathEnv); configuredPath != "" {
		return configuredPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve lifecycle config home directory: %w", err)
	}
	return filepath.Join(home, ".execd", "lifecycle.toml"), nil
}

func persistConfig(path string, cfg *Config) error {
	raw, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode lifecycle TOML: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lifecycle-*.tmp")
	if err != nil {
		return fmt.Errorf("create lifecycle config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod lifecycle config temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write lifecycle config temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync lifecycle config temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close lifecycle config temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace lifecycle config: %w", err)
	}
	if runtime.GOOS != "windows" {
		// The rename already made the fsynced contents live. Directory sync is
		// best-effort because some container filesystems reject it with EINVAL.
		if dir, err := os.Open(filepath.Dir(path)); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}
