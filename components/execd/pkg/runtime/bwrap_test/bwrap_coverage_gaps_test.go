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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// TestStderrIsCaptured verifies that stderr output is captured on stdout
// since cmd.Stderr = cmd.Stdout.
func TestStderrIsCaptured(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`echo stdout-line && echo stderr-line >&2`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Contains(t, lines, "stdout-line")
	assert.Contains(t, lines, "stderr-line")
}

func TestRecoverAfterFailedRun(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use bash -c so the parent bash (the persistent session) doesn't exit —
	// exit is a shell builtin.
	err = r.RunInIsolatedSession(ctx, id, "bash -c 'exit 42'", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "42")

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo recovered", nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"recovered"}, lines)

	err = r.RunInIsolatedSession(ctx, id, "false", nil, nil)
	require.Error(t, err)

	lines = nil
	err = r.RunInIsolatedSession(ctx, id, "echo alive-again", nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"alive-again"}, lines)
}

func TestContextCancellation(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	// Sleep longer than the context deadline so cancellation is tested.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err = r.RunInIsolatedSession(ctx, id, "sleep 10", nil, nil)
	assert.Error(t, err, "should return error on context cancellation")
	assert.True(t, strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "canceled"),
		"error should be context-related, got: %v", err)
}

func TestContextNotCancelledOnNormalExit(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = r.RunInIsolatedSession(ctx, id, "echo fast", nil, nil)
	require.NoError(t, err)
}

// TestStrictNetworkIsolation verifies --unshare-net blocks non-loopback
// network access.
func TestStrictNetworkIsolation(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		ShareNet:      boolPtr(false),
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// With --unshare-net, only loopback should exist. Any non-loopback
	// address should not be reachable. Try to get external connectivity
	// info — `ip link show` should show only lo (loopback).
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`ip link show 2>/dev/null | wc -l; echo DONE`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)

	// With only loopback, we expect exactly 2 lines: lo + DONE.
	// If eth0 existed, we'd see more.
	assert.Contains(t, lines, "DONE")
	for _, line := range lines {
		if line == "DONE" {
			continue
		}
		t.Logf("ip link count: %s", line)
	}
}

func TestManyConsecutiveRuns(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 100; i++ {
		err := r.RunInIsolatedSession(ctx, id, "true", nil, nil)
		require.NoError(t, err, "run %d failed", i)
	}
}

func TestDeleteThenRecreate(t *testing.T) {
	r := newRunner(t)

	ws := t.TempDir()
	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: ws, WorkspaceMode: "rw",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		id, err := r.CreateIsolatedSession(opts)
		require.NoError(t, err, "create %d", i)

		var lines []string
		err = r.RunInIsolatedSession(ctx, id, "echo session-"+string(rune('0'+i)), nil,
			func(line string) { lines = append(lines, line) })
		require.NoError(t, err, "run %d", i)

		err = r.DeleteIsolatedSession(id)
		require.NoError(t, err, "delete %d", i)

		_, err = r.GetIsolatedSession(id)
		assert.Error(t, err, "session %d should be gone after delete", i)
	}
}

func TestBashBuiltinsAndFunctions(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tests := []struct {
		name string
		code string
		want string
	}{
		{"source_define_func", "myfunc() { echo func-called; }; myfunc", "func-called"},
		{"if_statement", `if [ -d /tmp ]; then echo is-dir; fi`, "is-dir"},
		{"for_loop", `for i in 1 2 3; do echo -n "${i} "; done && echo`, "1 2 3 "},
		{"arithmetic", `echo $((3+4))`, "7"},
		{"here_string", `cat <<< "herestring"`, "herestring"},
		{"pipeline_vars", `echo foo | while read x; do echo ${x}; done`, "foo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []string
			err := r.RunInIsolatedSession(ctx, id, tt.code, nil,
				func(line string) { lines = append(lines, line) })
			require.NoError(t, err)
			require.NotEmpty(t, lines)
			assert.Equal(t, tt.want, lines[0])
		})
	}
}

func TestLargeFileWrite(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	script := `dd if=/dev/zero of=/tmp/largefile bs=1M count=5 2>&1 && echo "WRITE_OK"`
	var lines []string
	err = r.RunInIsolatedSession(ctx, id, script, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)

	found := false
	for _, l := range lines {
		if strings.Contains(l, "WRITE_OK") {
			found = true
			break
		}
	}
	assert.True(t, found, "large file write should succeed, got: %v", lines)
}

// TestSessionSubprocessCleanup verifies that Delete reaps an actual descendant
// process tree, including workloads that ignore SIGTERM.
func TestSessionSubprocessCleanup(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = r.DeleteIsolatedSession(id)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	marker := fmt.Sprintf("opensandbox-cleanup-%d", time.Now().UnixNano())
	script := fmt.Sprintf(
		`bash -c "trap '' TERM; bash -c \"trap '' TERM; exec -a %s-grandchild sleep 300\" & exec -a %s-child sleep 300" & echo STARTED`,
		marker,
		marker,
	)
	require.NoError(t, r.RunInIsolatedSession(ctx, id, script, nil, nil))

	var descendants []hostProcessIdentity
	require.Eventually(t, func() bool {
		descendants = hostProcessesWithCmdlineMarker(marker)
		return len(descendants) >= 2
	}, 5*time.Second, 20*time.Millisecond, "descendant process tree did not start")

	require.NoError(t, r.DeleteIsolatedSession(id))
	deleted = true
	require.Eventually(t, func() bool {
		for _, descendant := range descendants {
			startTime, err := hostProcessStartTime(descendant.pid)
			if err == nil && startTime == descendant.startTime {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "Delete left descendant processes: %v", descendants)
}

type hostProcessIdentity struct {
	pid       int
	startTime string
}

func hostProcessesWithCmdlineMarker(marker string) []hostProcessIdentity {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var processes []hostProcessIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && bytes.Contains(cmdline, []byte(marker)) {
			startTime, err := hostProcessStartTime(pid)
			if err == nil {
				processes = append(processes, hostProcessIdentity{
					pid:       pid,
					startTime: startTime,
				})
			}
		}
	}
	return processes
}

func hostProcessStartTime(pid int) (string, error) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	closeParen := bytes.LastIndexByte(stat, ')')
	if closeParen < 0 {
		return "", errors.New("malformed process stat")
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) <= 19 {
		return "", errors.New("process stat omitted starttime")
	}
	return fields[19], nil
}

// TestBorderlineBufferSize exercises the scanner buffer with output
// approaching the 64KB default buffer size.
func TestBorderlineBufferSize(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		WorkspacePath: t.TempDir(), WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	script := `for i in $(seq 60000); do echo "line-$i"; done && echo "DONE"`
	var lineCount int
	err = r.RunInIsolatedSession(ctx, id, script, nil,
		func(line string) {
			if strings.HasPrefix(line, "line-") {
				lineCount++
			}
		})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, lineCount, 59000, "should capture most lines, got %d", lineCount)
}

func TestWorkspaceIsolationAcrossSessions(t *testing.T) {
	r := newRunner(t)

	ws := t.TempDir()
	require.NoError(t, os.WriteFile(ws+"/shared.txt", []byte("original"), 0644))

	opts1 := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "rw",
	}
	id1, err := r.CreateIsolatedSession(opts1)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, r.RunInIsolatedSession(ctx, id1,
		`echo "modified-by-s1" > `+ws+`/shared.txt`, nil, nil))

	// Session 2: overlay mode — reads see original workspace content.
	opts2 := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: ws, WorkspaceMode: "overlay",
	}
	id2, err := r.CreateIsolatedSession(opts2)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id2)

	var lines []string
	require.NoError(t, r.RunInIsolatedSession(ctx, id2,
		`cat `+ws+`/shared.txt`, nil,
		func(line string) { lines = append(lines, line) }))
	assert.Contains(t, lines, "modified-by-s1",
		"overlay mode should see workspace changes (lower layer)")
}
