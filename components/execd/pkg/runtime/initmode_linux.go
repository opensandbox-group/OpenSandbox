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

// Init mode (OSEP-0018, phase 1): execd is the sandbox init. It reaps every
// child through a single reaper, forwards application signals to the user
// entrypoint, and owns the container lifecycle (entrypoint exit code is
// propagated to the runtime).
//
// The reaper is the only wait4-family caller in init mode, so execd never
// calls os/exec.Cmd.Wait for its own children. Callers build the process with
// exec.Command as usual but launch and wait through the managedProcess
// abstraction, which reproduces the pipe teardown Cmd.Wait would otherwise
// perform.
//
// The reaper registry lock spans child start and registration: a child is
// added to the owned map before any concurrent drain can observe it, so the
// start/register race is closed structurally. Any child observed that is not
// owned is a reparented orphan and is reaped and logged.

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/alibaba/opensandbox/internal/safego"
	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

var (
	initShutdownGrace    = 10 * time.Second
	initReaper           *reaper
	reaperSweepInterval  = 200 * time.Millisecond
	initForwardedSignals = []os.Signal{
		syscall.SIGTERM,
		syscall.SIGHUP,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
		syscall.SIGWINCH,
	}
)

// siginfoWait mirrors the kernel siginfo_t fields waitid fills. The vendored
// x/sys Siginfo only exposes the first three fields, and the union member
// offsets are arch-specific, so this struct is the 64-bit Linux layout
// (verified on amd64 and arm64): the union is 8-byte aligned, so si_pid sits
// at offset 16, si_uid at 20, si_status at 24. waitid's si_status is the raw
// exit code or signal number, not the wait4 status encoding.
type siginfoWait struct {
	signo  int32
	errno  int32
	code   int32
	_      int32
	pid    int32
	uid    uint32
	status int32
	_      [104]byte
}

// waitStatus converts waitid's si_status/si_code into the syscall.WaitStatus
// encoding the rest of the code base understands. si_code values are the
// stable UAPI CLD_* constants (linux/siginfo.h).
func (i *siginfoWait) waitStatus() syscall.WaitStatus {
	switch i.code {
	case 1: // CLD_EXITED
		return syscall.WaitStatus(uint32(i.status&0xff) << 8)
	case 2, 3: // CLD_KILLED, CLD_DUMPED
		return syscall.WaitStatus(uint32(i.status & 0x7f))
	default:
		return 0
	}
}

func waitidObserve(info *siginfoWait) error {
	_, _, errno := unix.Syscall6(unix.SYS_WAITID,
		uintptr(unix.P_ALL), 0,
		uintptr(unsafe.Pointer(info)),
		uintptr(unix.WEXITED|unix.WNOHANG|unix.WNOWAIT),
		0, 0)
	if errno == 0 {
		return nil
	}
	if errno == unix.EINTR {
		return unix.EINTR
	}
	return errno
}

func waitidConsume(pid int) (syscall.WaitStatus, error) {
	var info siginfoWait
	_, _, errno := unix.Syscall6(unix.SYS_WAITID,
		uintptr(unix.P_PID), uintptr(pid),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unix.WEXITED|unix.WNOHANG),
		0, 0)
	if errno == 0 {
		return info.waitStatus(), nil
	}
	if errno == unix.EINTR {
		return 0, unix.EINTR
	}
	return 0, errno
}

// reaper is the single wait4-family caller while init mode is active.
type reaper struct {
	mu       sync.Mutex
	owned    map[int]*managedProcess
	sigchld  chan os.Signal
	quit     chan struct{}
	quitOnce sync.Once //nolint:unused // test-only lifecycle; see stop
	done     chan struct{}
}

func newReaper() *reaper {
	return &reaper{
		owned: map[int]*managedProcess{},
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// start registers the SIGCHLD notification synchronously so no child can
// exit before the handler exists (a lost SIGCHLD would strand its status).
func (r *reaper) start() {
	r.sigchld = make(chan os.Signal, 1)
	signal.Notify(r.sigchld, syscall.SIGCHLD)
}

// stop terminates the reaper and waits until its signal subscription is
// removed. Test-only in practice; execd runs one reaper for its lifetime.
//
//nolint:unused // test-only lifecycle; execd runs one reaper for its lifetime
func (r *reaper) stop() {
	r.quitOnce.Do(func() { close(r.quit) })
	<-r.done
}

func (r *reaper) run() {
	defer func() {
		signal.Stop(r.sigchld)
		close(r.done)
	}()
	// The ticker is a backstop: SIGCHLD may be coalesced or (in edge cases)
	// lost, so a periodic drain keeps the process table bounded regardless.
	sweep := time.NewTicker(reaperSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-r.quit:
			return
		case <-r.sigchld:
			r.drain()
		case <-sweep.C:
			r.drain()
		}
	}
}

func (r *reaper) drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var info siginfoWait
		if err := waitidObserve(&info); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if !errors.Is(err, unix.ECHILD) {
				log.Warn("init: reaper observe: %v", err)
			}
			return
		}
		if info.pid == 0 {
			return
		}
		pid := int(info.pid)
		if mp := r.owned[pid]; mp != nil {
			// The pre-reap barrier runs between the WNOWAIT observe and the
			// consuming wait, while the kernel still reserves the PID/PGID
			// (isolated sessions rely on this to avoid signalling a recycled
			// process group).
			if mp.preReap != nil {
				mp.preReap()
			}
			ws, err := waitidConsume(pid)
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				log.Error("init: reaper consume pid %d: %v", pid, err)
				return
			}
			// Drop the child from the registry once reaped: stale entries
			// would grow without bound and shutdown could signal a recycled
			// process group.
			delete(r.owned, pid)
			mp.deliver(ws)
			continue
		}
		// Unknown child: reparented orphan. Reap it so the process table
		// stays bounded; its status is not delivered to anyone.
		ws, err := waitidConsume(pid)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			log.Error("init: reaper consume orphan pid %d: %v", pid, err)
			return
		}
		log.Info("init: reaped orphan pid=%d status=%s", pid, ws)
	}
}

// managedProcess wraps an exec.Cmd whose status is delivered by the reaper.
// In non-init mode it falls back to plain Cmd.Start/Cmd.Wait, so callers
// share one launch path regardless of mode.
type managedProcess struct {
	cmd         *exec.Cmd
	preReap     func()
	noHardening bool
	stripEnv    []string // nil = default blacklist; explicit list overrides
	done        chan struct{}
	once        sync.Once
	ws          syscall.WaitStatus
	exitErr     error
}

func newManagedProcess(cmd *exec.Cmd) *managedProcess {
	return &managedProcess{cmd: cmd, done: make(chan struct{})}
}

func (mp *managedProcess) pid() int {
	return mp.cmd.Process.Pid
}

func (mp *managedProcess) deliver(ws syscall.WaitStatus) {
	mp.once.Do(func() {
		mp.ws = ws
		mp.exitErr = exitStatusError(ws)
		close(mp.done)
	})
}

func (mp *managedProcess) Wait() error {
	if initReaper == nil {
		return mp.cmd.Wait()
	}
	<-mp.done
	return mp.exitErr
}

// ExitCode returns the process exit code, or -1 if it has not exited (or was
// killed by a signal), matching os.ProcessState.ExitCode semantics.
func (mp *managedProcess) ExitCode() int {
	if initReaper == nil {
		return mp.cmd.ProcessState.ExitCode()
	}
	select {
	case <-mp.done:
		return mp.ws.ExitStatus()
	default:
		return -1
	}
}

func (mp *managedProcess) exitStatus() syscall.WaitStatus {
	return mp.ws
}

type launchOption func(*managedProcess)

func withPreReap(fn func()) launchOption {
	return func(mp *managedProcess) {
		mp.preReap = fn
	}
}

// withoutHardening exempts a launch from the hardening floor. Used for the
// bwrap process of isolated sessions, whose workload is already reduced
// inside the namespace and whose own syscalls (unshare) the floor would deny.
func withoutHardening() launchOption {
	return func(mp *managedProcess) {
		mp.noHardening = true
	}
}

// bootstrapEnv overrides the env strip for the user entrypoint: the image's
// own entrypoint scripts may need JUPYTER_TOKEN/EXECD_ENVS to configure
// themselves (e.g. the code-interpreter entrypoint), so those survive — but
// EXECD_ACCESS_TOKEN is execd's control-plane credential and must never
// reach the long-lived entrypoint (its Jupyter kernels are user code).
func bootstrapEnv() launchOption {
	return func(mp *managedProcess) {
		mp.stripEnv = []string{"EXECD_ACCESS_TOKEN"}
	}
}

// launchManagedWith starts the command and registers it with the reaper.
// startFn is called under the reaper lock so the child cannot be observed
// (and misclassified as an orphan) before registration. When the hardening
// floor is active, cmd is first rewritten to exec through the launcher.
func launchManagedWith(cmd *exec.Cmd, startFn func() error, opts ...launchOption) (*managedProcess, error) {
	mp := newManagedProcess(cmd)
	for _, o := range opts {
		o(mp)
	}
	policyFile, err := hardenCmd(cmd, mp.noHardening, mp.stripEnv)
	if err != nil {
		return nil, err
	}
	if policyFile != nil {
		defer policyFile.Close()
	}
	if initReaper == nil {
		if err := startFn(); err != nil {
			return nil, err
		}
		return mp, nil
	}
	initReaper.mu.Lock()
	defer initReaper.mu.Unlock()
	if err := startFn(); err != nil {
		return nil, err
	}
	initReaper.owned[cmd.Process.Pid] = mp
	return mp, nil
}

func launchManaged(cmd *exec.Cmd, opts ...launchOption) (*managedProcess, error) {
	return launchManagedWith(cmd, cmd.Start, opts...)
}

// waitManagedWithBarrier mirrors waitCommandWithExitBarrier: in init mode the
// pre-reap barrier was registered at launch and runs inside the reaper, so
// this just waits; otherwise the original WNOWAIT barrier path applies.
func waitManagedWithBarrier(mp *managedProcess, mark func(error)) error {
	if initReaper == nil {
		return waitCommandWithExitBarrier(mp.cmd, mark)
	}
	return mp.Wait()
}

// processExitError is the error returned by managedProcess.Wait in init mode
// when the child did not exit cleanly. It mirrors exec.ExitError's contract
// without needing a constructed os.ProcessState.
type processExitError struct {
	code int
	msg  string
}

func (e *processExitError) Error() string {
	return e.msg
}

func (e *processExitError) ExitCode() int {
	return e.code
}

func exitStatusError(ws syscall.WaitStatus) error {
	if ws.Exited() {
		if code := ws.ExitStatus(); code == 0 {
			return nil
		} else {
			return &processExitError{code: code, msg: fmt.Sprintf("exit status %d", code)}
		}
	}
	return &processExitError{code: -1, msg: fmt.Sprintf("signal: %v", ws.Signal())}
}

// StartInitMode activates init duties: non-dumpable self, subreaper fallback
// when not PID 1, the reaper, the user entrypoint, signal forwarding, and the
// container lifecycle owner. It returns once the entrypoint is launched; the
// process is torn down via os.Exit when the entrypoint exits or SIGTERM
// arrives.
func StartInitMode(entryArgs []string) {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		log.Warn("init: PR_SET_DUMPABLE(0) failed: %v", err)
	}
	if os.Getpid() != 1 {
		// Pool path (or a misconfigured background launch): execd is not the
		// kernel init, so orphaned descendants reparent to it only if it is a
		// subreaper. The kernel signal shield is lost in this mode.
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			log.Warn("init: PR_SET_CHILD_SUBREAPER failed: %v", err)
		}
	}
	initReaper = newReaper()
	initReaper.start()
	safego.Go(initReaper.run)
	log.Info("init: execd is the sandbox init (pid=%d mode=%s)", os.Getpid(), initModeName())

	// Register the application-signal subscription before the entrypoint
	// starts: an early SIGTERM must reach the forwarding loop instead of
	// hitting the runtime default handler.
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, initForwardedSignals...)

	entry := launchEntrypoint(entryArgs)
	if entry == nil {
		signal.Stop(sigCh)
		return
	}
	safego.Go(func() { forwardInitSignals(entry, sigCh) })
	safego.Go(func() { waitEntrypointExit(entry) })
}

func launchEntrypoint(args []string) *managedProcess {
	if len(args) == 0 {
		log.Warn("init: --init set but no user command provided; no entrypoint to supervise")
		return nil
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	mp, err := launchManaged(cmd, bootstrapEnv())
	if err != nil {
		log.Error("init: failed to start user entrypoint %q: %v", args[0], err)
		os.Exit(1)
	}
	log.Info("init: user entrypoint started pid=%d argv=%v", mp.pid(), args)
	return mp
}

// waitEntrypointExit owns the container lifecycle: when the entrypoint exits,
// the other children are stopped gracefully and execd exits with the
// entrypoint's status so Docker/kubelet observe it.
func waitEntrypointExit(entry *managedProcess) {
	entryErr := entry.Wait()
	code := initExitCode(entry)
	log.Info("init: user entrypoint exited: code=%d err=%v", code, entryErr)
	stopChildrenExcept(entry)
	log.Info("init: exiting with entrypoint status %d", code)
	os.Exit(code)
}

// initExitCode converts a delivered status into the container exit code,
// following the shell convention of 128+signal for signalled processes.
func initExitCode(mp *managedProcess) int {
	ws := mp.exitStatus()
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if ws.Exited() {
		return ws.ExitStatus()
	}
	return 1
}

// forwardInitSignals forwards application signals to the entrypoint process
// group. SIGTERM additionally starts the graceful shutdown sequence, matching
// the runtime-initiated container stop contract (Docker/K8s send SIGTERM to
// PID 1).
func forwardInitSignals(entry *managedProcess, ch <-chan os.Signal) {
	for sig := range ch {
		s, ok := sig.(syscall.Signal)
		if !ok {
			continue
		}
		if s == syscall.SIGTERM {
			log.Info("init: received SIGTERM; forwarding to workload and shutting down")
			terminateInit(entry)
			return
		}
		log.Info("init: forwarding %v to workload", s)
		if err := killGroup(entry.pid(), s); err != nil {
			log.Warn("init: forward %v to entrypoint group: %v", s, err)
		}
	}
}

// terminateInit performs the SIGTERM shutdown: forward TERM to the entrypoint
// tree, stop the other children, then exit once the entrypoint is reaped
// (SIGKILL after a bounded grace).
func terminateInit(entry *managedProcess) {
	if err := killGroup(entry.pid(), syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Warn("init: SIGTERM entrypoint group: %v", err)
	}
	stopChildrenExcept(entry)
	deadline := time.After(initShutdownGrace)
	select {
	case <-entry.done:
	case <-deadline:
		if err := killGroup(entry.pid(), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.Warn("init: SIGKILL entrypoint group: %v", err)
		}
		select {
		case <-entry.done:
		case <-time.After(5 * time.Second):
		}
	}
	log.Info("init: exiting after SIGTERM shutdown")
	os.Exit(initExitCode(entry))
}

// stopChildrenExcept signals every other tracked child group with SIGTERM,
// waits up to the shutdown grace (total budget across all children), then
// SIGKILLs the survivors. Reaping is done by the reaper; the kernel reaps
// anything left when execd exits.
func stopChildrenExcept(keep *managedProcess) {
	// SIGTERM and the final SIGKILL pass are sent while holding the reaper
	// lock: the pid is verified against the owned map and the reaper cannot
	// consume (and release) the PID/PGID between verification and kill, so
	// a recycled process group can never be signalled.
	others := initReaper.signalOthers(keep, syscall.SIGTERM)
	if len(others) == 0 {
		return
	}
	deadline := time.Now().Add(initShutdownGrace)
	for _, mp := range others {
		select {
		case <-mp.done:
		case <-time.After(time.Until(deadline)):
		}
	}
	initReaper.signalOthers(keep, syscall.SIGKILL)
}

// signalOthers delivers sig to every still-tracked child group except keep,
// while holding the reaper lock. It returns the targets that were signalled.
func (r *reaper) signalOthers(keep *managedProcess, sig syscall.Signal) []*managedProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	var others []*managedProcess
	for pid, mp := range r.owned {
		if mp == keep {
			continue
		}
		others = append(others, mp)
		if err := killGroup(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.Warn("init: %v child group %d: %v", sig, pid, err)
		}
	}
	return others
}

// killGroup sends sig to the child's process group; all managed children are
// launched with Setpgid, so the group id equals the child pid.
func killGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

func initModeName() string {
	if os.Getpid() == 1 {
		return "pid1"
	}
	return "subreaper"
}

// InitModeReport reports the init mode actually in effect for the
// capabilities endpoint.
func InitModeReport() (mode string, signalShield bool) {
	if initReaper == nil {
		return "none", false
	}
	if os.Getpid() == 1 {
		return "pid1", true
	}
	return "subreaper", false
}

// initModeActive reports whether the init-mode signal/runtime ownership is in
// effect (execd started with --init). Shared launch paths consult it to avoid
// competing with forwardInitSignals.
func initModeActive() bool {
	return initReaper != nil
}
