// Copyright 2025 Alibaba Group Holding Ltd.
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
// +build !windows

package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/alibaba/opensandbox/internal/safego"
	"github.com/creack/pty"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

// PTYSession is the public interface for an interactive PTY/pipe session.
// The concrete implementation (*ptySession) is unexported; callers outside
// this package must use this interface.
type PTYSession interface {
	LockWS() bool
	UnlockWS()
	IsRunning() bool
	IsPTY() bool
	ExitCode() int
	Done() <-chan struct{}
	StartPTY() error
	StartPipe() error
	WriteStdin(p []byte) (int, error)
	AttachOutput() (io.Reader, io.Reader, func())
	AttachOutputWithSnapshot(since int64) (io.Reader, io.Reader, func(), []byte, int64)
	SendSignal(name string)
	ResizePTY(cols, rows uint16) error
}

// IsPTYSessionSupported reports whether PTY sessions are supported on this platform.
func IsPTYSessionSupported() bool { return true }

func NewPTYSessionID() string {
	return uuidString()
}

// ptySession manages a single interactive PTY or pipe-mode bash process.
//
// Lifecycle:
//  1. Create via newPTYSession.
//  2. Call StartPTY() or StartPipe() from the WS handler (after LockWS).
//  3. Zero or more clients call AttachOutput() to receive live output.
//  4. The bash process exits → Done() closes → exit frame sent.
//  5. Call close() to terminate an early session and release resources.
type ptySession struct {
	id  string
	cwd string

	mu      sync.Mutex
	closing bool

	// Process tracking (guarded by mu)
	pid          int           // PID of the running bash process (0 = not running)
	lastExitCode int           // exit code; -1 until process exits
	doneCh       chan struct{} // closed when process exits (non-nil after Start*)

	// Stdin (PTY master in PTY mode; write end of os.Pipe in pipe mode)
	stdin io.WriteCloser

	// PTY-specific
	isPTY bool
	ptmx  *os.File // PTY master fd; nil in pipe mode

	// Replay
	replay *replayBuffer

	// WS exclusive lock: only one WebSocket client at a time.
	wsConnected atomic.Bool

	// Output broadcast (guards stdoutW / stderrW).
	// The broadcast goroutine holds outMu only while reading the pointer; writes
	// to the pipe happen outside the lock to avoid blocking broadcast on slow clients.
	outMu   sync.Mutex
	stdoutW *io.PipeWriter // current per-connection sink; nil when no client attached
	stderrW *io.PipeWriter // nil in PTY mode
}

func newPTYSession(id, cwd string) *ptySession {
	return &ptySession{
		id:           id,
		cwd:          cwd,
		replay:       newReplayBuffer(),
		lastExitCode: -1,
	}
}

// LockWS attempts to acquire the exclusive WebSocket connection lock.
// Returns true on success, false if another client is already connected.
func (s *ptySession) LockWS() bool {
	return s.wsConnected.CompareAndSwap(false, true)
}

// UnlockWS releases the WebSocket connection lock.
func (s *ptySession) UnlockWS() {
	s.wsConnected.Store(false)
}

// IsRunning returns true if the bash process is currently alive.
func (s *ptySession) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid != 0
}

// IsPTY returns true when the session was started in PTY mode.
func (s *ptySession) IsPTY() bool {
	return s.isPTY
}

// ExitCode returns the exit code of the last process, or -1 if it has not exited yet.
func (s *ptySession) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastExitCode
}

// Done returns a channel that is closed when the bash process exits.
// Returns nil if the process has not been started yet.
func (s *ptySession) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneCh
}

// ReplayBuffer returns the session's replay buffer (thread-safe).
func (s *ptySession) ReplayBuffer() *replayBuffer {
	return s.replay
}

// StartPTY launches bash via pty.StartWithSize.
// Must be called with the WS lock held.
func (s *ptySession) StartPTY() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pid != 0 {
		return errors.New("pty session already started")
	}
	if s.closing {
		return errors.New("pty session is closing")
	}

	cmd := exec.Command("bash", "--norc", "--noprofile")
	cmd.Env = os.Environ()
	if s.cwd != "" {
		cmd.Dir = s.cwd
	}
	// Do NOT set Setpgid: pty.StartWithSize sets Setsid+Setctty internally.
	// Combining Setsid+Setpgid causes EPERM (setpgid is illegal for a session leader).

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		return fmt.Errorf("pty.StartWithSize: %w", err)
	}

	s.ptmx = ptmx
	s.isPTY = true
	s.pid = cmd.Process.Pid
	s.doneCh = make(chan struct{})
	s.stdin = ptmx // write to the PTY master to feed stdin

	safego.Go(func() { s.broadcastPTY() })
	safego.Go(func() { s.waitAndExit(cmd, ptmx) })

	return nil
}

// StartPipe launches bash with plain stdin/stdout/stderr os.Pipes.
// Must be called with the WS lock held.
func (s *ptySession) StartPipe() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pid != 0 {
		return errors.New("pty session already started")
	}
	if s.closing {
		return errors.New("pty session is closing")
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	cmd := exec.Command("bash", "--norc", "--noprofile")
	cmd.Env = os.Environ()
	if s.cwd != "" {
		cmd.Dir = s.cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return fmt.Errorf("cmd.Start: %w", err)
	}

	// Close the child-side ends in the parent — the child has its own copies.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	s.isPTY = false
	s.pid = cmd.Process.Pid
	s.doneCh = make(chan struct{})
	s.stdin = stdinW

	safego.Go(func() { s.broadcastPipe(stdoutR, true) })
	safego.Go(func() { s.broadcastPipe(stderrR, false) })
	safego.Go(func() { s.waitAndExitPipe(cmd, stdinW, stdoutR, stderrR) })

	return nil
}

// broadcastPTY reads from the PTY master and fans out to replay + active WS client.
func (s *ptySession) broadcastPTY() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.writeAndFanout(buf[:n], true)
		}
		if err != nil {
			// EIO or EOF when the child exits — normal termination
			break
		}
	}
}

// broadcastPipe reads from a pipe (stdout or stderr) and fans out to replay + active WS client.
func (s *ptySession) broadcastPipe(r *os.File, isStdout bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.writeAndFanout(buf[:n], isStdout)
		}
		if err != nil {
			break
		}
	}
	_ = r.Close()
}

// writeAndFanout writes chunk to the replay buffer and delivers it to the
// active per-connection pipe, atomically under outMu.
//
// Holding outMu across both operations closes the window where bytes written
// to replay after ReadFrom but before AttachOutput would be silently dropped.
// Lock order is always outMu → replay.mu (both paths), so no deadlock is possible.
func (s *ptySession) writeAndFanout(chunk []byte, isStdout bool) {
	s.outMu.Lock()
	s.replay.write(chunk) // acquires replay.mu inside (outMu → replay.mu)
	var w *io.PipeWriter
	if isStdout {
		w = s.stdoutW
	} else {
		w = s.stderrW
	}
	s.outMu.Unlock()

	if w != nil {
		if _, err := w.Write(chunk); err != nil {
			// Pipe was closed (client detached) — ignore.
			log.Warning("pty fanout write: %v", err)
		}
	}
}

// waitAndExit waits for the PTY-mode process and updates session state on exit.
func (s *ptySession) waitAndExit(cmd *exec.Cmd, ptmx *os.File) {
	_ = cmd.Wait()

	// Close the PTY master to unblock the broadcast goroutine.
	_ = ptmx.Close()

	s.mu.Lock()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	s.lastExitCode = exitCode
	s.pid = 0
	doneCh := s.doneCh
	s.mu.Unlock()

	close(doneCh)
}

// waitAndExitPipe waits for the pipe-mode process and updates session state on exit.
func (s *ptySession) waitAndExitPipe(cmd *exec.Cmd, stdinW, stdoutR, stderrR *os.File) {
	_ = cmd.Wait()

	// Close stdin write-end so the child (if still running) sees EOF.
	_ = stdinW.Close()

	s.mu.Lock()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	s.lastExitCode = exitCode
	s.pid = 0
	doneCh := s.doneCh
	s.mu.Unlock()

	close(doneCh)
}

// WriteStdin writes p to bash stdin (PTY master or pipe write-end).
func (s *ptySession) WriteStdin(p []byte) (int, error) {
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w == nil {
		return 0, errors.New("session not started")
	}
	return w.Write(p)
}

// AttachOutput creates a fresh per-connection io.Pipe and swaps it into the
// broadcast fanout path.
//
// Ordering guarantee (no duplicates on reconnect):
//   - Caller must snapshot the replay buffer BEFORE calling AttachOutput.
//   - Bytes produced between the snapshot and AttachOutput are delivered via
//     the live pipe only (not in the snapshot), so each byte arrives exactly once.
//
// Returns (stdout reader, stderr reader [nil in PTY mode], detach func).
// Calling detach() closes the writers, sending EOF to the readers and
// unblocking all pump goroutines.
func (s *ptySession) AttachOutput() (io.Reader, io.Reader, func()) {
	stdoutR, stdoutW := io.Pipe()

	s.outMu.Lock()
	s.stdoutW = stdoutW
	s.outMu.Unlock()

	if s.isPTY {
		detach := func() {
			s.outMu.Lock()
			s.stdoutW = nil
			s.outMu.Unlock()
			_ = stdoutW.Close()
		}
		return stdoutR, nil, detach
	}

	// Pipe mode: also attach stderr.
	stderrR, stderrW := io.Pipe()

	s.outMu.Lock()
	s.stderrW = stderrW
	s.outMu.Unlock()

	detach := func() {
		s.outMu.Lock()
		s.stdoutW = nil
		s.stderrW = nil
		s.outMu.Unlock()
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}
	return stdoutR, stderrR, detach
}

// AttachOutputWithSnapshot atomically snapshots the replay buffer and attaches
// the per-connection output pipe, eliminating the output-loss window that exists
// when ReadFrom and AttachOutput are called separately.
//
// Must be used together with writeAndFanout (which holds outMu during both
// replay.write and the fanout pointer read).
//
// Lock order is always outMu → replay.mu (both paths), so no deadlock is possible.
//
// Returns (stdoutR, stderrR [nil in PTY mode], detach, snapshotBytes, snapshotOffset).
func (s *ptySession) AttachOutputWithSnapshot(since int64) (io.Reader, io.Reader, func(), []byte, int64) {
	stdoutR, stdoutW := io.Pipe()
	var stderrR io.Reader
	var stderrW *io.PipeWriter
	if !s.isPTY {
		stderrR, stderrW = io.Pipe()
	}

	s.outMu.Lock()
	snapshotBytes, snapshotOffset := s.replay.ReadFrom(since) // acquires replay.mu inside
	s.stdoutW = stdoutW
	if stderrW != nil {
		s.stderrW = stderrW
	}
	s.outMu.Unlock()

	detach := func() {
		s.outMu.Lock()
		s.stdoutW = nil
		if stderrW != nil {
			s.stderrW = nil
		}
		s.outMu.Unlock()
		_ = stdoutW.Close()
		if stderrW != nil {
			_ = stderrW.Close()
		}
	}
	return stdoutR, stderrR, detach, snapshotBytes, snapshotOffset
}

// SendSignal sends the named signal to the process group.
// Recognised names: SIGINT, SIGTERM, SIGKILL, SIGQUIT, SIGHUP.
func (s *ptySession) SendSignal(name string) {
	s.mu.Lock()
	pid := s.pid
	s.mu.Unlock()
	if pid == 0 {
		return
	}

	sig := parseSignalName(name)
	if sig == 0 {
		log.Warning("ptySession.SendSignal: unknown signal %q", name)
		return
	}

	// In PTY mode (setsid), pgid == pid automatically.
	// In pipe mode (Setpgid), pgid is also == pid.
	// Either way, Kill(-pid, sig) sends to the process group.
	if err := syscall.Kill(-pid, sig); err != nil {
		log.Warning("ptySession.SendSignal kill(-%d, %v): %v", pid, sig, err)
	}
}

func parseSignalName(name string) syscall.Signal {
	switch name {
	case "SIGINT":
		return syscall.SIGINT
	case "SIGTERM":
		return syscall.SIGTERM
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGQUIT":
		return syscall.SIGQUIT
	case "SIGHUP":
		return syscall.SIGHUP
	default:
		return 0
	}
}

// ResizePTY updates the terminal window size (PTY mode only; no-op in pipe mode).
func (s *ptySession) ResizePTY(cols, rows uint16) error {
	s.mu.Lock()
	ptmx := s.ptmx
	s.mu.Unlock()
	if ptmx == nil {
		return nil // pipe mode or not started
	}
	return pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// close terminates the session and releases all resources.
// Safe to call multiple times.
func (s *ptySession) close() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	pid := s.pid
	ptmx := s.ptmx
	stdin := s.stdin
	s.mu.Unlock()

	if pid != 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if ptmx != nil {
		_ = ptmx.Close()
	} else if stdin != nil {
		_ = stdin.Close()
	}

	// Detach any active WS output pipe so pump goroutines unblock.
	s.outMu.Lock()
	stdoutW := s.stdoutW
	stderrW := s.stderrW
	s.stdoutW = nil
	s.stderrW = nil
	s.outMu.Unlock()
	if stdoutW != nil {
		_ = stdoutW.Close()
	}
	if stderrW != nil {
		_ = stderrW.Close()
	}
}

// CreatePTYSession creates a new PTY session and stores it in the map.
func (c *Controller) CreatePTYSession(id, cwd string) (PTYSession, error) {
	resolvedCwd, err := pathutil.ExpandPath(cwd)
	if err != nil {
		return nil, fmt.Errorf("error resolving PTY session work directory: %w", err)
	}
	if resolvedCwd != "" {
		err := os.MkdirAll(resolvedCwd, os.ModePerm)
		if err != nil {
			return nil, fmt.Errorf("error creating PTY session work directory: %w", err)
		}
	}
	s := newPTYSession(id, resolvedCwd)
	c.ptySessionMap.Store(id, s)
	log.Info("created pty session %s", id)
	return s, nil
}

// getPTYSession looks up a PTY session by ID. Returns nil if not found.
// For internal use only; outside callers should use GetPTYSession.
func (c *Controller) getPTYSession(id string) *ptySession {
	if v, ok := c.ptySessionMap.Load(id); ok {
		if s, ok := v.(*ptySession); ok {
			return s
		}
	}
	return nil
}

// GetPTYSession looks up a PTY session by ID. Returns nil if not found.
func (c *Controller) GetPTYSession(id string) PTYSession {
	s := c.getPTYSession(id)
	if s == nil {
		return nil
	}
	return s
}

// DeletePTYSession terminates and removes a PTY session.
// Returns ErrContextNotFound if the session does not exist.
func (c *Controller) DeletePTYSession(id string) error {
	s := c.getPTYSession(id)
	if s == nil {
		return ErrContextNotFound
	}
	s.close()
	c.ptySessionMap.Delete(id)
	log.Info("deleted pty session %s", id)
	return nil
}

// GetPTYSessionStatus returns status information for a PTY session.
func (c *Controller) GetPTYSessionStatus(id string) (running bool, outputOffset int64, err error) {
	s := c.getPTYSession(id)
	if s == nil {
		return false, 0, ErrContextNotFound
	}
	return s.IsRunning(), s.replay.Total(), nil
}
