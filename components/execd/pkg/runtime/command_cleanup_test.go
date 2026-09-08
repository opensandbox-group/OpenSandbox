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
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunCommandRemovesForegroundOutputFiles(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("shell command and TMPDIR behavior differ on Windows")
	}

	t.Setenv("TMPDIR", t.TempDir())
	c := NewController("", "")
	var session string
	req := &ExecuteCodeRequest{
		Code: "printf cleanup-proof",
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(id string) { session = id },
			OnExecuteComplete: func(time.Duration) {},
		},
	}

	require.NoError(t, c.runCommand(context.Background(), req))
	require.NotEmpty(t, session)
	require.NoFileExists(t, c.stdoutFileName(session))
	require.NoFileExists(t, c.stderrFileName(session))
	status, err := c.GetCommandStatus(session)
	require.NoError(t, err)
	require.False(t, status.Running)
}

func TestCommandOutputDirRejectsSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	target := t.TempDir()
	c := NewController("", "")
	require.NoError(t, os.Symlink(target, c.commandOutputDir()))

	_, err := c.combinedOutputDescriptor("test-session")
	require.Error(t, err)
	require.Contains(t, err.Error(), "without following symlinks")
	require.NoFileExists(t, filepath.Join(target, "test-session.output"))
}

func TestCommandOutputDirRejectsUnsafePermissions(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Unix permission validation does not apply on Windows")
	}

	t.Setenv("TMPDIR", t.TempDir())
	c := NewController("", "")
	require.NoError(t, os.Mkdir(c.commandOutputDir(), 0o777))
	require.NoError(t, os.Chmod(c.commandOutputDir(), 0o777))

	_, err := c.combinedOutputDescriptor("test-session")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsafe permissions")
}

func TestSeekBackgroundCommandOutputRejectsSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}

	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "secret")
	link := filepath.Join(tempDir, "command.output")
	require.NoError(t, os.WriteFile(target, []byte("must-not-leak"), 0o600))
	require.NoError(t, os.Symlink(target, link))
	c := NewController("", "")
	c.storeCommandKernel("session", &commandKernel{
		stdoutPath:   link,
		stderrPath:   link,
		isBackground: true,
	})

	output, _, err := c.SeekBackgroundCommandOutput("session", 0)
	require.Error(t, err)
	require.Empty(t, output)
}

func TestCleanupOrphanedCommandOutputsIsScopedAndAgeBounded(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("TMPDIR behavior differs on Windows")
	}

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	c := NewController("", "")
	now := time.Now()
	old := now.Add(-commandOutputRetention - time.Minute)
	recent := now.Add(-time.Minute)

	legacyOld := filepath.Join(tempDir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.stdout")
	legacyRecent := filepath.Join(tempDir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.stderr")
	unrelated := filepath.Join(tempDir, "notes.stdout")
	symlink := filepath.Join(tempDir, "cccccccccccccccccccccccccccccccc.stdout")
	require.NoError(t, os.WriteFile(legacyOld, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(legacyRecent, []byte("recent"), 0o600))
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(unrelated, symlink))
	require.NoError(t, os.Chtimes(legacyOld, old, old))
	require.NoError(t, os.Chtimes(legacyRecent, recent, recent))

	require.NoError(t, os.MkdirAll(c.commandOutputDir(), 0o700))
	orphan := filepath.Join(c.commandOutputDir(), "dddddddddddddddddddddddddddddddd.output")
	running := filepath.Join(c.commandOutputDir(), "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.output")
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o600))
	require.NoError(t, os.WriteFile(running, []byte("running"), 0o600))
	require.NoError(t, os.Chtimes(orphan, old, old))
	require.NoError(t, os.Chtimes(running, old, old))
	c.storeCommandKernel("running", &commandKernel{
		stdoutPath:   running,
		stderrPath:   running,
		running:      true,
		isBackground: true,
	})

	c.cleanupOrphanedCommandOutputs(now)

	require.NoFileExists(t, legacyOld)
	require.FileExists(t, legacyRecent)
	require.FileExists(t, unrelated)
	_, err := os.Lstat(symlink)
	require.NoError(t, err)
	require.NoFileExists(t, orphan)
	require.FileExists(t, running)
}

func TestCleanupFinishedCommandsRetainsRunningAndRecentCommands(t *testing.T) {
	tempDir := t.TempDir()
	c := NewController("", "")
	now := time.Now()
	oldFinished := now.Add(-commandOutputRetention - time.Minute)
	recentFinished := now.Add(-time.Minute)

	oldPath := filepath.Join(tempDir, "old.output")
	recentPath := filepath.Join(tempDir, "recent.output")
	runningPath := filepath.Join(tempDir, "running.output")
	for _, path := range []string{oldPath, recentPath, runningPath} {
		require.NoError(t, os.WriteFile(path, []byte("output"), 0o600))
	}

	c.storeCommandKernel("old", &commandKernel{
		stdoutPath: oldPath,
		stderrPath: oldPath,
		finishedAt: &oldFinished,
	})
	c.storeCommandKernel("recent", &commandKernel{
		stdoutPath: recentPath,
		stderrPath: recentPath,
		finishedAt: &recentFinished,
	})
	c.storeCommandKernel("running", &commandKernel{
		stdoutPath: runningPath,
		stderrPath: runningPath,
		running:    true,
	})

	c.cleanupFinishedCommands(now.Add(-commandOutputRetention))

	require.NoFileExists(t, oldPath)
	require.FileExists(t, recentPath)
	require.FileExists(t, runningPath)
	require.Nil(t, c.getCommandKernel("old"))
	require.NotNil(t, c.getCommandKernel("recent"))
	require.NotNil(t, c.getCommandKernel("running"))
}
