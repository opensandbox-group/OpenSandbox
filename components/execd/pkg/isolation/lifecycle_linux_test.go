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

package isolation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newStatusTestLifecycle(
	t *testing.T,
) (*bwrapLifecycle, *os.File) {
	t.Helper()
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		statusReader.Close()
		statusWriter.Close()
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		statusReader.Close()
		statusWriter.Close()
		setupReader.Close()
		setupWriter.Close()
		t.Fatal(err)
	}
	setupReader.Close()
	controlChildFD := int(controlChild.Fd())
	controlChild.Close()
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		controlChildFD,
		controlSocketInode,
		true,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
	})
	return lifecycle, statusWriter
}

func waitForDrain(t *testing.T, lifecycle *bwrapLifecycle) {
	t.Helper()
	select {
	case <-lifecycle.DrainDone():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bwrap status drain")
	}
}

func TestBwrapStatusDrainAcceptsUnknownObjectsAndCapturesExit(t *testing.T) {
	lifecycle, writer := newStatusTestLifecycle(t)
	if _, err := writer.Write([]byte(
		"{\"future-event\":{\"value\":1}}\n" +
			"{\"child-pid\":123,\"net-namespace\":456,\"future-member\":true}\n" +
			"{\"another-future-event\":null}\n" +
			"{\"exit-code\":0,\"future-member\":\"ok\"}\n",
	)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	status := <-lifecycle.sandboxStatus
	if status.err != nil {
		t.Fatal(status.err)
	}
	if status.pid != 123 || status.netNamespaceID != 456 {
		t.Fatalf("status = %+v", status)
	}
	waitForDrain(t, lifecycle)
	if err := lifecycle.DrainError(); err != nil {
		t.Fatal(err)
	}
	if exitCode, ok := lifecycle.ExitCode(); !ok || exitCode != 0 {
		t.Fatalf("ExitCode = (%d, %v), want (0, true)", exitCode, ok)
	}
}

func TestBwrapStatusDrainRejectsInvalidStreams(t *testing.T) {
	oversized := strings.Repeat(" ", maxBwrapStatusDocumentBytes+1) + "\n"
	tooMany := strings.Repeat("{}\n", maxBwrapStatusDocuments+1)
	tests := []struct {
		name       string
		stream     string
		wantError  string
		childFirst bool
	}{
		{
			name:      "eof before child",
			stream:    "{\"future\":true}\n",
			wantError: "EOF before child-pid",
		},
		{
			name:       "eof before exit",
			stream:     "{\"child-pid\":1,\"net-namespace\":2}\n",
			wantError:  "EOF before exit-code",
			childFirst: true,
		},
		{
			name:      "malformed json",
			stream:    "{not-json}\n",
			wantError: "decode bwrap status",
		},
		{
			name:      "non-object",
			stream:    "null\n",
			wantError: "not an object",
		},
		{
			name:      "missing namespace",
			stream:    "{\"child-pid\":1}\n",
			wantError: "omitted net-namespace",
		},
		{
			name:      "zero pid",
			stream:    "{\"child-pid\":0,\"net-namespace\":2}\n",
			wantError: "child-pid value 0 is out of range",
		},
		{
			name:      "fractional pid",
			stream:    "{\"child-pid\":1.5,\"net-namespace\":2}\n",
			wantError: "not an unsigned integer",
		},
		{
			name: "duplicate child",
			stream: "{\"child-pid\":1,\"net-namespace\":2}\n" +
				"{\"child-pid\":3,\"net-namespace\":4}\n",
			wantError:  "repeated child-pid",
			childFirst: true,
		},
		{
			name: "duplicate exit",
			stream: "{\"child-pid\":1,\"net-namespace\":2}\n" +
				"{\"exit-code\":0}\n{\"exit-code\":1}\n",
			wantError:  "repeated exit-code",
			childFirst: true,
		},
		{
			name:      "oversized document",
			stream:    oversized,
			wantError: "token too long",
		},
		{
			name:      "too many documents",
			stream:    tooMany,
			wantError: "exceeded 1024 documents",
		},
		{
			name: "exit out of range",
			stream: "{\"child-pid\":1,\"net-namespace\":2}\n" +
				"{\"exit-code\":256}\n",
			wantError:  "exit-code value 256 is out of range",
			childFirst: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, writer := newStatusTestLifecycle(t)
			writeDone := make(chan struct{})
			go func() {
				_, _ = writer.Write([]byte(test.stream))
				_ = writer.Close()
				close(writeDone)
			}()
			if !test.childFirst {
				status := <-lifecycle.sandboxStatus
				if status.err == nil {
					t.Fatalf("sandbox status unexpectedly succeeded: %+v", status)
				}
			}
			waitForDrain(t, lifecycle)
			<-writeDone
			err := lifecycle.DrainError()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("DrainError = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestLifecycleUsesCredentialsAndProcIdentityBeforeReady(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		int(controlChild.Fd()),
		controlSocketInode,
		true,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	})

	sandboxPID := os.Getppid()
	netNamespaceID, err := readNetNamespaceID(sandboxPID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(
		statusWriter,
		"{\"child-pid\":%d,\"net-namespace\":%d}\n",
		sandboxPID,
		netNamespaceID,
	); err != nil {
		t.Fatal(err)
	}

	setupReleased := make(chan byte, 1)
	go func() {
		var release [1]byte
		n, _ := setupReader.Read(release[:])
		if n == 1 {
			setupReleased <- release[0]
		}
	}()
	waitingSent := make(chan struct{})
	go func() {
		<-setupReleased
		_, _ = controlChild.Write([]byte(sessionGateWaitingFrame))
		close(waitingSent)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity, err := lifecycle.WaitForIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-waitingSent
	if identity.PID != os.Getpid() {
		t.Fatalf("workload PID = %d, want credential PID %d", identity.PID, os.Getpid())
	}
	if identity.SandboxPID != sandboxPID {
		t.Fatalf("sandbox PID = %d, want %d", identity.SandboxPID, sandboxPID)
	}
	if identity.NetNamespaceID != netNamespaceID {
		t.Fatalf("net namespace = %d, want %d", identity.NetNamespaceID, netNamespaceID)
	}
	if identity.ProcessStartTimeTicks == 0 {
		t.Fatal("workload start time was not captured")
	}

	readyRead := make(chan string, 1)
	go func() {
		buffer := make([]byte, len(sessionGateReadyFrame)+1)
		n, _ := controlChild.Read(buffer)
		readyRead <- string(buffer[:n])
	}()
	if err := lifecycle.MarkReady(); err != nil {
		t.Fatal(err)
	}
	if got := <-readyRead; got != sessionGateReadyFrame {
		t.Fatalf("ready frame = %q, want %q", got, sessionGateReadyFrame)
	}
	if _, err := statusWriter.Write([]byte("{\"exit-code\":0}\n")); err != nil {
		t.Fatal(err)
	}
	if err := statusWriter.Close(); err != nil {
		t.Fatal(err)
	}
	waitForDrain(t, lifecycle)
	if err := lifecycle.DrainError(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleDerivesSharedNetworkIdentityWhenStatusOmitsNamespace(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		int(controlChild.Fd()),
		controlSocketInode,
		false,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	})

	sandboxPID := os.Getppid()
	expectedNamespaceID, err := readNetNamespaceID(sandboxPID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(
		statusWriter,
		"{\"child-pid\":%d}\n",
		sandboxPID,
	); err != nil {
		t.Fatal(err)
	}

	setupReleased := make(chan struct{})
	go func() {
		var release [1]byte
		if n, _ := setupReader.Read(release[:]); n == 1 {
			close(setupReleased)
			_, _ = controlChild.Write([]byte(sessionGateWaitingFrame))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	identity, err := lifecycle.WaitForIdentity(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	<-setupReleased
	if identity.NetNamespaceID != expectedNamespaceID {
		t.Fatalf(
			"shared network namespace = %d, want %d",
			identity.NetNamespaceID,
			expectedNamespaceID,
		)
	}

	readyRead := make(chan string, 1)
	go func() {
		buffer := make([]byte, len(sessionGateReadyFrame)+1)
		n, _ := controlChild.Read(buffer)
		readyRead <- string(buffer[:n])
	}()
	if err := lifecycle.MarkReady(); err != nil {
		t.Fatal(err)
	}
	if got := <-readyRead; got != sessionGateReadyFrame {
		t.Fatalf("ready frame = %q, want %q", got, sessionGateReadyFrame)
	}
	if _, err := statusWriter.Write([]byte("{\"exit-code\":0}\n")); err != nil {
		t.Fatal(err)
	}
	if err := statusWriter.Close(); err != nil {
		t.Fatal(err)
	}
	waitForDrain(t, lifecycle)
	if err := lifecycle.DrainError(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleNamespaceMismatchFailsBeforeNativeReady(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		int(controlChild.Fd()),
		controlSocketInode,
		true,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	})

	sandboxPID := os.Getppid()
	netNamespaceID, err := readNetNamespaceID(sandboxPID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(
		statusWriter,
		"{\"child-pid\":%d,\"net-namespace\":%d}\n",
		sandboxPID,
		netNamespaceID+1,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = lifecycle.WaitForIdentity(ctx)
	if err == nil || !strings.Contains(err.Error(), "network namespace mismatch") {
		t.Fatalf("WaitForIdentity error = %v", err)
	}

	var setupByte [1]byte
	if n, readErr := setupReader.Read(setupByte[:]); n != 0 || readErr == nil {
		t.Fatalf("setup gate = (%d, %v), want EOF without readiness", n, readErr)
	}
	var controlByte [1]byte
	if n, readErr := controlChild.Read(controlByte[:]); n != 0 || readErr == nil {
		t.Fatalf("control gate = (%d, %v), want EOF without readiness", n, readErr)
	}
}

func TestLifecycleStatusFailureAfterIdentityDeniesReadyWithoutDeadlock(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		int(controlChild.Fd()),
		controlSocketInode,
		true,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	})

	sandboxPID := os.Getppid()
	netNamespaceID, err := readNetNamespaceID(sandboxPID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(
		statusWriter,
		"{\"child-pid\":%d,\"net-namespace\":%d}\n",
		sandboxPID,
		netNamespaceID,
	); err != nil {
		t.Fatal(err)
	}
	go func() {
		var release [1]byte
		if n, _ := setupReader.Read(release[:]); n == 1 {
			_, _ = controlChild.Write([]byte(sessionGateWaitingFrame))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = lifecycle.WaitForIdentity(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statusWriter.Write([]byte("{malformed}\n")); err != nil {
		t.Fatal(err)
	}
	waitForDrain(t, lifecycle)
	if err := lifecycle.MarkReady(); !errors.Is(err, errWorkloadLifecycleAborted) {
		t.Fatalf("MarkReady = %v, want lifecycle aborted", err)
	}
	var controlByte [1]byte
	if n, readErr := controlChild.Read(controlByte[:]); n != 0 || readErr == nil {
		t.Fatalf("native gate = (%d, %v), want EOF without READY", n, readErr)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- lifecycle.Close()
	}()
	select {
	case closeErr := <-closeDone:
		if closeErr == nil || !strings.Contains(closeErr.Error(), "decode bwrap status") {
			t.Fatalf("Close error = %v, want status decode failure", closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked after identity-to-ready lifecycle failure")
	}
}

func TestLifecycleContextCancellationClosesAllGates(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		int(controlChild.Fd()),
		controlSocketInode,
		true,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = lifecycle.WaitForIdentity(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForIdentity = %v, want context.Canceled", err)
	}
	waitForDrain(t, lifecycle)
}

func TestLifecycleAbortedEOFIsExpectedTeardown(t *testing.T) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		int(controlChild.Fd()),
		controlSocketInode,
		true,
	)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	})

	// Model the peer EOF race after Abort has made teardown irreversible but
	// before closing the local reader becomes visible to Scanner.
	lifecycle.mu.Lock()
	lifecycle.state = lifecycleAborted
	if lifecycle.control != nil {
		_ = lifecycle.control.Close()
		lifecycle.control = nil
	}
	if lifecycle.setup != nil {
		_ = lifecycle.setup.Close()
		lifecycle.setup = nil
	}
	lifecycle.mu.Unlock()
	if err := statusWriter.Close(); err != nil {
		t.Fatal(err)
	}
	waitForDrain(t, lifecycle)
	if err := lifecycle.Close(); err != nil {
		t.Fatalf("Close after aborted EOF = %v, want nil", err)
	}
}

func TestLifecycleAbortAndCloseAreConcurrentAndDoNotLeakFDs(t *testing.T) {
	countFDs := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	baseline := countFDs()

	const iterations = 32
	for iteration := 0; iteration < iterations; iteration++ {
		statusReader, statusWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		setupReader, setupWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
		if err != nil {
			t.Fatal(err)
		}
		lifecycle := newBwrapLifecycle(
			statusReader,
			setupWriter,
			controlParent,
			int(controlChild.Fd()),
			controlSocketInode,
			true,
		)

		var waitGroup sync.WaitGroup
		for caller := 0; caller < 8; caller++ {
			waitGroup.Add(1)
			go func(caller int) {
				defer waitGroup.Done()
				if caller%2 == 0 {
					lifecycle.Abort()
				} else {
					_ = lifecycle.Close()
				}
			}(caller)
		}
		waitGroup.Wait()
		_ = lifecycle.Close()
		_ = statusWriter.Close()
		_ = setupReader.Close()
		_ = controlChild.Close()
	}

	if after := countFDs(); after != baseline {
		t.Fatalf(
			"lifecycle descriptor count changed after %d iterations: before=%d after=%d",
			iterations,
			baseline,
			after,
		)
	}
}

func TestOpenSessionGateAcceptsManagedRuntimePath(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed-gate")
	if err := os.WriteFile(managed, []byte("managed"), 0o755); err != nil {
		t.Fatal(err)
	}

	file, err := openSessionGatePath(managed)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, 16)
	n, err := file.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:n]); got != "managed" {
		t.Fatalf("opened gate contents = %q, want managed path", got)
	}
}

func TestOpenSessionGateRejectsMissingManagedRuntimePath(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "missing-managed-gate")

	file, err := openSessionGatePath(managed)
	if file != nil {
		file.Close()
		t.Fatal("missing managed gate unexpectedly opened")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing managed gate error = %v", err)
	}
}

func TestOpenSessionGateRejectsUnsafeManagedRuntimePath(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed-gate")
	if err := os.WriteFile(managed, []byte("unsafe"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(managed, 0o777); err != nil {
		t.Fatal(err)
	}

	file, err := openSessionGatePath(managed)
	if file != nil {
		file.Close()
		t.Fatal("unsafe managed gate unexpectedly opened")
	}
	if err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("open unsafe managed gate error = %v", err)
	}
}

func TestSessionGateHelperFailsClosedAndExecutesOnlyAfterReady(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler is unavailable")
	}
	source := filepath.Join("..", "..", "native", "session-gate.c")
	binary := filepath.Join(t.TempDir(), "opensandbox-session-gate")
	compile := exec.Command(
		compiler,
		"-O2",
		"-Wall",
		"-Wextra",
		"-Werror",
		"-o",
		binary,
		source,
	)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile native gate: %v\n%s", err, output)
	}

	tests := []struct {
		name       string
		reply      *string
		script     string
		wantExit   int
		wantSignal syscall.Signal
		wantMarker bool
	}{
		{name: "control eof", wantExit: 125},
		{name: "wrong frame", reply: stringPointer("WRONG"), wantExit: 125},
		{
			name:       "exact ready",
			reply:      stringPointer(sessionGateReadyFrame),
			wantExit:   0,
			wantMarker: true,
		},
		{
			name:       "exact ready preserves default sigpipe",
			reply:      stringPointer(sessionGateReadyFrame),
			script:     "kill -s PIPE $$; exit 42",
			wantSignal: syscall.SIGPIPE,
		},
		{
			name:     "ready with trailing byte",
			reply:    stringPointer(sessionGateReadyFrame + "X"),
			wantExit: 125,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controlParent, controlChild, _, err := newGateSocketpair()
			if err != nil {
				t.Fatal(err)
			}
			defer controlParent.Close()
			gateExecutable, err := os.Open(binary)
			if err != nil {
				controlChild.Close()
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "executed")
			script := "printf executed > " + marker
			if test.script != "" {
				script = test.script
			}
			command := exec.Command(
				binary,
				"3",
				"4",
				"--",
				"/bin/sh",
				"-c",
				script,
			)
			command.ExtraFiles = []*os.File{controlChild, gateExecutable}
			if err := command.Start(); err != nil {
				controlChild.Close()
				gateExecutable.Close()
				t.Fatal(err)
			}
			controlChild.Close()
			gateExecutable.Close()

			payload := make([]byte, len(sessionGateWaitingFrame)+1)
			oob := make([]byte, 256)
			n, oobn, flags, _, err := controlParent.ReadMsgUnix(payload, oob)
			if err != nil {
				t.Fatal(err)
			}
			if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 ||
				string(payload[:n]) != sessionGateWaitingFrame {
				t.Fatalf("waiting frame = (%q, flags=%d)", payload[:n], flags)
			}
			credentials, err := parseSingleUnixCredentials(oob[:oobn])
			if err != nil {
				t.Fatal(err)
			}
			if int(credentials.Pid) != command.Process.Pid {
				t.Fatalf(
					"credential pid = %d, helper pid = %d",
					credentials.Pid,
					command.Process.Pid,
				)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("workload executed before READY: %v", err)
			}

			if test.reply == nil {
				if err := controlParent.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := controlParent.Write([]byte(*test.reply)); err != nil {
				t.Fatal(err)
			}
			waitErr := command.Wait()
			if test.wantSignal != 0 {
				var exitErr *exec.ExitError
				if !errors.As(waitErr, &exitErr) {
					t.Fatalf("workload was not terminated by %s: %v", test.wantSignal, waitErr)
				}
				waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
				if !ok || !waitStatus.Signaled() || waitStatus.Signal() != test.wantSignal {
					t.Fatalf(
						"workload status = %v, want signal %s",
						waitStatus,
						test.wantSignal,
					)
				}
			} else {
				exitCode := 0
				if waitErr != nil {
					var exitErr *exec.ExitError
					if !errors.As(waitErr, &exitErr) {
						t.Fatal(waitErr)
					}
					exitCode = exitErr.ExitCode()
				}
				if exitCode != test.wantExit {
					t.Fatalf("exit code = %d, want %d", exitCode, test.wantExit)
				}
			}
			_, markerErr := os.Stat(marker)
			if test.wantMarker && markerErr != nil {
				t.Fatalf("ready workload did not execute: %v", markerErr)
			}
			if !test.wantMarker && !os.IsNotExist(markerErr) {
				t.Fatalf("denied workload executed: %v", markerErr)
			}
		})
	}
}

func TestLifecycleArgvExecutesGateDescriptorAfterRestoringProc(t *testing.T) {
	uid := uint32(65534)
	gid := uint32(65534)
	opts := WrapOptions{
		Profile:       ProfileStrict,
		Workspace:     WorkspaceSpec{Path: "/workspace", Mode: WorkspaceRW},
		UpperDir:      "/var/lib/opensandbox/upper/session/upper",
		ExtraWritable: []string{"/data"},
		UidMode:       UidModeSetpriv,
		Uid:           &uid,
		Gid:           &gid,
		Binds: []BindMount{{
			Source:   "/source",
			Dest:     "/mounted",
			ReadOnly: true,
		}},
	}
	argv, err := buildArgvWithLifecycle(
		opts,
		"3",
		&bwrapLifecycleArgv{
			gateExecFD: "4",
			statusFD:   "5",
			blockFD:    "6",
			controlFD:  "7",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, required := range []string{
		"--proc /proc",
		"--block-fd 6",
		"--json-status-fd 5",
		"/proc/self/fd/4 7 4 --",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("argv missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "--ro-bind-fd") {
		t.Fatalf("lifecycle gate must execute by descriptor, not mount path: %v", argv)
	}
	gateIndex := indexArgvSequence(argv, "/proc/self/fd/4", "7", "4", "--")
	setprivIndex := indexArgvSequence(
		argv,
		"setpriv",
		"--reuid=65534",
		"--regid=65534",
		"--clear-groups",
	)
	if gateIndex < 0 || setprivIndex < 0 || gateIndex >= setprivIndex {
		t.Fatalf("lifecycle gate must run before setpriv: %v", argv)
	}
	procIndex := indexArgvSequence(argv, "--proc", "/proc")
	if procIndex < 0 {
		t.Fatalf("trusted proc mount missing: %v", argv)
	}
	for name, sequence := range map[string][]string{
		"workspace":      {"--bind", "/workspace", "/workspace"},
		"upper root":     {"--tmpfs", "/var/lib/opensandbox/upper"},
		"extra writable": {"--bind", "/data", "/data"},
		"explicit bind":  {"--ro-bind", "/source", "/mounted"},
	} {
		index := indexArgvSequence(argv, sequence...)
		if index < 0 {
			t.Fatalf("%s mount missing: %v", name, argv)
		}
		if procIndex < index {
			t.Fatalf("trusted proc mount must follow %s mount: %v", name, argv)
		}
	}
}

func TestBwrapLifecycleEndToEnd(t *testing.T) {
	originalBwrapPath := bwrapPath
	bwrapPath = findBwrap()
	originalSeccompBPF := seccompBPF
	seccompBPF = nil
	t.Cleanup(func() {
		bwrapPath = originalBwrapPath
		seccompBPF = originalSeccompBPF
	})
	if bwrapPath == "" {
		t.Skip("bubblewrap is unavailable")
	}
	gate, err := openSessionGate()
	if err != nil {
		t.Skipf("native workload gate is unavailable: %v", err)
	}
	_ = gate.Close()

	unprivilegedID := uint32(65534)
	tests := []struct {
		name       string
		markReady  bool
		shareNet   bool
		gateAlias  bool
		uid        *uint32
		gid        *uint32
		wantMarker bool
	}{
		{
			name:       "private network ready executes workload",
			markReady:  true,
			wantMarker: true,
		},
		{
			name:       "shared network ready executes workload",
			markReady:  true,
			shareNet:   true,
			wantMarker: true,
		},
		{
			name:      "non-root setpriv ready executes workload",
			markReady: true,
			shareNet:  true,
			uid:       &unprivilegedID,
			gid:       &unprivilegedID,
		},
		{
			name:       "caller symlink alias cannot replace gate",
			markReady:  true,
			gateAlias:  true,
			wantMarker: true,
		},
		{
			name:       "control eof denies workload",
			markReady:  false,
			wantMarker: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			marker := filepath.Join(workspace, "executed")
			fakeGateMarker := filepath.Join(workspace, "fake-gate-executed")
			var binds []BindMount
			if test.gateAlias {
				fakeProc := filepath.Join(workspace, "fake-proc")
				fakeFDDir := filepath.Join(fakeProc, "self", "fd")
				if err := os.MkdirAll(fakeFDDir, 0o755); err != nil {
					t.Fatal(err)
				}
				fakeGate := filepath.Join(fakeFDDir, "3")
				fakeGateScript := fmt.Sprintf(
					"#!/bin/sh\nprintf fake > %q\nexit 125\n",
					fakeGateMarker,
				)
				if err := os.WriteFile(fakeGate, []byte(fakeGateScript), 0o755); err != nil {
					t.Fatal(err)
				}
				procAlias := filepath.Join(workspace, "proc-alias")
				if err := os.Symlink("/proc", procAlias); err != nil {
					t.Fatal(err)
				}
				binds = []BindMount{{
					Source:   fakeProc,
					Dest:     procAlias,
					ReadOnly: true,
				}}
			}
			script := "printf executed > " + marker + "; exit 7"
			if test.uid != nil {
				script = fmt.Sprintf(
					"test \"$(id -u)\" = %d; printf %%s \"$(id -u)\"; exit 7",
					*test.uid,
				)
			}
			command := exec.Command(
				"/bin/sh",
				"-c",
				script,
			)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			implementation := &bwrapImpl{
				probe: ProbeResult{Available: true},
			}
			lifecycleInterface, err := implementation.WrapWithLifecycle(
				command,
				WrapOptions{
					Profile: ProfileStrict,
					Workspace: WorkspaceSpec{
						Path: workspace,
						Mode: WorkspaceRW,
					},
					ShareNet:       test.shareNet,
					UidMode:        UidModeSetpriv,
					Uid:            test.uid,
					Gid:            test.gid,
					Binds:          binds,
					EnvPassthrough: EnvSpec{Mode: EnvModeDeny},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			lifecycle := lifecycleInterface.(*bwrapLifecycle)
			t.Cleanup(func() {
				lifecycle.Abort()
				_ = lifecycle.Close()
				if command.Process != nil {
					_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				}
			})

			if err := command.Start(); err != nil {
				for _, file := range command.ExtraFiles {
					_ = file.Close()
				}
				t.Fatal(err)
			}
			for _, file := range command.ExtraFiles {
				_ = file.Close()
			}
			command.ExtraFiles = nil

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			identity, err := lifecycle.WaitForIdentity(ctx)
			cancel()
			if err != nil {
				if test.gateAlias {
					_ = command.Wait()
					if _, markerErr := os.Stat(fakeGateMarker); !os.IsNotExist(markerErr) {
						t.Fatalf(
							"caller-controlled proc alias executed fake gate: %v",
							markerErr,
						)
					}
					if _, markerErr := os.Stat(marker); !os.IsNotExist(markerErr) {
						t.Fatalf(
							"caller-controlled proc alias executed workload: %v",
							markerErr,
						)
					}
					// Rejecting an aliased proc mount before the gate starts is
					// an acceptable fail-closed outcome.
					return
				}
				waitErr := command.Wait()
				t.Fatalf("%v (wait: %v, stderr: %q)", err, waitErr, stderr.String())
			}
			if identity.PID <= 0 || identity.SandboxPID <= 0 ||
				identity.NetNamespaceID == 0 ||
				identity.ProcessStartTimeTicks == 0 {
				t.Fatalf("incomplete workload identity: %+v", identity)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("workload executed before gate decision: %v", err)
			}

			if test.markReady {
				if err := lifecycle.MarkReady(); err != nil {
					t.Fatal(err)
				}
			} else {
				lifecycle.Abort()
			}
			waitErr := command.Wait()
			select {
			case <-lifecycle.DrainDone():
			case <-time.After(5 * time.Second):
				t.Fatal("timed out draining bwrap exit status")
			}

			_, markerErr := os.Stat(marker)
			if test.wantMarker && markerErr != nil {
				t.Fatalf(
					"ready workload did not execute: %v (wait: %v, stderr: %q)",
					markerErr,
					waitErr,
					stderr.String(),
				)
			}
			if !test.wantMarker && !os.IsNotExist(markerErr) {
				t.Fatalf("denied workload executed: %v", markerErr)
			}
			if test.uid != nil {
				if got, want := stdout.String(), strconv.FormatUint(uint64(*test.uid), 10); got != want {
					t.Fatalf("workload uid output = %q, want %q", got, want)
				}
			}
			if _, err := os.Stat(fakeGateMarker); !os.IsNotExist(err) {
				t.Fatalf("caller-controlled gate alias executed: %v", err)
			}
			if test.markReady {
				if drainErr := lifecycle.DrainError(); drainErr != nil {
					t.Fatalf("valid bwrap lifecycle stream failed: %v", drainErr)
				}
				if exitCode, ok := lifecycle.ExitCode(); !ok || exitCode != 7 {
					t.Fatalf("bwrap ExitCode = (%d, %v), want (7, true)", exitCode, ok)
				}
			}
		})
	}
}

func indexArgvSequence(argv []string, sequence ...string) int {
	for i := 0; i+len(sequence) <= len(argv); i++ {
		matched := true
		for j := range sequence {
			if argv[i+j] != sequence[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

func stringPointer(value string) *string {
	return &value
}

func TestReadProcessIdentityRejectsMissingPID(t *testing.T) {
	_, _, err := readProcessIdentity(1<<31 - 1)
	if err == nil {
		t.Fatal("missing pid was accepted")
	}
}

func TestParseStatusUintRejectsSignedAndStringValues(t *testing.T) {
	for _, value := range []string{"-1", `"1"`, "1e2"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			if _, err := parseStatusUint([]byte(value), "value", 1000, false); err == nil {
				t.Fatalf("value %s was accepted", value)
			}
		})
	}
}
