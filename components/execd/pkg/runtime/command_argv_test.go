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
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/require"
)

// The test binary serves as a portable native executable.
func TestArgvHelper(t *testing.T) {
	if os.Getenv("EXECD_ARGV_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			cwd, _ := os.Getwd()
			_ = json.NewEncoder(os.Stdout).Encode([]any{os.Args[i+1:], cwd, os.Getenv("LITERAL")})
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestRunNativeCommandForegroundAndBackground(t *testing.T) {
	t.Setenv("PATH", "") // Do not rely on the daemon PATH.
	t.Setenv("LITERAL", "inherited")
	executable, err := os.Executable()
	require.NoError(t, err)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	envFile := filepath.Join(t.TempDir(), "envs")
	require.NoError(t, os.WriteFile(envFile, []byte("ARGV_DIR="+cwd+"\nPATH=/missing\nLITERAL=file\n"), 0600))
	t.Setenv("EXECD_ENVS", envFile)
	args := []string{"", "a b", "$HOME", "%PATH%", "~", "*", "x'y", `a"b`, `back\slash\`, "中文", "; echo injected"}
	expected, err := json.Marshal([]any{args, cwd, "$HOME"})
	require.NoError(t, err)

	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprint(background), func(t *testing.T) {
			c := NewController("", "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var id, output string
			var executionError *execute.ErrorOutput
			req := &ExecuteCodeRequest{
				Argv: append([]string{filepath.Base(executable), "-test.run=^TestArgvHelper$", "--"}, args...),
				Cwd:  "$ARGV_DIR",
				Envs: map[string]string{"EXECD_ARGV_HELPER": "1", "PATH": filepath.Dir(executable), "LITERAL": "$HOME"},
				Hooks: ExecuteResultHook{
					OnExecuteInit:   func(s string) { id = s },
					OnExecuteStdout: func(s string) { output += s },
					OnExecuteError:  func(e *execute.ErrorOutput) { executionError = e },
				},
			}
			if background {
				req.Argv[0] = executable
			}
			if runtime.GOOS == "windows" {
				req.Cwd = "$argv_dir"
			}
			req.SetDefaultHooks()
			if background {
				err = c.runBackgroundCommand(ctx, cancel, req)
			} else {
				err = c.runCommand(ctx, req)
			}
			require.NoError(t, err)
			require.Nil(t, executionError)
			require.Eventually(t, func() bool {
				status, err := c.GetCommandStatus(id)
				return err == nil && !status.Running
			}, 5*time.Second, 10*time.Millisecond)
			status, err := c.GetCommandStatus(id)
			require.NoError(t, err)
			require.Equal(t, 0, *status.ExitCode)
			require.Equal(t, req.commandContent(), status.Content)
			if background {
				data, _, err := c.SeekBackgroundCommandOutput(id, 0)
				require.NoError(t, err)
				output = string(data)
			}
			require.JSONEq(t, string(expected), output)

			cmd, err := prepareCommand(ctx, req)
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				require.Contains(t, cmd.Env, "PWD="+cwd)
			}
			req.Envs["PWD"] = "explicit"
			cmd, err = prepareCommand(ctx, req)
			require.NoError(t, err)
			require.Contains(t, cmd.Env, "PWD=explicit")
		})
	}
}

func TestNativeCommandResolution(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	dir, name := filepath.Dir(executable), filepath.Base(executable)
	path, err := resolveExecutable("."+string(os.PathSeparator)+name, dir, nil)
	require.NoError(t, err)
	require.Equal(t, executable, path)
	_, err = resolveExecutable(name, dir, []string{"PATH=."})
	require.ErrorIs(t, err, exec.ErrNotFound)
	if runtime.GOOS == "windows" {
		_, err = resolveExecutable("script.cmd", dir, nil)
		require.Error(t, err)
	}
	req := &ExecuteCodeRequest{Argv: []string{"opensandbox-nonexistent-command"}, Envs: map[string]string{"PATH": ""}}
	cmd, err := prepareCommand(context.Background(), req)
	require.NoError(t, err)
	require.ErrorIs(t, cmd.Err, exec.ErrNotFound)
	req.Cwd = "$ARGV_UNDEFINED_CWD"
	cmd, err = prepareCommand(context.Background(), req)
	require.Error(t, err)
	require.Nil(t, cmd)
}
