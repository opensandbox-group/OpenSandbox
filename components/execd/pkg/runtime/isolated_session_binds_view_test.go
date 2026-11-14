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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

// newBindsViewSession builds a synthetic session the way
// CreateIsolatedSession leaves one behind: resolved overlays (with upper
// directories for persisted overlay mode) and symlink-resolved bind sources.
func newBindsViewSession(
	t *testing.T,
	overlays []IsolatedOverlayOptions,
	uppers map[int]string,
	binds []isolation.BindMount,
) *isolatedSession {
	t.Helper()
	s := newIsolatedSession("binds-view", &IsolatedSessionOptions{
		Overlays: overlays,
		Binds:    binds,
	}, newStubIsolator(), nil)
	for i, upper := range uppers {
		s.overlays[i].upperDir = upper
	}
	return s
}

func TestNewMergedView_BindRouting(t *testing.T) {
	// Resolve symlinks: MergedView rejects symlinked roots, and t.TempDir()
	// sits under a symlink on macOS.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsLower := filepath.Join(base, "ws")
	wsUpper := filepath.Join(base, "upper-ws")
	rwSrc := filepath.Join(base, "bind-src")
	roSrc := filepath.Join(base, "bind-ro-src")
	for _, dir := range []string{wsLower, wsUpper, rwSrc, roSrc} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rwSrc, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roSrc, "ro.txt"), []byte("ro-value"), 0o644); err != nil {
		t.Fatal(err)
	}

	persist := true
	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "overlay", Persist: &persist}},
		map[int]string{0: wsUpper},
		[]isolation.BindMount{
			{Source: rwSrc, Dest: "/data"},
			{Source: roSrc, Dest: "/configs", ReadOnly: true},
		},
	)
	view := newMergedView(s)

	t.Run("read_via_dest_serves_source", func(t *testing.T) {
		data, err := view.ReadFile("/data/seed.txt")
		if err != nil || string(data) != "seed" {
			t.Fatalf("ReadFile /data/seed.txt = %q, %v", data, err)
		}
	})

	t.Run("write_via_dest_lands_in_source", func(t *testing.T) {
		if err := view.WriteFile("/data/api.txt", []byte("api"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(rwSrc, "api.txt"))
		if err != nil || string(got) != "api" {
			t.Fatalf("bind write did not reach the host source: %v, %q", err, got)
		}
	})

	t.Run("readdir_via_dest_lists_source", func(t *testing.T) {
		entries, err := view.ReadDir("/data")
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		for _, want := range []string{"seed.txt", "api.txt"} {
			found := false
			for _, n := range names {
				if n == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("ReadDir /data missing %q, got %v", want, names)
			}
		}
	})

	t.Run("readonly_bind_rejects_writes", func(t *testing.T) {
		data, err := view.ReadFile("/configs/ro.txt")
		if err != nil || string(data) != "ro-value" {
			t.Fatalf("ReadFile /configs/ro.txt = %q, %v", data, err)
		}
		err = view.WriteFile("/configs/new.txt", []byte("x"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("write to ro bind = %v, want read-only denial", err)
		}
	})

	t.Run("path_outside_all_mounts_rejected", func(t *testing.T) {
		if _, err := view.Stat("/etc/hostname"); !errors.Is(err, isolation.ErrPathOutsideOverlays) {
			t.Fatalf("Stat /etc/hostname = %v, want ErrPathOutsideOverlays", err)
		}
	})

	t.Run("relative_paths_still_resolve_against_workspace", func(t *testing.T) {
		if err := view.WriteFile("relative.txt", []byte("rel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(wsUpper, "relative.txt")); err != nil {
			t.Fatalf("relative write did not resolve against the workspace overlay: %v", err)
		}
	})

	t.Run("rename_within_bind_moves_source", func(t *testing.T) {
		if err := view.Rename("/data/seed.txt", "/data/renamed.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(rwSrc, "renamed.txt")); err != nil {
			t.Fatalf("renamed file missing in source: %v", err)
		}
	})

	t.Run("rename_across_mounts_rejected", func(t *testing.T) {
		err := view.Rename("/data/api.txt", filepath.Join(wsLower, "stolen.txt"))
		if err == nil || !strings.Contains(err.Error(), "rename across mounts") {
			t.Fatalf("rename bind→overlay = %v, want cross-mount rejection", err)
		}
	})
}

// TestNewMergedView_BindShadowsOverlayOnSameDest mirrors bwrap: binds are
// appended after overlay segments, so a bind sharing an overlay's dest
// shadows it inside the namespace — the files view must agree.
func TestNewMergedView_BindShadowsOverlayOnSameDest(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(base, "shared")
	upper := filepath.Join(base, "upper-shared")
	bindSrc := filepath.Join(base, "bind-src")
	for _, dir := range []string{shared, upper, bindSrc} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: shared, Mode: "rw"}},
		nil,
		[]isolation.BindMount{{Source: bindSrc, Dest: shared}},
	)
	view := newMergedView(s)

	if err := view.WriteFile(filepath.Join(shared, "who.txt"), []byte("bind"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bindSrc, "who.txt")); err != nil {
		t.Fatalf("write went to the overlay instead of the shadowing bind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "who.txt")); !os.IsNotExist(err) {
		t.Fatalf("write leaked into the overlay path: %v", err)
	}
}

// TestNewMergedView_BindDestDefaultsToSource covers the bwrap parity rule
// that an empty Dest binds source onto itself, and the files view follows.
func TestNewMergedView_BindDestDefaultsToSource(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rwSrc := filepath.Join(base, "rw-src")
	wsLower := filepath.Join(base, "ws")
	for _, dir := range []string{rwSrc, wsLower} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "rw"}},
		nil,
		[]isolation.BindMount{{Source: rwSrc}},
	)
	view := newMergedView(s)

	if err := view.WriteFile(filepath.Join(rwSrc, "same.txt"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rwSrc, "same.txt"))
	if err != nil || string(got) != "same" {
		t.Fatalf("source==dest bind write broken: %v, %q", err, got)
	}
}

// TestNewMergedView_BindViewPassthroughSemantics pins the no-overlay
// semantics of bind views: symlinks resolve like the namespace, ".wh."
// names stay real files, and search results are rebased onto the dest.
func TestNewMergedView_BindViewPassthroughSemantics(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsLower := filepath.Join(base, "ws")
	rwSrc := filepath.Join(base, "bind-src")
	for _, dir := range []string{wsLower, rwSrc} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rwSrc, "plain.txt"), []byte("P"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rwSrc, ".wh.plain.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(rwSrc, "plain.txt"), filepath.Join(rwSrc, "alias.txt")); err != nil {
		t.Fatal(err)
	}

	persist := true
	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "overlay", Persist: &persist}},
		nil,
		[]isolation.BindMount{{Source: rwSrc, Dest: "/data"}},
	)
	view := newMergedView(s)

	// Symlinked entries read through, like `cat` inside the namespace.
	got, err := view.ReadFile("/data/alias.txt")
	if err != nil || string(got) != "P" {
		t.Fatalf("ReadFile through bind symlink = %q, %v", got, err)
	}

	// ".wh."-prefixed names are real files in a bind, not whiteouts.
	if got, err = view.ReadFile("/data/.wh.plain.txt"); err != nil {
		t.Fatalf("whiteout-style file must stay readable in a bind: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected content in .wh. file: %q", got)
	}

	// Search results must come back in namespace space so follow-up Stats
	// (as the SearchFiles handler performs) route to the bind, not the
	// workspace relative base.
	results, err := view.Search("/data", "**.txt")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range results {
		if p == "/data/plain.txt" {
			found = true
		}
		if _, err := view.Stat(p); err != nil {
			t.Fatalf("search result %s does not stat: %v", p, err)
		}
	}
	if !found {
		t.Fatalf("search results missing rebased /data/plain.txt: %v", results)
	}

	// Agent-planted symlinks inside a rw bind pointing outside the bind source
	// must be rejected on both mutations and reads (#2332).
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rwSrc, "link")); err != nil {
		t.Fatal(err)
	}
	if err := view.WriteFile("/data/link/pwn.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile through symlinked parent accepted")
	}
	if err := view.Remove("/data/link/victim.txt"); err == nil {
		t.Fatal("Remove through symlinked parent accepted")
	}
	if err := view.Chmod("/data/link/victim.txt", 0o777); err == nil {
		t.Fatal("Chmod through symlinked parent accepted")
	}
	if err := view.MkdirAll("/data/link/newdir", 0o755); err == nil {
		t.Fatal("MkdirAll through symlinked parent accepted")
	}
	if err := view.Rename("/data/link/victim.txt", "/data/renamed.txt"); err == nil {
		t.Fatal("Rename with symlinked source parent accepted")
	}
	if err := view.Rename("/data/plain.txt", "/data/link/moved.txt"); err == nil {
		t.Fatal("Rename with symlinked destination parent accepted")
	}
	if _, err := view.ReadFile("/data/link/victim.txt"); err == nil {
		t.Fatal("ReadFile through escaping symlink accepted")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim deleted or moved through symlink escape: %v", err)
	}
	// RemoveAll on the planted link drops the link, never its target tree.
	if err := view.RemoveAll("/data/link"); err != nil {
		t.Fatalf("RemoveAll of the planted link itself: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("RemoveAll followed the planted link: %v", err)
	}
}

// TestNewMergedView_BindsWithoutOverlaysDoesNotPanic guards the defensive
// branch: the RelativeBase index must not go out of range when a session
// somehow carries binds but no overlays (Create rejects this today).
func TestNewMergedView_BindsWithoutOverlaysDoesNotPanic(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newBindsViewSession(t, nil, nil,
		[]isolation.BindMount{{Source: src, Dest: "/data"}},
	)
	view := newMergedView(s)

	if err := view.WriteFile("/data/x.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "x.txt")); err != nil {
		t.Fatalf("bind write missing in source: %v", err)
	}
}

// TestNewMergedView_SingleOverlayRejectsPathsOutsideWorkspace pins the
// single-overlay, no-binds session shape: the historical bare MergedView is
// preserved, but absolute paths outside the workspace (bind-like
// destinations, the strict profile's private /tmp) must be rejected with
// ErrPathOutsideOverlays instead of silently resolving against the
// workspace upper, where they would touch a different file than the
// namespace path refers to.
func TestNewMergedView_SingleOverlayRejectsPathsOutsideWorkspace(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsLower := filepath.Join(base, "ws")
	wsUpper := filepath.Join(base, "upper-ws")
	for _, dir := range []string{wsLower, wsUpper} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	persist := true
	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "overlay", Persist: &persist}},
		map[int]string{0: wsUpper},
		nil,
	)
	view := newMergedView(s)
	if _, ok := view.(*isolation.MergedView); !ok {
		t.Fatalf("view type %T, want *isolation.MergedView for the single-overlay shape", view)
	}

	for _, p := range []string{"/tmp/secret.txt", "/etc/hostname", "/mnt/skills/a.txt"} {
		if _, err := view.Stat(p); !errors.Is(err, isolation.ErrPathOutsideOverlays) {
			t.Errorf("Stat %s = %v, want ErrPathOutsideOverlays", p, err)
		}
		if err := view.WriteFile(p, []byte("x"), 0o644); !errors.Is(err, isolation.ErrPathOutsideOverlays) {
			t.Errorf("WriteFile %s = %v, want ErrPathOutsideOverlays", p, err)
		}
	}

	// Nothing leaked into the workspace upper.
	if _, err := os.Stat(filepath.Join(wsUpper, "tmp")); !os.IsNotExist(err) {
		t.Errorf("rejected write leaked into workspace upper: %v", err)
	}

	// Workspace-scoped access keeps working.
	if err := os.WriteFile(filepath.Join(wsLower, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Stat(filepath.Join(wsLower, "seed.txt")); err != nil {
		t.Errorf("Stat under workspace = %v, want nil", err)
	}
	data, err := view.ReadFile("seed.txt")
	if err != nil || string(data) != "seed" {
		t.Errorf("relative read = (%q, %v), want (\"seed\", nil)", data, err)
	}
	if err := view.WriteFile(filepath.Join(wsLower, "from-api.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("workspace write = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(wsUpper, "from-api.txt")); err != nil {
		t.Fatalf("workspace write missing in upper: %v", err)
	}
}

// TestValidateBindMountStack pins the mount-stack collisions the files view
// cannot represent: duplicates, ancestor binds, and trusted-base shadowing.
// Bind == overlay/extra_writable dest is allowed (the later bind shadows in
// both views).
func TestValidateBindMountStack(t *testing.T) {
	cases := []struct {
		name        string
		overlays    []IsolatedOverlayOptions
		extra       []string
		binds       []BindMountSpec
		wantFailure string
	}{
		{
			name:     "bind equal to overlay dest is allowed",
			overlays: []IsolatedOverlayOptions{{Path: "/data"}},
			binds:    []BindMountSpec{{source: "/a", dest: "/data"}},
		},
		{
			name:     "bind inside overlay dest is allowed",
			overlays: []IsolatedOverlayOptions{{Path: "/data"}},
			binds:    []BindMountSpec{{source: "/a", dest: "/data/proj"}},
		},
		{
			name:  "bind equal to extra_writable is allowed",
			extra: []string{"/mnt/scratch"},
			binds: []BindMountSpec{{source: "/a", dest: "/mnt/scratch"}},
		},
		{
			name:        "duplicate bind dests rejected",
			binds:       []BindMountSpec{{source: "/a", dest: "/mnt/x"}, {source: "/b", dest: "/mnt/x"}},
			wantFailure: "duplicate bind destination",
		},
		{
			name:        "bind ancestor of overlay rejected",
			overlays:    []IsolatedOverlayOptions{{Path: "/data/proj"}},
			binds:       []BindMountSpec{{source: "/a", dest: "/data"}},
			wantFailure: "shadows mount",
		},
		{
			name:        "bind ancestor of extra_writable rejected",
			extra:       []string{"/mnt/scratch/sub"},
			binds:       []BindMountSpec{{source: "/a", dest: "/mnt/scratch"}},
			wantFailure: "shadows mount",
		},
		{
			name:        "bind ancestor of another bind rejected",
			binds:       []BindMountSpec{{source: "/a", dest: "/data"}, {source: "/b", dest: "/data/proj"}},
			wantFailure: "nested bind destinations",
		},
		{
			name:        "bind under /run rejected",
			binds:       []BindMountSpec{{source: "/a", dest: "/run/secrets"}},
			wantFailure: "trusted base mount",
		},
		{
			name:        "bind at /dev rejected",
			binds:       []BindMountSpec{{source: "/a", dest: "/dev"}},
			wantFailure: "trusted base mount",
		},
		{
			name:     "unrelated bind and overlay allowed",
			overlays: []IsolatedOverlayOptions{{Path: "/workspace"}},
			binds:    []BindMountSpec{{source: "/a", dest: "/mnt/data"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binds := make([]isolation.BindMount, 0, len(tc.binds))
			for _, b := range tc.binds {
				binds = append(binds, isolation.BindMount{Source: b.source, Dest: b.dest})
			}
			err := validateBindMountStack(tc.overlays, tc.extra, binds)
			if tc.wantFailure == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantFailure) {
				t.Fatalf("error %v, want containing %q", err, tc.wantFailure)
			}
		})
	}
}

type BindMountSpec struct {
	source string
	dest   string
}

// TestNewMergedView_TmpBindRouting covers the hosted private /tmp: the
// strict profile binds a host directory under the session's upper-root
// subtree as /tmp, and the files view must serve the same content run
// writes there (issue #2413).
func TestNewMergedView_TmpBindRouting(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsLower := filepath.Join(base, "ws")
	wsUpper := filepath.Join(base, "upper-ws")
	tmpHost := filepath.Join(base, "session-dir", "tmp")
	for _, dir := range []string{wsLower, wsUpper, tmpHost} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpHost, "seed.txt"), []byte("tmp-seed"), 0o644); err != nil {
		t.Fatal(err)
	}

	persist := true
	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "overlay", Persist: &persist}},
		map[int]string{0: wsUpper},
		nil,
	)
	s.tmpDir = tmpHost
	view := newMergedView(s)
	if _, ok := view.(*isolation.MultiMergedView); !ok {
		t.Fatalf("view type %T, want *isolation.MultiMergedView once /tmp is hosted", view)
	}

	t.Run("read_and_write_via_tmp_reach_host_dir", func(t *testing.T) {
		data, err := view.ReadFile("/tmp/seed.txt")
		if err != nil || string(data) != "tmp-seed" {
			t.Fatalf("ReadFile /tmp/seed.txt = %q, %v", data, err)
		}
		if err := view.WriteFile("/tmp/api.txt", []byte("api"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(tmpHost, "api.txt"))
		if err != nil || string(got) != "api" {
			t.Fatalf("/tmp write did not reach the host backing dir: %v, %q", err, got)
		}
	})

	t.Run("readdir_lists_tmp", func(t *testing.T) {
		entries, err := view.ReadDir("/tmp")
		if err != nil {
			t.Fatal(err)
		}
		names := make(map[string]bool, len(entries))
		for _, e := range entries {
			names[e.Name()] = true
		}
		for _, want := range []string{"seed.txt", "api.txt"} {
			if !names[want] {
				t.Fatalf("ReadDir /tmp missing %q, got %v", want, names)
			}
		}
	})

	t.Run("workspace_routing_unaffected", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(wsLower, "seed.txt"), []byte("seed"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := view.Stat(filepath.Join(wsLower, "seed.txt")); err != nil {
			t.Fatalf("workspace Stat = %v", err)
		}
		if _, err := view.Stat("/etc/hostname"); !errors.Is(err, isolation.ErrPathOutsideOverlays) {
			t.Fatalf("Stat /etc/hostname = %v, want ErrPathOutsideOverlays", err)
		}
	})
}

// TestNewMergedView_BindOnTmpShadowsTmpBind mirrors bwrap argv order: a
// caller bind at dest /tmp is applied after the session's /tmp bind and
// shadows it inside the namespace, so the files view must route /tmp to
// the caller's bind source too.
func TestNewMergedView_BindOnTmpShadowsTmpBind(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsLower := filepath.Join(base, "ws")
	tmpHost := filepath.Join(base, "session-dir", "tmp")
	bindSrc := filepath.Join(base, "bind-src")
	for _, dir := range []string{wsLower, tmpHost, bindSrc} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpHost, "marker.txt"), []byte("session-tmp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindSrc, "marker.txt"), []byte("caller-bind"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "rw"}},
		nil,
		[]isolation.BindMount{{Source: bindSrc, Dest: "/tmp"}},
	)
	s.tmpDir = tmpHost
	view := newMergedView(s)

	data, err := view.ReadFile("/tmp/marker.txt")
	if err != nil || string(data) != "caller-bind" {
		t.Fatalf("ReadFile /tmp/marker.txt = %q, %v; want the caller bind to shadow the session /tmp bind", data, err)
	}
}

// TestNewMergedView_TmpMountRootRemovalRejected: removing the /tmp mount
// root would unlink the host backing dir under the live mount; the router
// rejects it while child removal keeps working.
func TestNewMergedView_TmpMountRootRemovalRejected(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wsLower := filepath.Join(base, "ws")
	tmpHost := filepath.Join(base, "session-dir", "tmp")
	for _, dir := range []string{wsLower, tmpHost} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpHost, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newBindsViewSession(t,
		[]IsolatedOverlayOptions{{Path: wsLower, Mode: "rw"}},
		nil,
		nil,
	)
	s.tmpDir = tmpHost
	view := newMergedView(s)

	if err := view.Remove("/tmp"); err == nil || !strings.Contains(err.Error(), "mount root") {
		t.Fatalf("Remove /tmp = %v, want mount-root rejection", err)
	}
	if err := view.RemoveAll("/tmp"); err == nil || !strings.Contains(err.Error(), "mount root") {
		t.Fatalf("RemoveAll /tmp = %v, want mount-root rejection", err)
	}
	if _, err := os.Stat(tmpHost); err != nil {
		t.Fatalf("backing dir must survive removal attempts: %v", err)
	}
	if err := view.Remove("/tmp/f.txt"); err != nil {
		t.Fatalf("child removal = %v, want nil", err)
	}
}
