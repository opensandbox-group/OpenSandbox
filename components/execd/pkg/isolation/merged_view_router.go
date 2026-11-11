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
// registered session mount (overlay or bind).
var ErrPathOutsideOverlays = errors.New("path is outside all session mounts")

// OverlayView binds one mount path (the destination inside the namespace)
// to the view serving it.
type OverlayView struct {
	// Path is the mount destination inside the namespace; routing matches
	// request paths against it by longest prefix.
	Path string
	FS   vfs.FS
	// Root is the path the view is actually rooted at when it differs from
	// Path — a bind view serves dest-prefixed namespace paths from the
	// bind's host source directory. Empty means Root == Path and request
	// paths are forwarded unchanged; otherwise the matched Path prefix is
	// stripped and Root is prepended before the operation is delegated.
	Root string
	// RelativeBase marks this view as the base for relative paths.
	// Single-workspace sessions treat relative paths as workspace-relative,
	// so callers with a workspace overlay mark its view; otherwise a /
	// root overlay would silently capture every relative path. When no
	// view is marked, relative paths resolve against the shallowest
	// overlay; when several views are marked, the last one wins.
	RelativeBase bool
}

// MultiMergedView routes filesystem operations to the mount-scoped view
// whose mount path is the longest prefix of the requested path. It lets one
// session expose several independent mounts (workspace, system root, extra
// project directories, bind mounts) through a single vfs.FS.
//
// Routing rules:
//   - Absolute paths route to the longest matching mount prefix, so
//     /workspace/a resolves against the /workspace mount even when a
//     broader / root mount is also registered. A view mounted at /
//     matches every absolute path.
//   - Equal-length prefixes keep registration order: register bind views
//     before overlay views to mirror bwrap, where a later mount shadows
//     earlier ones sharing a destination.
//   - Relative paths resolve against the view marked RelativeBase (the
//     workspace view), falling back to the shallowest (root-most) mount
//     when none is marked — mirroring single-workspace behavior where
//     relative paths are workspace-relative.
//   - Rename is routed by both paths; a rename whose source and
//     destination resolve to different mounts is rejected.
//   - Paths outside every mount return ErrPathOutsideOverlays.
type MultiMergedView struct {
	routes       []overlayRoute // sorted by prefix length, longest first
	relativeBase vfs.FS
}

type overlayRoute struct {
	prefix string
	root   string // view root; empty forwards paths unchanged
	fs     vfs.FS
}

var _ vfs.FS = (*MultiMergedView)(nil)

// NewMultiMergedView builds a router from mount views. Mount paths are
// cleaned; non-absolute mount paths can never match an absolute request
// path and are dropped, as are non-absolute view roots.
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
		route := overlayRoute{prefix: prefix, fs: v.FS}
		if v.Root != "" {
			root := filepath.Clean(v.Root)
			if !filepath.IsAbs(root) {
				continue
			}
			route.root = root
		}
		routes = append(routes, route)
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

// route resolves the view serving path and returns the path rewritten to
// the view's own root. Relative paths go to the view marked RelativeBase,
// or the shallowest registered mount when none is.
func (m *MultiMergedView) route(path string) (*overlayRoute, string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		if m.relativeBase == nil {
			return nil, "", fmt.Errorf("%w: %s", ErrPathOutsideOverlays, path)
		}
		// Relative paths only ever reach non-rewritten workspace views;
		// bind views are never marked RelativeBase.
		for i := range m.routes {
			if m.routes[i].fs == m.relativeBase {
				return &m.routes[i], cleaned, nil
			}
		}
		return &overlayRoute{fs: m.relativeBase}, cleaned, nil
	}
	for i := range m.routes {
		r := &m.routes[i]
		// r.prefix+"/" would be "//" for a "/" prefix, which no cleaned
		// absolute path starts with; match the root view directly.
		if cleaned == r.prefix || r.prefix == "/" ||
			strings.HasPrefix(cleaned, r.prefix+"/") {
			if r.root == "" || r.root == r.prefix {
				return r, cleaned, nil
			}
			rel := strings.TrimPrefix(cleaned, r.prefix) // "" or "/suffix"
			return r, filepath.Join(r.root, rel), nil
		}
	}
	return nil, "", fmt.Errorf("%w: %s", ErrPathOutsideOverlays, path)
}

func (m *MultiMergedView) Stat(path string) (os.FileInfo, error) {
	rt, p, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return rt.fs.Stat(p)
}

func (m *MultiMergedView) ReadFile(path string) ([]byte, error) {
	rt, p, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return rt.fs.ReadFile(p)
}

func (m *MultiMergedView) WriteFile(path string, data []byte, perm os.FileMode) error {
	rt, p, err := m.route(path)
	if err != nil {
		return err
	}
	return rt.fs.WriteFile(p, data, perm)
}

func (m *MultiMergedView) WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error) {
	rt, p, err := m.route(path)
	if err != nil {
		return 0, err
	}
	return rt.fs.WriteFileReader(p, r, perm)
}

func (m *MultiMergedView) Remove(path string) error {
	rt, p, err := m.route(path)
	if err != nil {
		return err
	}
	if err := rejectMountRootRemoval(rt, path); err != nil {
		return err
	}
	return rt.fs.Remove(p)
}

func (m *MultiMergedView) RemoveAll(path string) error {
	rt, p, err := m.route(path)
	if err != nil {
		return err
	}
	if err := rejectMountRootRemoval(rt, path); err != nil {
		return err
	}
	return rt.fs.RemoveAll(p)
}

// rejectMountRootRemoval mirrors mountpoint semantics: a bind-style mount
// root (caller bind, hosted /tmp) must not be unlinked under a live mount.
// Overlay views keep their historical behavior.
func rejectMountRootRemoval(rt *overlayRoute, path string) error {
	if rt.root != "" && rt.root != rt.prefix && filepath.Clean(path) == rt.prefix {
		return fmt.Errorf("cannot remove mount root %q", rt.prefix)
	}
	return nil
}

func (m *MultiMergedView) MkdirAll(path string, perm os.FileMode) error {
	rt, p, err := m.route(path)
	if err != nil {
		return err
	}
	return rt.fs.MkdirAll(p, perm)
}

// Rename routes both paths and rejects a rename whose source and
// destination resolve to different mounts: delegating to the source view
// would silently place the destination inside that view's upper.
func (m *MultiMergedView) Rename(oldPath, newPath string) error {
	rt, oldP, err := m.route(oldPath)
	if err != nil {
		return err
	}
	dstRT, newP, err := m.route(newPath)
	if err != nil {
		return err
	}
	if dstRT.fs != rt.fs {
		return fmt.Errorf("rename across mounts is not supported: %s -> %s", oldPath, newPath)
	}
	return rt.fs.Rename(oldP, newP)
}

func (m *MultiMergedView) Chmod(path string, mode os.FileMode) error {
	rt, p, err := m.route(path)
	if err != nil {
		return err
	}
	return rt.fs.Chmod(p, mode)
}

func (m *MultiMergedView) ReadDir(path string) ([]os.DirEntry, error) {
	rt, p, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return rt.fs.ReadDir(p)
}

func (m *MultiMergedView) Open(path string) (*os.File, error) {
	rt, p, err := m.route(path)
	if err != nil {
		return nil, err
	}
	return rt.fs.Open(p)
}

// Search routes by the search root. Views with a rewritten root (binds)
// return paths relative to that root, so results are rebased onto the
// namespace mount path and follow-up Stat/Open calls route back to the
// same view instead of falling to the workspace relative base.
func (m *MultiMergedView) Search(root, pattern string) ([]string, error) {
	rt, p, err := m.route(root)
	if err != nil {
		return nil, err
	}
	results, err := rt.fs.Search(p, pattern)
	if err != nil || rt.root == "" || rt.root == rt.prefix {
		return results, err
	}
	rebased := make([]string, 0, len(results))
	for _, rel := range results {
		rebased = append(rebased, filepath.Join(rt.prefix, rel))
	}
	return rebased, nil
}

func (m *MultiMergedView) ReplaceContent(path, old, newStr string) error {
	rt, p, err := m.route(path)
	if err != nil {
		return err
	}
	return rt.fs.ReplaceContent(p, old, newStr)
}
