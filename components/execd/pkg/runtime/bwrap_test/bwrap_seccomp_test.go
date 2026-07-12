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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestSeccompFilterActive(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check /proc/self/status for seccomp mode. Mode 2 = SECCOMP_MODE_FILTER.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`grep Seccomp: /proc/self/status`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.NotEmpty(t, lines, "should have Seccomp line in /proc/self/status")

	t.Logf("Seccomp status: %s", lines[0])
	assert.Contains(t, strings.TrimSpace(lines[0]), "2",
		"Seccomp should be mode 2 (SECCOMP_MODE_FILTER)")
}

func TestSeccompAllowsNormalSyscalls(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Common operations that exercise various syscalls: open, read, write,
	// fork, exec, waitpid, stat, getdents, etc. All must work.
	tests := []struct {
		name string
		code string
		want string
	}{
		{"echo", `echo hello-seccomp`, "hello-seccomp"},
		{"stat", `stat -c %s /proc/self/exe`, ""},                // any non-zero output is fine
		{"readdir", `ls /proc/self >/dev/null && echo ok`, "ok"}, // getdents
		{"fork_exec", `sh -c 'echo child-ok'`, "child-ok"},
		{"pipe", `echo pipe-test | cat`, "pipe-test"},
		{"env", `export FOO=bar && echo $FOO`, "bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []string
			err := r.RunInIsolatedSession(ctx, id, tt.code, nil,
				func(line string) { lines = append(lines, line) })
			require.NoError(t, err, "command should succeed: %s", tt.code)
			if tt.want != "" {
				require.NotEmpty(t, lines)
				assert.Equal(t, tt.want, lines[0])
			}
		})
	}
}

func TestSeccompBlocksPtrace(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt to ptrace-attach to init (PID 1 in the bwrap namespace).
	// This should fail with EACCES from seccomp.
	// Use bash -c to capture exit code — strace/ptrace isn't a bash builtin,
	// but we can use Python if available, or check the exit code of a
	// command that uses ptrace.
	//
	// strace is the easiest way: strace -p 1 should fail before even starting.
	// If strace isn't installed, we skip with a clear message.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		`if command -v strace >/dev/null 2>&1; then strace -p 1 2>&1 | head -1; else echo 'strace-not-installed'; fi`, nil,
		func(line string) { lines = append(lines, line) })

	if len(lines) > 0 && lines[0] == "strace-not-installed" {
		t.Skip("strace not available in sandbox")
	}

	// If strace is available, the seccomp filter blocks ptrace and strace
	// should report "Permission denied" (EACCES) rather than "Operation
	// not permitted" (EPERM from capabilities).
	require.NoError(t, err)
	t.Logf("strace/ptrace result: %v", lines)
	if len(lines) > 0 {
		combined := strings.Join(lines, " ")
		assert.True(t,
			strings.Contains(strings.ToLower(combined), "permission denied") ||
				strings.Contains(strings.ToLower(combined), "operation not permitted"),
			"ptrace should be blocked, got: %s", combined)
	}
}

func TestSeccompBlocksMount(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt a real mount syscall. bwrap namespace init (PID 1) has full
	// capabilities inside the non-user namespace, so mount would succeed
	// if seccomp didn't block it at the syscall level with EACCES.
	err = r.RunInIsolatedSession(ctx, id,
		`mkdir -p /tmp/mnt && mount -t tmpfs tmpfs /tmp/mnt 2>&1`, nil,
		nil)
	require.Error(t, err, "mount should be blocked by seccomp")
	t.Logf("mount error: %v", err)
}

func TestSeccompPersistsAcrossRuns(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run 1: check seccomp is active.
	var lines []string
	require.NoError(t, r.RunInIsolatedSession(ctx, id,
		`grep Seccomp: /proc/self/status`, nil,
		func(line string) { lines = append(lines, line) }))
	require.NotEmpty(t, lines)
	assert.Contains(t, strings.TrimSpace(lines[0]), "2", "first run: seccomp should be mode 2")

	// Run 2: seccomp should still be active (not a one-shot).
	lines = nil
	require.NoError(t, r.RunInIsolatedSession(ctx, id,
		`grep Seccomp: /proc/self/status`, nil,
		func(line string) { lines = append(lines, line) }))
	require.NotEmpty(t, lines)
	assert.Contains(t, strings.TrimSpace(lines[0]), "2", "second run: seccomp should still be mode 2")
}

// TestSeccompConfigOverride_E2E verifies the full path:
// TOML config → LoadConfig → NewBwrap(cfg) → generateSeccompDenyBPF → bwrap session.
// Uses a custom denylist that blocks "unshare" but does NOT block "mount",
// then verifies mount succeeds (it would fail with the default denylist).
func TestSeccompConfigOverride_E2E(t *testing.T) {
	// Write a TOML config with a custom seccomp denylist.
	cfgPath := filepath.Join(t.TempDir(), "isolation.toml")
	tomlContent := `
upper_root = "` + t.TempDir() + `"
upper_max_bytes = 1073741824
allowed_writable = ["/tmp"]

[seccomp]
# Only block unshare — notably does NOT block mount.
deny = ["unshare"]
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(tomlContent), 0o644))

	cfg, err := isolation.LoadConfig(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Seccomp, "config should have seccomp override")
	assert.Equal(t, []string{"unshare"}, cfg.Seccomp.Deny)

	r := newRunnerWithConfig(t, cfg)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seccomp should still be active (mode 2).
	var lines []string
	require.NoError(t, r.RunInIsolatedSession(ctx, id,
		`grep Seccomp: /proc/self/status`, nil,
		func(line string) { lines = append(lines, line) }))
	require.NotEmpty(t, lines)
	assert.Contains(t, strings.TrimSpace(lines[0]), "2")

	// mount should succeed — it's NOT in our custom denylist.
	err = r.RunInIsolatedSession(ctx, id,
		`mkdir -p /tmp/test-mnt && mount -t tmpfs tmpfs /tmp/test-mnt && echo mount-ok && umount /tmp/test-mnt`, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err, "mount should succeed with custom seccomp that doesn't block it")
}

// TestSeccompConfigDefault_E2E verifies that nil Seccomp (no [seccomp]
// section) uses the built-in denylist — mount is blocked.
func TestSeccompConfigDefault_E2E(t *testing.T) {
	cfg := isolation.Config{
		UpperRoot:       t.TempDir(),
		UpperMaxBytes:   1 << 30,
		AllowedWritable: []string{"/tmp"},
		Seccomp:         nil, // use built-in denylist
	}

	r := newRunnerWithConfig(t, cfg)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// mount should fail — blocked by built-in seccomp denylist.
	err = r.RunInIsolatedSession(ctx, id,
		`mkdir -p /tmp/mnt2 && mount -t tmpfs tmpfs /tmp/mnt2 2>&1`, nil,
		nil)
	require.Error(t, err, "mount should be blocked by default seccomp denylist")
}
