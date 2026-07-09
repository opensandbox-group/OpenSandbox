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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

// stubIsolator returns Available=true but Wrap is a no-op (for happy-path tests).
type stubIsolator struct {
	available bool
	caps      isolation.Capabilities
}

func (s *stubIsolator) Name() string                                    { return "stub" }
func (s *stubIsolator) Available() bool                                 { return s.available }
func (s *stubIsolator) Capabilities() isolation.Capabilities            { return s.caps }
func (s *stubIsolator) Wrap(_ *exec.Cmd, _ isolation.WrapOptions) error { return nil }

func newStubIsolator() *stubIsolator {
	return &stubIsolator{
		available: true,
		caps: isolation.Capabilities{
			Available:       true,
			Isolator:        "stub",
			CommitSupported: false,
			DiffSupported:   false,
		},
	}
}

func newTestRunner(t *testing.T) *IsolatedRunner {
	t.Helper()
	ctrl := NewController("", "")
	mgr, err := isolation.NewUpperManager(t.TempDir(), 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	return &IsolatedRunner{
		ctrl:     ctrl,
		isolator: newStubIsolator(),
		upperMgr: mgr,
	}
}

func TestNewIsolatedRunner(t *testing.T) {
	runner := newTestRunner(t)
	if runner == nil {
		t.Fatal("runner is nil")
	}
	if !runner.Available() {
		t.Error("runner should be available with stub isolator")
	}
}

func TestCreateIsolatedSession_HappyPath(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: filepath.Join(t.TempDir(), "workspace"),
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatalf("CreateIsolatedSession: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty session ID")
	}

	// Verify session is tracked.
	s := runner.lookup(id)
	if s == nil {
		t.Fatal("session not found after create")
	}
	if s.opts.Profile != "strict" {
		t.Errorf("profile = %q, want strict", s.opts.Profile)
	}

	// Clean up.
	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Errorf("DeleteIsolatedSession: %v", err)
	}
}

func TestGetIsolatedSession_NotFound(t *testing.T) {
	runner := newTestRunner(t)
	_, err := runner.GetIsolatedSession("nonexistent")
	if err != ErrContextNotFound {
		t.Errorf("expected ErrContextNotFound, got %v", err)
	}
}

func TestGetIsolatedSession_Found(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: filepath.Join(t.TempDir(), "ws"),
		WorkspaceMode: "overlay",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}

	state, err := runner.GetIsolatedSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "active" {
		t.Errorf("status = %q, want active", state.Status)
	}
	if state.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	runner.DeleteIsolatedSession(id)
}

func TestDeleteIsolatedSession_NotFound(t *testing.T) {
	runner := newTestRunner(t)
	err := runner.DeleteIsolatedSession("nonexistent")
	if err != ErrContextNotFound {
		t.Errorf("expected ErrContextNotFound, got %v", err)
	}
}

func TestDeleteIsolatedSession_Success(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: "/tmp",
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}

	if err := runner.DeleteIsolatedSession(id); err != nil {
		t.Fatal(err)
	}

	// Verify removed.
	if s := runner.lookup(id); s != nil {
		t.Error("session should be removed after delete")
	}
}

func TestRunInIsolatedSession_HappyPath(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: filepath.Join(t.TempDir(), "workspace"),
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// echo should succeed (exit 0).
	err = runner.RunInIsolatedSession(ctx, id, "echo hello", nil, nil)
	if err != nil {
		t.Errorf("RunInIsolatedSession: %v", err)
	}

	// Verify lastRunAt was updated.
	s := runner.lookup(id)
	if s == nil {
		t.Fatal("session disappeared")
	}
	if s.lastRunAt.Before(s.createdAt) {
		t.Error("lastRunAt should be >= createdAt after run")
	}
}

func TestRunInIsolatedSession_NotFound(t *testing.T) {
	runner := newTestRunner(t)
	ctx := context.Background()
	err := runner.RunInIsolatedSession(ctx, "nonexistent", "echo hi", nil, nil)
	if err != ErrContextNotFound {
		t.Errorf("expected ErrContextNotFound, got %v", err)
	}
}

func TestRunInIsolatedSession_ExitCode(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: "/tmp",
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// bash -c 'exit 42' produces exit code 42 without killing the session.
	err = runner.RunInIsolatedSession(ctx, id, "bash -c 'exit 42'", nil, nil)
	if err == nil {
		t.Error("expected error for non-zero exit code")
	}
}

func TestCapabilities(t *testing.T) {
	runner := newTestRunner(t)
	caps := runner.Capabilities()
	if !caps.Available {
		t.Error("caps.Available should be true")
	}
	if caps.Isolator != "stub" {
		t.Errorf("Isolator = %q, want stub", caps.Isolator)
	}
}

func TestIsolatedSessionOptions_Defaults(t *testing.T) {
	opts := &IsolatedSessionOptions{
		WorkspacePath: "/ws",
	}
	if opts.Profile != "" {
		t.Error("Profile should default to empty (controller sets strict)")
	}
	if opts.WorkspaceMode != "" {
		t.Error("WorkspaceMode should default to empty (controller sets overlay)")
	}
	if opts.ShareNet != nil {
		t.Error("ShareNet should default to nil (start defaults to true)")
	}
}

func TestRunInIsolatedSession_StdoutCallback(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: "/tmp",
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lines []string
	onStdout := func(line string) {
		lines = append(lines, line)
	}

	err = runner.RunInIsolatedSession(ctx, id, "echo hello", nil, onStdout)
	if err != nil {
		t.Fatalf("RunInIsolatedSession: %v", err)
	}

	if len(lines) != 1 || lines[0] != "hello" {
		t.Errorf("expected [hello], got %v", lines)
	}
}

func TestRunInIsolatedSession_MultiLine(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: "/tmp",
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lines []string
	onStdout := func(line string) {
		lines = append(lines, line)
	}

	code := "echo one\necho two\necho three"
	err = runner.RunInIsolatedSession(ctx, id, code, nil, onStdout)
	if err != nil {
		t.Fatalf("RunInIsolatedSession: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	for i, want := range []string{"one", "two", "three"} {
		if lines[i] != want {
			t.Errorf("line[%d] = %q, want %q", i, lines[i], want)
		}
	}
}

func TestRunInIsolatedSession_EnvPersistence(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: "/tmp",
		WorkspaceMode: "rw",
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run 1: set env var in bash session.
	err = runner.RunInIsolatedSession(ctx, id, "export MY_VAR=hello_from_session", nil, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Run 2: echo the env var to verify persistence.
	var lines []string
	onStdout := func(line string) {
		lines = append(lines, line)
	}
	err = runner.RunInIsolatedSession(ctx, id, "echo $MY_VAR", nil, onStdout)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}

	if len(lines) != 1 || lines[0] != "hello_from_session" {
		t.Errorf("env not persisted: got %v", lines)
	}
}

func TestRunInIsolatedSession_ConcurrentSessions(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: "/tmp",
		WorkspaceMode: "rw",
	}

	id1, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id1)

	id2, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Set different env vars in each session.
	runner.RunInIsolatedSession(ctx, id1, "export SESSION=one", nil, nil)
	runner.RunInIsolatedSession(ctx, id2, "export SESSION=two", nil, nil)

	// Read back — each session should have its own value.
	var out1, out2 []string
	runner.RunInIsolatedSession(ctx, id1, "echo $SESSION", nil, func(l string) { out1 = append(out1, l) })
	runner.RunInIsolatedSession(ctx, id2, "echo $SESSION", nil, func(l string) { out2 = append(out2, l) })

	if len(out1) != 1 || out1[0] != "one" {
		t.Errorf("session 1: expected [one], got %v", out1)
	}
	if len(out2) != 1 || out2[0] != "two" {
		t.Errorf("session 2: expected [two], got %v", out2)
	}
}

// TestValidateBinds_SymlinkBypass verifies that a symlink placed inside an
// allowed directory cannot be used to smuggle a bind source whose real target
// lies outside the allowlist.
func TestValidateBinds_SymlinkBypass(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()

	// allowed/link -> outside (a directory outside the allowlist).
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	r := &IsolatedRunner{allowedWritable: []string{allowed}}

	// A direct path under the allowlist is fine.
	if err := r.validateBinds([]isolation.BindMount{{Source: allowed}}); err != nil {
		t.Errorf("direct allowlisted source should be accepted: %v", err)
	}

	// The symlink resolves outside the allowlist and must be rejected.
	err := r.validateBinds([]isolation.BindMount{{Source: link}})
	if err == nil {
		t.Fatal("expected symlinked source resolving outside allowlist to be rejected")
	}
	if !strings.Contains(err.Error(), "not in allowlist") {
		t.Errorf("expected allowlist rejection, got: %v", err)
	}

	// A symlink whose target stays inside the allowlist is still accepted, and
	// the source is rewritten to the resolved real path so bwrap mounts the
	// resolved target (closing the TOCTOU window).
	innerTarget := filepath.Join(allowed, "real")
	if err := os.Mkdir(innerTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	innerLink := filepath.Join(allowed, "inner")
	if err := os.Symlink(innerTarget, innerLink); err != nil {
		t.Fatal(err)
	}
	binds := []isolation.BindMount{{Source: innerLink}}
	if err := r.validateBinds(binds); err != nil {
		t.Errorf("symlink resolving inside allowlist should be accepted: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(innerLink)
	if binds[0].Source != wantResolved {
		t.Errorf("bind source should be rewritten to resolved path %q, got %q", wantResolved, binds[0].Source)
	}

	// A non-existent source under the allowlist must be rejected: a missing
	// leaf could otherwise be swapped to an out-of-allowlist symlink between
	// validation and bwrap start.
	missing := filepath.Join(allowed, "does-not-exist-yet")
	err = r.validateBinds([]isolation.BindMount{{Source: missing}})
	if err == nil {
		t.Fatal("expected non-existent bind source to be rejected")
	}
	if !strings.Contains(err.Error(), "must be an existing path") {
		t.Errorf("expected existing-path rejection, got: %v", err)
	}
}
