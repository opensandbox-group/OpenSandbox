//go:build linux

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

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func startReaperForTest(t *testing.T) {
	t.Helper()
	if initReaper != nil {
		t.Fatal("init reaper already running")
	}
	initReaper = newReaper()
	initReaper.start()
	go initReaper.run()
	t.Cleanup(func() {
		initReaper.stop()
		initReaper = nil
	})
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear within %s", path, timeout)
}

// exitStatusByPid observes a child's status with WNOWAIT without consuming it.
func exitStatusByPid(t *testing.T, pid int) (syscall.WaitStatus, bool) {
	t.Helper()
	var info siginfoWait
	_, _, errno := syscall.RawSyscall6(syscall.SYS_WAITID,
		uintptr(unix.P_PID), uintptr(pid),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unix.WEXITED|unix.WNOHANG|unix.WNOWAIT),
		0, 0)
	if errno == syscall.ECHILD {
		return 0, false
	}
	if errno != 0 {
		t.Fatalf("waitid observe: %v", errno)
	}
	return info.waitStatus(), true
}

func TestManagedProcessExitStatus(t *testing.T) {
	startReaperForTest(t)

	cmd := exec.Command("sh", "-c", "exit 7")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = mp.Wait()
	var exitCodeErr exitCoder
	if !errors.As(err, &exitCodeErr) || exitCodeErr.ExitCode() != 7 {
		t.Fatalf("Wait error = %v, want exit status 7", err)
	}
	if mp.ExitCode() != 7 {
		t.Fatalf("ExitCode = %d, want 7", mp.ExitCode())
	}
}

func TestManagedProcessExitZero(t *testing.T) {
	startReaperForTest(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Wait(); err != nil {
		t.Fatalf("Wait error = %v, want nil", err)
	}
}

func TestManagedProcessKilledBySignal(t *testing.T) {
	startReaperForTest(t)

	cmd := exec.Command("sh", "-c", "kill -TERM $$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Wait(); err == nil {
		t.Fatal("Wait error = nil, want signal error")
	}
	if mp.ExitCode() != -1 {
		t.Fatalf("ExitCode = %d, want -1 for signalled process", mp.ExitCode())
	}
}

func TestManagedProcessDispatchIsPerPid(t *testing.T) {
	startReaperForTest(t)

	codes := []int{0, 1, 7, 42, 255}
	var mps []*managedProcess
	for _, code := range codes {
		cmd := exec.Command("sh", "-c", exitScript(code))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		mp, err := launchManaged(cmd)
		if err != nil {
			t.Fatal(err)
		}
		mps = append(mps, mp)
	}
	// Wait in reverse order: a cross-delivered status would surface here.
	for i := len(mps) - 1; i >= 0; i-- {
		mp := mps[i]
		if err := mp.Wait(); err != nil && i == 0 {
			t.Fatalf("child %d (exit 0): Wait error = %v", i, err)
		}
		if got := mp.ExitCode(); got != codes[i] {
			t.Fatalf("child %d exit code = %d, want %d", i, got, codes[i])
		}
	}
}

func exitScript(code int) string {
	return "exit " + strconv.Itoa(code)
}

func TestReaperReapsOrphan(t *testing.T) {
	startReaperForTest(t)

	cmd := exec.Command("sh", "-c", "exit 3")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := exitStatusByPid(t, pid); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("orphan pid %d was not reaped by the reaper", pid)
}

func TestPreReapBarrierRunsBeforeWaitReturns(t *testing.T) {
	startReaperForTest(t)

	barrierRan := make(chan struct{})
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mp, err := launchManaged(cmd, withPreReap(func() { close(barrierRan) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-barrierRan:
	default:
		t.Fatal("pre-reap barrier did not run before Wait returned")
	}
}

func TestInitExitCode(t *testing.T) {
	startReaperForTest(t)

	tests := []struct {
		script string
		want   int
	}{
		{"exit 7", 7},
		{"kill -TERM $$", 128 + 15},
		{"exit 0", 0},
	}
	for _, tt := range tests {
		cmd := exec.Command("sh", "-c", tt.script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		mp, err := launchManaged(cmd)
		if err != nil {
			t.Fatal(err)
		}
		_ = mp.Wait()
		if got := initExitCode(mp); got != tt.want {
			t.Fatalf("initExitCode(%q) = %d, want %d", tt.script, got, tt.want)
		}
	}
}

func TestForwardSignalToWorkload(t *testing.T) {
	startReaperForTest(t)

	dir := t.TempDir()
	marker := filepath.Join(dir, "usr1")
	installed := filepath.Join(dir, "installed")
	cmd := exec.Command("sh", "-c", "trap 'touch \"$MARKER\"' USR1; touch \"$INSTALLED\"; while :; do sleep 0.05; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "MARKER="+marker, "INSTALLED="+installed)
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = killGroup(mp.pid(), syscall.SIGKILL)
		_ = mp.Wait()
	}()

	// Wait until the trap is installed; a signal sent earlier is dropped while
	// the shell initializes (dash blocks signals during startup).
	waitForFile(t, installed, 3*time.Second)
	if err := killGroup(mp.pid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, marker, 3*time.Second)
}

func TestStopChildrenGraceThenKill(t *testing.T) {
	startReaperForTest(t)
	oldGrace := initShutdownGrace
	initShutdownGrace = 500 * time.Millisecond
	defer func() { initShutdownGrace = oldGrace }()

	dir := t.TempDir()
	termMarker := filepath.Join(dir, "term")
	termInstalled := filepath.Join(dir, "term-installed")
	termCmd := exec.Command("sh", "-c",
		"trap 'touch \"$MARKER\"; exit 0' TERM; touch \"$INSTALLED\"; while :; do sleep 0.05; done")
	termCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	termCmd.Env = append(os.Environ(), "MARKER="+termMarker, "INSTALLED="+termInstalled)
	termMp, err := launchManaged(termCmd)
	if err != nil {
		t.Fatal(err)
	}

	// This child ignores SIGTERM and must be SIGKILLed after the grace period.
	stubbornInstalled := filepath.Join(dir, "stubborn-installed")
	stubbornCmd := exec.Command("sh", "-c",
		"trap '' TERM; touch \"$INSTALLED\"; while :; do sleep 0.05; done")
	stubbornCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stubbornCmd.Env = append(os.Environ(), "INSTALLED="+stubbornInstalled)
	stubbornMp, err := launchManaged(stubbornCmd)
	if err != nil {
		t.Fatal(err)
	}

	waitForFile(t, termInstalled, 3*time.Second)
	waitForFile(t, stubbornInstalled, 3*time.Second)

	stopChildrenExcept(nil)

	waitForFile(t, termMarker, 2*time.Second)
	for _, mp := range []*managedProcess{termMp, stubbornMp} {
		select {
		case <-mp.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("child pid %d was not stopped", mp.pid())
		}
	}
	if ws := termMp.exitStatus(); !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("term-trapping child status = %#v, want clean exit", ws)
	}
}

func TestInitModeReport(t *testing.T) {
	if mode, _ := InitModeReport(); mode != "none" {
		t.Fatalf("InitModeReport before init = %q, want none", mode)
	}
	startReaperForTest(t)
	if mode, shield := InitModeReport(); mode != "subreaper" || shield {
		t.Fatalf("InitModeReport after init = %q/%v, want subreaper/false (test is not PID 1)", mode, shield)
	}
}

func TestManagedProcessWithoutReaperUsesCmdWait(t *testing.T) {
	if initReaper != nil {
		t.Fatal("reaper unexpectedly running")
	}
	cmd := exec.Command("sh", "-c", "exit 4")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mp, err := launchManaged(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Wait(); err == nil {
		t.Fatal("Wait error = nil, want exit status 4")
	}
	if mp.ExitCode() != 4 {
		t.Fatalf("ExitCode = %d, want 4", mp.ExitCode())
	}
}
