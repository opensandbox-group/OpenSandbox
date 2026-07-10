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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
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
	stopGC          chan struct{}
}

// NewIsolatedRunner creates the isolated session runner.
func NewIsolatedRunner(ctrl *Controller, iso isolation.Isolator, cfg isolation.Config) (*IsolatedRunner, error) {
	mgr, err := isolation.NewUpperManager(cfg.UpperRoot, cfg.UpperMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("isolated runner: upper manager: %w", err)
	}
	r := &IsolatedRunner{
		ctrl:            ctrl,
		isolator:        iso,
		upperMgr:        mgr,
		allowedWritable: cfg.AllowedWritable,
		stopGC:          make(chan struct{}),
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
	now := time.Now()
	r.ctrl.isolatedSessionMap.Range(func(key, value any) bool {
		s, ok := value.(*isolatedSession)
		if !ok {
			return true
		}

		sessionID := s.id

		if s.dead() {
			log.Info("idle GC: cleaning up dead session %s", sessionID)
			if err := r.DeleteIsolatedSession(sessionID); err != nil {
				log.Warn("idle GC: delete dead session %s: %v", sessionID, err)
			}
			return true
		}

		s.mu.RLock()
		timeout := time.Duration(s.opts.IdleTimeoutSeconds) * time.Second
		idle := now.Sub(s.lastRunAt)
		s.mu.RUnlock()

		if timeout > 0 && idle > timeout {
			if !s.runMu.TryLock() {
				return true
			}
			s.runMu.Unlock()
			log.Info("idle GC: deleting session %s (idle %v > timeout %v)", sessionID, idle, timeout)
			if err := r.DeleteIsolatedSession(sessionID); err != nil {
				log.Warn("idle GC: delete session %s: %v", sessionID, err)
			}
		}
		return true
	})
}

// StopGC stops the background GC goroutine.
func (r *IsolatedRunner) StopGC() {
	close(r.stopGC)
}

// Available reports whether the isolator is ready.
func (r *IsolatedRunner) Available() bool {
	return r.isolator.Available()
}

// CreateIsolatedSession starts a new bwrap + bash session.
func (r *IsolatedRunner) CreateIsolatedSession(opts *IsolatedSessionOptions) (string, error) {
	if err := r.validateExtraWritable(opts.ExtraWritable); err != nil {
		return "", err
	}

	if err := r.validateBinds(opts.Binds); err != nil {
		return "", err
	}

	if err := os.MkdirAll(opts.WorkspacePath, 0o755); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}

	id := uuid.New().String()
	session := newIsolatedSession(id, opts, r.isolator)

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
		if session.upperID != "" {
			_ = r.upperMgr.Remove(session.upperID)
		}
		return "", fmt.Errorf("start bwrap: %w", err)
	}

	r.ctrl.isolatedSessionMap.Store(id, session)
	log.Info("created isolated session %s (profile=%s, mode=%s)", id, opts.Profile, opts.WorkspaceMode)
	return id, nil
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
type IsolatedSessionState struct {
	Status               string
	CreatedAt            time.Time
	LastRunAt            time.Time
	IdleRemainingSeconds *int
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
// envs are exported in the bash session before code runs.
func (r *IsolatedRunner) RunInIsolatedSession(ctx context.Context, id string, code string, envs map[string]string, onStdout StdoutCallback) error {
	s := r.lookup(id)
	if s == nil {
		return ErrContextNotFound
	}

	// Serialize concurrent runs on the same session.
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if s.dead() {
		return fmt.Errorf("session process has exited")
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
	// without killing the persistent bash session. Closing stdin would
	// terminate bash entirely.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if s.cmd != nil && s.cmd.Process != nil {
				_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGINT)
			}
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

	if err := s.stop(); err != nil {
		log.Warn("stop isolated session %s: %v", id, err)
	}

	if s.upperID != "" {
		if err := r.upperMgr.Remove(s.upperID); err != nil {
			log.Warn("remove upper dir for session %s: %v", id, err)
		}
	}

	r.ctrl.isolatedSessionMap.Delete(id)
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

// GetMergedView returns a VFS for the session's filesystem.
func (r *IsolatedRunner) GetMergedView(id string) (vfs.FS, error) {
	s := r.lookup(id)
	if s == nil {
		return nil, ErrContextNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

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

	return isolation.NewMergedView(s.opts.WorkspacePath, upper, mode, uid, gid), nil
}

// Capabilities returns the current isolator capabilities.
func (r *IsolatedRunner) Capabilities() isolation.Capabilities {
	return r.isolator.Capabilities()
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
