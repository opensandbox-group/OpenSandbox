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

//go:build !windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
)

func TestRunCommand_DoesNotCreateTempFiles(t *testing.T) {
	requireBash(t)

	missingTmp := filepath.Join(t.TempDir(), "nonexistent-tmp")
	t.Setenv("TMPDIR", missingTmp)

	c := NewController("", "")
	var (
		mu          sync.Mutex
		stdoutParts []string
		stderrParts []string
		session     string
	)
	req := &ExecuteCodeRequest{
		Code: `printf 'out-line\nout-tail'; printf 'err-line\nerr-tail' >&2`,
		Cwd:  t.TempDir(),
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(id string) { session = id },
			OnExecuteStdout: func(s string) {
				mu.Lock()
				stdoutParts = append(stdoutParts, s)
				mu.Unlock()
			},
			OnExecuteStderr: func(s string) {
				mu.Lock()
				stderrParts = append(stderrParts, s)
				mu.Unlock()
			},
			OnExecuteError: func(err *execute.ErrorOutput) {
				require.Failf(t, "unexpected error hook", "%+v", err)
			},
			OnExecuteComplete: func(time.Duration) {},
		},
	}

	require.NoError(t, c.runCommand(context.Background(), req))
	require.NotEmpty(t, session)
	require.Equal(t, "out-line\nout-tail", strings.Join(stdoutParts, ""))
	require.Equal(t, "err-line\nerr-tail", strings.Join(stderrParts, ""))
	_, err := os.Stat(missingTmp)
	require.True(t, os.IsNotExist(err), "foreground runCommand must not touch TMPDIR")
}

func TestRunCommand_OrphanedChildHoldingPipeDoesNotHang(t *testing.T) {
	requireBash(t)

	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout strings.Builder
	req := &ExecuteCodeRequest{
		// Deliberately leave stdout/stderr inherited by the background sleep
		// child so the pipe write end stays open after the leader exits.
		Code: `sleep 30 & echo $! > "` + pidFile + `"; echo done`,
		Cwd:  t.TempDir(),
		Hooks: ExecuteResultHook{
			OnExecuteInit:   func(string) {},
			OnExecuteStdout: func(s string) { stdout.WriteString(s) },
			OnExecuteStderr: func(string) {},
			OnExecuteError: func(err *execute.ErrorOutput) {
				require.Failf(t, "unexpected error hook", "%+v", err)
			},
			OnExecuteComplete: func(time.Duration) {},
		},
	}

	start := time.Now()
	require.NoError(t, c.runCommand(ctx, req))
	require.Less(t, time.Since(start), 2*time.Second, "orphaned child holding pipe must not hang runCommand")
	require.Equal(t, "done\n", stdout.String())
}

func TestRunCommand_LargeOutputWithSlowCallbackDrainsCompletely(t *testing.T) {
	requireBash(t)

	const lineCount = 4000
	const lineBody = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // 64 bytes
	want := strings.Repeat(lineBody+"\n", lineCount) + "final-fragment"

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		got        strings.Builder
		slowedOnce bool
	)
	req := &ExecuteCodeRequest{
		Code: `awk 'BEGIN { for (i = 0; i < 4000; i++) printf "%s\n", "` + lineBody + `"; printf "final-fragment" }'`,
		Cwd:  t.TempDir(),
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(string) {},
			OnExecuteStdout: func(s string) {
				if !slowedOnce {
					slowedOnce = true
					// Exceed commandPipeDrainGrace to verify slow consumers still drain to EOF.
					time.Sleep(commandPipeDrainGrace + 30*time.Millisecond)
				}
				got.WriteString(s)
			},
			OnExecuteStderr: func(string) {},
			OnExecuteError: func(err *execute.ErrorOutput) {
				require.Failf(t, "unexpected error hook", "%+v", err)
			},
			OnExecuteComplete: func(time.Duration) {},
		},
	}

	require.NoError(t, c.runCommand(ctx, req))
	require.Equal(t, want, got.String())
}
