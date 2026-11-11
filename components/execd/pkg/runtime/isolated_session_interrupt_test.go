// Copyright 2026 The OpenSandbox Authors

//go:build !windows

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
	"errors"
	"testing"
	"time"
)

func newInterruptTestSession(t *testing.T) (*IsolatedRunner, string) {
	t.Helper()
	runner := newTestRunner(t)
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.DeleteIsolatedSession(id) })
	return runner, id
}

// TestRunInIsolatedSession_InterruptKeepsSessionAlive verifies that a
// cancelled run interrupts the command but leaves the session alive and
// usable for the next run.
func TestRunInIsolatedSession_InterruptKeepsSessionAlive(t *testing.T) {
	runner, id := newInterruptTestSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := runner.RunInIsolatedSession(ctx, id, "sleep 30", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interrupted run error = %v, want context.DeadlineExceeded", err)
	}

	s := runner.lookup(id)
	if s == nil {
		t.Fatal("session was removed after run cancellation")
	}
	if s.dead() {
		t.Fatal("session is dead after run cancellation")
	}

	// The same session must accept a follow-up run.
	var lines []string
	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()
	err = runner.RunInIsolatedSession(runCtx, id, "echo next", nil, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("follow-up run: %v", err)
	}
	if len(lines) != 1 || lines[0] != "next" {
		t.Fatalf("follow-up output = %v, want [next]", lines)
	}
}

// TestRunInIsolatedSession_InterruptedRunDoesNotLeakIntoNext verifies that an
// interrupt aborts a subshell-wrapped (envs) script before its later lines
// run, without polluting the next run's output.
func TestRunInIsolatedSession_InterruptedRunDoesNotLeakIntoNext(t *testing.T) {
	runner, id := newInterruptTestSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := runner.RunInIsolatedSession(
		ctx,
		id,
		"sleep 30\necho SHOULD_NOT_APPEAR",
		map[string]string{"RUN_ENVELOPE": "1"},
		nil,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interrupted run error = %v, want context.DeadlineExceeded", err)
	}

	var lines []string
	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()
	err = runner.RunInIsolatedSession(runCtx, id, "echo next", nil, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("follow-up run: %v", err)
	}
	if len(lines) != 1 || lines[0] != "next" {
		t.Fatalf("follow-up output = %v, want exactly [next]", lines)
	}
}

// TestRunInIsolatedSession_CancelledScriptTailDoesNotKillSession verifies
// that repeated SIGINTs drain a cancelled no-envs script tail without
// tripping the SIGKILL escalation, so the session survives follow-up runs.
func TestRunInIsolatedSession_CancelledScriptTailDoesNotKillSession(t *testing.T) {
	runner, id := newInterruptTestSession(t)

	originalEscalation := isolatedInterruptEscalationTimeout
	isolatedInterruptEscalationTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { isolatedInterruptEscalationTimeout = originalEscalation })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := runner.RunInIsolatedSession(ctx, id, "sleep 30\nsleep 30", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interrupted run error = %v, want context.DeadlineExceeded", err)
	}

	// Past the escalation deadline: a single-SIGINT watcher would have
	// SIGKILLed the session by now.
	time.Sleep(2 * time.Second)

	s := runner.lookup(id)
	if s == nil {
		t.Fatal("session was removed after cancelled-run tail drained")
	}
	if s.dead() {
		t.Fatal("session was killed after cancelled-run tail drained")
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runCancel()
	if err := runner.RunInIsolatedSession(runCtx, id, "echo next", nil, nil); err != nil {
		t.Fatalf("follow-up run: %v", err)
	}
}

// TestRunInIsolatedSession_EscalatesWhenInterruptIgnored verifies that a
// SIGINT-immune command cannot wedge the session forever: the watcher
// escalates to SIGKILL and the session reports dead.
func TestRunInIsolatedSession_EscalatesWhenInterruptIgnored(t *testing.T) {
	runner, id := newInterruptTestSession(t)

	originalEscalation := isolatedInterruptEscalationTimeout
	isolatedInterruptEscalationTimeout = 300 * time.Millisecond
	t.Cleanup(func() { isolatedInterruptEscalationTimeout = originalEscalation })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runner.RunInIsolatedSession(ctx, id, "trap '' INT; sleep 30", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("escalated run error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("escalated run took %v; SIGINT-immune command was not killed", elapsed)
	}

	// SIGKILL reaps the session process; the registry entry only goes away
	// on explicit delete, so assert dead() instead.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := runner.lookup(id)
		if s == nil {
			t.Fatal("session record should still exist until deletion")
		}
		if s.dead() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session should be dead after SIGKILL escalation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := runner.RunInIsolatedSession(context.Background(), id, "echo hi", nil, nil); !errors.Is(err, ErrSessionNotActive) {
		t.Fatalf("run on escalated session error = %v, want ErrSessionNotActive", err)
	}
}
