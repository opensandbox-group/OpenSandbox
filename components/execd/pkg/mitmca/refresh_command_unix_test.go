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

//go:build !windows

package mitmca

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunRefreshCommandCancellationKillsProcessGroup(t *testing.T) {
	tempDir := t.TempDir()
	pidPath := filepath.Join(tempDir, "child.pid")
	bootstrapPath := filepath.Join(tempDir, "bootstrap.sh")
	t.Setenv("CHILD_PID_PATH", pidPath)
	require.NoError(t, os.WriteFile(bootstrapPath, []byte(`sleep 30 &
printf '%s\n' "$!" > "$CHILD_PID_PATH"
wait
`), 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runRefreshCommand(ctx, bootstrapPath)
		done <- err
	}()

	var childPID int
	require.Eventually(t, func() bool {
		contents, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(string(contents)))
		return err == nil && childPID > 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, syscall.Kill(childPID, 0))

	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("refresh command did not return after cancellation")
	}

	require.Eventually(t, func() bool {
		err := syscall.Kill(childPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, 2*time.Second, 20*time.Millisecond)
}
