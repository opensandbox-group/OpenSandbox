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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/sessionresource"
)

// IsolatedSessionOptions bundles the parameters for creating an isolated session.
type IsolatedSessionOptions struct {
	Profile            string
	WorkspacePath      string
	WorkspaceMode      string
	ExtraWritable      []string
	Binds              []isolation.BindMount
	ShareNet           *bool
	EnvPassthroughMode string
	EnvPassthroughKeys []string
	Uid                *uint32
	Gid                *uint32
	UidMode            string // "setpriv" (default) or "userns"
	IdleTimeoutSeconds int
}

type sessionNamespacePins interface {
	Directory() string
	NetPath() string
	UserPath() string
	Identity() sessionresource.NamespaceIdentity
	Close() error
}

type sessionNamespacePinner func(
	context.Context,
	isolation.WorkloadIdentity,
) (sessionNamespacePins, error)

// isolatedSession holds a long-running shell process inside a bwrap namespace.
type isolatedSession struct {
	id                   string
	mu                   sync.RWMutex
	runMu                sync.Mutex // serializes concurrent Run calls
	operationMu          sync.Mutex // protects filesystem-operation admission
	activeOperations     int
	operationsDrained    chan struct{} // closed when activeOperations reaches zero
	opts                 *IsolatedSessionOptions
	cmd                  *exec.Cmd
	stdin                io.WriteCloser
	stdout               io.ReadCloser
	processWaited        chan struct{} // closed immediately after cmd.Wait returns
	processSignalMu      sync.Mutex    // serializes process-group signals with the pre-reap barrier
	processExited        bool          // set before Linux can reap and reuse the numeric PID/PGID
	stopping             atomic.Bool   // once true, the session never admits another Run
	lifecycleInvalid     atomic.Bool   // trusted lifecycle accounting was lost unexpectedly
	doneCh               chan struct{} // closed after process wait and lifecycle drain
	lifecycleMonitorDone chan struct{} // closed after drain-failure monitor exits
	upperID              string        // key in UpperManager, used for Release/Remove
	upperDir             string
	workDir              string
	createdAt            time.Time
	lastRunAt            time.Time
	isolator             isolation.Isolator
	lifecycle            isolation.WorkloadLifecycle
	identity             isolation.WorkloadIdentity
	// activeBackgroundRuns: in-flight detached runs. While non-zero, idle GC
	// must not collect the session (background processes die with the bwrap
	// process group).
	activeBackgroundRuns atomic.Int64
	namespacePinner      sessionNamespacePinner
	namespacePins        sessionNamespacePins
}

const isolatedSessionStartupTimeout = 10 * time.Second

var (
	isolatedSessionStopTimeout = 5 * time.Second
	signalSessionProcessGroup  = func(pid int, signal syscall.Signal) error {
		return syscall.Kill(-pid, signal)
	}
)

func newIsolatedSession(
	id string,
	opts *IsolatedSessionOptions,
	iso isolation.Isolator,
	namespacePinner sessionNamespacePinner,
) *isolatedSession {
	return &isolatedSession{
		id:              id,
		opts:            opts,
		isolator:        iso,
		namespacePinner: namespacePinner,
		processWaited:   make(chan struct{}),
		doneCh:          make(chan struct{}),
		createdAt:       time.Now(),
		lastRunAt:       time.Now(),
	}
}

// start launches bwrap and the preferred shell inside a namespace.
func (s *isolatedSession) start() error {
	shell, args := shellCommand()
	cmd := exec.Command(shell, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd = cmd

	wrapOpts := isolation.WrapOptions{
		ExtraWritable: s.opts.ExtraWritable,
		Binds:         s.opts.Binds,
		ShareNet:      true,
	}

	switch s.opts.Profile {
	case string(isolation.ProfileBalanced):
		wrapOpts.Profile = isolation.ProfileBalanced
	case string(isolation.ProfileStrict), "":
		wrapOpts.Profile = isolation.ProfileStrict
	default:
		return fmt.Errorf("unknown isolation profile %q", s.opts.Profile)
	}

	wrapOpts.Workspace.Path = s.opts.WorkspacePath
	switch isolation.WorkspaceMode(s.opts.WorkspaceMode) {
	case isolation.WorkspaceRW:
		wrapOpts.Workspace.Mode = isolation.WorkspaceRW
	case isolation.WorkspaceRO:
		wrapOpts.Workspace.Mode = isolation.WorkspaceRO
	default:
		wrapOpts.Workspace.Mode = isolation.WorkspaceOverlay
	}

	if s.opts.ShareNet != nil {
		wrapOpts.ShareNet = *s.opts.ShareNet
	}
	if s.opts.EnvPassthroughMode != "" {
		wrapOpts.EnvPassthrough.Mode = isolation.EnvMode(s.opts.EnvPassthroughMode)
		wrapOpts.EnvPassthrough.Keys = s.opts.EnvPassthroughKeys
	} else {
		wrapOpts.EnvPassthrough.Mode = isolation.EnvModeDeny
	}
	wrapOpts.Uid = s.opts.Uid
	wrapOpts.Gid = s.opts.Gid
	if s.opts.UidMode != "" {
		wrapOpts.UidMode = isolation.UidMode(s.opts.UidMode)
	}
	wrapOpts.UpperDir = s.upperDir
	wrapOpts.WorkDir = s.workDir

	lifecycleIsolator, ok := s.isolator.(isolation.LifecycleIsolator)
	if !ok {
		return ErrSessionLifecycleUnavailable
	}
	lifecycle, err := lifecycleIsolator.WrapWithLifecycle(cmd, wrapOpts)
	if err != nil {
		closeCommandExtraFiles(cmd)
		if lifecycle != nil {
			s.lifecycle = lifecycle
			return s.failStartup(err)
		}
		return err
	}
	if lifecycle == nil {
		closeCommandExtraFiles(cmd)
		return ErrSessionLifecycleUnavailable
	}
	s.lifecycle = lifecycle

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		closeCommandExtraFiles(cmd)
		return s.failStartup(err)
	}
	s.stdin = stdinW
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		closeCommandExtraFiles(cmd)
		return s.failStartup(err)
	}
	s.stdout = stdoutR
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW

	mp, err := launchManaged(
		cmd,
		withPreReap(func() { s.markProcessExitedBeforeReap(nil) }),
		// bwrap needs unshare/mount + capabilities to build the namespace;
		// its workload is already reduced inside by bwrap's own seccomp and
		// the session gate.
		withoutHardening(),
	)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		closeCommandExtraFiles(cmd)
		return s.failStartup(fmt.Errorf("start %s: %w", shell, err))
	}

	// Close the child-side ends in the parent — the child has its own copies.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	closeCommandExtraFiles(cmd)

	go func() {
		_ = waitManagedWithBarrier(mp, s.markProcessExitedBeforeReap)
		// Publish process reaping before waiting for lifecycle accounting.
		// Once Wait returns, the numeric PID/PGID may be reused and must never
		// be signalled again.
		close(s.processWaited)
		// A session is not fully reaped until bubblewrap's status stream has
		// also reached a validated terminal state.
		<-lifecycle.DrainDone()
		close(s.doneCh)
	}()
	s.lifecycleMonitorDone = make(chan struct{})
	go func() {
		defer close(s.lifecycleMonitorDone)
		<-lifecycle.DrainDone()
		if lifecycle.DrainError() != nil &&
			!s.stopping.Load() &&
			cmd.Process != nil {
			// Losing trusted lifecycle accounting invalidates the workload
			// identity. Publish the terminal state before attempting the kill
			// so no later Run can enter even if signalling fails.
			s.lifecycleInvalid.Store(true)
			if err := s.signalProcessGroupIfRunning(syscall.SIGKILL); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				log.Error(
					"kill isolated session after lifecycle failure: %v",
					err,
				)
			}
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(
		context.Background(),
		isolatedSessionStartupTimeout,
	)
	prepareErr := s.prepareStartupIdentity(
		startupCtx,
		lifecycle,
		wrapOpts,
	)
	cancelStartup()
	if prepareErr != nil {
		return s.failStartup(prepareErr)
	}

	if err := lifecycle.MarkReady(); err != nil {
		return s.failStartup(fmt.Errorf("release isolated workload gate: %w", err))
	}
	if err := s.startupTerminalError(lifecycle); err != nil {
		return s.failStartup(err)
	}

	// Brief startup check — if bwrap fails immediately (bad capabilities,
	// missing binary inside namespace, lifecycle trust loss, etc.) we detect it
	// here instead of returning a terminal session to the caller.
	startupTimer := time.NewTimer(100 * time.Millisecond)
	select {
	case <-s.processWaited:
		if !startupTimer.Stop() {
			<-startupTimer.C
		}
		return s.failStartup(fmt.Errorf("bwrap process exited immediately after start"))
	case <-lifecycle.DrainDone():
		if !startupTimer.Stop() {
			<-startupTimer.C
		}
		return s.failStartup(lifecycleStartupErrorAfterDrain(lifecycle))
	case <-startupTimer.C:
	}
	// DrainDone can close concurrently with the timer. Recheck it before
	// publishing the session so the random select winner cannot turn a
	// lifecycle failure into a successful Create.
	if err := s.startupTerminalError(lifecycle); err != nil {
		return s.failStartup(err)
	}

	return nil
}

func (s *isolatedSession) prepareStartupIdentity(
	ctx context.Context,
	lifecycle isolation.WorkloadLifecycle,
	wrapOpts isolation.WrapOptions,
) error {
	identity, err := lifecycle.WaitForIdentity(ctx)
	if err != nil {
		return fmt.Errorf("wait for isolated workload identity: %w", err)
	}
	s.identity = identity

	if !s.requiresNamespacePins(wrapOpts) {
		return nil
	}
	if s.namespacePinner == nil {
		return ErrSessionNamespaceUnavailable
	}

	pins, err := s.namespacePinner(ctx, identity)
	if pins != nil {
		// A pinner can return ownership with an error when its rollback was
		// incomplete. Retain it so failStartup and later GC retries can finish
		// cleanup after the workload has stopped.
		s.namespacePins = pins
	}
	if err != nil {
		return errors.Join(
			ErrSessionNamespaceUnavailable,
			fmt.Errorf("pin isolated session namespaces: %w", err),
		)
	}
	if pins == nil {
		return ErrSessionNamespaceUnavailable
	}
	return nil
}

func (s *isolatedSession) requiresNamespacePins(
	wrapOpts isolation.WrapOptions,
) bool {
	// This phase preserves the legacy share_net default. Every explicitly
	// private NetNS is pinned, including the legacy setpriv loopback-only
	// topology. A later network-profile PR will make private+userns the secure
	// default and reject setpriv for the network backend.
	return !wrapOpts.ShareNet
}

func (s *isolatedSession) startupTerminalError(
	lifecycle isolation.WorkloadLifecycle,
) error {
	select {
	case <-s.processWaited:
		return fmt.Errorf("bwrap process exited immediately after start")
	default:
	}
	return lifecycleStartupError(lifecycle)
}

func lifecycleStartupError(lifecycle isolation.WorkloadLifecycle) error {
	select {
	case <-lifecycle.DrainDone():
		return lifecycleStartupErrorAfterDrain(lifecycle)
	default:
		return nil
	}
}

func lifecycleStartupErrorAfterDrain(lifecycle isolation.WorkloadLifecycle) error {
	if err := lifecycle.DrainError(); err != nil {
		return fmt.Errorf("isolated session lifecycle failed during startup: %w", err)
	}
	return fmt.Errorf("isolated session lifecycle ended during startup")
}

// stop kills the bwrap process group and waits for process reaping.
func (s *isolatedSession) stop() error {
	s.stopping.Store(true)

	hasProcess := s.cmd != nil && s.cmd.Process != nil
	if s.lifecycle != nil {
		// Deny an unreleased startup gate and mark the status stream as an
		// intentional teardown before killing the process. The Linux pre-reap
		// barrier below still prevents signalling a reused process group if the
		// gate exits between Abort and the signal.
		s.lifecycle.Abort()
	}
	processDone, processGroupKillErr := s.stopProcess(hasProcess)
	if hasProcess {
		if !processDone {
			// lifecycle.Close may wait for its status-drain goroutine. Do not
			// turn an unreapable workload into an unbounded session teardown.
			return errors.Join(
				processGroupKillErr,
				ErrSessionTeardownTimeout,
			)
		}
	} else if s.lifecycle != nil &&
		!waitForSessionDrain(s.lifecycle.DrainDone(), isolatedSessionStopTimeout) {
		return ErrSessionTeardownTimeout
	}
	if processDone && processGroupKillErr != nil {
		// A group signal can race a naturally exiting or already-zombie
		// leader and return EPERM on some kernels. Once both cmd.Wait and the
		// trusted lifecycle drain are complete, teardown is confirmed and the
		// transient signal error must not turn a successful delete into 500.
		log.Warn("%v; session process and lifecycle are fully reaped", processGroupKillErr)
	}
	namespaceErr := s.closeNamespacePins()
	lifecycleErr := s.closeLifecycle()
	return errors.Join(namespaceErr, lifecycleErr)
}

func (s *isolatedSession) stopProcess(hasProcess bool) (bool, error) {
	processDone := false
	if hasProcess {
		select {
		case <-s.doneCh:
			processDone = true
		default:
		}
	}

	var processGroupKillErr error
	if hasProcess && !processDone {
		// Signal while the session pipes and lifecycle gate are still open.
		// Closing stdin first lets an interactive shell exit and be reaped just
		// before a group signal, creating a stale-PID window.
		if err := s.signalProcessGroupIfRunning(syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			processGroupKillErr = fmt.Errorf(
				"kill isolated session process group: %w",
				err,
			)
		}
	}

	s.closeProcessPipes()
	if hasProcess && !processDone {
		processDone = waitForSessionDrain(s.doneCh, isolatedSessionStopTimeout)
	}
	return processDone, processGroupKillErr
}

func (s *isolatedSession) closeProcessPipes() {
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.stdout != nil {
		_ = s.stdout.Close()
		s.stdout = nil
	}
}

func waitForSessionDrain(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	select {
	case <-done:
		if !timer.Stop() {
			<-timer.C
		}
		return true
	case <-timer.C:
		return false
	}
}

func (s *isolatedSession) closeLifecycle() error {
	if s.lifecycleMonitorDone != nil {
		// DrainDone is closed before doneCh, so the monitor is guaranteed to
		// make progress here. Waiting gives teardown ownership of the monitor
		// and prevents it from outliving the session or test hooks it uses.
		<-s.lifecycleMonitorDone
	}

	var cleanupErr error
	if s.lifecycle != nil {
		if err := s.lifecycle.Close(); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("close isolated session lifecycle: %w", err),
			)
		}
		// Close owns and releases every lifecycle descriptor even when it
		// reports a terminal status-stream error. Retaining the pointer would
		// only make later retries return the same immutable drain error.
		s.lifecycle = nil
	}
	return cleanupErr
}

func (s *isolatedSession) closeNamespacePins() error {
	if s.namespacePins == nil {
		return nil
	}
	if err := s.namespacePins.Close(); err != nil {
		return errors.Join(
			ErrSessionNamespaceCleanup,
			fmt.Errorf("close isolated session namespace pins: %w", err),
		)
	}
	s.namespacePins = nil
	return nil
}

// markProcessExitedBeforeReap publishes the Linux WNOWAIT exit barrier before
// cmd.Wait can reap and release the numeric PID/PGID. If the kernel barrier
// itself fails, disable numeric group signals rather than risk targeting a
// reused identity; the bounded teardown path will force sandbox-level cleanup.
func (s *isolatedSession) markProcessExitedBeforeReap(barrierErr error) {
	s.processSignalMu.Lock()
	defer s.processSignalMu.Unlock()

	if barrierErr != nil {
		log.Error("isolated session exit barrier failed: %v", barrierErr)
	}
	s.processExited = true
}

// signalProcessGroupIfRunning serializes the signal syscall with the Linux
// pre-reap exit barrier. The group leader remains an unreaped child while this
// mutex is held, so its numeric PID/PGID cannot be reused for another session.
func (s *isolatedSession) signalProcessGroupIfRunning(signal syscall.Signal) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	s.processSignalMu.Lock()
	defer s.processSignalMu.Unlock()
	if s.processExited {
		return nil
	}
	select {
	case <-s.processWaited:
		s.processExited = true
		return nil
	default:
	}
	return signalSessionProcessGroup(s.cmd.Process.Pid, signal)
}

// dead returns true once the session can no longer admit work. Process exit,
// lifecycle trust loss, and an in-progress teardown are all terminal states.
func (s *isolatedSession) dead() bool {
	if s.stopping.Load() || s.lifecycleInvalid.Load() {
		return true
	}
	if s.processWaited != nil {
		select {
		case <-s.processWaited:
			return true
		default:
		}
	}
	// Keep synthetic and pre-lifecycle tests compatible; production sessions
	// always have processWaited.
	if s.doneCh != nil {
		select {
		case <-s.doneCh:
			return true
		default:
		}
	}
	return false
}

// beginOperation admits one filesystem operation while the session is active.
// Delete sets stopping before waiting for admitted operations, so no new
// operation can appear after the drain snapshot.
func (s *isolatedSession) beginOperation() error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if s.dead() {
		return ErrSessionNotActive
	}
	if s.activeOperations == 0 {
		s.operationsDrained = make(chan struct{})
	}
	s.activeOperations++
	return nil
}

func (s *isolatedSession) endOperation() {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if s.activeOperations == 0 {
		return
	}
	s.activeOperations--
	if s.activeOperations == 0 {
		close(s.operationsDrained)
	}
}

func (s *isolatedSession) waitForOperations(timeout time.Duration) error {
	s.operationMu.Lock()
	if s.activeOperations == 0 {
		s.operationMu.Unlock()
		return nil
	}
	drained := s.operationsDrained
	s.operationMu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drained:
		return nil
	case <-timer.C:
		return fmt.Errorf(
			"%w: filesystem operations did not drain",
			ErrSessionTeardownTimeout,
		)
	}
}

func (s *isolatedSession) failStartup(startErr error) error {
	if cleanupErr := s.stop(); cleanupErr != nil {
		return errors.Join(startErr, fmt.Errorf("clean up failed session startup: %w", cleanupErr))
	}
	return startErr
}

func closeCommandExtraFiles(cmd *exec.Cmd) {
	for _, file := range cmd.ExtraFiles {
		if file != nil {
			_ = file.Close()
		}
	}
	cmd.ExtraFiles = nil
}
