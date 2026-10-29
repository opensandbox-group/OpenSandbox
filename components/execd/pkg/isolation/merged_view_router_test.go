// Copyright 2026 The OpenSandbox Authors
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

package isolation

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

// newRouterFixture builds two nested overlay views over real temp dirs:
//
//	<tmp>/ws       → workspace overlay (lower + upper)
//	<tmp>/ws/sub   → nested overlay (lower + upper)
func newRouterFixture(t *testing.T) (*MultiMergedView, string) {
	t.Helper()

	// Resolve symlinks: MergedView rejects symlinked path components, and
	// t.TempDir() sits under the /var symlink on macOS.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	wsLower := filepath.Join(base, "ws")
	wsUpper := filepath.Join(base, "upper-ws")
	subLower := filepath.Join(wsLower, "sub")
	subUpper := filepath.Join(base, "upper-sub")

	for _, d := range []string{wsLower, wsUpper, subLower, subUpper} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	router := NewMultiMergedView([]OverlayView{
		{
			Path: wsLower,
			FS:   NewMergedView(wsLower, wsUpper, WorkspaceOverlay, uid, gid),
		},
		{
			Path: subLower,
			FS:   NewMergedView(subLower, subUpper, WorkspaceOverlay, uid, gid),
		},
	})
	return router, base
}

func TestMultiMergedView_LongestPrefixRouting(t *testing.T) {
	router, base := newRouterFixture(t)
	wsLower := filepath.Join(base, "ws")
	subLower := filepath.Join(wsLower, "sub")

	// Upper-only file in the workspace overlay.
	require.NoError(t, os.WriteFile(filepath.Join(base, "upper-ws", "a.txt"), []byte("A"), 0o644))
	// Upper-only file in the nested sub overlay.
	require.NoError(t, os.WriteFile(filepath.Join(base, "upper-sub", "b.txt"), []byte("B"), 0o644))

	t.Run("workspace_path_routes_to_shallow_view", func(t *testing.T) {
		data, err := router.ReadFile(filepath.Join(wsLower, "a.txt"))
		require.NoError(t, err)
		assert.Equal(t, "A", string(data))
	})

	t.Run("nested_path_routes_to_deepest_view", func(t *testing.T) {
		data, err := router.ReadFile(filepath.Join(wsLower, "sub", "b.txt"))
		require.NoError(t, err)
		assert.Equal(t, "B", string(data))
	})

	t.Run("stat_and_readdir_follow_routing", func(t *testing.T) {
		info, err := router.Stat(filepath.Join(wsLower, "sub", "b.txt"))
		require.NoError(t, err)
		assert.False(t, info.IsDir())

		entries, err := router.ReadDir(wsLower)
		require.NoError(t, err)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.Contains(t, names, "a.txt")
		assert.Contains(t, names, "sub")
	})

	t.Run("relative_path_resolves_against_shallowest_overlay", func(t *testing.T) {
		data, err := router.ReadFile("a.txt")
		require.NoError(t, err)
		assert.Equal(t, "A", string(data))
	})

	t.Run("write_routes_to_routed_upper", func(t *testing.T) {
		target := filepath.Join(wsLower, "sub", "new.txt")
		require.NoError(t, router.WriteFile(target, []byte("N"), 0o644))
		data, err := os.ReadFile(filepath.Join(base, "upper-sub", "new.txt"))
		require.NoError(t, err)
		assert.Equal(t, "N", string(data))
	})

	t.Run("remove_creates_whiteout_in_routed_upper", func(t *testing.T) {
		lowerOnly := filepath.Join(subLower, "gone.txt")
		require.NoError(t, os.WriteFile(lowerOnly, []byte("G"), 0o644))

		require.NoError(t, router.Remove(lowerOnly))

		_, err := router.ReadFile(lowerOnly)
		assert.True(t, os.IsNotExist(err), "removed file must be masked")
		_, err = os.Stat(filepath.Join(base, "upper-sub", ".wh.gone.txt"))
		assert.NoError(t, err, "whiteout must land in the routed upper")
	})

	t.Run("path_outside_all_overlays_rejected", func(t *testing.T) {
		_, err := router.ReadFile("/etc/passwd")
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrPathOutsideOverlays))

		require.Error(t, router.WriteFile("/etc/evil", []byte("x"), 0o644))
	})
}

func TestMultiMergedView_ModeSemanticsPreserved(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	roLower := filepath.Join(base, "ro")
	roUpper := filepath.Join(base, "upper-ro")
	require.NoError(t, os.MkdirAll(roLower, 0o755))
	require.NoError(t, os.MkdirAll(roUpper, 0o755))

	router := NewMultiMergedView([]OverlayView{
		{
			Path: roLower,
			FS:   NewMergedView(roLower, roUpper, WorkspaceRO, 0, 0),
		},
	})

	require.NoError(t, os.WriteFile(filepath.Join(roUpper, "ro.txt"), []byte("R"), 0o644))

	data, err := router.ReadFile(filepath.Join(roLower, "ro.txt"))
	require.NoError(t, err)
	assert.Equal(t, "R", string(data))

	err = router.WriteFile(filepath.Join(roLower, "denied.txt"), []byte("x"), 0o644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
}

func TestMultiMergedView_EmptyRouter(t *testing.T) {
	router := NewMultiMergedView(nil)
	_, err := router.Stat("/anything")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPathOutsideOverlays))
}

// stubFS is a vfs.FS whose ReadFile returns the view name, letting routing
// tests assert which view served a path without real directories.
type stubFS struct {
	name       string
	renameArgs [2]string
}

func (s *stubFS) Stat(string) (os.FileInfo, error)            { return nil, os.ErrNotExist }
func (s *stubFS) ReadFile(string) ([]byte, error)             { return []byte(s.name), nil }
func (s *stubFS) WriteFile(string, []byte, os.FileMode) error { return nil }
func (s *stubFS) WriteFileReader(string, io.Reader, os.FileMode) (int64, error) {
	return 0, nil
}
func (s *stubFS) Remove(string) error                         { return nil }
func (s *stubFS) RemoveAll(string) error                      { return nil }
func (s *stubFS) MkdirAll(string, os.FileMode) error          { return nil }
func (s *stubFS) Chmod(string, os.FileMode) error             { return nil }
func (s *stubFS) ReadDir(string) ([]os.DirEntry, error)       { return nil, nil }
func (s *stubFS) Open(string) (*os.File, error)               { return nil, os.ErrNotExist }
func (s *stubFS) Search(string, string) ([]string, error)     { return nil, nil }
func (s *stubFS) ReplaceContent(string, string, string) error { return nil }
func (s *stubFS) Rename(oldPath, newPath string) error {
	s.renameArgs = [2]string{oldPath, newPath}
	return nil
}

var _ vfs.FS = (*stubFS)(nil)

func TestMultiMergedView_RootOverlayRouting(t *testing.T) {
	rootFS := &stubFS{name: "root"}
	wsFS := &stubFS{name: "workspace"}
	router := NewMultiMergedView([]OverlayView{
		{Path: "/", FS: rootFS},
		{Path: "/workspace", FS: wsFS},
	})

	t.Run("root_prefix_serves_every_absolute_path", func(t *testing.T) {
		for _, p := range []string{"/", "/etc/hosts", "/usr/bin/sh"} {
			data, err := router.ReadFile(p)
			require.NoError(t, err, "path %s must route", p)
			assert.Equal(t, "root", string(data), "path %s must route to the / view", p)
		}
	})

	t.Run("nested_prefix_still_wins", func(t *testing.T) {
		data, err := router.ReadFile("/workspace/a.txt")
		require.NoError(t, err)
		assert.Equal(t, "workspace", string(data))
	})
}

func TestMultiMergedView_RelativeBaseDesignation(t *testing.T) {
	rootFS := &stubFS{name: "root"}
	wsFS := &stubFS{name: "workspace"}
	router := NewMultiMergedView([]OverlayView{
		{Path: "/", FS: rootFS},
		{Path: "/workspace", FS: wsFS, RelativeBase: true},
	})

	data, err := router.ReadFile("a.txt")
	require.NoError(t, err)
	assert.Equal(t, "workspace", string(data),
		"relative paths must resolve against the view marked RelativeBase, not the shallowest overlay")

	t.Run("last_marked_view_wins", func(t *testing.T) {
		a := &stubFS{name: "a"}
		b := &stubFS{name: "b"}
		router := NewMultiMergedView([]OverlayView{
			{Path: "/a", FS: a, RelativeBase: true},
			{Path: "/b", FS: b, RelativeBase: true},
		})
		data, err := router.ReadFile("x.txt")
		require.NoError(t, err)
		assert.Equal(t, "b", string(data), "the last RelativeBase marking wins")
	})

	t.Run("fallback_picks_shallowest_by_depth_not_name_length", func(t *testing.T) {
		storeFS := &stubFS{name: "store"}
		deepFS := &stubFS{name: "deep"}
		router := NewMultiMergedView([]OverlayView{
			{Path: "/data-store", FS: storeFS},
			{Path: "/a/b", FS: deepFS},
		})
		data, err := router.ReadFile("x.txt")
		require.NoError(t, err)
		assert.Equal(t, "store", string(data),
			"the depth-1 /data-store view, not the shorter-named /a/b view, is the relative base")
	})
}

func TestMultiMergedView_RenameRouting(t *testing.T) {
	wsFS := &stubFS{name: "workspace"}
	dataFS := &stubFS{name: "data"}
	router := NewMultiMergedView([]OverlayView{
		{Path: "/workspace", FS: wsFS},
		{Path: "/data", FS: dataFS},
	})

	t.Run("same_overlay_rename_delegates", func(t *testing.T) {
		require.NoError(t, router.Rename("/workspace/a", "/workspace/b"))
		assert.Equal(t, [2]string{"/workspace/a", "/workspace/b"}, wsFS.renameArgs)
	})

	t.Run("cross_overlay_rename_rejected", func(t *testing.T) {
		err := router.Rename("/workspace/a", "/data/b")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rename across mounts is not supported")
		assert.Empty(t, dataFS.renameArgs, "the destination view must not receive a cross-overlay rename")
	})
}

// newBindRouterFixture builds a workspace overlay at <tmp>/ws plus two binds
// whose view roots differ from their mount paths:
//
//	<tmp>/ws        → overlay (lower + upper), routed by /ws
//	<tmp>/bind-rw   → rw bind source, routed by /data
//	<tmp>/bind-ro   → ro bind source, routed by /configs
func newBindRouterFixture(t *testing.T) (*MultiMergedView, string) {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	wsLower := filepath.Join(base, "ws")
	wsUpper := filepath.Join(base, "upper-ws")
	rwSrc := filepath.Join(base, "bind-rw")
	roSrc := filepath.Join(base, "bind-ro")

	for _, d := range []string{wsLower, wsUpper, rwSrc, roSrc} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	router := NewMultiMergedView([]OverlayView{
		{
			Path: "/data",
			Root: rwSrc,
			FS:   NewBindView(rwSrc, false, uid, gid),
		},
		{
			Path: "/configs",
			Root: roSrc,
			FS:   NewBindView(roSrc, true, uid, gid),
		},
		{
			Path:         wsLower,
			FS:           NewMergedView(wsLower, wsUpper, WorkspaceOverlay, uid, gid),
			RelativeBase: true,
		},
	})
	return router, base
}

func TestMultiMergedView_RewrittenRootRouting(t *testing.T) {
	router, base := newBindRouterFixture(t)
	rwSrc := filepath.Join(base, "bind-rw")
	roSrc := filepath.Join(base, "bind-ro")

	require.NoError(t, os.WriteFile(filepath.Join(rwSrc, "seed.txt"), []byte("seed"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(roSrc, "ro.txt"), []byte("ro"), 0o644))

	t.Run("read_via_mount_path_serves_view_root", func(t *testing.T) {
		data, err := router.ReadFile("/data/seed.txt")
		require.NoError(t, err)
		assert.Equal(t, "seed", string(data))
	})

	t.Run("write_via_mount_path_lands_in_view_root", func(t *testing.T) {
		require.NoError(t, router.WriteFile("/data/api.txt", []byte("api"), 0o644))
		data, err := os.ReadFile(filepath.Join(rwSrc, "api.txt"))
		require.NoError(t, err)
		assert.Equal(t, "api", string(data))
	})

	t.Run("stat_and_readdir_follow_rewrite", func(t *testing.T) {
		info, err := router.Stat("/data/seed.txt")
		require.NoError(t, err)
		assert.False(t, info.IsDir())

		entries, err := router.ReadDir("/configs")
		require.NoError(t, err)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.Contains(t, names, "ro.txt")
	})

	t.Run("ro_mode_semantics_survive_rewrite", func(t *testing.T) {
		data, err := router.ReadFile("/configs/ro.txt")
		require.NoError(t, err)
		assert.Equal(t, "ro", string(data))

		err = router.WriteFile("/configs/denied.txt", []byte("x"), 0o644)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read-only")
	})

	t.Run("search_results_rebase_onto_mount_path", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(rwSrc, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(rwSrc, "sub", "deep.txt"), []byte("D"), 0o644))

		results, err := router.Search("/data", "**.txt")
		require.NoError(t, err)
		assert.Contains(t, results, "/data/seed.txt")
		assert.Contains(t, results, "/data/sub/deep.txt")

		// The rebased paths must Stat back onto the same view — this is what
		// the SearchFiles handler does per result.
		for _, p := range results {
			_, err := router.Stat(p)
			require.NoError(t, err, "rebased search result %s must stat", p)
		}
	})

	t.Run("bind_follows_symlinks_like_the_namespace", func(t *testing.T) {
		// An intra-bind symlink inside the bind source resolves on the host
		// for reads, matching how the kernel resolves it for processes
		// inside the namespace.
		target := filepath.Join(rwSrc, "real.txt")
		require.NoError(t, os.WriteFile(target, []byte("via-link"), 0o644))
		link := filepath.Join(rwSrc, "alias.txt")
		require.NoError(t, os.Symlink(target, link))

		data, err := router.ReadFile("/data/alias.txt")
		require.NoError(t, err)
		assert.Equal(t, "via-link", string(data))
	})

	t.Run("bind_rejects_symlink_escape_through_planted_links", func(t *testing.T) {
		// An agent inside a rw bind can plant a symlink pointing outside the
		// bind source; neither mutations nor reads through the files API may
		// escape the bind source onto the host (#2332).
		outside := filepath.Join(base, "outside-host")
		require.NoError(t, os.MkdirAll(outside, 0o755))
		victim := filepath.Join(outside, "victim.txt")
		require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))

		escapeLink := filepath.Join(rwSrc, "escape")
		require.NoError(t, os.Symlink(outside, escapeLink))

		// Mutations through the symlinked parent are rejected.
		assert.Error(t, router.WriteFile("/data/escape/pwn.txt", []byte("x"), 0o644))
		_, err := router.WriteFileReader("/data/escape/stream.txt", strings.NewReader("x"), 0o644)
		assert.Error(t, err)
		assert.Error(t, router.Remove("/data/escape/victim.txt"))
		assert.Error(t, router.MkdirAll("/data/escape/newdir", 0o755))
		assert.Error(t, router.Chmod("/data/escape/victim.txt", 0o777))
		assert.Error(t, router.ReplaceContent("/data/escape/victim.txt", "untouched", "pwned"))
		assert.Error(t, router.Rename("/data/escape/victim.txt", "/data/stolen.txt"))
		assert.Error(t, router.Rename("/data/seed.txt", "/data/escape/moved.txt"))

		// Reads through a symlink escaping the bind source are also rejected.
		_, err = router.ReadFile("/data/escape/victim.txt")
		assert.Error(t, err)
		_, err = router.Stat("/data/escape/victim.txt")
		assert.Error(t, err)
		_, err = router.ReadDir("/data/escape")
		assert.Error(t, err)
		_, err = router.Open("/data/escape/victim.txt")
		assert.Error(t, err)

		// Host victim file and directory remain untouched.
		got, err := os.ReadFile(victim)
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(got))
		_, err = os.Stat(filepath.Join(outside, "pwn.txt"))
		assert.True(t, os.IsNotExist(err))
		_, err = os.Stat(filepath.Join(outside, "newdir"))
		assert.True(t, os.IsNotExist(err))

		// RemoveAll on the planted link removes the link itself (like rm -rf
		// inside the namespace), never the tree it points at.
		require.NoError(t, router.RemoveAll("/data/escape"))
		_, err = os.Lstat(escapeLink)
		assert.True(t, os.IsNotExist(err))
		got, err = os.ReadFile(victim)
		require.NoError(t, err)
		assert.Equal(t, "untouched", string(got))
	})

	t.Run("bind_has_no_whiteout_semantics", func(t *testing.T) {
		// ".wh."-prefixed files are real host files in a bind, not overlay
		// whiteout markers.
		require.NoError(t, os.WriteFile(filepath.Join(rwSrc, "plain.txt"), []byte("P"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(rwSrc, ".wh.plain.txt"), []byte(""), 0o644))

		data, err := router.ReadFile("/data/plain.txt")
		require.NoError(t, err)
		assert.Equal(t, "P", string(data))

		data, err = router.ReadFile("/data/.wh.plain.txt")
		require.NoError(t, err, "whiteout-style names must stay readable in a bind")
		assert.Empty(t, string(data))
	})

	t.Run("nested_mount_prefix_wins_over_broader", func(t *testing.T) {
		// A bind dest nested under the workspace prefix routes to the bind,
		// not the workspace overlay (longest prefix, rewritten root or not).
		wsFS := &stubFS{name: "workspace"}
		nestedFS := &stubFS{name: "nested-bind"}
		nestedRouter := NewMultiMergedView([]OverlayView{
			{Path: "/ws/nested-bind", Root: "/bind-src", FS: nestedFS},
			{Path: "/ws", FS: wsFS},
		})
		data, err := nestedRouter.ReadFile("/ws/nested-bind/n.txt")
		require.NoError(t, err)
		assert.Equal(t, "nested-bind", string(data))

		data, err = nestedRouter.ReadFile("/ws/other.txt")
		require.NoError(t, err)
		assert.Equal(t, "workspace", string(data))
	})

	t.Run("path_outside_all_mounts_rejected", func(t *testing.T) {
		_, err := router.ReadFile("/etc/passwd")
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrPathOutsideOverlays))

		require.Error(t, router.WriteFile("/etc/evil", []byte("x"), 0o644))
	})

	t.Run("rewrite_preserves_relative_base", func(t *testing.T) {
		require.NoError(t, router.WriteFile("relative.txt", []byte("R"), 0o644))
		_, err := os.Stat(filepath.Join(base, "upper-ws", "relative.txt"))
		require.NoError(t, err, "relative write must resolve against the workspace overlay, not a bind root")
	})
}

func TestMultiMergedView_EqualPrefixInsertionOrderWins(t *testing.T) {
	// Binds register before overlays so equal-prefix ties mirror bwrap's
	// later-mount-wins shadowing.
	bindFS := &stubFS{name: "bind"}
	overlayFS := &stubFS{name: "overlay"}
	router := NewMultiMergedView([]OverlayView{
		{Path: "/shared", Root: "/bind-src", FS: bindFS},
		{Path: "/shared", FS: overlayFS},
	})
	data, err := router.ReadFile("/shared/x")
	require.NoError(t, err)
	assert.Equal(t, "bind", string(data))
}
