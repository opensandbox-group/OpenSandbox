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
	"fmt"
	"os"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

// loadExtraEnvFromFile reads key=value lines from EXECD_ENVS (if set).
// Empty lines and lines starting with '#' are ignored.
func loadExtraEnvFromFile() map[string]string {
	path := os.Getenv("EXECD_ENVS")
	if path == "" {
		return nil
	}
	resolvedPath, err := pathutil.ExpandPath(path)
	if err != nil {
		log.Warn("EXECD_ENVS: failed to resolve file path %s: %v", path, err)
		return nil
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		log.Warn("EXECD_ENVS: failed to read file %s: %v", resolvedPath, err)
		return nil
	}

	envs := make(map[string]string)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			log.Warn("EXECD_ENVS: skip malformed line: %s", line)
			continue
		}
		envs[kv[0]] = os.ExpandEnv(kv[1])
	}

	return envs
}

// mergeEnvs overlays extra into base and returns a merged slice.
func mergeEnvs(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}

	merged := make(map[string]string, len(base)+len(extra))
	for _, kv := range base {
		pair := strings.SplitN(kv, "=", 2)
		if len(pair) == 2 {
			merged[pair[0]] = pair[1]
		}
	}

	for k, v := range extra {
		merged[k] = v
	}

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}

	return out
}

// mergeExtraEnvs merges environment maps from file and request-level overrides.
func mergeExtraEnvs(fromFile, fromRequest map[string]string) map[string]string {
	if len(fromRequest) == 0 {
		return fromFile
	}

	merged := make(map[string]string, len(fromFile)+len(fromRequest))
	for k, v := range fromFile {
		merged[k] = v
	}
	for k, v := range fromRequest {
		merged[k] = v
	}

	return merged
}
