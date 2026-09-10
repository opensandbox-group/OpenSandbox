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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

const goosWindows = "windows"

func (r *ExecuteCodeRequest) commandContent() string {
	if r.Argv == nil {
		return r.Code
	}
	data, _ := json.Marshal(r.Argv)
	return string(data)
}

// ValidateCommandWorkingDir validates cwd with file and request environment overrides.
func ValidateCommandWorkingDir(cwd string, envs map[string]string) error {
	if cwd == "" {
		return nil
	}
	return ValidateWorkingDirWithEnv(cwd, mergeExtraEnvs(loadExtraEnvFromFile(), envs))
}

// prepareCommand shares cwd and environment between executable lookup and startup.
func prepareCommand(ctx context.Context, request *ExecuteCodeRequest) (*exec.Cmd, error) {
	overrides := mergeExtraEnvs(loadExtraEnvFromFile(), request.Envs)
	cwd, err := pathutil.ExpandPathWithEnv(request.Cwd, overrides)
	if err != nil {
		return nil, err
	}
	// Derive PWD from cwd before applying explicit overrides.
	env := mergeEnvs((&exec.Cmd{Dir: cwd}).Environ(), overrides)
	var cmd *exec.Cmd
	if request.Argv != nil {
		cmd = nativeCommand(ctx, request.Argv, cwd, env)
	} else {
		cmd = newShellCommand(ctx, request.Code)
	}
	cmd.Dir = cwd
	cmd.Env = env
	return cmd, nil
}

// nativeCommand preserves lookup failures as command start errors.
func nativeCommand(ctx context.Context, argv []string, cwd string, env []string) *exec.Cmd {
	if len(argv) == 0 || argv[0] == "" {
		cmd := exec.CommandContext(ctx, "")
		cmd.Err = fmt.Errorf("argv requires a non-empty executable")
		return cmd
	}
	name, err := resolveExecutable(argv[0], cwd, env)
	cmd := exec.CommandContext(ctx, name, argv[1:]...)
	cmd.Args = append([]string(nil), argv...)
	cmd.Err = err
	return cmd
}

func resolveExecutable(name, cwd string, env []string) (string, error) {
	original := name
	if runtime.GOOS == goosWindows {
		ext := filepath.Ext(name)
		if strings.EqualFold(ext, ".bat") || strings.EqualFold(ext, ".cmd") {
			return name, fmt.Errorf("native argv requires an executable, not a batch file")
		}
		if ext == "" {
			name += ".exe"
		}
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	if runtime.GOOS == goosWindows {
		if filepath.VolumeName(name) != "" {
			return name, fmt.Errorf("drive-relative executable paths require an absolute path")
		}
		if strings.HasPrefix(name, `\`) || strings.HasPrefix(name, "/") {
			dir, err := filepath.Abs(cwd)
			return filepath.VolumeName(dir) + name, err
		}
	}
	separators := string(os.PathSeparator)
	if runtime.GOOS == goosWindows {
		separators += "/:"
	}
	if strings.ContainsAny(name, separators) {
		return filepath.Abs(filepath.Join(cwd, name))
	}
	path := ""
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (key == "PATH" || runtime.GOOS == goosWindows && strings.EqualFold(key, "PATH")) {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && (runtime.GOOS == goosWindows || info.Mode()&0111 != 0) {
			return candidate, nil
		}
	}
	return name, &exec.Error{Name: original, Err: exec.ErrNotFound}
}
