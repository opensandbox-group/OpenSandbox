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
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

func TestExitedSessionIsCleanedBeforeIdleGC(t *testing.T) {
	exitMarker := filepath.Join(t.TempDir(), "exit")
	lifecycle := newLifecycleHarness()
	isolator := &lifecycleHarnessIsolator{
		lifecycle: lifecycle,
		configure: func(cmd *exec.Cmd) {
			cmd.Args = []string{
				cmd.Path,
				"-c",
				`while [ ! -f "$OPENSANDBOX_TEST_EXIT_MARKER" ]; do :; done`,
			}
			cmd.Env = append(
				os.Environ(),
				"OPENSANDBOX_TEST_EXIT_MARKER="+exitMarker,
			)
		},
	}
	pins := &lifecycleNamespacePins{}
	runner := newLifecycleCleanupRunner(t, isolator, pins)
	t.Cleanup(func() {
		lifecycle.finish(nil)
		_ = os.WriteFile(exitMarker, nil, 0o600)
		_ = runner.Close()
	})

	privateNetwork := false
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: t.TempDir(),
		WorkspaceMode: string(isolation.WorkspaceOverlay),
		ShareNet:      &privateNetwork,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := runner.lookup(id)
	if session == nil {
		t.Fatal("created session was not published")
	}
	upperParent := filepath.Dir(session.upperDir)

	// Closing the lifecycle stream and allowing the workload to exit publishes
	// doneCh. No GC loop is running for this test runner, so only the
	// cleanupExitedSession watcher can remove the session.
	cleanupStarted := time.Now()
	lifecycle.finish(nil)
	if err := os.WriteFile(exitMarker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for runner.lookup(id) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runner.lookup(id) != nil {
		t.Fatal("exited session was not cleaned before the idle GC interval")
	}
	if elapsed := time.Since(cleanupStarted); elapsed >= 5*time.Second {
		t.Fatalf("exited-session cleanup took %v", elapsed)
	}
	if got := namespacePinCloseCount(pins); got != 1 {
		t.Fatalf("namespace pin close count = %d, want 1", got)
	}
	if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
		t.Fatalf("exited-session cleanup retained upper %s: %v", upperParent, err)
	}
}

func TestIsolatedRunnerCloseStopsAdmissionAndCleansActiveSessions(
	t *testing.T,
) {
	useDirectProcessKill(t)

	lifecycle := newLifecycleHarness()
	isolator := &lifecycleHarnessIsolator{
		lifecycle: lifecycle,
		configure: func(cmd *exec.Cmd) {
			// exec avoids leaving a child process behind when the direct-process
			// signal hook terminates the workload.
			cmd.Args = []string{cmd.Path, "-c", "exec sleep 30"}
		},
	}
	pins := &lifecycleNamespacePins{}
	runner := newLifecycleCleanupRunner(t, isolator, pins)
	t.Cleanup(func() {
		lifecycle.finish(nil)
		_ = runner.Close()
	})

	privateNetwork := false
	createOpts := func() *IsolatedSessionOptions {
		return &IsolatedSessionOptions{
			WorkspacePath: t.TempDir(),
			WorkspaceMode: string(isolation.WorkspaceOverlay),
			ShareNet:      &privateNetwork,
		}
	}
	id, err := runner.CreateIsolatedSession(createOpts())
	if err != nil {
		t.Fatal(err)
	}
	session := runner.lookup(id)
	if session == nil {
		t.Fatal("created session was not published")
	}
	upperParent := filepath.Dir(session.upperDir)

	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if runner.lookup(id) != nil {
		t.Fatal("runner Close retained an active session")
	}
	if got := namespacePinCloseCount(pins); got != 1 {
		t.Fatalf("namespace pin close count = %d, want 1", got)
	}
	if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
		t.Fatalf("runner Close retained upper %s: %v", upperParent, err)
	}

	if id, err := runner.CreateIsolatedSession(createOpts()); id != "" ||
		!errors.Is(err, ErrIsolatedRunnerClosed) {
		t.Fatalf(
			"Create after Close = id %q, error %v; want %v",
			id,
			err,
			ErrIsolatedRunnerClosed,
		)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := namespacePinCloseCount(pins); got != 1 {
		t.Fatalf("idempotent Close closed namespace pins %d times", got)
	}
}

func TestIsolatedRunnerCloseRetriesPendingStartupCleanup(t *testing.T) {
	runner := newLifecycleCleanupRunner(t, newStubIsolator(), nil)
	upperID, upperDir, workDir, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	upperParent := filepath.Dir(upperDir)

	pinErr := errors.New("namespace pin is busy")
	pins := &lifecycleNamespacePins{closeErr: pinErr}
	session := &isolatedSession{
		id:            "pending-startup-close-retry",
		opts:          &IsolatedSessionOptions{},
		processWaited: make(chan struct{}),
		doneCh:        make(chan struct{}),
		upperID:       upperID,
		upperDir:      upperDir,
		workDir:       workDir,
		namespacePins: pins,
	}
	runner.pendingStartupCleanup.Store(session.id, session)

	err = runner.Close()
	if !errors.Is(err, ErrSessionNamespaceCleanup) ||
		!errors.Is(err, pinErr) {
		t.Fatalf("first Close error = %v", err)
	}
	if got := pendingStartupCount(runner); got != 1 {
		t.Fatalf("pending startup count after failed Close = %d, want 1", got)
	}
	if _, err := os.Stat(upperParent); err != nil {
		t.Fatalf("failed Close discarded pending-startup upper: %v", err)
	}

	pins.mu.Lock()
	pins.closeErr = nil
	pins.mu.Unlock()
	if err := runner.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if got := pendingStartupCount(runner); got != 0 {
		t.Fatalf("pending startup count after retry = %d, want 0", got)
	}
	if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
		t.Fatalf("retry Close retained pending-startup upper %s: %v", upperParent, err)
	}
	if got := namespacePinCloseCount(pins); got != 2 {
		t.Fatalf("namespace pin close attempts = %d, want 2", got)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("idempotent Close after retry: %v", err)
	}
}

func newLifecycleCleanupRunner(
	t *testing.T,
	isolator isolation.Isolator,
	pins sessionNamespacePins,
) *IsolatedRunner {
	t.Helper()
	upperMgr, err := isolation.NewUpperManager(t.TempDir(), 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	runner := &IsolatedRunner{
		ctrl:     NewController("", ""),
		isolator: isolator,
		upperMgr: upperMgr,
	}
	if pins != nil {
		runner.namespacePinner = func(
			context.Context,
			isolation.WorkloadIdentity,
		) (sessionNamespacePins, error) {
			return pins, nil
		}
	}
	return runner
}

func namespacePinCloseCount(pins *lifecycleNamespacePins) int {
	pins.mu.Lock()
	defer pins.mu.Unlock()
	return pins.closed
}

func pendingStartupCount(runner *IsolatedRunner) int {
	count := 0
	runner.pendingStartupCleanup.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
