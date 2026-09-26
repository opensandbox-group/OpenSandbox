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
	Path string
	FS   vfs.FS
}

// MultiMergedView routes filesystem operations to the overlay-scoped view
// whose mount path is the longest prefix of the requested path. It lets one
// session expose several independent overlay mounts (workspace, system root,
// extra project directories) through a single vfs.FS.
//
// Routing rules:
//   - Absolute paths route to the longest matching overlay prefix, so
//     /workspace/a resolves against the /workspace overlay even when a
//     broader / root overlay is also registered.
//   - Relative paths resolve against the shallowest (root-most) overlay,
//     mirroring single-workspace behavior where relative paths are
//     workspace-relative.
//   - Rename is routed by its source path; cross-overlay renames are not
//     supported.
//   - Paths outside every overlay return ErrPathOutsideOverlays.
type MultiMergedView struct {
	routes []overlayRoute // sorted by prefix length, longest first
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
	for _, v := range views {
		if v.FS == nil {
			continue
		}
		prefix := filepath.Clean(v.Path)
		if !filepath.IsAbs(prefix) {
			continue
		}
		routes = append(routes, overlayRoute{prefix: prefix, fs: v.FS})
	}
	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[i].prefix) > len(routes[j].prefix)
	})
	return &MultiMergedView{routes: routes}
}

// route resolves the view serving path. Relative paths fall through to the
// shallowest registered overlay (last in the longest-first route table).
func (m *MultiMergedView) route(path string) (vfs.FS, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		if len(m.routes) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrPathOutsideOverlays, path)
		}
		return m.routes[len(m.routes)-1].fs, nil
	}
	for _, r := range m.routes {
		if cleaned == r.prefix || strings.HasPrefix(cleaned, r.prefix+"/") {
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

func (m *MultiMergedView) Rename(oldPath, newPath string) error {
	fs, err := m.route(oldPath)
	if err != nil {
		return err
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
