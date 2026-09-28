// Copyright 2026 The OpenSandbox Authors

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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

// capturingIsolator records the WrapOptions passed to WrapWithLifecycle.
type capturingIsolator struct {
	stubIsolator

	mu    sync.Mutex
	wraps []isolation.WrapOptions
}

func (c *capturingIsolator) WrapWithLifecycle(
	cmd *exec.Cmd,
	opts isolation.WrapOptions,
) (isolation.WorkloadLifecycle, error) {
	c.mu.Lock()
	c.wraps = append(c.wraps, opts)
	c.mu.Unlock()
	return newStubWorkloadLifecycle(), nil
}

func (c *capturingIsolator) lastWrap() (isolation.WrapOptions, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.wraps) == 0 {
		return isolation.WrapOptions{}, false
	}
	return c.wraps[len(c.wraps)-1], true
}

func TestCreateIsolatedSession_MultiOverlayAllocation(t *testing.T) {
	runner := newTestRunner(t)
	iso := &capturingIsolator{stubIsolator: *newStubIsolator()}
	runner.isolator = iso

	base := t.TempDir()
	paths := make([]string, 5)
	for i := range paths {
		paths[i] = filepath.Join(base, string(rune('a'+i)))
		if err := os.MkdirAll(paths[i], 0o755); err != nil {
			t.Fatal(err)
		}
	}

	persist := true
	ephemeral := false
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		Overlays: []IsolatedOverlayOptions{
			{Path: paths[0], Mode: "overlay", Persist: &persist},
			{Path: paths[1], Mode: "overlay"}, // defaults to persist=true
			{Path: paths[2], Mode: "rw"},
			{Path: paths[3], Mode: "ro"},
			{Path: paths[4], Mode: "overlay", Persist: &ephemeral},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	session := runner.lookup(id)
	if session == nil {
		t.Fatal("created session was not published")
	}

	if session.upperID == "" {
		t.Fatal("persist overlays did not allocate an upper session entry")
	}
	// Three persist overlays share one AllocateN session directory with the
	// legacy pair 0 layout plus upper-1/upper-2.
	ovUpper := func(i int) string { return session.overlays[i].upperDir }
	if base0 := filepath.Base(ovUpper(0)); base0 != "upper" {
		t.Errorf("persist overlay 0 upper base = %q, want upper", base0)
	}
	if base1 := filepath.Base(ovUpper(1)); base1 != "upper-1" {
		t.Errorf("persist overlay 1 upper base = %q, want upper-1", base1)
	}
	if ovUpper(0) == ovUpper(1) {
		t.Error("persist overlays share one upper directory")
	}
	for i, want := range []bool{true, true, false, false, false} {
		if got := session.overlays[i].persist; got != want {
			t.Errorf("overlays[%d].persist = %v, want %v", i, got, want)
		}
	}
	if ovUpper(4) != "" {
		t.Errorf("ephemeral overlay upper = %q, want empty", ovUpper(4))
	}
	for _, i := range []int{2, 3} {
		if ovUpper(i) != "" {
			t.Errorf("rw/ro overlays[%d] upper = %q, want empty", i, ovUpper(i))
		}
	}

	wrap, ok := iso.lastWrap()
	if !ok {
		t.Fatal("isolator did not receive WrapOptions")
	}
	if len(wrap.Overlays) != 5 {
		t.Fatalf("WrapOptions.Overlays len = %d, want 5", len(wrap.Overlays))
	}
	for i, want := range []isolation.WorkspaceMode{
		isolation.WorkspaceOverlay,
		isolation.WorkspaceOverlay,
		isolation.WorkspaceRW,
		isolation.WorkspaceRO,
		isolation.WorkspaceOverlay,
	} {
		ov := wrap.Overlays[i]
		if ov.Path != paths[i] || ov.Mode != want {
			t.Errorf("WrapOptions.Overlays[%d] = {%s %s}, want {%s %s}",
				i, ov.Path, ov.Mode, paths[i], want)
		}
		if ov.UpperDir != ovUpper(i) {
			t.Errorf("WrapOptions.Overlays[%d].UpperDir = %q, want %q",
				i, ov.UpperDir, ovUpper(i))
		}
	}
}

func TestCreateIsolatedSession_AllEphemeralOverlaysSkipUpperAllocation(t *testing.T) {
	runner := newTestRunner(t)
	ephemeral := false
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		Overlays: []IsolatedOverlayOptions{
			{Path: filepath.Join(t.TempDir(), "ws"), Mode: "overlay", Persist: &ephemeral},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	session := runner.lookup(id)
	if session == nil {
		t.Fatal("created session was not published")
	}
	if session.upperID != "" {
		t.Errorf("ephemeral-only session allocated upper entry %q", session.upperID)
	}
	if session.overlays[0].upperDir != "" {
		t.Errorf("ephemeral overlay upper = %q, want empty", session.overlays[0].upperDir)
	}
}

func TestNormalizeIsolatedOptions_MergesWorkspaceWithOverlays(t *testing.T) {
	ephemeral := false
	opts := &IsolatedSessionOptions{
		WorkspacePath: "/ws",
		WorkspaceMode: "rw",
		Overlays: []IsolatedOverlayOptions{
			{Path: "/"},
			{Path: "/tmp", Mode: "overlay", Persist: &ephemeral},
			{Path: "/etc", Mode: "ro"},
		},
	}
	normalizeIsolatedOptions(opts)

	if opts.WorkspacePath != "" || opts.WorkspaceMode != "" {
		t.Errorf("legacy workspace fields not cleared: %q %q",
			opts.WorkspacePath, opts.WorkspaceMode)
	}
	want := []struct {
		path    string
		mode    string
		persist *bool
	}{
		{"/ws", "rw", nil},
		{"/", "overlay", boolPtr(true)},
		{"/tmp", "overlay", boolPtr(false)},
		{"/etc", "ro", nil},
	}
	if len(opts.Overlays) != len(want) {
		t.Fatalf("Overlays = %+v, want %d entries", opts.Overlays, len(want))
	}
	for i, w := range want {
		ov := opts.Overlays[i]
		if ov.Path != w.path || ov.Mode != w.mode {
			t.Errorf("Overlays[%d] = {%s %s}, want {%s %s}", i, ov.Path, ov.Mode, w.path, w.mode)
		}
		switch {
		case w.persist == nil && ov.Persist != nil:
			t.Errorf("Overlays[%d].Persist = %v, want nil", i, *ov.Persist)
		case w.persist != nil && (ov.Persist == nil || *ov.Persist != *w.persist):
			t.Errorf("Overlays[%d].Persist = %v, want %v", i, ov.Persist, *w.persist)
		}
	}
}

func TestNormalizeIsolatedOptions_WorkspaceDefaultsToOverlayPersist(t *testing.T) {
	opts := &IsolatedSessionOptions{WorkspacePath: "/ws"}
	normalizeIsolatedOptions(opts)
	if len(opts.Overlays) != 1 {
		t.Fatalf("Overlays = %+v, want 1 entry", opts.Overlays)
	}
	ov := opts.Overlays[0]
	if ov.Path != "/ws" || ov.Mode != "overlay" {
		t.Errorf("Overlays[0] = {%s %s}, want {/ws overlay}", ov.Path, ov.Mode)
	}
	if ov.Persist == nil || !*ov.Persist {
		t.Errorf("Overlays[0].Persist = %v, want true (legacy default)", ov.Persist)
	}
}

func TestGetIsolatedSession_EchoesMultiOverlays(t *testing.T) {
	runner := newTestRunner(t)

	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ephemeral := false
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		Overlays: []IsolatedOverlayOptions{
			{Path: ws},
			{Path: "/tmp", Mode: "rw"},
			{Path: filepath.Join(t.TempDir(), "eph"), Mode: "overlay", Persist: &ephemeral},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	state, err := runner.GetIsolatedSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Overlays) != 3 {
		t.Fatalf("Overlays len = %d, want 3", len(state.Overlays))
	}
	if state.Overlays[0].Path != ws ||
		state.Overlays[0].Mode != "overlay" ||
		state.Overlays[0].Persist == nil || !*state.Overlays[0].Persist {
		t.Errorf("Overlays[0] = %+v, want %s overlay persist=true", state.Overlays[0], ws)
	}
	if state.Overlays[1].Path != "/tmp" ||
		state.Overlays[1].Mode != "rw" ||
		state.Overlays[1].Persist != nil {
		t.Errorf("Overlays[1] = %+v, want /tmp rw persist=nil", state.Overlays[1])
	}
	if state.Overlays[2].Persist == nil || *state.Overlays[2].Persist {
		t.Errorf("Overlays[2].Persist = %v, want false", state.Overlays[2].Persist)
	}
}

func TestBackgroundRunPaths_MultiOverlay(t *testing.T) {
	upper := filepath.Join(t.TempDir(), "upper")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		overlays     []sessionOverlay
		wantRoot     string
		wantNSErr    bool
		wantNSErrMsg string
	}{
		{
			name:     "rw primary uses the workspace itself",
			overlays: []sessionOverlay{{path: "/ws", mode: isolation.WorkspaceRW}},
			wantRoot: "/ws",
		},
		{
			name: "persist overlay primary uses its upper",
			overlays: []sessionOverlay{{
				path:     "/ws",
				mode:     isolation.WorkspaceOverlay,
				persist:  true,
				upperDir: upper,
			}},
			wantRoot: upper,
		},
		{
			name: "ephemeral overlay primary is rejected",
			overlays: []sessionOverlay{{
				path:    "/ws",
				mode:    isolation.WorkspaceOverlay,
				persist: false,
			}},
			wantNSErr:    true,
			wantNSErrMsg: "no upper directory",
		},
		{
			name: "ephemeral primary is rejected even with a persist secondary",
			overlays: []sessionOverlay{
				{path: "/ws", mode: isolation.WorkspaceOverlay, persist: false},
				{path: "/data", mode: isolation.WorkspaceOverlay, persist: true, upperDir: upper},
			},
			wantNSErr:    true,
			wantNSErrMsg: "no upper directory",
		},
		{
			name:         "ro primary is rejected",
			overlays:     []sessionOverlay{{path: "/ws", mode: isolation.WorkspaceRO}},
			wantNSErr:    true,
			wantNSErrMsg: "read-only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &isolatedSession{
				id:       "bg-paths",
				opts:     &IsolatedSessionOptions{},
				overlays: tt.overlays,
			}
			paths, err := s.backgroundRunPaths()
			if tt.wantNSErr {
				if err == nil {
					t.Fatalf("backgroundRunPaths = %+v, want error", paths)
				}
				if tt.wantNSErrMsg != "" && !strings.Contains(err.Error(), tt.wantNSErrMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantNSErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if paths.hostRoot != tt.wantRoot {
				t.Errorf("hostRoot = %q, want %q", paths.hostRoot, tt.wantRoot)
			}
			wantRunDir := filepath.Join(tt.wantRoot, isolatedBackgroundRunDir)
			if paths.hostRunDir != wantRunDir {
				t.Errorf("hostRunDir = %q, want %q", paths.hostRunDir, wantRunDir)
			}
			if paths.nsRunDir != filepath.Join(tt.overlays[0].path, isolatedBackgroundRunDir) {
				t.Errorf("nsRunDir = %q, want under %q", paths.nsRunDir, tt.overlays[0].path)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestNewMergedView_MultiOverlayRouting(t *testing.T) {
	// Resolve symlinks: MergedView rejects symlinked lower/upper roots, and
	// t.TempDir() sits under a symlink on macOS.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rootUpper := filepath.Join(base, "upper-root")
	wsLower := filepath.Join(base, "ws")
	wsUpper := filepath.Join(base, "upper-ws")
	for _, dir := range []string{rootUpper, wsLower, wsUpper} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	persist := true
	s := newIsolatedSession("merged-view-routing", &IsolatedSessionOptions{
		Overlays: []IsolatedOverlayOptions{
			{Path: base, Mode: "overlay", Persist: &persist},
			{Path: wsLower, Mode: "overlay", Persist: &persist},
		},
	}, newStubIsolator(), nil)
	// Simulate allocation: pair order follows the persist overlays.
	s.overlays[0].upperDir = rootUpper
	s.overlays[1].upperDir = wsUpper

	view := newMergedView(s)

	// A write under the deeper overlay routes to its upper.
	if err := view.WriteFile(filepath.Join(wsLower, "hello.txt"), []byte("ws"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(wsUpper, "hello.txt"))
	if err != nil || string(got) != "ws" {
		t.Fatalf("workspace write landed in the wrong layer (%v, %q)", err, got)
	}
	if _, err := os.Stat(filepath.Join(rootUpper, "ws", "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("workspace write leaked into the root upper: %v", err)
	}

	// A write outside the deeper overlay routes to the shallower one.
	if err := view.WriteFile(filepath.Join(base, "root.txt"), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(rootUpper, "root.txt"))
	if err != nil || string(got) != "root" {
		t.Fatalf("root write landed in the wrong layer (%v, %q)", err, got)
	}

	// A path outside every overlay is rejected.
	if _, err := view.Stat("/etc/hostname"); err == nil {
		t.Fatal("path outside overlays accepted")
	}

	// Relative paths resolve against the primary (first) overlay.
	if err := view.WriteFile("relative.txt", []byte("rel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootUpper, "relative.txt")); err != nil {
		t.Fatalf("relative write did not resolve against the primary overlay: %v", err)
	}
}
