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

//go:build linux && bwrap

package bwrap_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestPIDIsolation(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{
		Profile: "strict", WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo $$", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	assert.True(t, len(lines[0]) <= 2, "PID in new namespace should be small, got: %s", lines[0])
}

func TestEchoOutput(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo hello-world-from-bwrap", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"hello-world-from-bwrap"}, lines)
}

func TestEnvPersistence(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, r.RunInIsolatedSession(ctx, id, "export MY_VAR=persisted_42", nil, nil))

	var lines []string
	require.NoError(t, r.RunInIsolatedSession(ctx, id, "echo $MY_VAR", nil, func(line string) {
		lines = append(lines, line)
	}))
	assert.Equal(t, []string{"persisted_42"}, lines)
}

func TestNonZeroExit(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = r.RunInIsolatedSession(ctx, id, "bash -c 'exit 13'", nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "13")
}

func TestMultipleRuns(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 10; i++ {
		expected := fmt.Sprintf("run-%d", i)
		var lines []string
		require.NoError(t, r.RunInIsolatedSession(ctx, id, "echo "+expected, nil, func(line string) {
			lines = append(lines, line)
		}), "run %d", i)
		assert.Equal(t, []string{expected}, lines, "run %d output", i)
	}
}

func TestStdinStreaming(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	script := `for i in $(seq 1 50); do echo "line-$i"; done`
	var lines []string
	require.NoError(t, r.RunInIsolatedSession(ctx, id, script, nil, func(line string) {
		lines = append(lines, line)
	}))

	assert.Len(t, lines, 50)
	for i := 1; i <= 50; i++ {
		assert.Equal(t, fmt.Sprintf("line-%d", i), lines[i-1], "line %d", i)
	}
}

func TestLargeOutput(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	script := `for i in $(seq 1 1000); do echo "line-$i"; done`
	var lines []string
	err = r.RunInIsolatedSession(ctx, id, script, nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	assert.Len(t, lines, 1000)
	assert.Equal(t, "line-1", lines[0])
	assert.Equal(t, "line-1000", lines[999])
}

func TestEmptyCode(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = r.RunInIsolatedSession(ctx, id, "", nil, nil)
	require.NoError(t, err, "empty code should not error")
}

func TestSessionState(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	state, err := r.GetIsolatedSession(id)
	require.NoError(t, err)
	assert.Equal(t, "active", state.Status)
	assert.False(t, state.CreatedAt.IsZero())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, r.RunInIsolatedSession(ctx, id, "true", nil, nil))

	state2, err := r.GetIsolatedSession(id)
	require.NoError(t, err)
	assert.True(t, !state2.LastRunAt.Before(state.LastRunAt), "lastRunAt should advance after run")
}

func TestCapabilities(t *testing.T) {
	r := newRunner(t)
	caps := r.Capabilities()
	assert.True(t, caps.Available)
	assert.Equal(t, "bwrap", caps.Isolator)
	assert.False(t, caps.CommitSupported)
	assert.False(t, caps.DiffSupported)
	// Version may be empty on older bwrap or different output formats.
	if caps.Version != "" {
		t.Logf("bwrap version: %s", caps.Version)
	}
}

func TestSessionCleanup(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.RunInIsolatedSession(ctx, id, "true", nil, nil)

	require.NoError(t, r.DeleteIsolatedSession(id))
	time.Sleep(200 * time.Millisecond)

	_, err = r.GetIsolatedSession(id)
	assert.Error(t, err)
}

func TestIdleGC(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath:      t.TempDir(),
		WorkspaceMode:      "rw",
		IdleTimeoutSeconds: 2,
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)

	// Run a command to set lastRunAt.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, r.RunInIsolatedSession(ctx, id, "true", nil, nil))

	// Session should exist immediately after run.
	_, err = r.GetIsolatedSession(id)
	require.NoError(t, err)

	// Wait for GC to collect it (2s timeout + 60s GC interval...
	// GC interval is 60s, too slow for test. Manually trigger.
	r.CollectIdle()

	// After GC, session should be gone (idle > 2s since we waited).
	// But since lastRunAt was just updated, it should still exist.
	_, err = r.GetIsolatedSession(id)
	require.NoError(t, err)

	// Wait past the idle timeout.
	time.Sleep(3 * time.Second)

	// Trigger GC again — now it should be collected.
	r.CollectIdle()

	_, err = r.GetIsolatedSession(id)
	assert.Error(t, err, "session should be deleted by GC after idle timeout")
}

func TestIdleGC_Disabled(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath:      t.TempDir(),
		WorkspaceMode:      "rw",
		IdleTimeoutSeconds: 0, // disabled
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	// CollectIdle should not delete sessions with timeout=0.
	r.CollectIdle()

	_, err = r.GetIsolatedSession(id)
	require.NoError(t, err, "session should still exist when GC disabled")
}

func TestTmpIsolation(t *testing.T) {
	r := newRunner(t)
	wsDir := t.TempDir()
	opts := &runtime.IsolatedSessionOptions{
		Profile: "strict", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, r.RunInIsolatedSession(ctx, id, "echo secret > /tmp/bwrap-test-file.txt", nil, nil))

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "cat /tmp/bwrap-test-file.txt", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	assert.Equal(t, "secret", strings.TrimSpace(lines[0]))

	_, err = os.Stat("/tmp/bwrap-test-file.txt")
	assert.True(t, os.IsNotExist(err), "tmpfs leak: file should not exist on host /tmp")
}
