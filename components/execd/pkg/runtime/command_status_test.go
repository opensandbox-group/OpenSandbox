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
	"strings"
	"testing"
	"time"

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

	// Poll until status is registered (runBackgroundCommand stores kernel asynchronously).
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
			// other hooks unused in this test
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

	// incremental seek from current cursor should return empty data and same-or-higher cursor
	output2, cursor2, err := c.SeekBackgroundCommandOutput(session, cursor)
	require.NoError(t, err, "SeekBackgroundCommandOutput (second call) error")
	require.Empty(t, output2, "expected no new output")
	require.GreaterOrEqual(t, cursor2, cursor, "cursor should not move backwards")
}
