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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

type Result struct {
	ExitCode   int
	Duration   time.Duration
	TimedOut   bool
	Incomplete bool
	Err        error
}

func RunHook(parent context.Context, hook Hook) Result {
	if len(hook.Command) == 0 {
		return Result{ExitCode: -1, Err: errors.New("hook command must not be empty")}
	}
	ctx, cancel := context.WithTimeout(parent, hook.timeout())
	defer cancel()

	cmd := exec.Command(hook.Command[0], hook.Command[1:]...)
	cmd.Env = sanitizedHookEnvironment()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	prepareCommand(cmd)

	started := time.Now()
	exitCode, err := runtime.RunManagedCommand(ctx, cmd, func() { terminateCommand(cmd) })

	result := Result{ExitCode: exitCode, Duration: time.Since(started), Err: err}
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		result.TimedOut = true
	}
	result.Incomplete = errors.Is(err, runtime.ErrManagedCommandCancelTimeout)
	return result
}

func sanitizedHookEnvironment() []string {
	blocked := make(map[string]struct{})
	for _, name := range isolation.ExecdConfigEnvBlacklist() {
		blocked[strings.ToUpper(name)] = struct{}{}
	}

	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, found := blocked[strings.ToUpper(name)]; found {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func RunPreStart(ctx context.Context, cfg *Config) error {
	if cfg == nil || cfg.PreStart == nil {
		return nil
	}
	result := RunHook(ctx, *cfg.PreStart)
	if result.TimedOut {
		return fmt.Errorf("command %v timed out after %s: %w", cfg.PreStart.Command, cfg.PreStart.timeout(), result.Err)
	}
	if result.Err != nil {
		if result.ExitCode >= 0 {
			return fmt.Errorf("command %v failed with exit code %d: %w", cfg.PreStart.Command, result.ExitCode, result.Err)
		}
		return fmt.Errorf("command %v failed: %w", cfg.PreStart.Command, result.Err)
	}
	return nil
}
