// Copyright 2025 Alibaba Group Holding Ltd.
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	goruntime "runtime"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadFromPos_SplitsOnCRAndLF(t *testing.T) {
	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "stdout.log")

	mutex := &sync.Mutex{}

	initial := "line1\nprog 10%\rprog 20%\rprog 30%\nlast\n"
	require.NoError(t, os.WriteFile(logFile, []byte(initial), 0o644))

	var got []string
	c := &Controller{}
	nextPos := c.readFromPos(mutex, logFile, 0, func(s string) { got = append(got, s) }, false)

	want := []string{"line1", "prog 10%", "prog 20%", "prog 30%", "last"}
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i], got[i], "token[%d] mismatch", i)
	}

	// append more content and ensure incremental read only yields the new part
	appendPart := "tail1\r\ntail2\n"
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appendPart)
	require.NoError(t, err, "append write")
	_ = f.Close()

	got = got[:0]
	c.readFromPos(mutex, logFile, nextPos, func(s string) { got = append(got, s) }, false)
	want = []string{"tail1", "tail2"}
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i], got[i], "incremental token[%d] mismatch", i)
	}
}

func TestReadFromPos_LongLine(t *testing.T) {
	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "stdout.log")

	// construct a single line larger than the default 64KB, but under 5MB
	longLine := strings.Repeat("x", 256*1024) + "\n" // 256KB
	require.NoError(t, os.WriteFile(logFile, []byte(longLine), 0o644))

	var got []string
	c := &Controller{}
	c.readFromPos(&sync.Mutex{}, logFile, 0, func(s string) { got = append(got, s) }, false)

	require.Len(t, got, 1, "expected one token")
	require.Equal(t, strings.TrimSuffix(longLine, "\n"), got[0], "long line mismatch")
}

func TestReadFromPos_FlushesTrailingLine(t *testing.T) {
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "stdout.log")
	content := []byte("line1\nlastline-without-newline")
	err := os.WriteFile(file, content, 0o644)
	assert.NoError(t, err)

	c := NewController("", "")
	mutex := &sync.Mutex{}
	var lines []string
	onExecute := func(text string) {
		lines = append(lines, text)
	}

	// First read: should only get complete lines with newlines
	pos := c.readFromPos(mutex, file, 0, onExecute, false)
	assert.GreaterOrEqual(t, pos, int64(0))
	assert.Equal(t, []string{"line1"}, lines)

	// Flush at end: should output the last line (without newline)
	c.readFromPos(mutex, file, pos, onExecute, true)
	assert.Equal(t, []string{"line1", "lastline-without-newline"}, lines)
}

func TestRunCommand_Echo(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	c := NewController("", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		sessionID   string
		stdoutLines []string
		stderrLines []string
		completeCh  = make(chan struct{}, 1)
	)

	req := &ExecuteCodeRequest{
		Code:    `echo "hello"; echo "errline" 1>&2`,
		Cwd:     t.TempDir(),
		Timeout: 5 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(s string) { sessionID = s },
			OnExecuteStdout: func(_ int64, s string) {
				stdoutLines = append(stdoutLines, s)
			},
			OnExecuteStderr: func(_ int64, s string) {
				stderrLines = append(stderrLines, s)
			},
			OnExecuteError: func(err *execute.ErrorOutput) {
				require.Failf(t, "unexpected error hook", "%+v", err)
			},
			OnExecuteComplete: func(_ time.Duration) {
				completeCh <- struct{}{}
			},
		},
	}

	require.NoError(t, c.runCommand(ctx, req))

	select {
	case <-completeCh:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timeout waiting for completion hook")
	}

	require.NotEmpty(t, sessionID, "expected session id to be set")
	require.Equal(t, []string{"hello"}, stdoutLines)
	require.Equal(t, []string{"errline"}, stderrLines)
}

func TestRunCommand_Error(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	c := NewController("", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		sessionID   string
		gotErr      *execute.ErrorOutput
		completeCh  = make(chan struct{}, 2)
		stdoutLines []string
		stderrLines []string
	)

	req := &ExecuteCodeRequest{
		Code:    `echo "before"; exit 3`,
		Cwd:     t.TempDir(),
		Timeout: 5 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit:   func(s string) { sessionID = s },
			OnExecuteStdout: func(_ int64, s string) { stdoutLines = append(stdoutLines, s) },
			OnExecuteStderr: func(_ int64, s string) { stderrLines = append(stderrLines, s) },
			OnExecuteError: func(err *execute.ErrorOutput) {
				gotErr = err
				completeCh <- struct{}{}
			},
			OnExecuteComplete: func(_ time.Duration) {
				completeCh <- struct{}{}
			},
		},
	}

	require.NoError(t, c.runCommand(ctx, req))

	select {
	case <-completeCh:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timeout waiting for completion hook")
	}

	require.NotEmpty(t, sessionID, "expected session id to be set")
	require.Equal(t, []string{"before"}, stdoutLines)
	require.Empty(t, stderrLines, "expected no stderr")
	require.NotNil(t, gotErr, "expected error hook to be called")
	require.Equal(t, "CommandExecError", gotErr.EName)
	require.Equal(t, "3", gotErr.EValue)
}

func TestRunCommand_ExpandsHomeInCwd(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	home := t.TempDir()
	target := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(target, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdoutLines []string
	req := &ExecuteCodeRequest{
		Code:    `pwd`,
		Cwd:     "~/workspace",
		Timeout: 5 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit:   func(_ string) {},
			OnExecuteStdout: func(_ int64, s string) { stdoutLines = append(stdoutLines, s) },
			OnExecuteStderr: func(_ int64, _ string) {},
			OnExecuteError: func(err *execute.ErrorOutput) {
				require.Failf(t, "unexpected error hook", "%+v", err)
			},
			OnExecuteComplete: func(_ time.Duration) {},
		},
	}

	require.NoError(t, c.runCommand(ctx, req))

	targetRealPath, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	targetRealPath = filepath.Clean(targetRealPath)

	found := false
	for _, line := range stdoutLines {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		pRealPath, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if filepath.Clean(pRealPath) == targetRealPath {
			found = true
			break
		}
	}
	require.True(t, found, "pwd output does not match expected cwd; got=%v target=%s", stdoutLines, target)
}

func TestRunCommand_ExpandsCwdFromRequestEnvWithHigherPriority(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	processDir := t.TempDir()
	requestDir := t.TempDir()
	t.Setenv("WORKDIR", processDir)

	c := NewController("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		stdoutLines []string
		gotErr      *execute.ErrorOutput
	)
	req := &ExecuteCodeRequest{
		Code:    `pwd`,
		Cwd:     `$WORKDIR`,
		Timeout: 5 * time.Second,
		Envs: map[string]string{
			"WORKDIR": requestDir,
		},
		Hooks: ExecuteResultHook{
			OnExecuteInit:   func(_ string) {},
			OnExecuteStdout: func(_ int64, s string) { stdoutLines = append(stdoutLines, s) },
			OnExecuteStderr: func(_ int64, _ string) {},
			OnExecuteError: func(err *execute.ErrorOutput) {
				gotErr = err
			},
			OnExecuteComplete: func(_ time.Duration) {},
		},
	}

	require.NoError(t, c.runCommand(ctx, req))
	require.Nil(t, gotErr, "expected cwd expansion to use request env")

	requestRealPath, err := filepath.EvalSymlinks(requestDir)
	require.NoError(t, err)
	requestRealPath = filepath.Clean(requestRealPath)

	found := false
	for _, line := range stdoutLines {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		pRealPath, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if filepath.Clean(pRealPath) == requestRealPath {
			found = true
			break
		}
	}
	require.True(t, found, "pwd output does not match request env cwd; got=%v requestDir=%s", stdoutLines, requestDir)
}

func TestRunCommand_StartErrorIncludesTraceback(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	c := NewController("", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		sessionID      string
		gotErr         *execute.ErrorOutput
		completeCalled bool
	)

	req := &ExecuteCodeRequest{
		Code:    `echo "hello"`,
		Cwd:     filepath.Join(t.TempDir(), "missing"),
		Timeout: 5 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit: func(s string) { sessionID = s },
			OnExecuteError: func(err *execute.ErrorOutput) {
				gotErr = err
			},
			OnExecuteComplete: func(_ time.Duration) {
				completeCalled = true
			},
		},
	}

	require.NoError(t, c.runCommand(ctx, req))

	require.NotEmpty(t, sessionID, "expected session id to be set")
	require.NotNil(t, gotErr, "expected error hook to be called")
	require.Equal(t, "CommandExecError", gotErr.EName)
	require.NotEmpty(t, gotErr.Traceback, "expected traceback to be populated")
	require.Equal(t, gotErr.EValue, gotErr.Traceback[0])
	require.False(t, completeCalled, "did not expect completion hook on start failure")
}

// TestStdLogDescriptor_AutoCreatesTempDir verifies that stdLogDescriptor
// recreates the temp directory when it has been deleted, rather than failing.
// Regression test for https://github.com/alibaba/OpenSandbox/issues/400.
func TestStdLogDescriptor_AutoCreatesTempDir(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("TMPDIR env var has no effect on Windows")
	}

	// Point os.TempDir() at a path that does not yet exist.
	missingDir := filepath.Join(t.TempDir(), "deleted_tmp")
	t.Setenv("TMPDIR", missingDir)

	c := NewController("", "")
	stdout, stderr, err := c.stdLogDescriptor("test-session")
	require.NoError(t, err)
	stdout.Close()
	stderr.Close()

	// The directory must have been created.
	info, err := os.Stat(missingDir)
	require.NoError(t, err, "expected temp dir to be created, stat error")
	require.True(t, info.IsDir(), "expected %s to be a directory", missingDir)
}

// TestCombinedOutputDescriptor_AutoCreatesTempDir verifies that
// combinedOutputDescriptor also recreates the temp directory when missing.
// Regression test for https://github.com/alibaba/OpenSandbox/issues/400.
func TestCombinedOutputDescriptor_AutoCreatesTempDir(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("TMPDIR env var has no effect on Windows")
	}

	missingDir := filepath.Join(t.TempDir(), "deleted_tmp")
	t.Setenv("TMPDIR", missingDir)

	c := NewController("", "")
	f, err := c.combinedOutputDescriptor("test-session")
	require.NoError(t, err)
	f.Close()

	info, err := os.Stat(missingDir)
	require.NoError(t, err, "expected temp dir to be created, stat error")
	require.True(t, info.IsDir(), "expected %s to be a directory", missingDir)
}
