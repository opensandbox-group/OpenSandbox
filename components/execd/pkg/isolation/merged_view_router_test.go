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

	base := t.TempDir()
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
	base := t.TempDir()
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
		assert.Contains(t, err.Error(), "rename across overlays is not supported")
		assert.Empty(t, dataFS.renameArgs, "the destination view must not receive a cross-overlay rename")
	})
}
