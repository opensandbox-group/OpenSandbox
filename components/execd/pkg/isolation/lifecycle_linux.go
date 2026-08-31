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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	sessionGateRuntimeHostPath = "/opt/opensandbox/opensandbox-session-gate"

	sessionGateWaitingFrame = "OPENSANDBOX_SESSION_WAITING_V1"
	sessionGateReadyFrame   = "OPENSANDBOX_SESSION_READY_V1"

	maxBwrapStatusDocumentBytes = 64 * 1024
	maxBwrapStatusDocuments     = 1024

	controlReadPollInterval = 100 * time.Millisecond
	controlWriteTimeout     = time.Second
)

var (
	errWorkloadLifecycleAborted = errors.New("workload lifecycle aborted")
	errWorkloadAlreadyReady     = errors.New("workload is already ready")
)

type lifecycleState uint8

const (
	lifecycleWaitingForBwrap lifecycleState = iota
	lifecycleWaitingForGate
	lifecycleIdentityReady
	lifecycleWorkloadReady
	lifecycleAborted
)

type bwrapSandboxStatus struct {
	pid            int
	netNamespaceID uint64
	err            error
}

type bwrapLifecycle struct {
	statusReader *os.File

	mu                  sync.Mutex
	state               lifecycleState
	requireNetNamespace bool
	setup               *os.File
	control             *net.UnixConn
	controlChildFD      int
	controlSocketInode  uint64
	identity            WorkloadIdentity

	sandboxStatus chan bwrapSandboxStatus
	drainDone     chan struct{}
	drainErr      error
	exitCode      int
	hasExitCode   bool
	closeOnce     sync.Once
}

func newBwrapLifecycle(
	statusReader *os.File,
	setupWriter *os.File,
	control *net.UnixConn,
	controlChildFD int,
	controlSocketInode uint64,
	requireNetNamespace bool,
) *bwrapLifecycle {
	lifecycle := &bwrapLifecycle{
		statusReader:        statusReader,
		state:               lifecycleWaitingForBwrap,
		requireNetNamespace: requireNetNamespace,
		setup:               setupWriter,
		control:             control,
		controlChildFD:      controlChildFD,
		controlSocketInode:  controlSocketInode,
		sandboxStatus:       make(chan bwrapSandboxStatus, 1),
		drainDone:           make(chan struct{}),
	}
	go lifecycle.drainStatus()
	return lifecycle
}

func (l *bwrapLifecycle) WaitForIdentity(ctx context.Context) (
	identity WorkloadIdentity,
	err error,
) {
	l.mu.Lock()
	switch l.state {
	case lifecycleIdentityReady, lifecycleWorkloadReady:
		identity = l.identity
		l.mu.Unlock()
		return identity, nil
	case lifecycleAborted:
		l.mu.Unlock()
		return WorkloadIdentity{}, errWorkloadLifecycleAborted
	case lifecycleWaitingForGate:
		l.mu.Unlock()
		return WorkloadIdentity{}, errors.New("workload identity wait already in progress")
	case lifecycleWaitingForBwrap:
		l.state = lifecycleWaitingForGate
	}
	l.mu.Unlock()

	defer func() {
		if err != nil {
			l.Abort()
		}
	}()

	var status bwrapSandboxStatus
	select {
	case status = <-l.sandboxStatus:
		if status.err != nil {
			return WorkloadIdentity{}, status.err
		}
	case <-ctx.Done():
		return WorkloadIdentity{}, fmt.Errorf("wait for bwrap status: %w", ctx.Err())
	}

	status, err = validateSandboxStatus(status, l.requireNetNamespace)
	if err != nil {
		return WorkloadIdentity{}, err
	}
	if err := l.releaseBwrapSetupGate(); err != nil {
		return WorkloadIdentity{}, err
	}

	workloadPID, err := l.waitForNativeGate(ctx)
	if err != nil {
		return WorkloadIdentity{}, err
	}
	identity, err = validateWorkloadIdentity(
		workloadPID,
		status,
		l.controlChildFD,
		l.controlSocketInode,
	)
	if err != nil {
		return WorkloadIdentity{}, err
	}

	l.mu.Lock()
	if l.state == lifecycleAborted {
		l.mu.Unlock()
		return WorkloadIdentity{}, errWorkloadLifecycleAborted
	}
	l.identity = identity
	l.state = lifecycleIdentityReady
	l.mu.Unlock()
	return identity, nil
}

func (l *bwrapLifecycle) releaseBwrapSetupGate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == lifecycleAborted {
		return errWorkloadLifecycleAborted
	}
	if l.setup == nil {
		return errors.New("bwrap setup gate is unavailable")
	}
	n, err := l.setup.Write([]byte{1})
	closeErr := l.setup.Close()
	l.setup = nil
	if err != nil {
		return fmt.Errorf("release bwrap setup gate: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("release bwrap setup gate: short write: got %d, want 1", n)
	}
	if closeErr != nil {
		return fmt.Errorf("close bwrap setup gate: %w", closeErr)
	}
	return nil
}

func (l *bwrapLifecycle) waitForNativeGate(ctx context.Context) (int, error) {
	payload := make([]byte, len(sessionGateWaitingFrame)+1)
	oob := make([]byte, unix.CmsgSpace(unix.SizeofUcred))

	for {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("wait for native workload gate: %w", err)
		}

		l.mu.Lock()
		if l.state == lifecycleAborted || l.control == nil {
			l.mu.Unlock()
			return 0, errWorkloadLifecycleAborted
		}
		control := l.control
		l.mu.Unlock()

		deadline := time.Now().Add(controlReadPollInterval)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := control.SetReadDeadline(deadline); err != nil {
			return 0, fmt.Errorf("set native workload gate deadline: %w", err)
		}

		n, oobn, flags, _, err := control.ReadMsgUnix(payload, oob)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return 0, fmt.Errorf("read native workload gate: %w", err)
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			return 0, errors.New("native workload gate frame or credentials were truncated")
		}
		if string(payload[:n]) != sessionGateWaitingFrame {
			return 0, errors.New("native workload gate sent an invalid waiting frame")
		}

		credentials, err := parseSingleUnixCredentials(oob[:oobn])
		if err != nil {
			return 0, err
		}
		if credentials.Pid <= 0 {
			return 0, fmt.Errorf("native workload gate sent invalid pid %d", credentials.Pid)
		}
		return int(credentials.Pid), nil
	}
}

func parseSingleUnixCredentials(oob []byte) (*unix.Ucred, error) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("parse native workload gate credentials: %w", err)
	}
	var credentials *unix.Ucred
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET ||
			message.Header.Type != unix.SCM_CREDENTIALS {
			return nil, fmt.Errorf(
				"native workload gate sent unexpected control message level=%d type=%d",
				message.Header.Level,
				message.Header.Type,
			)
		}
		if credentials != nil {
			return nil, errors.New("native workload gate sent duplicate credentials")
		}
		credentials, err = unix.ParseUnixCredentials(&message)
		if err != nil {
			return nil, fmt.Errorf("parse native workload gate credentials: %w", err)
		}
	}
	if credentials == nil {
		return nil, errors.New("native workload gate did not send credentials")
	}
	return credentials, nil
}

func validateSandboxStatus(
	status bwrapSandboxStatus,
	requireNetNamespace bool,
) (bwrapSandboxStatus, error) {
	if status.pid <= 0 {
		return bwrapSandboxStatus{}, fmt.Errorf(
			"bwrap status reported invalid child-pid %d",
			status.pid,
		)
	}
	actualNamespaceID, err := readNetNamespaceID(status.pid)
	if err != nil {
		return bwrapSandboxStatus{}, fmt.Errorf(
			"read bwrap child network namespace: %w",
			err,
		)
	}
	if status.netNamespaceID == 0 {
		if requireNetNamespace {
			return bwrapSandboxStatus{}, errors.New(
				"bwrap status did not report the requested private network namespace",
			)
		}
		// Bubblewrap only reports namespace IDs that it created. In share-net
		// mode the child is still blocked by --block-fd, so its PID cannot be
		// reused while we derive the inherited namespace from /proc.
		status.netNamespaceID = actualNamespaceID
		return status, nil
	}
	if actualNamespaceID != status.netNamespaceID {
		return bwrapSandboxStatus{}, fmt.Errorf(
			"bwrap child network namespace mismatch: status=%d proc=%d",
			status.netNamespaceID,
			actualNamespaceID,
		)
	}
	return status, nil
}

func validateWorkloadIdentity(
	workloadPID int,
	status bwrapSandboxStatus,
	controlFD int,
	controlSocketInode uint64,
) (WorkloadIdentity, error) {
	ppid, startTime, err := readProcessIdentity(workloadPID)
	if err != nil {
		return WorkloadIdentity{}, fmt.Errorf("read workload process identity: %w", err)
	}
	if ppid != status.pid {
		return WorkloadIdentity{}, fmt.Errorf(
			"workload parent mismatch: got %d, want bwrap child %d",
			ppid,
			status.pid,
		)
	}
	netNamespaceID, err := readNetNamespaceID(workloadPID)
	if err != nil {
		return WorkloadIdentity{}, fmt.Errorf("read workload network namespace: %w", err)
	}
	if netNamespaceID != status.netNamespaceID {
		return WorkloadIdentity{}, fmt.Errorf(
			"workload network namespace mismatch: status=%d proc=%d",
			status.netNamespaceID,
			netNamespaceID,
		)
	}
	if controlFD < 3 || controlSocketInode == 0 {
		return WorkloadIdentity{}, errors.New("native workload gate socket identity is unavailable")
	}
	actualSocketInode, err := readControlSocketInode(workloadPID, controlFD)
	if err != nil {
		return WorkloadIdentity{}, fmt.Errorf("read workload control socket identity: %w", err)
	}
	if actualSocketInode != controlSocketInode {
		return WorkloadIdentity{}, fmt.Errorf(
			"workload control socket mismatch: got %d, want %d",
			actualSocketInode,
			controlSocketInode,
		)
	}
	return WorkloadIdentity{
		PID:                   workloadPID,
		SandboxPID:            status.pid,
		NetNamespaceID:        netNamespaceID,
		ProcessStartTimeTicks: startTime,
	}, nil
}

func readControlSocketInode(pid, fd int) (uint64, error) {
	target, err := os.Readlink(
		filepath.Join("/proc", strconv.Itoa(pid), "fd", strconv.Itoa(fd)),
	)
	if err != nil {
		return 0, err
	}
	const prefix = "socket:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return 0, fmt.Errorf("unexpected control descriptor link %q", target)
	}
	inode, err := strconv.ParseUint(target[len(prefix):len(target)-1], 10, 64)
	if err != nil || inode == 0 {
		return 0, fmt.Errorf("unexpected control descriptor link %q", target)
	}
	return inode, nil
}

func readProcessIdentity(pid int) (ppid int, startTime uint64, err error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, err
	}
	// comm is parenthesized and may contain spaces or ')' characters. The last
	// ')' is followed by field 3 (state).
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return 0, 0, errors.New("malformed /proc stat: missing comm terminator")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	// fields[0] is field 3 (state), fields[1] is ppid, and fields[19] is
	// starttime (field 22).
	if len(fields) <= 19 {
		return 0, 0, fmt.Errorf("malformed /proc stat: got %d trailing fields", len(fields))
	}
	parsedPPID, err := strconv.ParseInt(fields[1], 10, 32)
	if err != nil || parsedPPID <= 0 {
		return 0, 0, fmt.Errorf("malformed /proc stat ppid %q", fields[1])
	}
	parsedStartTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || parsedStartTime == 0 {
		return 0, 0, fmt.Errorf("malformed /proc stat starttime %q", fields[19])
	}
	return int(parsedPPID), parsedStartTime, nil
}

func readNetNamespaceID(pid int) (uint64, error) {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "net"))
	if err != nil {
		return 0, err
	}
	const prefix = "net:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return 0, fmt.Errorf("unexpected network namespace link %q", target)
	}
	id, err := strconv.ParseUint(target[len(prefix):len(target)-1], 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("unexpected network namespace link %q", target)
	}
	return id, nil
}

func (l *bwrapLifecycle) MarkReady() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case lifecycleWorkloadReady:
		return errWorkloadAlreadyReady
	case lifecycleAborted:
		return errWorkloadLifecycleAborted
	case lifecycleIdentityReady:
		// Continue below.
	default:
		return errors.New("workload identity is not ready")
	}
	if l.control == nil {
		l.state = lifecycleAborted
		return errors.New("native workload gate is unavailable")
	}
	if err := l.control.SetWriteDeadline(time.Now().Add(controlWriteTimeout)); err != nil {
		l.abortLocked()
		return fmt.Errorf("set native workload gate write deadline: %w", err)
	}
	n, err := l.control.Write([]byte(sessionGateReadyFrame))
	if err != nil {
		l.abortLocked()
		return fmt.Errorf("release native workload gate: %w", err)
	}
	if n != len(sessionGateReadyFrame) {
		l.abortLocked()
		return fmt.Errorf(
			"release native workload gate: short write: got %d, want %d",
			n,
			len(sessionGateReadyFrame),
		)
	}
	// A complete SOCK_SEQPACKET write is the irreversible release point.
	// Cleanup failures cannot make an already-released workload fail closed,
	// so they must not turn a successful release into a MarkReady error.
	_ = l.control.Close()
	l.control = nil
	l.state = lifecycleWorkloadReady
	return nil
}

func (l *bwrapLifecycle) Abort() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.abortLocked()
}

func (l *bwrapLifecycle) abortLocked() {
	if l.state == lifecycleAborted {
		return
	}
	l.state = lifecycleAborted
	if l.control != nil {
		_ = l.control.Close()
		l.control = nil
	}
	// Close the native gate first. EOF on bubblewrap's block-fd is treated as
	// success by bubblewrap; if setup is then released, the native helper sees
	// a closed control socket and exits instead of executing the workload.
	if l.setup != nil {
		_ = l.setup.Close()
		l.setup = nil
	}
	if l.statusReader != nil {
		_ = l.statusReader.Close()
	}
}

func (l *bwrapLifecycle) DrainDone() <-chan struct{} {
	return l.drainDone
}

func (l *bwrapLifecycle) DrainError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drainErr
}

func (l *bwrapLifecycle) ExitCode() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exitCode, l.hasExitCode
}

func (l *bwrapLifecycle) Close() error {
	l.closeOnce.Do(func() {
		l.Abort()
		<-l.drainDone
	})
	err := l.DrainError()
	if errors.Is(err, errWorkloadLifecycleAborted) {
		// Closing an otherwise healthy lifecycle intentionally interrupts the
		// status reader after denying the native gate. That interruption is
		// expected teardown, not a cleanup failure.
		return nil
	}
	return err
}

func (l *bwrapLifecycle) drainStatus() {
	defer close(l.drainDone)
	defer l.statusReader.Close()

	scanner := bufio.NewScanner(l.statusReader)
	scanner.Buffer(make([]byte, 4096), maxBwrapStatusDocumentBytes)

	var (
		documentCount int
		sawChild      bool
		sawExit       bool
	)
	for scanner.Scan() {
		documentCount++
		if documentCount > maxBwrapStatusDocuments {
			l.finishStatusDrain(
				fmt.Errorf("bwrap status exceeded %d documents", maxBwrapStatusDocuments),
				sawChild,
			)
			return
		}

		var object map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &object); err != nil {
			l.finishStatusDrain(fmt.Errorf("decode bwrap status document: %w", err), sawChild)
			return
		}
		if object == nil {
			l.finishStatusDrain(errors.New("bwrap status document is not an object"), sawChild)
			return
		}

		childStatus, hasChild, err := l.parseChildStatus(object, sawChild)
		if err != nil {
			l.finishStatusDrain(err, sawChild)
			return
		}
		if hasChild {
			sawChild = true
			l.sandboxStatus <- childStatus
		}

		exitCode, hasExit, err := parseExitStatus(object, sawChild, sawExit)
		if err != nil {
			l.finishStatusDrain(err, sawChild)
			return
		}
		if hasExit {
			sawExit = true
			l.mu.Lock()
			l.exitCode = int(exitCode)
			l.hasExitCode = true
			l.mu.Unlock()
		}
	}

	if err := scanner.Err(); err != nil {
		if l.isAborted() {
			err = errWorkloadLifecycleAborted
		} else {
			err = fmt.Errorf("read bwrap status: %w", err)
		}
		l.finishStatusDrain(err, sawChild)
		return
	}
	// Abort may race bubblewrap closing its status writer. In that case Scanner
	// observes a clean EOF rather than os.ErrClosed, but missing status records
	// still describe an intentional teardown rather than a trust failure.
	if l.isAborted() {
		l.finishStatusDrain(errWorkloadLifecycleAborted, sawChild)
		return
	}
	if !sawChild {
		l.finishStatusDrain(
			errors.New("bwrap status reached EOF before child-pid"),
			false,
		)
		return
	}
	if !sawExit {
		l.finishStatusDrain(
			errors.New("bwrap status reached EOF before exit-code"),
			true,
		)
		return
	}
	l.finishStatusDrain(nil, true)
}

func (l *bwrapLifecycle) parseChildStatus(
	object map[string]json.RawMessage,
	sawChild bool,
) (bwrapSandboxStatus, bool, error) {
	childRaw, hasChild := object["child-pid"]
	if !hasChild {
		return bwrapSandboxStatus{}, false, nil
	}
	if sawChild {
		return bwrapSandboxStatus{}, false, errors.New("bwrap status repeated child-pid")
	}
	childPID, err := parseStatusUint(childRaw, "child-pid", 1<<31-1, false)
	if err != nil {
		return bwrapSandboxStatus{}, false, err
	}

	var netNamespaceID uint64
	netRaw, hasNetNamespace := object["net-namespace"]
	if !hasNetNamespace {
		if l.requireNetNamespace {
			return bwrapSandboxStatus{}, false, errors.New(
				"bwrap child status omitted net-namespace",
			)
		}
	} else {
		netNamespaceID, err = parseStatusUint(
			netRaw,
			"net-namespace",
			^uint64(0),
			false,
		)
		if err != nil {
			return bwrapSandboxStatus{}, false, err
		}
	}
	return bwrapSandboxStatus{
		pid:            int(childPID),
		netNamespaceID: netNamespaceID,
	}, true, nil
}

func parseExitStatus(
	object map[string]json.RawMessage,
	sawChild bool,
	sawExit bool,
) (uint64, bool, error) {
	exitRaw, hasExit := object["exit-code"]
	if !hasExit {
		return 0, false, nil
	}
	if !sawChild {
		return 0, false, errors.New("bwrap status reported exit-code before child-pid")
	}
	if sawExit {
		return 0, false, errors.New("bwrap status repeated exit-code")
	}
	exitCode, err := parseStatusUint(exitRaw, "exit-code", 255, true)
	if err != nil {
		return 0, false, err
	}
	return exitCode, true, nil
}

func (l *bwrapLifecycle) finishStatusDrain(err error, sawChild bool) {
	l.mu.Lock()
	if l.drainErr == nil {
		l.drainErr = err
	}
	if err != nil {
		l.abortLocked()
	}
	l.mu.Unlock()
	if !sawChild {
		select {
		case l.sandboxStatus <- bwrapSandboxStatus{err: err}:
		default:
		}
	}
}

func (l *bwrapLifecycle) isAborted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == lifecycleAborted
}

func parseStatusUint(
	raw json.RawMessage,
	name string,
	maximum uint64,
	allowZero bool,
) (uint64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("bwrap status %s is not an unsigned integer", name)
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("bwrap status %s is not an unsigned integer", name)
		}
	}
	value, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bwrap status %s is not an unsigned integer: %w", name, err)
	}
	if (!allowZero && value == 0) || value > maximum {
		return 0, fmt.Errorf("bwrap status %s value %d is out of range", name, value)
	}
	return value, nil
}

func openSessionGate() (*os.File, error) {
	return openSessionGatePath(sessionGateRuntimeHostPath)
}

func openSessionGatePath(path string) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open native workload gate %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), "opensandbox-session-gate")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open native workload gate: invalid descriptor")
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, fmt.Errorf("stat native workload gate: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, errors.New("native workload gate is not a regular file")
	}
	if stat.Mode&0o111 == 0 {
		file.Close()
		return nil, errors.New("native workload gate is not executable")
	}
	if stat.Mode&0o022 != 0 {
		file.Close()
		return nil, errors.New("native workload gate is group- or world-writable")
	}
	if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, fmt.Errorf(
			"native workload gate has untrusted owner uid %d",
			stat.Uid,
		)
	}
	return file, nil
}

func newGateSocketpair() (*net.UnixConn, *os.File, uint64, error) {
	fds, err := unix.Socketpair(
		unix.AF_UNIX,
		unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("create native workload gate socket: %w", err)
	}
	closeFDs := true
	defer func() {
		if closeFDs {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
		}
	}()
	if err := unix.SetsockoptInt(
		fds[0],
		unix.SOL_SOCKET,
		unix.SO_PASSCRED,
		1,
	); err != nil {
		return nil, nil, 0, fmt.Errorf("enable native workload gate credentials: %w", err)
	}
	var childStat unix.Stat_t
	if err := unix.Fstat(fds[1], &childStat); err != nil {
		return nil, nil, 0, fmt.Errorf("stat native workload gate socket: %w", err)
	}
	if childStat.Ino == 0 {
		return nil, nil, 0, errors.New("native workload gate socket has no inode")
	}

	parentFile := os.NewFile(uintptr(fds[0]), "session-gate-parent")
	childFile := os.NewFile(uintptr(fds[1]), "session-gate-child")
	if parentFile == nil || childFile == nil {
		if parentFile != nil {
			_ = parentFile.Close()
			fds[0] = -1
		}
		if childFile != nil {
			_ = childFile.Close()
			fds[1] = -1
		}
		return nil, nil, 0, errors.New("create native workload gate files")
	}
	rawConnection, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	fds[0] = -1
	if err != nil {
		_ = childFile.Close()
		fds[1] = -1
		return nil, nil, 0, fmt.Errorf("adopt native workload gate socket: %w", err)
	}
	parentConnection, ok := rawConnection.(*net.UnixConn)
	if !ok {
		_ = rawConnection.Close()
		_ = childFile.Close()
		fds[1] = -1
		return nil, nil, 0, errors.New("native workload gate is not a Unix socket")
	}
	closeFDs = false
	return parentConnection, childFile, childStat.Ino, nil
}

var _ WorkloadLifecycle = (*bwrapLifecycle)(nil)
