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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

func TestFailedStartupTimeoutRetainsPrivateCleanupOwnership(t *testing.T) {
	waitErr := errors.New("identity unavailable")
	readyErr := errors.New("ready gate failed")
	tests := []struct {
		name     string
		waitErr  error
		readyErr error
	}{
		{name: "wait for identity", waitErr: waitErr},
		{name: "mark ready", readyErr: readyErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := newLifecycleHarness()
			lifecycle.waitErr = tt.waitErr
			lifecycle.readyErr = tt.readyErr
			lifecycle.finishOnAbort = false
			isolator := &lifecycleHarnessIsolator{
				lifecycle: lifecycle,
				configure: func(cmd *exec.Cmd) {
					cmd.Args = []string{
						cmd.Path,
						"-c",
						"trap '' TERM; while :; do sleep 1; done",
					}
				},
			}
			upperMgr, err := isolation.NewUpperManager(t.TempDir(), 8<<30)
			if err != nil {
				t.Fatal(err)
			}
			runner := &IsolatedRunner{
				ctrl:     NewController("", ""),
				isolator: isolator,
				upperMgr: upperMgr,
			}

			originalSignal := signalSessionProcessGroup
			originalTimeout := isolatedSessionStopTimeout
			signalSessionProcessGroup = func(int, syscall.Signal) error {
				return syscall.EPERM
			}
			isolatedSessionStopTimeout = 20 * time.Millisecond
			t.Cleanup(func() {
				signalSessionProcessGroup = originalSignal
				isolatedSessionStopTimeout = originalTimeout
			})
			t.Cleanup(func() {
				signalSessionProcessGroup = originalSignal
				lifecycle.finish(nil)
				runner.CollectIdle()
				if countPendingStartups(runner) == 0 &&
					isolator.cmd != nil &&
					isolator.cmd.Process != nil &&
					isolator.cmd.ProcessState == nil {
					_ = originalSignal(isolator.cmd.Process.Pid, syscall.SIGKILL)
				}
			})

			id, createErr := runner.CreateIsolatedSession(&IsolatedSessionOptions{
				WorkspacePath: t.TempDir(),
				WorkspaceMode: string(isolation.WorkspaceOverlay),
			})
			if id != "" {
				t.Fatalf("failed create returned session ID %q", id)
			}
			if !errors.Is(createErr, ErrSessionTeardownTimeout) {
				t.Fatalf("create error = %v, want %v", createErr, ErrSessionTeardownTimeout)
			}

			pendingID, session := singlePendingStartup(t, runner)

			if _, err := runner.GetIsolatedSession(pendingID); !errors.Is(err, ErrContextNotFound) {
				t.Fatalf("Get failed-start session error = %v", err)
			}
			if err := runner.DeleteIsolatedSession(pendingID); !errors.Is(err, ErrContextNotFound) {
				t.Fatalf("Delete failed-start session error = %v", err)
			}
			if err := runner.RunInIsolatedSession(
				context.Background(),
				pendingID,
				"echo must-not-run",
				nil,
				nil,
			); !errors.Is(err, ErrContextNotFound) {
				t.Fatalf("Run failed-start session error = %v", err)
			}
			if sessions := runner.ListIsolatedSessions(); len(sessions) != 0 {
				t.Fatalf("failed-start session leaked into List: %+v", sessions)
			}

			upperParent := filepath.Dir(session.upperDir)
			if _, err := os.Stat(upperParent); err != nil {
				t.Fatalf("pending startup lost its upper: %v", err)
			}
			if freed := upperMgr.Collect(); len(freed) != 0 {
				t.Fatalf("pending startup upper was released early: %v", freed)
			}

			runner.CollectIdle()
			if got := countPendingStartups(runner); got != 1 {
				t.Fatalf("pending startup count after failed retry = %d, want 1", got)
			}
			if _, err := os.Stat(upperParent); err != nil {
				t.Fatalf("failed retry lost its upper: %v", err)
			}

			signalSessionProcessGroup = originalSignal
			lifecycle.finish(nil)
			runner.CollectIdle()
			if got := countPendingStartups(runner); got != 0 {
				t.Fatalf("pending startup count after successful retry = %d, want 0", got)
			}
			if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
				t.Fatalf("successful retry retained upper %s: %v", upperParent, err)
			}
			if session.cmd == nil || session.cmd.ProcessState == nil {
				t.Fatal("successful retry returned before reaping failed workload")
			}
		})
	}
}

func TestDeleteTimeoutRetainsSessionAndUpperForRetry(t *testing.T) {
	runner := newTestRunner(t)
	upperID, upperDir, workDir, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	processWaited := make(chan struct{})
	doneCh := make(chan struct{})
	session := &isolatedSession{
		id:            "delete-timeout",
		opts:          &IsolatedSessionOptions{IdleTimeoutSeconds: 1},
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		processWaited: processWaited,
		doneCh:        doneCh,
		upperID:       upperID,
		upperDir:      upperDir,
		workDir:       workDir,
	}
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	originalSignal := signalSessionProcessGroup
	originalTimeout := isolatedSessionStopTimeout
	signalSessionProcessGroup = func(int, syscall.Signal) error { return syscall.EPERM }
	isolatedSessionStopTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		signalSessionProcessGroup = originalSignal
		isolatedSessionStopTimeout = originalTimeout
	})

	err = runner.DeleteIsolatedSession(session.id)
	if !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("Delete error = %v, want %v", err, ErrSessionTeardownTimeout)
	}
	if runner.lookup(session.id) != session {
		t.Fatal("timed-out Delete lost session ownership")
	}
	if freed := runner.upperMgr.Collect(); len(freed) != 0 {
		t.Fatalf("timed-out Delete released upper early: %v", freed)
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); err != nil {
		t.Fatalf("timed-out Delete removed upper: %v", err)
	}

	close(processWaited)
	close(doneCh)
	if err := runner.DeleteIsolatedSession(session.id); err != nil {
		t.Fatal(err)
	}
	if runner.lookup(session.id) != nil {
		t.Fatal("successful retry retained session")
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); !os.IsNotExist(err) {
		t.Fatalf("successful retry retained upper: %v", err)
	}
}

func TestFilesystemOperationLeaseRetainsUpperUntilIdleGCRetry(t *testing.T) {
	runner := newTestRunner(t)
	upperID, upperDir, workDir, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	canonicalUpperDir, err := filepath.EvalSymlinks(upperDir)
	if err != nil {
		t.Fatal(err)
	}
	workspacePath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := &isolatedSession{
		id: "filesystem-operation-drain",
		opts: &IsolatedSessionOptions{
			WorkspacePath: workspacePath,
			WorkspaceMode: string(isolation.WorkspaceOverlay),
		},
		processWaited: make(chan struct{}),
		doneCh:        make(chan struct{}),
		upperID:       upperID,
		upperDir:      canonicalUpperDir,
		workDir:       workDir,
		lastRunAt:     time.Now(),
	}
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	originalTimeout := isolatedSessionStopTimeout
	isolatedSessionStopTimeout = 20 * time.Millisecond
	releaseUpload := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() {
			close(releaseUpload)
		})
		isolatedSessionStopTimeout = originalTimeout
		runner.CollectIdle()
	})

	view, err := runner.GetMergedView(session.id)
	if err != nil {
		t.Fatal(err)
	}
	reader := &blockingUploadReader{
		started: make(chan struct{}),
		release: releaseUpload,
	}
	uploadDone := make(chan error, 1)
	go func() {
		_, uploadErr := view.WriteFileReader("nested/upload.txt", reader, 0o600)
		uploadDone <- uploadErr
	}()
	select {
	case <-reader.started:
	case uploadErr := <-uploadDone:
		t.Fatalf("filesystem upload failed before acquiring its lease: %v", uploadErr)
	case <-time.After(time.Second):
		t.Fatal("filesystem upload did not acquire its operation lease")
	}

	err = runner.DeleteIsolatedSession(session.id)
	if !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("Delete error = %v, want %v", err, ErrSessionTeardownTimeout)
	}
	if runner.lookup(session.id) != session {
		t.Fatal("Delete lost ownership while a filesystem operation was active")
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); err != nil {
		t.Fatalf("Delete removed an upper with an active filesystem operation: %v", err)
	}
	newView, release, leaseErr := runner.GetMergedViewWithLease(session.id)
	if !errors.Is(leaseErr, ErrSessionNotActive) || newView != nil || release != nil {
		t.Fatalf(
			"request admission after Delete = (viewNil:%v, releaseNil:%v, err:%v), want nil view/release and %v",
			newView == nil,
			release == nil,
			leaseErr,
			ErrSessionNotActive,
		)
	}
	if err := view.WriteFile("orphan.txt", []byte("must not write"), 0o600); !errors.Is(err, ErrSessionNotActive) {
		t.Fatalf("new filesystem operation after Delete error = %v, want %v", err, ErrSessionNotActive)
	}

	releaseOnce.Do(func() {
		close(releaseUpload)
	})
	select {
	case uploadErr := <-uploadDone:
		if uploadErr != nil {
			t.Fatalf("admitted upload failed: %v", uploadErr)
		}
	case <-time.After(time.Second):
		t.Fatal("filesystem upload did not drain")
	}

	runner.CollectIdle()
	if runner.lookup(session.id) != nil {
		t.Fatal("idle GC did not retry cleanup after filesystem operations drained")
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); !os.IsNotExist(err) {
		t.Fatalf("idle GC retained or recreated the upper: %v", err)
	}
}

func TestOpenFileRequestLeaseRetainsUpperUntilRelease(t *testing.T) {
	runner := newTestRunner(t)
	upperID, upperDir, workDir, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	canonicalUpperDir, err := filepath.EvalSymlinks(upperDir)
	if err != nil {
		t.Fatal(err)
	}
	workspacePath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(canonicalUpperDir, "download.txt"),
		[]byte("download"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	session := &isolatedSession{
		id: "open-file-request-lease",
		opts: &IsolatedSessionOptions{
			WorkspacePath: workspacePath,
			WorkspaceMode: string(isolation.WorkspaceOverlay),
		},
		processWaited: make(chan struct{}),
		doneCh:        make(chan struct{}),
		upperID:       upperID,
		upperDir:      canonicalUpperDir,
		workDir:       workDir,
		lastRunAt:     time.Now(),
	}
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	originalTimeout := isolatedSessionStopTimeout
	isolatedSessionStopTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		isolatedSessionStopTimeout = originalTimeout
		runner.CollectIdle()
	})

	view, release, err := runner.GetMergedViewWithLease(session.id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	err = runner.DeleteIsolatedSession(session.id)
	if !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("Delete error = %v, want %v", err, ErrSessionTeardownTimeout)
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); err != nil {
		t.Fatalf("Delete removed an upper with an admitted download: %v", err)
	}
	file, err := view.Open("download.txt")
	if err != nil {
		t.Fatalf("admitted download could not open after teardown began: %v", err)
	}
	t.Cleanup(func() {
		_ = file.Close()
	})
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "download" {
		t.Fatalf("read admitted download = %q, %v", data, err)
	}

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	runner.CollectIdle()
	if runner.lookup(session.id) != nil {
		t.Fatal("idle GC did not retry cleanup after the download lease released")
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); !os.IsNotExist(err) {
		t.Fatalf("idle GC retained upper after download release: %v", err)
	}
}

func TestIdleGCHoldsRunExclusionThroughDelete(t *testing.T) {
	runner := newTestRunner(t)
	processWaited := make(chan struct{})
	doneCh := make(chan struct{})
	session := &isolatedSession{
		id:            "idle-delete",
		opts:          &IsolatedSessionOptions{IdleTimeoutSeconds: 1},
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		processWaited: processWaited,
		doneCh:        doneCh,
		lastRunAt:     time.Now().Add(-time.Minute),
	}
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	originalSignal := signalSessionProcessGroup
	t.Cleanup(func() {
		signalSessionProcessGroup = originalSignal
	})
	observedRunExclusion := false
	signalSessionProcessGroup = func(int, syscall.Signal) error {
		if session.runMu.TryLock() {
			session.runMu.Unlock()
			t.Error("idle GC released run exclusion before signalling teardown")
		} else {
			observedRunExclusion = true
		}
		close(processWaited)
		close(doneCh)
		return nil
	}

	runner.CollectIdle()
	if !observedRunExclusion {
		t.Fatal("idle GC did not exercise process teardown")
	}
	if runner.lookup(session.id) != nil {
		t.Fatal("idle GC retained expired session")
	}
}

func TestIdleGCSkipsActiveRun(t *testing.T) {
	runner := newTestRunner(t)
	session := &isolatedSession{
		id:        "active-run",
		opts:      &IsolatedSessionOptions{IdleTimeoutSeconds: 1},
		doneCh:    make(chan struct{}),
		lastRunAt: time.Now().Add(-time.Minute),
	}
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	session.runMu.Lock()
	session.mu.Lock()
	session.lastRunAt = time.Now()
	session.mu.Unlock()
	runner.CollectIdle()
	session.runMu.Unlock()

	if runner.lookup(session.id) != session {
		t.Fatal("idle GC deleted a session with an active Run")
	}
	runner.ctrl.isolatedSessionMap.Delete(session.id)
}

func TestIdleGCCleansProcessExitBeforeLifecycleDrain(t *testing.T) {
	runner := newTestRunner(t)
	processWaited := make(chan struct{})
	close(processWaited)
	session := &isolatedSession{
		id:            "process-exited-drain-stuck",
		opts:          &IsolatedSessionOptions{IdleTimeoutSeconds: 0},
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		processWaited: processWaited,
		doneCh:        make(chan struct{}),
	}
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	originalTimeout := isolatedSessionStopTimeout
	isolatedSessionStopTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		isolatedSessionStopTimeout = originalTimeout
	})

	runner.CollectIdle()
	if !session.stopping.Load() {
		t.Fatal("idle GC ignored a reaped process while lifecycle drain was stuck")
	}
	if runner.lookup(session.id) != session {
		t.Fatal("timed-out cleanup lost session ownership")
	}
}

func TestRunWaitsForCancellationWatcherBeforeUnlock(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	scriptReceived := make(chan struct{})
	allowResponse := make(chan struct{})
	stdin := &markerResponseWriter{
		stdout:         stdoutWriter,
		scriptReceived: scriptReceived,
		allowResponse:  allowResponse,
	}
	session := &isolatedSession{
		id:            "watcher-join",
		opts:          &IsolatedSessionOptions{},
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		stdin:         stdin,
		stdout:        stdoutReader,
		processWaited: make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	runner := newTestRunner(t)
	runner.ctrl.isolatedSessionMap.Store(session.id, session)

	originalSignal := signalSessionProcessGroup
	signalEntered := make(chan struct{})
	releaseSignal := make(chan struct{})
	signalSessionProcessGroup = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGINT {
			close(signalEntered)
			<-releaseSignal
		}
		return nil
	}
	t.Cleanup(func() {
		signalSessionProcessGroup = originalSignal
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- runner.RunInIsolatedSession(
			ctx,
			session.id,
			"echo completed",
			nil,
			nil,
		)
	}()

	<-signalEntered
	<-scriptReceived
	close(allowResponse)
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before its cancellation watcher exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if session.runMu.TryLock() {
		session.runMu.Unlock()
		t.Fatal("Run released runMu while its cancellation watcher was active")
	}

	close(releaseSignal)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation watcher exited")
	}
	runner.ctrl.isolatedSessionMap.Delete(session.id)
}

type markerResponseWriter struct {
	stdout         *io.PipeWriter
	scriptReceived chan struct{}
	allowResponse  chan struct{}
	closeOnce      sync.Once
}

type blockingUploadReader struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *blockingUploadReader) Read(buffer []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
	})
	<-r.release
	return copy(buffer, "uploaded"), io.EOF
}

func (w *markerResponseWriter) Write(payload []byte) (int, error) {
	script := string(payload)
	markerIndex := strings.LastIndex(script, isolatedRunEndMarkerPrefix)
	if markerIndex < 0 {
		return 0, errors.New("run script did not contain an end marker")
	}
	marker := strings.Fields(script[markerIndex:])[0]
	w.closeOnce.Do(func() {
		close(w.scriptReceived)
	})
	<-w.allowResponse
	go func() {
		_, _ = fmt.Fprintf(w.stdout, "%s 0\n", marker)
	}()
	return len(payload), nil
}

func (*markerResponseWriter) Close() error {
	return nil
}

func singlePendingStartup(
	t *testing.T,
	runner *IsolatedRunner,
) (string, *isolatedSession) {
	t.Helper()
	var (
		id      string
		session *isolatedSession
		count   int
	)
	runner.pendingStartupCleanup.Range(func(key, value any) bool {
		count++
		id, _ = key.(string)
		session, _ = value.(*isolatedSession)
		return true
	})
	if count != 1 || id == "" || session == nil {
		t.Fatalf("pending startup entries = %d, id=%q session=%v", count, id, session)
	}
	return id, session
}

func countPendingStartups(runner *IsolatedRunner) int {
	count := 0
	runner.pendingStartupCleanup.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
