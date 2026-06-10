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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const isolatedRunEndMarker = "__ISOLATED_RUN_END__"

// IsolatedRunner is the concrete isolated session runner.
type IsolatedRunner struct {
	ctrl     *Controller
	isolator isolation.Isolator
	upperMgr *isolation.UpperManager
	stopGC   chan struct{}
}

// NewIsolatedRunner creates the isolated session runner.
func NewIsolatedRunner(ctrl *Controller, iso isolation.Isolator, upperRoot string, upperMaxBytes int64) (*IsolatedRunner, error) {
	mgr, err := isolation.NewUpperManager(upperRoot, upperMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("isolated runner: upper manager: %w", err)
	}
	r := &IsolatedRunner{
		ctrl:     ctrl,
		isolator: iso,
		upperMgr: mgr,
		stopGC:   make(chan struct{}),
	}
	go r.gcLoop()
	return r, nil
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

// CollectIdle scans sessions and deletes those past their idle timeout.
func (r *IsolatedRunner) CollectIdle() {
	now := time.Now()
	r.ctrl.isolatedSessionMap.Range(func(key, value any) bool {
		s, ok := value.(*isolatedSession)
		if !ok {
			return true
		}

		s.mu.RLock()
		timeout := time.Duration(s.opts.IdleTimeoutSeconds) * time.Second
		idle := now.Sub(s.lastRunAt)
		sessionID := s.id
		s.mu.RUnlock()

		if timeout > 0 && idle > timeout {
			log.Info("idle GC: deleting session %s (idle %v > timeout %v)", sessionID, idle, timeout)
			if err := r.DeleteIsolatedSession(sessionID); err != nil {
				log.Warning("idle GC: delete session %s: %v", sessionID, err)
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
	id := uuid.New().String()
	session := newIsolatedSession(id, opts, r.isolator)

	// Allocate upper directory for overlay mode.
	if opts.WorkspaceMode == "overlay" || opts.WorkspaceMode == "" {
		upperID, upperDir, workDir, err := r.upperMgr.Allocate()
		if err != nil {
			return "", fmt.Errorf("allocate upper: %w", err)
		}
		session.upperDir = upperDir
		session.workDir = workDir
		_ = upperID // gc key
	}

	if err := session.start(); err != nil {
		if session.upperDir != "" {
			r.upperMgr.Release(id)
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

	state := &IsolatedSessionState{
		Status:    "active",
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

// IsolatedSessionState is returned by GetIsolatedSession.
type IsolatedSessionState struct {
	Status               string
	CreatedAt            time.Time
	LastRunAt            time.Time
	IdleRemainingSeconds *int
}

// StdoutCallback is called for each line of stdout output during Run.
type StdoutCallback func(line string)

// RunInIsolatedSession executes code in the session.
func (r *IsolatedRunner) RunInIsolatedSession(ctx context.Context, id string, code string, onStdout StdoutCallback) error {
	s := r.lookup(id)
	if s == nil {
		return ErrContextNotFound
	}

	s.mu.RLock()
	stdin := s.stdin
	stdout := s.stdout
	s.mu.RUnlock()

	if stdin == nil || stdout == nil {
		return fmt.Errorf("session not started")
	}

	// Build the command to execute. Write a marker after the command so we
	// know when output ends.
	script := code
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	script += fmt.Sprintf("echo %s $?\n", isolatedRunEndMarker)

	// Close stdin on context cancellation so blocked writes unblock.
	go func() {
		<-ctx.Done()
		stdin.Close()
	}()

	if _, err := io.WriteString(stdin, script); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}

	// Read output until marker.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var exitCode int
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, isolatedRunEndMarker) {
				parts := strings.Fields(line)
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
		return ctx.Err()
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdout: %w", err)
	}

	// Update lastRunAt.
	s.mu.Lock()
	s.lastRunAt = time.Now()
	s.mu.Unlock()

	if exitCode != 0 {
		return fmt.Errorf("command exited with code %d", exitCode)
	}

	return nil
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
		log.Warning("stop isolated session %s: %v", id, err)
	}

	if s.upperDir != "" {
		r.upperMgr.Release(id)
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

// GetMergedView returns a filesystem view for the session.
func (r *IsolatedRunner) GetMergedView(id string) (*isolation.MergedView, error) {
	s := r.lookup(id)
	if s == nil {
		return nil, ErrContextNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var uid, gid uint32
	if s.opts.Uid != nil {
		uid = *s.opts.Uid
	}
	if s.opts.Gid != nil {
		gid = *s.opts.Gid
	}

	mode := isolation.WorkspaceOverlay
	upper := s.upperDir
	switch s.opts.WorkspaceMode {
	case "rw":
		mode = isolation.WorkspaceRW
		upper = s.opts.WorkspacePath // writes go directly to workspace
	case "ro":
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
