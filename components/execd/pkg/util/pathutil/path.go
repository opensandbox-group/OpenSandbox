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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func envMapFromProcessAndOverrides(envOverrides map[string]string) map[string]string {
	out := make(map[string]string, len(envOverrides)+16)
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	for k, v := range envOverrides {
		out[k] = v
	}
	return out
}

func validateEnvVars(path string, env map[string]string) error {
	matches := envVarPattern.FindAllStringSubmatch(path, -1)
	if len(matches) == 0 {
		return nil
	}

	missingSet := make(map[string]struct{})
	for _, m := range matches {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if _, ok := env[name]; !ok {
			missingSet[name] = struct{}{}
		}
	}
	if len(missingSet) == 0 {
		return nil
	}

	missing := make([]string, 0, len(missingSet))
	for name := range missingSet {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return fmt.Errorf("path references undefined environment variables: %s", strings.Join(missing, ","))
}

// ExpandPathWithEnv expands environment variables and a leading "~" to user home.
// Environment resolution uses process env overlaid by envOverrides.
func ExpandPathWithEnv(path string, envOverrides map[string]string) (string, error) {
	if path == "" {
		return "", nil
	}
	env := envMapFromProcessAndOverrides(envOverrides)
	if err := validateEnvVars(path, env); err != nil {
		return "", err
	}

	expanded := os.Expand(path, func(key string) string {
		return env[key]
	})
	if expanded == "~" || strings.HasPrefix(expanded, "~/") || strings.HasPrefix(expanded, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if expanded == "~" {
			return home, nil
		}
		return filepath.Join(home, expanded[2:]), nil
	}

	return expanded, nil
}

// ExpandPath expands environment variables and a leading "~" to user home.
// It supports "~", "~/" and "~\" prefixes.
func ExpandPath(path string) (string, error) {
	return ExpandPathWithEnv(path, nil)
}

func ExpandAbsPath(path string) (string, error) {
	expanded, err := ExpandPathWithEnv(path, nil)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}
