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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

// ErrPathOutsideOverlays is returned when a path does not fall under any
// registered overlay mount.
var ErrPathOutsideOverlays = errors.New("path is outside all overlay mounts")

// OverlayView binds one overlay mount path (the destination inside the
// namespace) to the view serving it.
type OverlayView struct {
	// Path must equal the view's own root (for MergedView, its LowerDir):
	// routing forwards the requested path unchanged, so a view rooted
	// elsewhere would resolve paths outside its root instead of failing
	// with ErrPathOutsideOverlays.
	Path string
	FS   vfs.FS
	// RelativeBase marks this view as the base for relative paths.
	// Single-workspace sessions treat relative paths as workspace-relative,
	// so callers with a workspace overlay mark its view; otherwise a /
	// root overlay would silently capture every relative path. When no
	// view is marked, relative paths resolve against the shallowest
	// overlay; when several views are marked, the last one wins.
	RelativeBase bool
}

// MultiMergedView routes filesystem operations to the overlay-scoped view
// whose mount path is the longest prefix of the requested path. It lets one
// session expose several independent overlay mounts (workspace, system root,
// extra project directories) through a single vfs.FS.
//
// Routing rules:
//   - Absolute paths route to the longest matching overlay prefix, so
//     /workspace/a resolves against the /workspace overlay even when a
//     broader / root overlay is also registered. A view mounted at /
//     matches every absolute path.
//   - Relative paths resolve against the view marked RelativeBase (the
//     workspace view), falling back to the shallowest (root-most) overlay
//     when none is marked — mirroring single-workspace behavior where
//     relative paths are workspace-relative.
//   - Rename is routed by both paths; a rename whose source and
//     destination resolve to different overlays is rejected.
//   - Paths outside every overlay return ErrPathOutsideOverlays.
type MultiMergedView struct {
	routes       []overlayRoute // sorted by prefix length, longest first
	relativeBase vfs.FS
}

type overlayRoute struct {
	prefix string
	fs     vfs.FS
}

var _ vfs.FS = (*MultiMergedView)(nil)

// NewMultiMergedView builds a router from overlay views. Mount paths are
// cleaned; non-absolute mount paths can never match an absolute request
// path and are dropped.
func NewMultiMergedView(views []OverlayView) *MultiMergedView {
	routes := make([]overlayRoute, 0, len(views))
	var relativeBase vfs.FS
	for _, v := range views {
		if v.FS == nil {
			continue
		}
		prefix := filepath.Clean(v.Path)
		if !filepath.IsAbs(prefix) {
			continue
		}
		routes = append(routes, overlayRoute{prefix: prefix, fs: v.FS})
		if v.RelativeBase {
			relativeBase = v.FS
		}
	}
	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[i].prefix) > len(routes[j].prefix)
	})
	if relativeBase == nil && len(routes) > 0 {
		// Fall back to the shallowest overlay (fewest path segments),
		// mirroring single-workspace behavior.
		base := routes[0]
		for _, r := range routes[1:] {
			if prefixDepth(r.prefix) < prefixDepth(base.prefix) {
				base = r
			}
		}
		relativeBase = base.fs
	}
	return &MultiMergedView{routes: routes, relativeBase: relativeBase}
}

// prefixDepth counts the segments of a cleaned absolute path: "/" → 0,
// "/workspace" → 1, "/workspace/sub" → 2.
func prefixDepth(p string) int {
	if p == "/" {
		return 0
	}
	return strings.Count(p, "/")
}

// route resolves the view serving path. Relative paths go to the view
// marked RelativeBase, or the shallowest registered overlay when none is.
func (m *MultiMergedView) route(path string) (vfs.FS, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		if m.relativeBase == nil {
			return nil, fmt.Errorf("%w: %s", ErrPathOutsideOverlays, path)
		}
		return m.relativeBase, nil
	}
	for _, r := range m.routes {
		// r.prefix+"/" would be "//" for a "/" prefix, which no cleaned
		// absolute path starts with; match the root view directly.
		if cleaned == r.prefix || r.prefix == "/" ||
			strings.HasPrefix(cleaned, r.prefix+"/") {
			return r.fs, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrPathOutsideOverlays, path)
}

func (m *MultiMergedView) Stat(path string) (os.FileInfo, error) {
	fs, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return fs.Stat(path)
}

func (m *MultiMergedView) ReadFile(path string) ([]byte, error) {
	fs, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(path)
}

func (m *MultiMergedView) WriteFile(path string, data []byte, perm os.FileMode) error {
	fs, err := m.route(path)
	if err != nil {
		return err
	}
	return fs.WriteFile(path, data, perm)
}

func (m *MultiMergedView) WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error) {
	fs, err := m.route(path)
	if err != nil {
		return 0, err
	}
	return fs.WriteFileReader(path, r, perm)
}

func (m *MultiMergedView) Remove(path string) error {
	fs, err := m.route(path)
	if err != nil {
		return err
	}
	return fs.Remove(path)
}

func (m *MultiMergedView) RemoveAll(path string) error {
	fs, err := m.route(path)
	if err != nil {
		return err
	}
	return fs.RemoveAll(path)
}

func (m *MultiMergedView) MkdirAll(path string, perm os.FileMode) error {
	fs, err := m.route(path)
	if err != nil {
		return err
	}
	return fs.MkdirAll(path, perm)
}

// Rename routes both paths and rejects a rename whose source and
// destination resolve to different overlays: delegating to the source view
// would silently place the destination inside that view's upper.
func (m *MultiMergedView) Rename(oldPath, newPath string) error {
	fs, err := m.route(oldPath)
	if err != nil {
		return err
	}
	dstFS, err := m.route(newPath)
	if err != nil {
		return err
	}
	if dstFS != fs {
		return fmt.Errorf("rename across overlays is not supported: %s -> %s", oldPath, newPath)
	}
	return fs.Rename(oldPath, newPath)
}

func (m *MultiMergedView) Chmod(path string, mode os.FileMode) error {
	fs, err := m.route(path)
	if err != nil {
		return err
	}
	return fs.Chmod(path, mode)
}

func (m *MultiMergedView) ReadDir(path string) ([]os.DirEntry, error) {
	fs, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return fs.ReadDir(path)
}

func (m *MultiMergedView) Open(path string) (*os.File, error) {
	fs, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return fs.Open(path)
}

func (m *MultiMergedView) Search(root, pattern string) ([]string, error) {
	fs, err := m.route(root)
	if err != nil {
		return nil, err
	}
	return fs.Search(root, pattern)
}

func (m *MultiMergedView) ReplaceContent(path, old, newStr string) error {
	fs, err := m.route(path)
	if err != nil {
		return err
	}
	return fs.ReplaceContent(path, old, newStr)
}
