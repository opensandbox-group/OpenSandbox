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
)

// isolatedBackgroundRunDir is the workspace-relative directory for background
// run log and exit-code files (dot-dir, created by the session shell).
const isolatedBackgroundRunDir = ".execd/background-runs"

// isolatedBackgroundPollInterval is how often the watcher checks for the
// exit-code file of a background run.
const isolatedBackgroundPollInterval = 500 * time.Millisecond

// maxBackgroundLogReadBytes caps how much of a run's log a single read
// (remaining bytes after the cursor) loads into memory; clients page through
// the rest with the returned cursor.
const maxBackgroundLogReadBytes = 16 << 20 // 16 MiB

// isolatedBackgroundProbeTimeout bounds the preflight probe that verifies the
// run log directory is writable by the session uid before a run is accepted.
const isolatedBackgroundProbeTimeout = 5 * time.Second

// IsolatedBackgroundRun tracks a detached run inside an isolated session.
// Output and exit code are redirected by the session shell to files under the
// workspace's background-runs directory, readable from the host side.
type IsolatedBackgroundRun struct {
	ID        string
	SessionID string
	logPath   string
	exitPath  string
	// fallback*: used when the run directory cannot be created inside the
	// namespace (e.g. a lower-layer .execd blocks overlay copy-up); the exit
	// code is then written to the workspace root instead, so the watcher
	// still sees completion.
	fallbackLogPath  string
	fallbackExitPath string
	// logRoot is the host-side directory (upper layer for overlay workspaces,
	// the workspace for rw) all control-file operations are pinned to. Files
	// are opened relative to it without following symlinks: the workspace is
	// writable by sandbox code, which could otherwise replace a control file
	// with a symlink and have host-side execd read or truncate an arbitrary
	// host file through it.
	logRoot string

	mu         sync.Mutex
	startedAt  time.Time
	finishedAt *time.Time
	exitCode   *int
	errMsg     string
	running    bool
}

// relLogPath/relExitPath/relFallbackLogPath/relFallbackExitPath return the
// run's control files relative to logRoot (the run directory for the primary
// files, the workspace root for the fallbacks).
func (r *IsolatedBackgroundRun) relLogPath() string {
	return filepath.Join(isolatedBackgroundRunDir, r.ID+".log")
}

func (r *IsolatedBackgroundRun) relExitPath() string {
	return filepath.Join(isolatedBackgroundRunDir, r.ID+".code")
}

func (r *IsolatedBackgroundRun) relFallbackLogPath() string {
	return r.ID + ".log"
}

func (r *IsolatedBackgroundRun) relFallbackExitPath() string {
	return r.ID + ".code"
}

// IsolatedBackgroundRunSnapshot is a consistent read of a background run.
type IsolatedBackgroundRunSnapshot struct {
	RunID      string
	SessionID  string
	Running    bool
	ExitCode   *int
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}

func (r *IsolatedBackgroundRun) markFinished(exitCode *int, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.exitCode = exitCode
	r.errMsg = errMsg
	r.running = false
	r.finishedAt = &now
}

func (r *IsolatedBackgroundRun) snapshot() IsolatedBackgroundRunSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return IsolatedBackgroundRunSnapshot{
		RunID:      r.ID,
		SessionID:  r.SessionID,
		Running:    r.running,
		ExitCode:   r.exitCode,
		Error:      r.errMsg,
		StartedAt:  r.startedAt,
		FinishedAt: r.finishedAt,
	}
}

// RunInIsolatedSessionBackground starts code detached inside the session and
// returns immediately with a run ID. Runs are serialized per session (shared
// stdin), but this only holds the run mutex long enough to submit the script.
// The detached process dies with the bwrap process group, so Delete reaps it.
func (r *IsolatedRunner) RunInIsolatedSessionBackground(
	id string,
	code string,
	envs map[string]string,
) (string, time.Time, error) {
	s := r.lookup(id)
	if s == nil {
		return "", time.Time{}, ErrContextNotFound
	}

	s.runMu.Lock()
	defer s.runMu.Unlock()

	if s.dead() {
		return "", time.Time{}, ErrSessionNotActive
	}

	// Capture the session stdin once under the lock: Delete closes and nils
	// the pipes under s.mu, so a bare s.stdin write after the preflight could
	// dereference a nil writer. A captured pipe may still be closed by a
	// concurrent Delete — writes then fail cleanly with an error.
	s.mu.RLock()
	stdin := s.stdin
	s.mu.RUnlock()
	if stdin == nil {
		return "", time.Time{}, fmt.Errorf("session not started")
	}

	paths, err := s.backgroundRunPaths()
	if err != nil {
		return "", time.Time{}, err
	}

	runID := uuid.New().String()

	// Preflight: verify the session uid can write the run log directory (the
	// run dir, or the workspace root fallback) before accepting the run. A
	// session whose workspace is not writable by its own uid (e.g. an
	// execd-created workspace owned by root with a setpriv session running as
	// another uid) would otherwise accept the run and ghost it: no log or
	// exit-code file could ever be written.
	if err := r.preflightBackgroundLogDir(s, stdin, paths); err != nil {
		return "", time.Time{}, err
	}

	// The background job runs under `bash -c` (or `sh -c` when bash is
	// unavailable) so user code is parsed as its own program: shell syntax
	// errors in the code cannot break the control wrapper, which would
	// otherwise leave the run stuck `running` (the wrapper's completion lines
	// are only parsed after the code). The code's output is redirected to the
	// log file; its exit code is captured and written to the code file. The
	// script's mkdir creates the run dir as the session's own uid (so it is
	// writable even when the session runs as a different uid than execd); if
	// the dir cannot be created or is not writable (e.g. a lower-layer .execd
	// blocks overlay copy-up), $D falls back to the workspace root so
	// completion is always reported. $D is re-resolved after the code runs in
	// case the code deleted or replaced the run directory, so the completion
	// marker cannot be lost that way. The outer >/dev/null discards residual
	// diagnostics so nothing leaks into the session stdout that the next
	// foreground run's end-marker scan consumes.
	shell := getShell()
	script := "{ " + backgroundRunDirScript(paths) + "; "
	if len(envs) > 0 {
		for k, v := range envs {
			script += "export " + shellescape(k) + "=" + shellescape(v) + "; "
		}
	}
	script += shell + " -c " + shellescape(code) + " </dev/null >\"$D/" + runID + ".log\" 2>&1"
	script += "\ncode=$?; mkdir -p \"$D\" 2>/dev/null && [ -w \"$D\" ] || D=" +
		shellescape(paths.nsWorkspace)
	script += "\necho \"$code\" >\"$D/" + runID + ".code\"; } >/dev/null 2>&1 &\n"

	// Publish the run record before writing the script so a concurrent
	// DeleteIsolatedSession (which sweeps run records after removing the
	// session) can never run its sweep before our record exists, leaving it
	// orphaned in bgRuns. If the write then fails (e.g. the session was
	// deleted and the pipe is closed), the record is removed again.
	startedAt := time.Now()
	run := &IsolatedBackgroundRun{
		ID:               runID,
		SessionID:        id,
		logPath:          filepath.Join(paths.hostRunDir, runID+".log"),
		exitPath:         filepath.Join(paths.hostRunDir, runID+".code"),
		fallbackLogPath:  filepath.Join(paths.hostWorkspace, runID+".log"),
		fallbackExitPath: filepath.Join(paths.hostWorkspace, runID+".code"),
		logRoot:          paths.hostRoot,
		startedAt:        startedAt,
		running:          true,
	}
	s.activeBackgroundRuns.Add(1)
	r.bgRuns.Store(runID, run)

	if _, err := io.WriteString(stdin, script); err != nil {
		s.activeBackgroundRuns.Add(-1)
		r.bgRuns.Delete(runID)
		return "", time.Time{}, fmt.Errorf("write stdin: %w", err)
	}

	// Anchor the idle clock so a finished run's session is not reaped before
	// its idle window elapses.
	s.mu.Lock()
	s.lastRunAt = time.Now()
	s.mu.Unlock()

	// Recheck that the session is still registered: a concurrent Delete may
	// have removed it (and swept this run record) while we held the run
	// mutex, in which case returning the handle would hand the caller a run
	// ID that immediately 404s. The run script was already submitted, so
	// this only affects the response, not the work.
	if _, ok := r.ctrl.isolatedSessionMap.Load(id); !ok {
		s.activeBackgroundRuns.Add(-1)
		return "", time.Time{}, ErrSessionNotActive
	}

	go r.watchBackgroundRun(s, run)
	log.Info("started background run %s in session %s", runID, id)
	return runID, startedAt, nil
}

// backgroundRunDirScript returns a shell fragment that resolves $D to the run
// log directory if the session uid can create and write it, or to the
// workspace root otherwise. It must stay consistent between the preflight
// probe and the run script so both use the same location.
func backgroundRunDirScript(paths backgroundRunPaths) string {
	return "D=" + shellescape(paths.nsRunDir) +
		"; mkdir -p \"$D\" 2>/dev/null && [ -w \"$D\" ] || D=" +
		shellescape(paths.nsWorkspace)
}

// preflightBackgroundLogDir verifies through the session shell that the run
// log directory is writable by the session uid. The caller holds the run
// mutex, so the probe's marker scan on the shared stdout cannot race another
// run. The probe writes its result (0 = writable, 1 = not) to a unique
// control file whose host-side copy execd polls, instead of printing a
// marker to the shared stdout: a shell wedged mid-probe can then delay the
// request only until the bounded poll window elapses — it cannot hang the
// handler forever (a stdout-marker scan would block on the still-open pipe
// after its context expires). Returns an error when no writable log location
// exists or the probe does not complete in time.
func (r *IsolatedRunner) preflightBackgroundLogDir(
	s *isolatedSession,
	stdin io.Writer,
	paths backgroundRunPaths,
) error {
	probeName := ".probe-" + uuid.New().String()
	probeScript := backgroundRunDirScript(paths) +
		"\nif [ -w \"$D\" ]; then echo 0; else echo 1; fi >\"$D/" + probeName + "\"\n"

	if _, err := io.WriteString(stdin, probeScript); err != nil {
		return fmt.Errorf("write preflight probe: %w", err)
	}

	root, err := os.OpenRoot(paths.hostRoot)
	if err != nil {
		return fmt.Errorf("open background run log root: %w", err)
	}
	defer root.Close()

	probeRels := []string{filepath.Join(isolatedBackgroundRunDir, probeName), probeName}
	deadline := time.Now().Add(isolatedBackgroundProbeTimeout)
	for {
		for _, rel := range probeRels {
			content, ok := readIsolatedControlFile(root, rel, 64)
			if !ok {
				continue
			}
			_ = root.Remove(rel)
			if strings.TrimSpace(content) == "0" {
				return nil
			}
			return fmt.Errorf("background runs unavailable: workspace is not writable by the session uid")
		}
		if time.Now().After(deadline) {
			// The probe never completed: either no writable log location
			// exists (the probe could not write its result) or the session
			// shell is unresponsive. Both reject the run.
			return fmt.Errorf("background runs unavailable: workspace is not writable by the session uid (preflight probe did not complete)")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// watchBackgroundRun marks the run finished when the session shell writes the
// exit-code file, or when the session dies (the run dies with it).
func (r *IsolatedRunner) watchBackgroundRun(s *isolatedSession, run *IsolatedBackgroundRun) {
	root, err := os.OpenRoot(run.logRoot)
	if err != nil {
		// The control-file root is gone (e.g. the upper layer vanished); the
		// run can no longer be tracked, so it never completes normally.
		run.markFinished(nil, "session terminated")
		s.activeBackgroundRuns.Add(-1)
		return
	}
	defer root.Close()

	ticker := time.NewTicker(isolatedBackgroundPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.doneCh:
			// The session died (e.g. the run killed the session shell): mark
			// the run failed but keep the record and its (possibly partial)
			// log files so clients can observe "session terminated" and read
			// the output via run_status/run_logs. Both are swept when the
			// session is deleted or GC'd.
			run.markFinished(nil, "session terminated")
			s.activeBackgroundRuns.Add(-1)
			return
		case <-ticker.C:
			code, ok := readIsolatedRunExitCode(root, run.relExitPath())
			if !ok {
				code, ok = readIsolatedRunExitCode(root, run.relFallbackExitPath())
			}
			if !ok {
				continue
			}
			run.markFinished(&code, "")
			// Bound disk usage per run: truncate oversized logs to the read
			// cap once the run is done. Clients can never hold a cursor past
			// the cap (each read returns at most maxBackgroundLogReadBytes),
			// so the incremental protocol stays coherent.
			capIsolatedRunLog(root, run.relLogPath())
			capIsolatedRunLog(root, run.relFallbackLogPath())
			// Refresh lastRunAt before clearing the active-run counter so a
			// concurrent idle collector can never observe counter==0 with the
			// stale submission timestamp and reap the session before the
			// client's idle window starts.
			s.mu.Lock()
			s.lastRunAt = time.Now()
			s.mu.Unlock()
			s.activeBackgroundRuns.Add(-1)
			return
		}
	}
}

// GetIsolatedBackgroundRun returns a snapshot of a background run, or
// ErrContextNotFound when no such run exists (unknown run ID, or the session
// and its runs were deleted).
func (r *IsolatedRunner) GetIsolatedBackgroundRun(
	sessionID string,
	runID string,
) (*IsolatedBackgroundRunSnapshot, error) {
	v, ok := r.bgRuns.Load(runID)
	if !ok {
		return nil, ErrContextNotFound
	}
	run, ok := v.(*IsolatedBackgroundRun)
	if !ok || run.SessionID != sessionID {
		return nil, ErrContextNotFound
	}
	snap := run.snapshot()
	return &snap, nil
}

// SeekIsolatedBackgroundOutput returns the combined log of a background run
// from the given byte cursor (at most maxBackgroundLogReadBytes), plus the new
// cursor (end offset) for the next incremental read.
func (r *IsolatedRunner) SeekIsolatedBackgroundOutput(
	sessionID string,
	runID string,
	cursor int64,
) ([]byte, int64, error) {
	v, ok := r.bgRuns.Load(runID)
	if !ok {
		return nil, -1, ErrContextNotFound
	}
	run, ok := v.(*IsolatedBackgroundRun)
	if !ok || run.SessionID != sessionID {
		return nil, -1, ErrContextNotFound
	}
	if cursor < 0 {
		return nil, -1, fmt.Errorf("cursor cannot be negative")
	}

	root, err := os.OpenRoot(run.logRoot)
	if err != nil {
		return nil, -1, fmt.Errorf("open background run log root: %w", err)
	}
	defer root.Close()

	file, err := openIsolatedControlFile(root, run.relLogPath(), os.O_RDONLY)
	if err != nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)) {
		// The run dir may not exist yet (the shell creates it at redirect
		// time), or a lower-layer .execd entry blocks it; the log may instead
		// be at the workspace-root fallback.
		file, err = openIsolatedControlFile(root, run.relFallbackLogPath(), os.O_RDONLY)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The shell creates the log file at redirect time, asynchronously
			// after the run handle is returned; treat a missing log as empty.
			return nil, 0, nil
		}
		return nil, -1, fmt.Errorf("open background run log: %w", err)
	}
	defer file.Close()

	if _, err := file.Seek(cursor, io.SeekStart); err != nil {
		return nil, -1, fmt.Errorf("seek background run log: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBackgroundLogReadBytes))
	if err != nil {
		return nil, -1, fmt.Errorf("read background run log: %w", err)
	}
	newCursor, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, -1, fmt.Errorf("read background run log position: %w", err)
	}
	return data, newCursor, nil
}

// removeIsolatedRunFiles best-effort deletes the log/exit-code files of a run
// (primary and fallback locations). Overlay workspaces lose them with the
// upper directory anyway; rw workspaces would otherwise keep them in the
// user's persistent workspace. Removes go through the pinned root so
// sandbox-created symlinks can never redirect host-side deletion outside the
// workspace.
func removeIsolatedRunFiles(root *os.Root, run *IsolatedBackgroundRun) {
	for _, rel := range []string{
		run.relLogPath(),
		run.relExitPath(),
		run.relFallbackLogPath(),
		run.relFallbackExitPath(),
	} {
		_ = root.Remove(rel)
	}
}

// sweepIsolatedProbeFiles removes stale preflight probe markers from the run
// log directory and the workspace root. Probes can be abandoned when a
// preflight times out with a wedged session shell.
func sweepIsolatedProbeFiles(root *os.Root) {
	for _, dir := range []string{".", isolatedBackgroundRunDir} {
		d, err := root.Open(dir)
		if err != nil {
			continue
		}
		entries, err := d.ReadDir(-1)
		_ = d.Close()
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".probe-") {
				_ = root.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// removeSessionBackgroundRuns drops every run record of a session and removes
// the run's log/exit-code files plus stale preflight probe markers. Called
// from DeleteIsolatedSession once the session (and its upper layer, for
// overlay workspaces) is torn down.
func (r *IsolatedRunner) removeSessionBackgroundRuns(s *isolatedSession) {
	paths, err := s.backgroundRunPaths()
	if err != nil {
		// Read-only sessions reject background runs at submission, so there
		// is never anything to sweep here.
		return
	}
	root, err := os.OpenRoot(paths.hostRoot)
	if err != nil {
		return
	}
	defer root.Close()

	sweepIsolatedProbeFiles(root)
	runDirRemoved := false
	r.bgRuns.Range(func(key, value any) bool {
		run, ok := value.(*IsolatedBackgroundRun)
		if !ok || run.SessionID != s.id {
			return true
		}
		removeIsolatedRunFiles(root, run)
		// Best-effort: drop the execd-managed run dir when empty (rw
		// workspaces only; overlay uppers are already gone).
		if err := root.Remove(isolatedBackgroundRunDir); err == nil {
			runDirRemoved = true
		}
		r.bgRuns.Delete(key)
		return true
	})
	if runDirRemoved {
		// The .execd parent is removed only when the run dir removal
		// succeeded, so a user-owned .execd file or non-empty dir is never
		// touched.
		_ = root.Remove(".execd")
	}
}

// backgroundRunPaths holds namespace and host paths for a background run.
type backgroundRunPaths struct {
	nsRunDir      string // <workspace>/.execd/background-runs
	nsWorkspace   string // workspace path as seen inside the namespace
	hostRunDir    string // host-side run dir (upper layer for overlay, workspace for rw)
	hostWorkspace string // host-side workspace root (upper dir for overlay)
	hostRoot      string // host-side root all control-file ops are pinned to
}

// backgroundRunPaths returns the namespace and host paths of the background
// run directory. The host path is the upper layer for overlay workspaces and
// the workspace itself for rw workspaces; read-only workspaces reject
// background runs (no host-visible writable location for logs).
func (s *isolatedSession) backgroundRunPaths() (backgroundRunPaths, error) {
	paths := backgroundRunPaths{
		nsRunDir:      filepath.Join(s.opts.WorkspacePath, isolatedBackgroundRunDir),
		nsWorkspace:   s.opts.WorkspacePath,
		hostWorkspace: s.opts.WorkspacePath,
	}
	switch isolation.WorkspaceMode(s.opts.WorkspaceMode) {
	case isolation.WorkspaceRW:
		paths.hostRunDir = paths.nsRunDir
		paths.hostRoot = s.opts.WorkspacePath
	case isolation.WorkspaceOverlay, "":
		if s.upperDir == "" {
			return backgroundRunPaths{}, fmt.Errorf("background runs unavailable: session has no upper directory")
		}
		paths.hostRunDir = filepath.Join(s.upperDir, isolatedBackgroundRunDir)
		paths.hostWorkspace = s.upperDir
		paths.hostRoot = s.upperDir
	default: // WorkspaceRO
		return backgroundRunPaths{}, fmt.Errorf("background runs not supported in read-only workspace mode")
	}
	return paths, nil
}

// openIsolatedControlFile opens a sandbox-visible control file (run log,
// exit-code file, probe marker) relative to a pinned host root without
// following symlinks at any path component, and verifies the opened object is
// a regular file. The workspace is writable by sandbox code, which could
// otherwise replace a control file with a symlink and have host-side execd
// read, truncate, or remove an arbitrary host file through it.
func openIsolatedControlFile(root *os.Root, rel string, flag int) (*os.File, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("control file %s is a symlink", rel)
	}
	f, err := root.OpenFile(rel, flag, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("control file %s is not a regular file", rel)
	}
	return f, nil
}

// readIsolatedControlFile reads up to maxBytes from a control file relative
// to the pinned root. Missing, unreadable, or symlink-replaced files report
// not-ready.
func readIsolatedControlFile(root *os.Root, rel string, maxBytes int64) (string, bool) {
	f, err := openIsolatedControlFile(root, rel, os.O_RDONLY)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// capIsolatedRunLog truncates a run's log file to maxBackgroundLogReadBytes
// when it grew past the cap, bounding per-run disk usage. Best-effort; the
// truncate runs on the fd opened through the pinned root, so a symlink
// replacement can never redirect it to another host file.
func capIsolatedRunLog(root *os.Root, rel string) {
	f, err := openIsolatedControlFile(root, rel, os.O_WRONLY)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= maxBackgroundLogReadBytes {
		return
	}
	_ = f.Truncate(maxBackgroundLogReadBytes)
}

// readIsolatedRunExitCode reads a background run's exit-code file relative to
// the pinned root; missing or not-yet-written files report not-ready.
func readIsolatedRunExitCode(root *os.Root, rel string) (int, bool) {
	content, ok := readIsolatedControlFile(root, rel, 64)
	if !ok || content == "" {
		return 0, false
	}
	code, err := strconv.Atoi(content)
	if err != nil {
		return 0, false
	}
	return code, true
}
