// Copyright 2026 Alibaba Group Holding Ltd.

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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/sessionresource"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

const isolatedRunEndMarkerPrefix = "__ISOLATED_RUN_END__"

// IsolatedRunner is the concrete isolated session runner.
type IsolatedRunner struct {
	ctrl            *Controller
	isolator        isolation.Isolator
	upperMgr        *isolation.UpperManager
	allowedWritable []string
	namespacePinner sessionNamespacePinner
	stopGC          chan struct{}
	// bgRuns tracks detached background runs (map[runID]*IsolatedBackgroundRun),
	// swept when the owning session is deleted or GC'd.
	bgRuns      sync.Map
	gcDone      chan struct{}
	stopGCOnce  sync.Once
	admissionMu sync.RWMutex
	closeMu     sync.Mutex
	closed      bool
	// pendingStartupCleanup owns failed creates whose workload did not reap
	// within the bounded startup rollback. These sessions are deliberately
	// kept out of the public controller map and retried by the collector.
	pendingStartupCleanup sync.Map // map[sessionID]*isolatedSession
}

// NewIsolatedRunner creates the isolated session runner.
func NewIsolatedRunner(ctrl *Controller, iso isolation.Isolator, cfg isolation.Config) (*IsolatedRunner, error) {
	mgr, err := isolation.NewUpperManager(cfg.UpperRoot, cfg.UpperMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("isolated runner: upper manager: %w", err)
	}
	namespaceMgr, err := sessionresource.NewNamespaceManager(
		sessionresource.DefaultNamespaceRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("isolated runner: namespace manager: %w", err)
	}
	r := &IsolatedRunner{
		ctrl:            ctrl,
		isolator:        iso,
		upperMgr:        mgr,
		allowedWritable: cfg.AllowedWritable,
		stopGC:          make(chan struct{}),
		gcDone:          make(chan struct{}),
		namespacePinner: func(
			ctx context.Context,
			identity isolation.WorkloadIdentity,
		) (sessionNamespacePins, error) {
			return namespaceMgr.Pin(ctx, identity)
		},
	}
	go r.gcLoop()

	// Register with telemetry so gauges can read session/upper stats.
	telemetry.SetIsolationStatsProvider(r.statsSnapshot)

	return r, nil
}

// statsSnapshot returns current isolation stats for telemetry gauges.
func (r *IsolatedRunner) statsSnapshot() telemetry.IsolationStats {
	sessionCount := int64(0)
	r.ctrl.isolatedSessionMap.Range(func(_, _ any) bool {
		sessionCount++
		return true
	})
	usage, _ := r.upperMgr.Usage()
	return telemetry.IsolationStats{
		ActiveSessions:  sessionCount,
		UpperUsageBytes: usage,
	}
}

// startGC begins periodic idle session cleanup.
func (r *IsolatedRunner) gcLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	defer close(r.gcDone)
	for {
		select {
		case <-r.stopGC:
			return
		case <-ticker.C:
			r.CollectIdle()
		}
	}
}

// CollectIdle scans sessions and deletes those past their idle timeout
// or whose bwrap process has died.
func (r *IsolatedRunner) CollectIdle() {
	r.ctrl.isolatedSessionMap.Range(func(key, value any) bool {
		s, ok := value.(*isolatedSession)
		if !ok {
			return true
		}

		sessionID := s.id

		// A Run updates lastRunAt while holding runMu. Acquire it before reading
		// the timestamp and retain it through deletion so a fresh Run cannot
		// enter between the idle decision and teardown.
		if !s.runMu.TryLock() {
			return true
		}
		func() {
			defer s.runMu.Unlock()
			if current := r.lookup(sessionID); current != s {
				return
			}

			dead := s.dead()
			s.mu.RLock()
			timeout := time.Duration(s.opts.IdleTimeoutSeconds) * time.Second
			idle := time.Since(s.lastRunAt)
			s.mu.RUnlock()
			if !dead {
				if s.activeBackgroundRuns.Load() > 0 {
					return // background run in flight; session is deliberately busy
				}
				if timeout <= 0 || idle <= timeout {
					return
				}
			}

			if dead {
				log.Info("idle GC: cleaning up dead session %s", sessionID)
			} else {
				log.Info(
					"idle GC: deleting session %s (idle %v > timeout %v)",
					sessionID,
					idle,
					timeout,
				)
			}
			if err := r.DeleteIsolatedSession(sessionID); err != nil {
				log.Warn("idle GC: delete session %s: %v", sessionID, err)
			}
		}()
		return true
	})
	if err := r.collectPendingStartupCleanup(); err != nil {
		log.Warn("idle GC: retry failed session startups: %v", err)
	}
	if err := r.collectReleasedUppers(); err != nil {
		log.Warn("idle GC: retry released upper directories: %v", err)
	}
}

// collectPendingStartupCleanup retries failed creates that could not be reaped
// during their bounded startup rollback.
func (r *IsolatedRunner) collectPendingStartupCleanup() error {
	var cleanupErr error
	r.pendingStartupCleanup.Range(func(key, value any) bool {
		id, idOK := key.(string)
		session, sessionOK := value.(*isolatedSession)
		if !idOK || !sessionOK {
			r.pendingStartupCleanup.Delete(key)
			return true
		}
		if err := r.cleanupPendingStartup(id, session); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("clean up failed startup %s: %w", id, err),
			)
		}
		return true
	})
	return cleanupErr
}

func (r *IsolatedRunner) cleanupPendingStartup(
	id string,
	session *isolatedSession,
) error {
	session.mu.Lock()
	defer session.mu.Unlock()

	current, ok := r.pendingStartupCleanup.Load(id)
	if !ok || current != session {
		return nil
	}

	stopErr := session.stop()
	if errors.Is(stopErr, ErrSessionTeardownTimeout) ||
		errors.Is(stopErr, ErrSessionNamespaceCleanup) {
		// The process or trusted lifecycle drain is still live. Keep both the
		// private owner entry and upper allocation intact for a later retry.
		// Namespace cleanup errors follow the same ownership rule even after
		// the process has been fully reaped.
		return fmt.Errorf("stop session process: %w", stopErr)
	}

	var cleanupErr error
	if stopErr != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("stop session process: %w", stopErr),
		)
	}
	if session.upperID != "" {
		if err := r.upperMgr.Remove(session.upperID); err != nil {
			// Remove marks the upper released but retains it in UpperManager on
			// failure, transferring retry ownership away from this session.
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("remove failed session upper: %w", err),
			)
		}
	}
	r.pendingStartupCleanup.CompareAndDelete(id, session)
	return cleanupErr
}

func (r *IsolatedRunner) collectReleasedUppers() error {
	freed, err := r.upperMgr.CollectWithErrors()
	for _, id := range freed {
		log.Info("idle GC: removed released upper directory %s", id)
	}
	return err
}

// StopGC stops the background GC goroutine.
func (r *IsolatedRunner) StopGC() {
	if r == nil || r.stopGC == nil {
		return
	}
	r.stopGCOnce.Do(func() {
		close(r.stopGC)
	})
	if r.gcDone != nil {
		<-r.gcDone
	}
}

// Close stops new Session admission, waits for in-flight creates, stops the
// collector, and synchronously attempts cleanup of all runtime-owned sessions.
// It is safe to call repeatedly; retained cleanup ownership is retried.
func (r *IsolatedRunner) Close() error {
	if r == nil {
		return nil
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()

	r.admissionMu.Lock()
	r.closed = true
	r.admissionMu.Unlock()
	r.StopGC()

	var cleanupErr error
	r.ctrl.isolatedSessionMap.Range(func(key, value any) bool {
		id, idOK := key.(string)
		session, sessionOK := value.(*isolatedSession)
		if !idOK || !sessionOK {
			return true
		}
		if current := r.lookup(id); current != session {
			return true
		}
		if err := r.DeleteIsolatedSession(id); err != nil &&
			!errors.Is(err, ErrContextNotFound) {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("close isolated session %s: %w", id, err),
			)
		}
		return true
	})
	if err := r.collectPendingStartupCleanup(); err != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("close failed session startups: %w", err),
		)
	}
	if err := r.collectReleasedUppers(); err != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("close released upper directories: %w", err),
		)
	}
	return cleanupErr
}

// Available reports whether the isolator is ready.
func (r *IsolatedRunner) Available() bool {
	return r.isolator.Available()
}

// CreateIsolatedSession starts a new bwrap + shell session.
func (r *IsolatedRunner) CreateIsolatedSession(opts *IsolatedSessionOptions) (string, error) {
	r.admissionMu.RLock()
	defer r.admissionMu.RUnlock()
	if r.closed {
		return "", ErrIsolatedRunnerClosed
	}

	if err := r.validateExtraWritable(opts.ExtraWritable); err != nil {
		return "", err
	}

	if err := r.validateBinds(opts.Binds); err != nil {
		return "", err
	}

	// Validate uid mode before normalization to keep the validator's handling
	// of the empty/default mode self-contained.
	if err := r.validateUidModeAvailable(opts); err != nil {
		return "", err
	}

	// Normalize empty/omitted fields to the effective config execd will
	// actually apply. GetIsolatedSession echoes s.opts on attach so a
	// stateless client can rebuild a handle from just the sessionId;
	// echoing the raw create request (with empty strings for unset
	// fields) would surface "unknown" for a session that is in fact
	// running with a concrete profile/mode. Applying the same defaults
	// here as (*isolatedSession).start would apply keeps the echo
	// aligned with the effective config without changing runtime
	// behavior.
	normalizeIsolatedOptions(opts)

	if err := os.MkdirAll(opts.WorkspacePath, 0o755); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}

	id := uuid.New().String()
	session := newIsolatedSession(id, opts, r.isolator, r.namespacePinner)

	// Allocate upper directory for overlay mode.
	if opts.WorkspaceMode == string(isolation.WorkspaceOverlay) || opts.WorkspaceMode == "" {
		upperID, upperDir, workDir, err := r.upperMgr.Allocate()
		if err != nil {
			return "", fmt.Errorf("allocate upper: %w", err)
		}
		session.upperID = upperID
		session.upperDir = upperDir
		session.workDir = workDir
	}

	if err := session.start(); err != nil {
		startErr := fmt.Errorf("start bwrap: %w", err)
		if errors.Is(err, ErrSessionTeardownTimeout) ||
			errors.Is(err, ErrSessionNamespaceCleanup) {
			// Create returns no ID, so failed startup ownership cannot live in
			// the public session map. Retain it privately until GC can prove the
			// workload and lifecycle are fully reaped.
			r.pendingStartupCleanup.Store(id, session)
			return "", startErr
		}
		if session.upperID != "" {
			if cleanupErr := r.upperMgr.Remove(session.upperID); cleanupErr != nil {
				return "", errors.Join(
					startErr,
					fmt.Errorf("remove failed session upper: %w", cleanupErr),
				)
			}
		}
		return "", startErr
	}

	r.ctrl.isolatedSessionMap.Store(id, session)
	go r.cleanupExitedSession(id, session)
	log.Info("created isolated session %s (profile=%s, mode=%s)", id, opts.Profile, opts.WorkspaceMode)
	return id, nil
}

func (r *IsolatedRunner) cleanupExitedSession(
	id string,
	session *isolatedSession,
) {
	<-session.doneCh
	if session.stopping.Load() {
		return
	}
	if current := r.lookup(id); current != session {
		return
	}
	log.Info("isolated session %s exited; starting resource cleanup", id)
	if err := r.DeleteIsolatedSession(id); err != nil &&
		!errors.Is(err, ErrContextNotFound) {
		log.Warn("clean up exited isolated session %s: %v", id, err)
	}
}

// GetIsolatedSession returns session state.
func (r *IsolatedRunner) GetIsolatedSession(id string) (*IsolatedSessionState, error) {
	s := r.lookup(id)
	if s == nil {
		return nil, ErrContextNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	status := SessionStatusActive
	if s.dead() {
		status = SessionStatusDead
	}

	state := &IsolatedSessionState{
		Status:    status,
		CreatedAt: s.createdAt,
		LastRunAt: s.lastRunAt,

		Profile:            s.opts.Profile,
		WorkspacePath:      s.opts.WorkspacePath,
		WorkspaceMode:      s.opts.WorkspaceMode,
		ExtraWritable:      s.opts.ExtraWritable,
		Binds:              s.opts.Binds,
		ShareNet:           s.opts.ShareNet,
		EnvPassthroughMode: s.opts.EnvPassthroughMode,
		EnvPassthroughKeys: s.opts.EnvPassthroughKeys,
		Uid:                s.opts.Uid,
		Gid:                s.opts.Gid,
		UidMode:            s.opts.UidMode,
		IdleTimeoutSeconds: s.opts.IdleTimeoutSeconds,
	}

	if s.opts.IdleTimeoutSeconds > 0 {
		remaining := s.opts.IdleTimeoutSeconds - int(time.Since(s.lastRunAt).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		state.IdleRemainingSeconds = &remaining
	}

	return state, nil
}

// Session status values.
const (
	SessionStatusActive = "active"
	SessionStatusDead   = "dead"
)

// IsolatedSessionState is returned by GetIsolatedSession.
//
// Runtime status fields are always populated. Creation-parameter fields
// echo the parameters used to create the session so a stateless client can
// rebuild a session handle from just a sessionId.
type IsolatedSessionState struct {
	Status               string
	CreatedAt            time.Time
	LastRunAt            time.Time
	IdleRemainingSeconds *int

	// Creation-parameter echoes. Populated for sessions the current execd
	// process created; snapshot of the *IsolatedSessionOptions at GET time.
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
	UidMode            string
	IdleTimeoutSeconds int
}

// IsolatedSessionSummary describes a single session in a list response.
type IsolatedSessionSummary struct {
	SessionID string
	IsolatedSessionState
}

// ListIsolatedSessions returns a summary of all active isolated sessions.
func (r *IsolatedRunner) ListIsolatedSessions() []IsolatedSessionSummary {
	summaries := make([]IsolatedSessionSummary, 0)
	r.ctrl.isolatedSessionMap.Range(func(key, value any) bool {
		s, ok := value.(*isolatedSession)
		if !ok {
			return true
		}

		s.mu.RLock()
		status := SessionStatusActive
		if s.dead() {
			status = SessionStatusDead
		}
		summary := IsolatedSessionSummary{
			SessionID: s.id,
			IsolatedSessionState: IsolatedSessionState{
				Status:    status,
				CreatedAt: s.createdAt,
				LastRunAt: s.lastRunAt,
			},
		}
		if s.opts.IdleTimeoutSeconds > 0 {
			remaining := s.opts.IdleTimeoutSeconds - int(time.Since(s.lastRunAt).Seconds())
			if remaining < 0 {
				remaining = 0
			}
			summary.IdleRemainingSeconds = &remaining
		}
		s.mu.RUnlock()

		summaries = append(summaries, summary)
		return true
	})
	return summaries
}

// StdoutCallback is called for each line of stdout output during Run.
type StdoutCallback func(line string)

// RunInIsolatedSession executes code in the session.
// Runs are serialized per session via s.runMu.
// envs are exported in the shell session before code runs.
func (r *IsolatedRunner) RunInIsolatedSession(ctx context.Context, id string, code string, envs map[string]string, onStdout StdoutCallback) error {
	s := r.lookup(id)
	if s == nil {
		return ErrContextNotFound
	}

	// Serialize concurrent runs on the same session.
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if s.dead() {
		return fmt.Errorf("%w: session process has exited", ErrSessionNotActive)
	}

	s.mu.RLock()
	stdin := s.stdin
	stdout := s.stdout
	s.mu.RUnlock()

	if stdin == nil || stdout == nil {
		return fmt.Errorf("session not started")
	}

	// Prepend env exports before user code.
	runMarker := fmt.Sprintf("%s_%s", isolatedRunEndMarkerPrefix, uuid.New().String())

	var script string
	if len(envs) > 0 {
		script += "(\n"
		for k, v := range envs {
			script += fmt.Sprintf("export %s=%s\n", shellescape(k), shellescape(v))
		}
		script += code
		if !strings.HasSuffix(script, "\n") {
			script += "\n"
		}
		script += ")\n"
	} else {
		script += code
		if !strings.HasSuffix(script, "\n") {
			script += "\n"
		}
	}
	script += fmt.Sprintf("echo %s $?\n", runMarker)

	// On timeout/cancel, send SIGINT to interrupt the running command
	// without killing the persistent shell session. Closing stdin would
	// terminate the shell entirely.
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	defer func() {
		close(done)
		// Keep runMu held until the watcher has either observed done or
		// completed its SIGINT. Otherwise a delayed cancellation from this Run
		// can interrupt the next serialized Run.
		<-watcherDone
	}()
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = s.signalProcessGroupIfRunning(syscall.SIGINT)
		case <-done:
		}
	}()

	if _, err := io.WriteString(stdin, script); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}

	exitCode, err := scanUntilMarker(ctx, stdout, runMarker, onStdout)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.lastRunAt = time.Now()
	s.mu.Unlock()

	if exitCode != 0 {
		return fmt.Errorf("command exited with code %d", exitCode)
	}

	return nil
}

// scanUntilMarker reads stdout lines until the end marker is found.
// Returns the exit code from the marker line.
func scanUntilMarker(ctx context.Context, stdout io.ReadCloser, runMarker string, onStdout StdoutCallback) (int, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var exitCode int
	markerSeen := false
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		for scanner.Scan() {
			line := scanner.Text()
			// The marker may appear mid-line if the previous command's
			// output didn't end with a newline (e.g. cat of a file
			// without trailing newline).
			if idx := strings.Index(line, runMarker); idx >= 0 {
				markerSeen = true
				if idx > 0 && onStdout != nil {
					onStdout(line[:idx])
				}
				markerPart := line[idx:]
				parts := strings.Fields(markerPart)
				if len(parts) >= 2 {
					if code, convErr := strconv.Atoi(parts[1]); convErr == nil {
						exitCode = code
					}
				}
				return
			}
			if onStdout != nil {
				onStdout(line)
			}
		}
	}()

	select {
	case <-scanDone:
	case <-ctx.Done():
		// Wait for scanner goroutine to finish so it doesn't consume the
		// next run's output on the shared stdout pipe.
		<-scanDone
		return 0, ctx.Err()
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read stdout: %w", err)
	}
	if !markerSeen {
		return 1, fmt.Errorf("session process exited without end marker (process may have died or called exit)")
	}
	return exitCode, nil
}

// DeleteIsolatedSession destroys the session.
func (r *IsolatedRunner) DeleteIsolatedSession(id string) error {
	s := r.lookup(id)
	if s == nil {
		return ErrContextNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A concurrent delete may have completed while this caller waited for the
	// session lock. Do not stop or remove the same resources twice.
	if current := r.lookup(id); current != s {
		return ErrContextNotFound
	}

	var cleanupErr error
	if stopErr := s.stop(); stopErr != nil {
		log.Warn("stop isolated session %s: %v", id, stopErr)
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("stop session process: %w", stopErr),
		)
		// The process may still be using the overlay. Keep both the map entry
		// and upper allocation intact so a later Delete or GC can retry.
		if errors.Is(stopErr, ErrSessionTeardownTimeout) ||
			errors.Is(stopErr, ErrSessionNamespaceCleanup) {
			return cleanupErr
		}
	}
	if operationErr := s.waitForOperations(isolatedSessionStopTimeout); operationErr != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("drain session filesystem operations: %w", operationErr),
		)
		// An admitted filesystem write may still be targeting the upper.
		// Retain the map entry and upper allocation until a later Delete or GC
		// observes the operation drain and can safely remove both.
		return cleanupErr
	}
	if s.upperID != "" {
		if err := r.upperMgr.Remove(s.upperID); err != nil {
			log.Warn("remove upper dir for session %s: %v", id, err)
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("remove session upper: %w", err),
			)
		}
	}

	r.ctrl.isolatedSessionMap.CompareAndDelete(id, s)
	// Run logs are gone with the upper layer; drop the session's run records.
	// Swept even when a non-fatal cleanup error remains: the session is out
	// of the public map, so nothing else would ever sweep them.
	r.removeSessionBackgroundRuns(s)
	if cleanupErr != nil {
		return cleanupErr
	}
	log.Info("deleted isolated session %s", id)
	return nil
}

// DiffUpper returns an error (Phase 2).
func (r *IsolatedRunner) DiffUpper(id string, w io.Writer) error {
	return fmt.Errorf("diff not implemented yet")
}

// CommitUpper returns an error (Phase 2).
func (r *IsolatedRunner) CommitUpper(id string) error {
	return fmt.Errorf("commit not implemented yet")
}

// GetMergedView returns a VFS whose individual calls acquire session operation
// leases. Callers that retain a handle returned by Open must instead use
// GetMergedViewWithLease so the lease covers the handle's full lifetime.
func (r *IsolatedRunner) GetMergedView(id string) (vfs.FS, error) {
	s := r.lookup(id)
	if s == nil {
		return nil, ErrContextNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead() {
		return nil, ErrSessionNotActive
	}

	return &isolatedSessionFS{
		session:  s,
		delegate: newMergedView(s),
	}, nil
}

// GetMergedViewWithLease atomically admits a multi-step files request and
// returns a raw VFS plus an idempotent release function. Delete denies new
// leases and waits for this lease before removing the session upper.
func (r *IsolatedRunner) GetMergedViewWithLease(id string) (vfs.FS, func(), error) {
	s := r.lookup(id)
	if s == nil {
		return nil, nil, ErrContextNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dead() {
		return nil, nil, ErrSessionNotActive
	}
	if err := s.beginOperation(); err != nil {
		return nil, nil, err
	}

	var once sync.Once
	release := func() {
		once.Do(s.endOperation)
	}
	return newMergedView(s), release, nil
}

// newMergedView snapshots the immutable filesystem configuration. The caller
// must hold s.mu for reading while constructing the view.
func newMergedView(s *isolatedSession) vfs.FS {
	// MergedView chowns files on the host side (execd's namespace).
	// In setpriv mode the requested uid/gid are real host IDs, so use them.
	// In userns mode the requested uid/gid are in-namespace IDs mapped to
	// execd's real host uid/gid, so host-side files must use execd's own
	// host uid/gid — chowning to the in-namespace ID would fail with EPERM
	// (unprivileged execd) or create files that appear as nobody/overflow
	// inside the sandbox.
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	if isolation.UidMode(s.opts.UidMode) != isolation.UidModeUserns {
		if s.opts.Uid != nil {
			uid = *s.opts.Uid
		}
		if s.opts.Gid != nil {
			gid = *s.opts.Gid
		}
	}

	mode := isolation.WorkspaceOverlay
	upper := s.upperDir
	switch isolation.WorkspaceMode(s.opts.WorkspaceMode) {
	case isolation.WorkspaceRW:
		mode = isolation.WorkspaceRW
		upper = s.opts.WorkspacePath // writes go directly to workspace
	case isolation.WorkspaceRO:
		mode = isolation.WorkspaceRO
	}

	return isolation.NewMergedView(s.opts.WorkspacePath, upper, mode, uid, gid)
}

// Capabilities returns the current isolator capabilities.
func (r *IsolatedRunner) Capabilities() isolation.Capabilities {
	return r.isolator.Capabilities()
}

func (r *IsolatedRunner) validateUidModeAvailable(opts *IsolatedSessionOptions) error {
	mode := isolation.UidModeSetpriv
	if opts != nil && opts.UidMode != "" {
		mode = isolation.UidMode(opts.UidMode)
	}

	caps := r.isolator.Capabilities()
	switch mode {
	case isolation.UidModeSetpriv:
		if !caps.SetprivAvailable {
			return fmt.Errorf("%w: %s", ErrUidModeUnavailable, mode)
		}
		if setprivIdentitySwitchRequired(opts, uint32(os.Getuid()), uint32(os.Getgid())) &&
			!caps.SetprivSwitchAvailable {
			return fmt.Errorf("%w: %s cannot switch to the requested uid/gid", ErrUidModeUnavailable, mode)
		}
	case isolation.UidModeUserns:
		if !caps.UsernsAvailable {
			return fmt.Errorf("%w: %s", ErrUidModeUnavailable, mode)
		}
	default:
		return fmt.Errorf("%w: unknown uid mode %q", ErrUidModeUnavailable, mode)
	}
	return nil
}

func setprivIdentitySwitchRequired(opts *IsolatedSessionOptions, currentUID, currentGID uint32) bool {
	targetUID, targetGID := currentUID, currentGID
	if opts != nil {
		if opts.Uid != nil {
			targetUID = *opts.Uid
		}
		if opts.Gid != nil {
			targetGID = *opts.Gid
		}
	}
	return targetUID != currentUID || targetGID != currentGID
}

func (r *IsolatedRunner) lookup(id string) *isolatedSession {
	v, ok := r.ctrl.isolatedSessionMap.Load(id)
	if !ok {
		return nil
	}
	s, ok := v.(*isolatedSession)
	if !ok {
		return nil
	}
	return s
}

func (r *IsolatedRunner) validateExtraWritable(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if len(r.allowedWritable) == 0 {
		return fmt.Errorf("extra_writable not allowed: no paths in allowlist")
	}
	for i := range paths {
		resolved, err := r.resolveAllowedSource(paths[i])
		if err != nil {
			return fmt.Errorf("extra_writable path %q: %w", paths[i], err)
		}
		// Mount the fully-resolved path so validation and mount target agree.
		paths[i] = resolved
	}
	return nil
}

// resolveAllowedSource requires src to exist, fully resolves symlinks, checks
// the resolved real path against the writable allowlist, and returns it. It is
// shared by extra_writable and binds so both enforce identical semantics:
//   - the source must already exist (bwrap --bind requires this anyway), and
//   - the allowlist is enforced against the fully-resolved real path, leaving
//     no unresolved suffix that could be swapped to an out-of-allowlist symlink
//     between validation and bwrap start.
func (r *IsolatedRunner) resolveAllowedSource(src string) (string, error) {
	if src == "" {
		return "", fmt.Errorf("source is required")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(src))
	if err != nil {
		return "", fmt.Errorf("must be an existing path: %w", err)
	}
	if !r.pathAllowedResolved(resolved) {
		return "", fmt.Errorf("not in allowlist")
	}
	return resolved, nil
}

// validateBinds checks that every bind's source path falls within the writable
// allowlist. Read-only binds are validated too, so read access to arbitrary host
// paths outside the allowlist is not possible.
//
// The source of each bind must already exist and is fully resolved via
// filepath.EvalSymlinks; the resolved real path is written back in place. This
// enforces the allowlist against the real target and closes the TOCTOU window:
// bwrap is handed a fully-resolved path with no unresolved suffix, so a symlink
// created or swapped between validation and bwrap start cannot redirect the
// mount outside the allowlist. (bwrap's --bind requires the source to exist, so
// this adds no functional restriction.)
func (r *IsolatedRunner) validateBinds(binds []isolation.BindMount) error {
	if len(binds) == 0 {
		return nil
	}
	if len(r.allowedWritable) == 0 {
		return fmt.Errorf("binds not allowed: no paths in allowlist")
	}
	for i := range binds {
		resolved, err := r.resolveAllowedSource(binds[i].Source)
		if err != nil {
			return fmt.Errorf("binds source %q: %w", binds[i].Source, err)
		}
		// Mount the fully-resolved path so validation and mount target agree.
		binds[i].Source = resolved
	}
	return nil
}

// pathAllowedResolved reports whether an already symlink-resolved path is equal
// to, or nested under, any allowlist entry. Allowlist entries are themselves
// symlink-resolved so the comparison is between real paths on both sides.
func (r *IsolatedRunner) pathAllowedResolved(resolved string) bool {
	for _, allowed := range r.allowedWritable {
		allowedClean := resolveSymlinks(filepath.Clean(allowed))
		if resolved == allowedClean || strings.HasPrefix(resolved, allowedClean+"/") {
			return true
		}
	}
	return false
}

// resolveSymlinks returns the real path of p with symlinks resolved. Because p
// (or a leading component) may not exist yet, it resolves the longest existing
// prefix and re-appends the remaining components, so a symlinked ancestor is
// still followed while a not-yet-created leaf is preserved.
func resolveSymlinks(p string) string {
	if p == "" {
		return p
	}
	remaining := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Clean(filepath.Join(resolved, remaining))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root without an existing prefix; fall back to lexical.
			return p
		}
		remaining = filepath.Join(filepath.Base(cur), remaining)
		cur = parent
	}
}

// shellescape wraps s in single quotes, escaping embedded single quotes.
func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// normalizeIsolatedOptions fills in the effective values for fields that
// (*isolatedSession).start would otherwise substitute silently, so that
// GetIsolatedSession echoes back the configuration execd is actually
// running with. Only empty/omitted string fields are rewritten; explicit
// values (including unknown enum strings) are left untouched so that
// start() surfaces them as errors as before.
//
// Kept in sync with the switch statements in (*isolatedSession).start.
func normalizeIsolatedOptions(opts *IsolatedSessionOptions) {
	if opts == nil {
		return
	}
	if opts.Profile == "" {
		opts.Profile = string(isolation.ProfileStrict)
	}
	// start() treats any non-rw/non-ro string as overlay, but only "" is
	// really "unset" from the caller's perspective. Unknown enum values
	// are left in place so a future normalize→start mismatch is loud.
	if opts.WorkspaceMode == "" {
		opts.WorkspaceMode = string(isolation.WorkspaceOverlay)
	}
	if opts.EnvPassthroughMode == "" {
		// The pre-normalization behavior of start() was: on empty mode,
		// forward EnvSpec{Mode: deny} to bwrap WITHOUT the caller's Keys,
		// which bwrapEnvSegment then treats as "apply the built-in secret
		// blacklist" (see bwrapEnvSegment case EnvModeDeny with len(Keys)==0).
		//
		// If we normalize mode to "deny" while leaving Keys populated, bwrap
		// would instead unset only those caller-supplied keys and skip the
		// blacklist — a silent security regression for callers that supplied
		// keys without mode. Clear Keys here to preserve the effective
		// behavior (built-in blacklist wins on omitted mode).
		opts.EnvPassthroughMode = string(isolation.EnvModeDeny)
		opts.EnvPassthroughKeys = nil
	}
	if opts.UidMode == "" {
		opts.UidMode = string(isolation.UidModeSetpriv)
	}
}
