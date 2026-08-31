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

package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForBackgroundRun(
	t *testing.T,
	runner *IsolatedRunner,
	sessionID string,
	runID string,
) *IsolatedBackgroundRunSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := runner.GetIsolatedBackgroundRun(sessionID, runID)
		if err != nil {
			t.Fatalf("GetIsolatedBackgroundRun: %v", err)
		}
		if !snapshot.Running {
			return snapshot
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background run did not finish within deadline")
	return nil
}

func newBackgroundTestSession(t *testing.T, runner *IsolatedRunner, mode string) string {
	t.Helper()
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "ws"),
		WorkspaceMode: mode,
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}
	return id
}

func TestRunInIsolatedSessionBackground_CompletesWithLogs(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo hello-background", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	if runID == "" {
		t.Fatal("run ID is empty")
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", snapshot.ExitCode)
	}
	if snapshot.Error != "" {
		t.Errorf("Error = %q, want empty", snapshot.Error)
	}
	if snapshot.FinishedAt == nil {
		t.Error("FinishedAt is nil after completion")
	}

	output, cursor, err := runner.SeekIsolatedBackgroundOutput(id, runID, 0)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput: %v", err)
	}
	if !strings.Contains(string(output), "hello-background") {
		t.Errorf("output = %q, want to contain hello-background", output)
	}
	if cursor <= 0 {
		t.Errorf("cursor = %d, want > 0", cursor)
	}

	output2, cursor2, err := runner.SeekIsolatedBackgroundOutput(id, runID, cursor)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput (incremental): %v", err)
	}
	if len(output2) != 0 {
		t.Errorf("incremental output = %q, want empty", output2)
	}
	if cursor2 != cursor {
		t.Errorf("cursor2 = %d, want %d", cursor2, cursor)
	}
}

func TestRunInIsolatedSessionBackground_ExitCode(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "exit 7", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if snapshot.ExitCode == nil || *snapshot.ExitCode != 7 {
		t.Errorf("ExitCode = %v, want 7", snapshot.ExitCode)
	}
	if snapshot.Running {
		t.Error("run should be finished")
	}
}

func TestRunInIsolatedSessionBackground_Envs(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(
		id,
		"echo $BG_VAR",
		map[string]string{"BG_VAR": "hello-env"},
	)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	waitForBackgroundRun(t, runner, id, runID)

	output, _, err := runner.SeekIsolatedBackgroundOutput(id, runID, 0)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput: %v", err)
	}
	if !strings.Contains(string(output), "hello-env") {
		t.Errorf("output = %q, want to contain hello-env", output)
	}
}

// A background run must not leak output into the session stdout that the
// next foreground run's end-marker scan consumes.
func TestRunInIsolatedSessionBackground_DoesNotPolluteForegroundOutput(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo bg-output", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	waitForBackgroundRun(t, runner, id, runID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lines []string
	err = runner.RunInIsolatedSession(ctx, id, "echo fg-output", nil, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("RunInIsolatedSession: %v", err)
	}
	if len(lines) != 1 || lines[0] != "fg-output" {
		t.Errorf("foreground output = %v, want [fg-output]", lines)
	}
}

func TestRunInIsolatedSessionBackground_NotFound(t *testing.T) {
	runner := newTestRunner(t)
	_, _, err := runner.RunInIsolatedSessionBackground("nonexistent", "echo hi", nil)
	if !errors.Is(err, ErrContextNotFound) {
		t.Errorf("error = %v, want ErrContextNotFound", err)
	}

	_, err = runner.GetIsolatedBackgroundRun("nonexistent", "nope")
	if !errors.Is(err, ErrContextNotFound) {
		t.Errorf("GetIsolatedBackgroundRun: error = %v, want ErrContextNotFound", err)
	}
}

func TestRunInIsolatedSessionBackground_ReadOnlyWorkspaceRejected(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "ro")
	defer runner.DeleteIsolatedSession(id)

	_, _, err := runner.RunInIsolatedSessionBackground(id, "echo hi", nil)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %v, want read-only rejection", err)
	}
}

func TestGetIsolatedBackgroundRun_WrongSession(t *testing.T) {
	runner := newTestRunner(t)
	id1 := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id1)
	id2 := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id2)

	runID, _, err := runner.RunInIsolatedSessionBackground(id1, "echo hi", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	_, err = runner.GetIsolatedBackgroundRun(id2, runID)
	if !errors.Is(err, ErrContextNotFound) {
		t.Errorf("error = %v, want ErrContextNotFound", err)
	}
}

// An in-flight background run must keep its session alive past the idle
// timeout; the session is collected once the run finishes.
func TestBackgroundRun_BlocksIdleGC(t *testing.T) {
	runner := newTestRunner(t)
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath:      filepath.Join(t.TempDir(), "ws"),
		WorkspaceMode:      "rw",
		IdleTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "sleep 2", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)
	runner.CollectIdle()
	if runner.lookup(id) == nil {
		t.Fatal("session was collected while a background run was active")
	}

	waitForBackgroundRun(t, runner, id, runID)

	time.Sleep(1100 * time.Millisecond)
	runner.CollectIdle()
	if runner.lookup(id) != nil {
		t.Error("session should be collected after the background run finished")
	}

	if _, err := runner.GetIsolatedBackgroundRun(id, runID); !errors.Is(err, ErrContextNotFound) {
		t.Errorf("GetIsolatedBackgroundRun after GC: error = %v, want ErrContextNotFound", err)
	}
}

// Deleting a session with an in-flight background run reaps the run and
// removes its records.
func TestDeleteSessionWithActiveBackgroundRun(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "sleep 10", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Fatalf("DeleteIsolatedSession: %v", err)
	}

	if _, err := runner.GetIsolatedBackgroundRun(id, runID); !errors.Is(err, ErrContextNotFound) {
		t.Errorf("GetIsolatedBackgroundRun after delete: error = %v, want ErrContextNotFound", err)
	}
}

// A stdin-reading background command (cat) must exit immediately and must
// not consume script lines meant for the next foreground run.
func TestRunInIsolatedSessionBackground_StdinDetached(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "cat", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0 (cat with stdin detached)", snapshot.ExitCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lines []string
	err = runner.RunInIsolatedSession(ctx, id, "echo after-cat", nil, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("RunInIsolatedSession: %v", err)
	}
	if len(lines) != 1 || lines[0] != "after-cat" {
		t.Errorf("foreground output = %v, want [after-cat]", lines)
	}
}

// TestRunInIsolatedSessionBackground_FallbackWhenRunDirBlocked verifies that a
// blocked background run dir (a lower-layer .execd entry) falls back to the
// workspace root so the run still completes and its output is still readable.
func TestRunInIsolatedSessionBackground_FallbackWhenRunDirBlocked(t *testing.T) {
	runner := newTestRunner(t)
	ws := filepath.Join(t.TempDir(), "ws")
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: ws,
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}
	defer runner.DeleteIsolatedSession(id)

	// A regular file named .execd makes mkdir -p .execd/background-runs fail.
	if err := os.WriteFile(filepath.Join(ws, ".execd"), []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo fallback-ok", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", snapshot.ExitCode)
	}

	output, _, err := runner.SeekIsolatedBackgroundOutput(id, runID, 0)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput: %v", err)
	}
	if !strings.Contains(string(output), "fallback-ok") {
		t.Errorf("output = %q, want to contain fallback-ok", output)
	}
}

// TestSeekIsolatedBackgroundOutput_CapsReadSize verifies a single read returns
// at most maxBackgroundLogReadBytes, with the cursor advancing so the client
// can page through the remainder.
func TestSeekIsolatedBackgroundOutput_CapsReadSize(t *testing.T) {
	runner := newTestRunner(t)
	dir := t.TempDir()
	runDir := filepath.Join(dir, isolatedBackgroundRunDir)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runDir, "run-capped.log")
	payload := bytes.Repeat([]byte("x"), maxBackgroundLogReadBytes+1234)
	if err := os.WriteFile(logPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	run := &IsolatedBackgroundRun{
		ID:        "run-capped",
		SessionID: "session-capped",
		logPath:   logPath,
		logRoot:   dir,
	}
	runner.bgRuns.Store(run.ID, run)

	data, cursor, err := runner.SeekIsolatedBackgroundOutput(run.SessionID, run.ID, 0)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput: %v", err)
	}
	if len(data) != maxBackgroundLogReadBytes {
		t.Errorf("len(data) = %d, want %d", len(data), maxBackgroundLogReadBytes)
	}
	if cursor != maxBackgroundLogReadBytes {
		t.Errorf("cursor = %d, want %d", cursor, maxBackgroundLogReadBytes)
	}

	// The remainder is served on the next poll.
	data2, cursor2, err := runner.SeekIsolatedBackgroundOutput(run.SessionID, run.ID, cursor)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput (remainder): %v", err)
	}
	if len(data2) != 1234 {
		t.Errorf("len(remainder) = %d, want 1234", len(data2))
	}
	if cursor2 != int64(len(payload)) {
		t.Errorf("cursor2 = %d, want %d", cursor2, len(payload))
	}
}

// TestDeleteSessionRemovesRWRunArtifacts verifies that run log/exit files in
// an rw workspace (and the execd-managed run directory) are removed when the
// session is deleted, so they do not accumulate in the user's workspace.
func TestDeleteSessionRemovesRWRunArtifacts(t *testing.T) {
	runner := newTestRunner(t)
	ws := filepath.Join(t.TempDir(), "ws")
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: ws,
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo cleanup-me", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	waitForBackgroundRun(t, runner, id, runID)

	runDir := filepath.Join(ws, isolatedBackgroundRunDir)
	if _, err := os.Stat(filepath.Join(runDir, runID+".log")); err != nil {
		t.Fatalf("run log should exist before delete: %v", err)
	}

	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Fatalf("DeleteIsolatedSession: %v", err)
	}

	// Run files and the now-empty execd-managed dirs are gone.
	if _, err := os.Stat(filepath.Join(runDir, runID+".log")); !os.IsNotExist(err) {
		t.Errorf("run log should be removed after delete, stat err = %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir should be removed after delete, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".execd")); !os.IsNotExist(err) {
		t.Errorf(".execd should be removed when empty, stat err = %v", err)
	}
}

// TestRunInIsolatedSessionBackground_RejectsUnwritableWorkspace verifies that
// a session whose workspace root is not writable by its own uid gets a clean
// rejection instead of an accepted run that can never write its exit code
// (a ghost run).
func TestRunInIsolatedSessionBackground_RejectsUnwritableWorkspace(t *testing.T) {
	runner := newTestRunner(t)
	ws := filepath.Join(t.TempDir(), "ws")
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: ws,
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}
	defer runner.DeleteIsolatedSession(id)

	// Remove write permission from the workspace root (the run dir cannot be
	// created and the fallback is equally unwritable).
	if err := os.Chmod(ws, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ws, 0o755)

	_, _, err = runner.RunInIsolatedSessionBackground(id, "echo hi", nil)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error = %v, want writability rejection", err)
	}
}

// TestSessionDeathPreservesRunRecordUntilDelete verifies that a run whose
// session dies mid-flight keeps its record (reported as "session terminated")
// until the session is deleted, instead of disappearing immediately.
func TestSessionDeathPreservesRunRecordUntilDelete(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "sleep 30", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	// Kill the session's whole process group so the shell and the detached
	// background job die together (no exit-code file can be written).
	s := runner.lookup(id)
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		t.Fatal("session process unavailable")
	}
	if err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill session process group: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := runner.GetIsolatedBackgroundRun(id, runID)
		if err != nil {
			t.Fatalf("GetIsolatedBackgroundRun: %v", err)
		}
		if !snapshot.Running {
			if snapshot.Error != "session terminated" {
				t.Errorf("Error = %q, want %q", snapshot.Error, "session terminated")
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The record survives until the session is deleted, then it is swept.
	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Fatalf("DeleteIsolatedSession: %v", err)
	}
	if _, err := runner.GetIsolatedBackgroundRun(id, runID); !errors.Is(err, ErrContextNotFound) {
		t.Errorf("GetIsolatedBackgroundRun after delete: error = %v, want ErrContextNotFound", err)
	}
}

// TestDeleteKeepsUserExecdFile verifies that the cleanup of the run directory
// never removes a user-owned .execd file (the fallback blocker case).
func TestDeleteKeepsUserExecdFile(t *testing.T) {
	runner := newTestRunner(t)
	ws := filepath.Join(t.TempDir(), "ws")
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: ws,
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}

	// A user-owned regular file named .execd forces the workspace-root
	// fallback for log files.
	if err := os.WriteFile(filepath.Join(ws, ".execd"), []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo fallback-again", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	waitForBackgroundRun(t, runner, id, runID)

	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Fatalf("DeleteIsolatedSession: %v", err)
	}

	// The user's file must survive; the fallback run log is gone.
	data, err := os.ReadFile(filepath.Join(ws, ".execd"))
	if err != nil {
		t.Fatalf("user .execd file was removed: %v", err)
	}
	if string(data) != "user data" {
		t.Errorf(".execd content = %q, want %q", data, "user data")
	}
	if _, err := os.Stat(filepath.Join(ws, runID+".log")); !os.IsNotExist(err) {
		t.Errorf("fallback run log should be removed after delete, stat err = %v", err)
	}
}

// TestRunInIsolatedSessionBackground_NilStdinNoPanic verifies the submission
// handles a session whose pipes were closed/nil'd by a concurrent Delete
// without dereferencing a nil writer.
func TestRunInIsolatedSessionBackground_NilStdinNoPanic(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	s := runner.lookup(id)
	s.mu.Lock()
	s.stdin = nil
	s.mu.Unlock()

	_, _, err := runner.RunInIsolatedSessionBackground(id, "echo hi", nil)
	if err == nil || !strings.Contains(err.Error(), "session not started") {
		t.Errorf("error = %v, want 'session not started'", err)
	}
}

// TestBackgroundRunLogCappedOnDisk verifies that a run's log file is truncated
// to maxBackgroundLogReadBytes when the run completes, bounding disk usage.
func TestBackgroundRunLogCappedOnDisk(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(
		id, "dd if=/dev/zero bs=1048576 count=20 2>/dev/null", nil,
	)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	waitForBackgroundRun(t, runner, id, runID)

	v, ok := runner.bgRuns.Load(runID)
	if !ok {
		t.Fatal("run record missing")
	}
	run := v.(*IsolatedBackgroundRun)
	info, err := os.Stat(run.logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if info.Size() != maxBackgroundLogReadBytes {
		t.Errorf("log size = %d, want %d (capped)", info.Size(), maxBackgroundLogReadBytes)
	}
}

// TestSeekIsolatedBackgroundOutput_RejectsNegativeCursor verifies the cursor
// is validated before seeking (the spec declares minimum: 0).
func TestSeekIsolatedBackgroundOutput_RejectsNegativeCursor(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo hi", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	waitForBackgroundRun(t, runner, id, runID)

	_, _, err = runner.SeekIsolatedBackgroundOutput(id, runID, -1)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("error = %v, want negative-cursor rejection", err)
	}
}

// TestRunInIsolatedSessionBackground_ReturnsStartedAt verifies the submitted
// handle and run_status report the same start time.
func TestRunInIsolatedSessionBackground_ReturnsStartedAt(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, startedAt, err := runner.RunInIsolatedSessionBackground(id, "echo hi", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	if startedAt.IsZero() {
		t.Fatal("startedAt is zero")
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if !snapshot.StartedAt.Equal(startedAt) {
		t.Errorf("status StartedAt = %v, want %v (the submitted handle's time)", snapshot.StartedAt, startedAt)
	}
}

func TestReadIsolatedRunExitCode(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := os.WriteFile(filepath.Join(dir, "ok.code"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, ok := readIsolatedRunExitCode(root, "ok.code"); !ok || code != 0 {
		t.Errorf("ok.code = (%d, %v), want (0, true)", code, ok)
	}

	if err := os.WriteFile(filepath.Join(dir, "empty.code"), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readIsolatedRunExitCode(root, "empty.code"); ok {
		t.Error("empty.code should not be ready")
	}

	if err := os.WriteFile(filepath.Join(dir, "bogus.code"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readIsolatedRunExitCode(root, "bogus.code"); ok {
		t.Error("bogus.code should not be ready")
	}

	if _, ok := readIsolatedRunExitCode(root, "missing.code"); ok {
		t.Error("missing.code should not be ready")
	}

	// A symlink-replaced exit-code file must be refused, not read through.
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "linked.code")); err != nil {
		t.Fatal(err)
	}
	if _, ok := readIsolatedRunExitCode(root, "linked.code"); ok {
		t.Error("linked.code should not be readable through the symlink")
	}
}

// TestBackgroundRunCompletesDespiteShellSyntaxError verifies that user code
// with a shell syntax error cannot break the control wrapper: the run must
// finish (non-zero exit) and the session must stay alive.
func TestBackgroundRunCompletesDespiteShellSyntaxError(t *testing.T) {
	runner := newTestRunner(t)
	id := newBackgroundTestSession(t, runner, "rw")
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, `if [ 1 = 1 ]; then echo "unterminated`, nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if snapshot.Running {
		t.Fatal("run should have finished")
	}
	if snapshot.ExitCode == nil || *snapshot.ExitCode == 0 {
		t.Errorf("ExitCode = %v, want non-zero (syntax error)", snapshot.ExitCode)
	}

	// The session shell must still be usable after the broken code.
	if err := runner.RunInIsolatedSession(context.Background(), id, "echo still-alive", nil, nil); err != nil {
		t.Errorf("foreground run after broken background code: %v", err)
	}

	output, _, err := runner.SeekIsolatedBackgroundOutput(id, runID, 0)
	if err != nil {
		t.Fatalf("SeekIsolatedBackgroundOutput: %v", err)
	}
	if !strings.Contains(string(output), "unexpected EOF") &&
		!strings.Contains(string(output), "syntax error") {
		t.Errorf("log output = %q, want a shell syntax diagnostic", output)
	}
}

// TestBackgroundRunCompletesAfterRunDirDeleted verifies that a background
// command deleting its own .execd/background-runs directory cannot strand the
// run: the exit-code marker falls back to the workspace root and the watcher
// still reports completion.
func TestBackgroundRunCompletesAfterRunDirDeleted(t *testing.T) {
	runner := newTestRunner(t)
	ws := filepath.Join(t.TempDir(), "ws")
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: ws,
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}
	defer runner.DeleteIsolatedSession(id)

	// Replace the execd-managed run directory with a regular file while the
	// run is in flight, so the wrapper's completion-marker re-resolve falls
	// back to the workspace root (mkdir -p "$D" fails on the file).
	execdPath := shellescape(filepath.Join(ws, ".execd"))
	runID, _, err := runner.RunInIsolatedSessionBackground(
		id, "rm -rf "+execdPath+" && touch "+execdPath+" && echo gone", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}

	snapshot := waitForBackgroundRun(t, runner, id, runID)
	if snapshot.Running {
		t.Fatal("run should have finished")
	}
	if snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", snapshot.ExitCode)
	}

	// The completion marker was written to the workspace-root fallback.
	if _, err := os.Stat(filepath.Join(ws, runID+".code")); err != nil {
		t.Errorf("fallback exit-code file should exist, stat err = %v", err)
	}
}

// TestSeekIsolatedBackgroundOutput_RefusesSymlinkedLog verifies that a
// background command replacing its log file with a symlink cannot make
// host-side execd read an arbitrary host file through the /logs endpoint.
func TestSeekIsolatedBackgroundOutput_RefusesSymlinkedLog(t *testing.T) {
	runner := newTestRunner(t)
	ws := filepath.Join(t.TempDir(), "ws")
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: ws,
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}
	defer runner.DeleteIsolatedSession(id)

	runID, _, err := runner.RunInIsolatedSessionBackground(id, "echo legit-output", nil)
	if err != nil {
		t.Fatalf("RunInIsolatedSessionBackground: %v", err)
	}
	waitForBackgroundRun(t, runner, id, runID)

	// Replace the run's log with a symlink to a host file that is NOT the log.
	secretPath := filepath.Join(t.TempDir(), "host-secret.txt")
	if err := os.WriteFile(secretPath, []byte("HOST-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(ws, isolatedBackgroundRunDir, runID+".log")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, logPath); err != nil {
		t.Fatal(err)
	}

	output, _, err := runner.SeekIsolatedBackgroundOutput(id, runID, 0)
	if err == nil {
		t.Fatalf("SeekIsolatedBackgroundOutput should refuse the symlink, got output %q", output)
	}
	if strings.Contains(string(output), "HOST-SECRET") {
		t.Fatal("host file content leaked through the symlinked log")
	}
}

// TestCapIsolatedRunLog_RefusesSymlinkTarget verifies that the completion-time
// log cap cannot truncate an arbitrary host file through a symlink.
func TestCapIsolatedRunLog_RefusesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	target := filepath.Join(dir, "big-target.bin")
	if err := os.WriteFile(target, make([]byte, 32<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.log")); err != nil {
		t.Fatal(err)
	}

	capIsolatedRunLog(root, "linked.log")

	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 32<<20 {
		t.Errorf("symlink target truncated to %d bytes, want untouched %d", st.Size(), 32<<20)
	}
}
