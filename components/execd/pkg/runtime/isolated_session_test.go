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
	"errors"
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
			Available:              true,
			Isolator:               "stub",
			SetprivAvailable:       true,
			SetprivSwitchAvailable: true,
			UsernsAvailable:        true,
			CommitSupported:        false,
			DiffSupported:          false,
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

func TestCreateIsolatedSession_RejectsOnlyUnavailableUidMode(t *testing.T) {
	customUID := uint32(424242)
	tests := []struct {
		name              string
		mode              string
		setprivAvailable  bool
		identityAvailable bool
		usernsAvailable   bool
		uid               *uint32
		wantUnavailable   bool
	}{
		{name: "setpriv supported", mode: "setpriv", setprivAvailable: true},
		{name: "setpriv custom identity supported", mode: "setpriv", setprivAvailable: true, identityAvailable: true, uid: &customUID},
		{name: "setpriv custom identity unsupported", mode: "setpriv", setprivAvailable: true, uid: &customUID, wantUnavailable: true},
		{name: "setpriv unsupported", mode: "setpriv", usernsAvailable: true, wantUnavailable: true},
		{name: "default setpriv unsupported", mode: "", usernsAvailable: true, wantUnavailable: true},
		{name: "userns supported", mode: "userns", usernsAvailable: true},
		{name: "userns unsupported", mode: "userns", setprivAvailable: true, wantUnavailable: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newTestRunner(t)
			stub := runner.isolator.(*stubIsolator)
			stub.caps.SetprivAvailable = tt.setprivAvailable
			stub.caps.SetprivSwitchAvailable = tt.identityAvailable
			stub.caps.UsernsAvailable = tt.usernsAvailable

			opts := &IsolatedSessionOptions{
				WorkspacePath: filepath.Join(t.TempDir(), "workspace"),
				WorkspaceMode: "rw",
				UidMode:       tt.mode,
				Uid:           tt.uid,
			}
			id, err := runner.CreateIsolatedSession(opts)
			if tt.wantUnavailable {
				if !errors.Is(err, ErrUidModeUnavailable) {
					t.Fatalf("error = %v, want ErrUidModeUnavailable", err)
				}
				if id != "" {
					t.Errorf("session id = %q, want empty", id)
				}
				return
			}

			if err != nil {
				t.Fatalf("CreateIsolatedSession: %v", err)
			}
			defer runner.DeleteIsolatedSession(id)
		})
	}
}

func TestValidateUidModeAvailable_RejectsUnknownMode(t *testing.T) {
	runner := newTestRunner(t)
	err := runner.validateUidModeAvailable(&IsolatedSessionOptions{UidMode: "bogus"})
	if !errors.Is(err, ErrUidModeUnavailable) {
		t.Fatalf("error = %v, want ErrUidModeUnavailable", err)
	}
}

func TestValidateUidModeAvailable_EmptyModeUsesSetpriv(t *testing.T) {
	runner := newTestRunner(t)
	stub := runner.isolator.(*stubIsolator)
	stub.caps.SetprivAvailable = false
	stub.caps.UsernsAvailable = true

	err := runner.validateUidModeAvailable(&IsolatedSessionOptions{})
	if !errors.Is(err, ErrUidModeUnavailable) {
		t.Fatalf("error = %v, want ErrUidModeUnavailable", err)
	}
}

func TestSetprivIdentitySwitchRequired(t *testing.T) {
	currentUID, currentGID := uint32(1000), uint32(1001)
	otherUID, otherGID := uint32(2000), uint32(2001)
	tests := []struct {
		name string
		opts *IsolatedSessionOptions
		want bool
	}{
		{name: "omitted identity", opts: &IsolatedSessionOptions{}},
		{name: "same identity", opts: &IsolatedSessionOptions{Uid: &currentUID, Gid: &currentGID}},
		{name: "different uid", opts: &IsolatedSessionOptions{Uid: &otherUID}, want: true},
		{name: "different gid", opts: &IsolatedSessionOptions{Gid: &otherGID}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := setprivIdentitySwitchRequired(tt.opts, currentUID, currentGID); got != tt.want {
				t.Errorf("setprivIdentitySwitchRequired() = %v, want %v", got, tt.want)
			}
		})
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

// TestGetIsolatedSession_ReturnsCreationParams verifies GetIsolatedSession
// echoes back the parameters the session was created with, so a
// stateless client (that only holds the sessionId) can rebuild a session
// handle without needing to have retained the original create request.
func TestGetIsolatedSession_ReturnsCreationParams(t *testing.T) {
	runner := newTestRunner(t)

	// Extend the runner's writable allowlist so a bind can be validated.
	bindSrc := filepath.Join(t.TempDir(), "bind-src")
	if err := os.MkdirAll(bindSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	extraDir := filepath.Join(t.TempDir(), "extra")
	if err := os.MkdirAll(extraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runner.allowedWritable = []string{bindSrc, extraDir}

	shareNet := true
	uid := uint32(1234)
	gid := uint32(5678)

	opts := &IsolatedSessionOptions{
		Profile:            "balanced",
		WorkspacePath:      filepath.Join(t.TempDir(), "ws"),
		WorkspaceMode:      "overlay",
		ExtraWritable:      []string{extraDir},
		Binds:              []isolation.BindMount{{Source: bindSrc, Dest: "/mnt/in", ReadOnly: true}},
		ShareNet:           &shareNet,
		EnvPassthroughMode: "allow",
		EnvPassthroughKeys: []string{"HOME", "PATH"},
		Uid:                &uid,
		Gid:                &gid,
		UidMode:            "setpriv",
		IdleTimeoutSeconds: 900,
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	state, err := runner.GetIsolatedSession(id)
	if err != nil {
		t.Fatal(err)
	}

	if state.Profile != "balanced" {
		t.Errorf("Profile = %q, want balanced", state.Profile)
	}
	if state.WorkspacePath == "" {
		t.Error("WorkspacePath is empty")
	}
	if state.WorkspaceMode != "overlay" {
		t.Errorf("WorkspaceMode = %q, want overlay", state.WorkspaceMode)
	}
	if len(state.ExtraWritable) != 1 {
		t.Errorf("ExtraWritable len = %d, want 1", len(state.ExtraWritable))
	}
	if len(state.Binds) != 1 || state.Binds[0].Dest != "/mnt/in" || !state.Binds[0].ReadOnly {
		t.Errorf("Binds = %+v, want [{Source:%s Dest:/mnt/in ReadOnly:true}]", state.Binds, bindSrc)
	}
	if state.ShareNet == nil || !*state.ShareNet {
		t.Error("ShareNet not echoed")
	}
	if state.EnvPassthroughMode != "allow" {
		t.Errorf("EnvPassthroughMode = %q, want allow", state.EnvPassthroughMode)
	}
	if len(state.EnvPassthroughKeys) != 2 {
		t.Errorf("EnvPassthroughKeys len = %d, want 2", len(state.EnvPassthroughKeys))
	}
	if state.Uid == nil || *state.Uid != 1234 {
		t.Errorf("Uid = %v, want 1234", state.Uid)
	}
	if state.Gid == nil || *state.Gid != 5678 {
		t.Errorf("Gid = %v, want 5678", state.Gid)
	}
	if state.UidMode != "setpriv" {
		t.Errorf("UidMode = %q, want setpriv", state.UidMode)
	}
	if state.IdleTimeoutSeconds != 900 {
		t.Errorf("IdleTimeoutSeconds = %d, want 900", state.IdleTimeoutSeconds)
	}
	if state.IdleRemainingSeconds == nil {
		t.Error("IdleRemainingSeconds nil despite IdleTimeoutSeconds > 0")
	}
}

// TestGetIsolatedSession_EchoesEffectiveDefaults verifies that a session
// created with omitted Profile/WorkspaceMode/EnvPassthroughMode/UidMode
// is echoed back with the effective values execd actually applied, not
// the empty strings the caller sent. This lets a stateless client
// (attaching by sessionId only) rebuild handle info that matches the
// running configuration.
func TestGetIsolatedSession_EchoesEffectiveDefaults(t *testing.T) {
	runner := newTestRunner(t)

	// Bare-minimum create request: only workspace path is required.
	opts := &IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "ws"),
		// Profile / WorkspaceMode / EnvPassthroughMode / UidMode all omitted.
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	state, err := runner.GetIsolatedSession(id)
	if err != nil {
		t.Fatal(err)
	}

	// Each of these must be the effective value, not "".
	if state.Profile != "strict" {
		t.Errorf("Profile = %q, want strict (execd default)", state.Profile)
	}
	if state.WorkspaceMode != "overlay" {
		t.Errorf("WorkspaceMode = %q, want overlay (execd default)", state.WorkspaceMode)
	}
	if state.EnvPassthroughMode != "deny" {
		t.Errorf("EnvPassthroughMode = %q, want deny (execd default)", state.EnvPassthroughMode)
	}
	if state.UidMode != "setpriv" {
		t.Errorf("UidMode = %q, want setpriv (execd default)", state.UidMode)
	}
}

// TestNormalize_EnvPassthroughEmptyModeDropsKeys verifies that a create
// request supplying env_passthrough.keys without an explicit mode has
// its keys dropped during normalization. Rationale: the pre-normalize
// behavior of start() forwarded EnvSpec{Mode: deny, Keys: nil} to
// bwrap on empty mode, which bwrapEnvSegment interprets as "apply the
// built-in secret blacklist". Normalizing mode to "deny" while
// preserving keys would flip bwrap to "unset only those keys, skip
// the blacklist" — a silent security regression. Keys must be
// dropped so the effective config (blacklist wins) is echoed and run
// unchanged.
func TestNormalize_EnvPassthroughEmptyModeDropsKeys(t *testing.T) {
	runner := newTestRunner(t)

	opts := &IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "ws"),
		// mode omitted, but caller supplied keys — must be dropped
		// so the built-in secret blacklist is not silently bypassed.
		EnvPassthroughMode: "",
		EnvPassthroughKeys: []string{"USER_TOKEN", "AWS_SECRET_ACCESS_KEY"},
	}

	id, err := runner.CreateIsolatedSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	state, err := runner.GetIsolatedSession(id)
	if err != nil {
		t.Fatal(err)
	}

	if state.EnvPassthroughMode != "deny" {
		t.Errorf("EnvPassthroughMode = %q, want deny", state.EnvPassthroughMode)
	}
	if len(state.EnvPassthroughKeys) != 0 {
		t.Errorf("EnvPassthroughKeys should be dropped when mode was omitted, got %v", state.EnvPassthroughKeys)
	}

	// Caller-supplied keys are preserved when mode is explicit.
	opts2 := &IsolatedSessionOptions{
		WorkspacePath:      filepath.Join(t.TempDir(), "ws2"),
		EnvPassthroughMode: "deny",
		EnvPassthroughKeys: []string{"USER_TOKEN"},
	}
	id2, err := runner.CreateIsolatedSession(opts2)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id2)

	state2, err := runner.GetIsolatedSession(id2)
	if err != nil {
		t.Fatal(err)
	}
	if state2.EnvPassthroughMode != "deny" {
		t.Errorf("explicit deny: mode = %q, want deny", state2.EnvPassthroughMode)
	}
	if len(state2.EnvPassthroughKeys) != 1 || state2.EnvPassthroughKeys[0] != "USER_TOKEN" {
		t.Errorf("explicit deny should preserve keys, got %v", state2.EnvPassthroughKeys)
	}
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
