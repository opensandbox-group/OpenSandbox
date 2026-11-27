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

//go:build windows
// +build windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

// runWindowsShellCommand returns stdout as one string because the output tail
// emits a CR and the following LF as separate events.
func runWindowsShellCommand(t *testing.T, code string) (string, *execute.ErrorOutput) {
	t.Helper()

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		stdout strings.Builder
		gotErr *execute.ErrorOutput
	)
	req := &ExecuteCodeRequest{
		Code:    code,
		Cwd:     t.TempDir(),
		Timeout: 10 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(string) {},
			OnExecuteStdout:   func(s string) { stdout.WriteString(s) },
			OnExecuteStderr:   func(string) {},
			OnExecuteError:    func(err *execute.ErrorOutput) { gotErr = err },
			OnExecuteComplete: func(time.Duration) {},
		},
	}
	require.NoError(t, c.runCommand(ctx, req))
	return stdout.String(), gotErr
}

func TestRunCommand_WindowsPassesQuotesToCmdUnchanged(t *testing.T) {
	stdout, gotErr := runWindowsShellCommand(t, `echo "hello world"`)

	require.Nil(t, gotErr)
	require.Equal(t, "\"hello world\"\r\n", stdout)
}

// TestRunCommand_WindowsBackgroundPassesQuotesToCmdUnchanged covers the API
// shape from issue #3143: {"command": "echo \"hi\"", "background": true}.
func TestRunCommand_WindowsBackgroundPassesQuotesToCmdUnchanged(t *testing.T) {
	c := NewController("", "")

	var session string
	req := &ExecuteCodeRequest{
		Language: BackgroundCommand,
		Code:     `echo "hi"`,
		Cwd:      t.TempDir(),
		Timeout:  10 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(s string) { session = s },
			OnExecuteStdout:   func(string) {},
			OnExecuteStderr:   func(string) {},
			OnExecuteError:    func(*execute.ErrorOutput) {},
			OnExecuteComplete: func(time.Duration) {},
		},
	}
	require.NoError(t, c.Execute(req))
	require.NotEmpty(t, session)

	require.Eventually(t, func() bool {
		status, err := c.GetCommandStatus(session)
		return err == nil && !status.Running
	}, 10*time.Second, 50*time.Millisecond)

	status, err := c.GetCommandStatus(session)
	require.NoError(t, err)
	require.NotNil(t, status.ExitCode)
	require.Equal(t, 0, *status.ExitCode)

	output, _, err := c.SeekBackgroundCommandOutput(session, 0)
	require.NoError(t, err)
	require.Equal(t, "\"hi\"\r\n", string(output))
}

func TestRunCommand_WindowsRunsQuotedExecutablePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with space")
	require.NoError(t, os.Mkdir(dir, 0o755))
	script := filepath.Join(dir, "hello.cmd")
	require.NoError(t, os.WriteFile(script, []byte("@echo quoted-ok %1\r\n"), 0o644))

	stdout, gotErr := runWindowsShellCommand(t, `"`+script+`" "arg one"`)

	require.Nil(t, gotErr)
	require.Equal(t, "quoted-ok \"arg one\"\r\n", stdout)
}
