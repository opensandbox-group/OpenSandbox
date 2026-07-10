// Copyright 2026 Alibaba Group Holding Ltd.
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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

// MergedView provides a userspace overlay filesystem view: reads check upper
// first then fall through to lower; writes always go to upper.
//
// Limitation (overlay mode): MergedView writes to the upper directory on the
// host are NOT visible inside a running bwrap overlayfs mount. The kernel VFS
// caches directory entries at mount time; direct modifications to the upper
// directory bypass the overlay and are invisible to processes inside the
// namespace. Only the Run→API direction works (process writes go through the
// overlay to upper, MergedView reads upper on host). For bidirectional
// file exchange, use workspace mode "rw" instead of "overlay".
// Compile-time check: MergedView satisfies vfs.FS.
var _ vfs.FS = (*MergedView)(nil)

type MergedView struct {
	LowerDir string
	UpperDir string
	Uid, Gid uint32
	Mode     WorkspaceMode
}

// NewMergedView creates a merged view. upperDir may be empty (tmpfs).
func NewMergedView(lower, upper string, mode WorkspaceMode, uid, gid uint32) *MergedView {
	return &MergedView{
		LowerDir: lower,
		UpperDir: upper,
		Uid:      uid,
		Gid:      gid,
		Mode:     mode,
	}
}

// resolveUpper returns the upper path for a relative path.
func (m *MergedView) resolveUpper(rel string) string {
	return filepath.Join(m.UpperDir, rel)
}

// resolveLower returns the lower path for a relative path.
func (m *MergedView) resolveLower(rel string) string {
	return filepath.Join(m.LowerDir, rel)
}

// safePath validates and returns a path relative to the workspace.
// Absolute paths under LowerDir are stripped to relative; all others
// are cleaned normally.
func (m *MergedView) safePath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	// Strip workspace prefix from absolute paths so Join works correctly.
	if m.LowerDir != "" && strings.HasPrefix(cleaned, m.LowerDir+"/") {
		cleaned = strings.TrimPrefix(cleaned, m.LowerDir+"/")
	} else if m.LowerDir != "" && cleaned == m.LowerDir {
		cleaned = "."
	}
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path traversal denied: %s", path)
	}
	return cleaned, nil
}

func (m *MergedView) rejectSymlink(path string) error {
	// Check every existing component, not just the final element.
	var check string
	for _, seg := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if seg == "" {
			check = string(filepath.Separator)
			continue
		}
		check = filepath.Join(check, seg)
		info, err := os.Lstat(check)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink target denied: %s", check)
		}
	}
	return nil
}

// createWhiteout creates a whiteout marker in upper to hide a lower entry.
func (m *MergedView) createWhiteout(rel string) error {
	whName := filepath.Join(filepath.Dir(rel), ".wh."+filepath.Base(rel))
	whPath := m.resolveUpper(whName)
	if err := os.MkdirAll(filepath.Dir(whPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(whPath)
	if err != nil {
		return err
	}
	return f.Close()
}

// hasWhiteout checks if a whiteout marker exists for the given relative path.
func (m *MergedView) hasWhiteout(rel string) bool {
	if m.UpperDir == "" {
		return false
	}
	whName := filepath.Join(filepath.Dir(rel), ".wh."+filepath.Base(rel))
	_, err := os.Lstat(m.resolveUpper(whName))
	return err == nil
}

// Stat returns file info for a path. Checks upper first, then lower.
func (m *MergedView) Stat(path string) (os.FileInfo, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	if m.UpperDir != "" {
		upperPath := m.resolveUpper(rel)
		if err := m.rejectSymlink(upperPath); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(upperPath); err == nil {
			return info, nil
		}
		if m.hasWhiteout(rel) {
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		}
	}
	return os.Lstat(m.resolveLower(rel))
}

// ReadDir lists directory contents, merging upper and lower entries.
func (m *MergedView) ReadDir(path string) ([]os.DirEntry, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	entryMap := make(map[string]os.DirEntry)

	// Lower first.
	lowerEntries, lowerErr := os.ReadDir(m.resolveLower(rel))
	for _, e := range lowerEntries {
		entryMap[e.Name()] = e
	}

	// Upper overlays — takes precedence over lower.
	var upperErr error
	if m.UpperDir != "" {
		var upperEntries []os.DirEntry
		upperEntries, upperErr = os.ReadDir(m.resolveUpper(rel))
		for _, e := range upperEntries {
			if strings.HasPrefix(e.Name(), ".wh.") {
				origName := strings.TrimPrefix(e.Name(), ".wh.")
				delete(entryMap, origName)
				continue
			}
			entryMap[e.Name()] = e
		}
	}

	if len(entryMap) == 0 && os.IsNotExist(lowerErr) && (m.UpperDir == "" || os.IsNotExist(upperErr)) {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrNotExist}
	}

	entries := make([]os.DirEntry, 0, len(entryMap))
	for _, e := range entryMap {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	return entries, nil
}

// Open opens a file for reading. Checks upper first, then lower.
func (m *MergedView) Open(path string) (*os.File, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	if m.UpperDir != "" {
		upperPath := m.resolveUpper(rel)
		if err := m.rejectSymlink(upperPath); err != nil {
			return nil, err
		}
		if f, err := os.Open(upperPath); err == nil {
			return f, nil
		}
		if m.hasWhiteout(rel) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
	}
	return os.Open(m.resolveLower(rel))
}

// ReadFile reads file content. Checks upper first, then lower.
func (m *MergedView) ReadFile(path string) ([]byte, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	if m.UpperDir != "" {
		upperPath := m.resolveUpper(rel)
		if err := m.rejectSymlink(upperPath); err != nil {
			return nil, err
		}
		if data, err := os.ReadFile(upperPath); err == nil {
			return data, nil
		}
		if m.hasWhiteout(rel) {
			return nil, &os.PathError{Op: "read", Path: path, Err: os.ErrNotExist}
		}
	}
	return os.ReadFile(m.resolveLower(rel))
}

// WriteFile writes data to upper directory.
// In overlay mode, files written here are visible to subsequent MergedView
// reads but NOT to processes inside the bwrap namespace (see MergedView doc).
func (m *MergedView) WriteFile(path string, data []byte, perm os.FileMode) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("write denied: workspace is read-only")
	}
	if m.UpperDir == "" {
		return fmt.Errorf("no upper directory")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return err
	}

	upperPath := m.resolveUpper(rel)
	if err := m.rejectSymlink(upperPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(upperPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(upperPath, data, perm); err != nil {
		return err
	}
	return os.Chown(upperPath, int(m.Uid), int(m.Gid))
}

// WriteFileReader writes from a reader to upper directory.
func (m *MergedView) WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error) {
	if m.Mode == WorkspaceRO {
		return 0, fmt.Errorf("write denied: workspace is read-only")
	}
	if m.UpperDir == "" {
		return 0, fmt.Errorf("no upper directory")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return 0, err
	}

	upperPath := m.resolveUpper(rel)
	if err := m.rejectSymlink(upperPath); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(upperPath), 0o755); err != nil {
		return 0, err
	}

	f, err := os.OpenFile(upperPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, r)
	if err != nil {
		return n, err
	}
	return n, os.Chown(upperPath, int(m.Uid), int(m.Gid))
}

// Remove deletes a file. For overlay mode, creates a whiteout to hide
// lower-only files.
func (m *MergedView) Remove(path string) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("remove denied: workspace is read-only")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return err
	}

	if m.UpperDir != "" {
		upperPath := m.resolveUpper(rel)
		if _, err := os.Stat(upperPath); err == nil {
			if err := os.Remove(upperPath); err != nil {
				return err
			}
			// If it also exists in lower, create whiteout to hide it.
			if _, err := os.Stat(m.resolveLower(rel)); err == nil {
				return m.createWhiteout(rel)
			}
			return nil
		}
	}

	// File exists only in lower — create whiteout to mask it.
	lowerPath := m.resolveLower(rel)
	if _, err := os.Stat(lowerPath); err == nil {
		if m.UpperDir == "" {
			return fmt.Errorf("remove denied: no upper directory")
		}
		return m.createWhiteout(rel)
	}

	return fs.ErrNotExist
}

// RemoveAll deletes a path recursively. In overlay mode, removes the upper
// tree and creates a whiteout to hide any lower entry at the same path.
func (m *MergedView) RemoveAll(path string) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("remove denied: workspace is read-only")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return err
	}

	if m.UpperDir == "" {
		return fmt.Errorf("remove denied: no upper directory")
	}

	upperPath := m.resolveUpper(rel)
	if err := os.RemoveAll(upperPath); err != nil {
		return err
	}

	if _, err := os.Stat(m.resolveLower(rel)); err == nil {
		return m.createWhiteout(rel)
	}
	return nil
}

// MkdirAll creates directories in upper.
func (m *MergedView) MkdirAll(path string, perm os.FileMode) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("mkdir denied: workspace is read-only")
	}
	if m.UpperDir == "" {
		return fmt.Errorf("no upper directory")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return err
	}
	upperPath := m.resolveUpper(rel)
	if err := os.MkdirAll(upperPath, perm); err != nil {
		return err
	}
	return os.Chown(upperPath, int(m.Uid), int(m.Gid))
}

// Rename moves a file within upper, or copies lower→upper then creates
// a whiteout to hide the lower source.
func (m *MergedView) Rename(oldPath, newPath string) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("rename denied: workspace is read-only")
	}

	oldRel, err := m.safePath(oldPath)
	if err != nil {
		return err
	}
	newRel, err := m.safePath(newPath)
	if err != nil {
		return err
	}

	if m.UpperDir == "" {
		return fmt.Errorf("no upper directory")
	}

	oldUpper := m.resolveUpper(oldRel)
	newUpper := m.resolveUpper(newRel)

	copiedUp := false
	if _, err := os.Stat(oldUpper); os.IsNotExist(err) {
		data, err := os.ReadFile(m.resolveLower(oldRel))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(oldUpper), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(oldUpper, data, 0o644); err != nil { //nolint:gosec
			return err
		}
		copiedUp = true
	}

	if err := os.MkdirAll(filepath.Dir(newUpper), 0o755); err != nil {
		return err
	}
	if err := os.Rename(oldUpper, newUpper); err != nil {
		return err
	}

	// If source existed in lower (either copied-up or both layers),
	// create whiteout to hide the lower source.
	if copiedUp {
		return m.createWhiteout(oldRel)
	}
	if _, err := os.Stat(m.resolveLower(oldRel)); err == nil {
		return m.createWhiteout(oldRel)
	}
	return nil
}

// Chmod changes permissions on a path. Copy-up from lower if needed
// to avoid mutating the original workspace.
func (m *MergedView) Chmod(path string, mode os.FileMode) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("chmod denied: workspace is read-only")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return err
	}

	if m.UpperDir != "" {
		upperPath := m.resolveUpper(rel)
		if _, err := os.Stat(upperPath); err == nil {
			return os.Chmod(upperPath, mode)
		}
		// File only in lower — copy-up first to avoid mutating original.
		lowerPath := m.resolveLower(rel)
		data, err := os.ReadFile(lowerPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(upperPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(upperPath, data, mode); err != nil {
			return err
		}
		return os.Chown(upperPath, int(m.Uid), int(m.Gid))
	}

	if m.Mode == WorkspaceRW {
		return os.Chmod(m.resolveLower(rel), mode)
	}
	return fmt.Errorf("chmod denied: no upper directory")
}

// Search walks the merged view under root and returns matching file paths.
// Directories are excluded from results. Whiteout entries are respected.
func (m *MergedView) Search(root, pattern string) ([]string, error) { //nolint:gocognit
	rootRel, err := m.safePath(root)
	if err != nil {
		return nil, err
	}

	var results []string
	seen := make(map[string]bool)
	whiteouts := make(map[string]bool)

	// Collect whiteouts from upper first.
	if m.UpperDir != "" {
		upperRoot := m.resolveUpper(rootRel)
		_ = filepath.WalkDir(upperRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable dirs
			}
			if strings.HasPrefix(d.Name(), ".wh.") {
				rel, _ := filepath.Rel(m.UpperDir, p)
				origName := strings.TrimPrefix(d.Name(), ".wh.")
				origRel := filepath.Join(filepath.Dir(rel), origName)
				whiteouts[origRel] = true
			}
			return nil
		})
	}

	// Walk lower.
	if m.LowerDir != "" {
		lowerRoot := m.resolveLower(rootRel)
		_ = filepath.WalkDir(lowerRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable dirs
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(m.LowerDir, p)
			if whiteouts[rel] {
				return nil
			}
			if matched, _ := filepath.Match(pattern, filepath.Base(rel)); matched {
				seen[rel] = true
				results = append(results, rel)
			}
			return nil
		})
	}

	// Walk upper.
	if m.UpperDir != "" {
		upperRoot := m.resolveUpper(rootRel)
		_ = filepath.WalkDir(upperRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable dirs
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".wh.") {
				return nil
			}
			rel, _ := filepath.Rel(m.UpperDir, p)
			if matched, _ := filepath.Match(pattern, filepath.Base(rel)); matched && !seen[rel] {
				results = append(results, rel)
			}
			return nil
		})
	}

	sort.Strings(results)
	return results, nil
}

// ReplaceContent reads a file, replaces text, and writes to upper.
func (m *MergedView) ReplaceContent(path, old, newStr string) error {
	data, err := m.ReadFile(path)
	if err != nil {
		return err
	}
	content := strings.ReplaceAll(string(data), old, newStr)
	return m.WriteFile(path, []byte(content), 0o644) //nolint:gosec
}
