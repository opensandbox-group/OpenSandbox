// Copyright 2025 The OpenSandbox Authors
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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestGetCommandStatus_NotFound(t *testing.T) {
	c := NewController("", "")

	_, err := c.GetCommandStatus("missing")
	require.Error(t, err, "expected error for missing session")
}

func TestGetCommandStatus_Running(t *testing.T) {
	c := NewController("", "")

	var session string
	req := &ExecuteCodeRequest{
		Language: BackgroundCommand,
		Code:     "sleep 2",
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(id string) { session = id },
			OnExecuteComplete: func(time.Duration) {},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.runBackgroundCommand(ctx, cancel, req))
	require.NotEmpty(t, session, "session should be set by OnExecuteInit")

	deadline := time.Now().Add(5 * time.Second)
	var (
		status *CommandStatus
		err    error
	)
	for time.Now().Before(deadline) {
		status, err = c.GetCommandStatus(session)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "not found") {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		require.NoError(t, err, "GetCommandStatus unexpected error")
	}
	require.NoError(t, err, "GetCommandStatus error after retry")

	require.NotNil(t, status)
	require.True(t, status.Running, "expected running=true")
	require.Nil(t, status.ExitCode, "expected exitCode to be nil while running")
	require.Nil(t, status.FinishedAt, "expected finishedAt to be nil while running")
	require.False(t, status.StartedAt.IsZero(), "expected startedAt to be set")
	t.Log(status)
}

func TestSeekBackgroundCommandOutput_Completed(t *testing.T) {
	c := NewController("", "")

	tmpDir := t.TempDir()
	session := "sess-done"
	stdoutPath := filepath.Join(tmpDir, session+".stdout")

	stdoutContent := "hello stdout"
	require.NoError(t, os.WriteFile(stdoutPath, []byte(stdoutContent), 0o644))

	started := time.Now().Add(-2 * time.Second)
	finished := time.Now()
	exitCode := 0
	kernel := &commandKernel{
		pid:          456,
		stdoutPath:   stdoutPath,
		isBackground: true,
		startedAt:    started,
		finishedAt:   &finished,
		exitCode:     &exitCode,
		errMsg:       "",
		running:      false,
	}
	c.storeCommandKernel(session, kernel)

	output, cursor, err := c.SeekBackgroundCommandOutput(session, 0)
	require.NoError(t, err, "GetCommandOutput error")

	require.Greater(t, cursor, int64(0), "expected cursor>=0")
	require.Equal(t, stdoutContent, string(output))
}

func TestSeekBackgroundCommandOutput_ClampsCursorPastEOF(t *testing.T) {
	c := NewController("", "")

	tmpDir := t.TempDir()
	session := "sess-past-eof"
	stdoutPath := filepath.Join(tmpDir, session+".stdout")
	require.NoError(t, os.WriteFile(stdoutPath, []byte("hello"), 0o644))

	c.storeCommandKernel(session, &commandKernel{
		pid:          789,
		stdoutPath:   stdoutPath,
		isBackground: true,
		startedAt:    time.Now(),
		running:      true,
	})

	output, cursor, err := c.SeekBackgroundCommandOutput(session, 10_000_000)
	require.NoError(t, err)
	require.Empty(t, output)
	require.Equal(t, int64(5), cursor, "cursor should be clamped to the current end of the log")

	require.NoError(t, os.WriteFile(stdoutPath, []byte("hello world"), 0o644))
	output, cursor, err = c.SeekBackgroundCommandOutput(session, cursor)
	require.NoError(t, err)
	require.Equal(t, " world", string(output))
	require.Equal(t, int64(11), cursor)
}

func TestSeekBackgroundCommandOutput_CapsReadSize(t *testing.T) {
	c := NewController("", "")

	tmpDir := t.TempDir()
	session := "sess-capped"
	stdoutPath := filepath.Join(tmpDir, session+".stdout")

	payload := bytes.Repeat([]byte("x"), maxBackgroundLogReadBytes+1234)
	require.NoError(t, os.WriteFile(stdoutPath, payload, 0o644))

	c.storeCommandKernel(session, &commandKernel{
		pid:          791,
		stdoutPath:   stdoutPath,
		isBackground: true,
		startedAt:    time.Now(),
		running:      true,
	})

	output, cursor, err := c.SeekBackgroundCommandOutput(session, 0)
	require.NoError(t, err)
	require.Len(t, output, maxBackgroundLogReadBytes, "a single read must not exceed the cap")
	require.Equal(t, int64(maxBackgroundLogReadBytes), cursor, "cursor should advance to the end of the returned bytes")

	output, cursor, err = c.SeekBackgroundCommandOutput(session, cursor)
	require.NoError(t, err)
	require.Len(t, output, 1234, "the remainder should be returned on the next poll")
	require.Equal(t, int64(len(payload)), cursor)
}

func TestSeekBackgroundCommandOutput_KeepsUTF8Boundary(t *testing.T) {
	c := NewController("", "")

	tmpDir := t.TempDir()
	session := "sess-rune"
	stdoutPath := filepath.Join(tmpDir, session+".stdout")

	// Place a 3-byte rune so that it straddles the cap: the first byte is
	// the last byte of the first page, the other two are the first bytes of
	// the second page.
	const multibyte = "中"
	payload := append(bytes.Repeat([]byte("a"), maxBackgroundLogReadBytes-1), []byte(multibyte+"tail")...)
	require.NoError(t, os.WriteFile(stdoutPath, payload, 0o644))

	c.storeCommandKernel(session, &commandKernel{
		pid:          792,
		stdoutPath:   stdoutPath,
		isBackground: true,
		startedAt:    time.Now(),
		running:      true,
	})

	first, cursor, err := c.SeekBackgroundCommandOutput(session, 0)
	require.NoError(t, err)
	require.Len(t, first, maxBackgroundLogReadBytes-1, "first page should end before the split rune")
	require.Equal(t, int64(maxBackgroundLogReadBytes-1), cursor, "cursor should point at the rune start")
	require.True(t, utf8.Valid(first), "first page must be valid UTF-8 on its own")

	second, cursor, err := c.SeekBackgroundCommandOutput(session, cursor)
	require.NoError(t, err)
	require.Equal(t, multibyte+"tail", string(second), "second page should start with the whole rune")
	require.Equal(t, int64(len(payload)), cursor)

	require.Equal(t, string(payload), string(first)+string(second), "pages should reassemble the log")
}

func TestSeekBackgroundCommandOutput_RejectsNegativeCursor(t *testing.T) {
	c := NewController("", "")

	tmpDir := t.TempDir()
	session := "sess-negative"
	stdoutPath := filepath.Join(tmpDir, session+".stdout")
	require.NoError(t, os.WriteFile(stdoutPath, []byte("hello"), 0o644))

	c.storeCommandKernel(session, &commandKernel{
		pid:          790,
		stdoutPath:   stdoutPath,
		isBackground: true,
		startedAt:    time.Now(),
		running:      true,
	})

	_, _, err := c.SeekBackgroundCommandOutput(session, -1)
	require.ErrorContains(t, err, "negative")
}

func TestSeekBackgroundCommandOutput_WithRunBackgroundCommand(t *testing.T) {
	c := NewController("", "")

	expected := "line1\nline2\n"
	var session string
	req := &ExecuteCodeRequest{
		Language: BackgroundCommand,
		Code:     "printf 'line1\nline2\n'",
		Hooks: ExecuteResultHook{
			OnExecuteInit:     func(id string) { session = id },
			OnExecuteComplete: func(executionTime time.Duration) {},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.runBackgroundCommand(ctx, cancel, req))
	require.NotEmpty(t, session, "session should be set by OnExecuteInit")

	var (
		output []byte
		cursor int64
		err    error
	)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		output, cursor, err = c.SeekBackgroundCommandOutput(session, 0)
		if err == nil && len(output) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, err, "SeekBackgroundCommandOutput error")
	require.Equal(t, expected, string(output))
	require.GreaterOrEqual(t, cursor, int64(len(expected)), "cursor should advance to end of file")

	output2, cursor2, err := c.SeekBackgroundCommandOutput(session, cursor)
	require.NoError(t, err, "SeekBackgroundCommandOutput (second call) error")
	require.Empty(t, output2, "expected no new output")
	require.GreaterOrEqual(t, cursor2, cursor, "cursor should not move backwards")
}

func TestSeekBackgroundCommandOutput_DefersIncompleteUTF8WhileRunning(t *testing.T) {
	controller := NewController("", "")
	path := filepath.Join(t.TempDir(), "growing.log")
	const id = "background-utf8"
	controller.storeCommandKernel(id, &commandKernel{
		stdoutPath:   path,
		isBackground: true,
		running:      true,
	})
	checkGrowingBackgroundLogUTF8(t, path, func(cursor int64) ([]byte, int64, error) {
		return controller.SeekBackgroundCommandOutput(id, cursor)
	}, func() {
		controller.markCommandFinished(id, 0, "")
	})
}

// Both log readers must keep a partial character behind the cursor until it
// arrives in full, and let a final read make progress over a truncated tail.
func checkGrowingBackgroundLogUTF8(
	t *testing.T,
	path string,
	read func(int64) ([]byte, int64, error),
	finish func(),
) {
	t.Helper()
	const prefix, character = "start:", "🌍"
	require.NoError(t, os.WriteFile(path, []byte(prefix), 0o644))
	appendOutput := func(output string) {
		t.Helper()
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		require.NoError(t, err)
		_, writeErr := file.WriteString(output)
		require.NoError(t, file.Close())
		require.NoError(t, writeErr)
	}
	checkPage := func(start int64, expected string, next int64) int64 {
		t.Helper()
		data, cursor, err := read(start)
		require.NoError(t, err)
		require.Equal(t, expected, string(data))
		require.Equal(t, next, cursor)
		return cursor
	}

	appendOutput(character[:1])
	cursor := checkPage(0, prefix, int64(len(prefix)))
	appendOutput(character[1:3])
	cursor = checkPage(cursor, "", cursor)
	appendOutput(character[3:] + "\n")
	cursor = checkPage(cursor, character+"\n", int64(len(prefix+character+"\n")))

	// An incomplete suffix held while the process is alive still belongs to
	// the raw log if the process exits without writing the rest.
	appendOutput(character[:2])
	cursor = checkPage(cursor, "", cursor)
	finish()
	cursor = checkPage(cursor, character[:2], cursor+2)
	checkPage(cursor, "", cursor)
}
