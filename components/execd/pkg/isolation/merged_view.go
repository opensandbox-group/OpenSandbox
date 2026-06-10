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
)

// MergedView provides an overlay filesystem view: reads check upper first
// then fall through to lower; writes always go to upper.
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

// safePath validates and cleans a relative path.
func (m *MergedView) safePath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path traversal denied: %s", path)
	}
	return cleaned, nil
}

// Stat returns file info for a path. Checks upper first, then lower.
func (m *MergedView) Stat(path string) (os.FileInfo, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	if m.UpperDir != "" {
		if info, err := os.Stat(m.resolveUpper(rel)); err == nil {
			return info, nil
		}
	}
	return os.Stat(m.resolveLower(rel))
}

// ReadDir lists directory contents, merging upper and lower entries.
func (m *MergedView) ReadDir(path string) ([]os.DirEntry, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var entries []os.DirEntry

	// Lower first.
	lowerEntries, _ := os.ReadDir(m.resolveLower(rel))
	for _, e := range lowerEntries {
		seen[e.Name()] = true
		entries = append(entries, e)
	}

	// Upper overlays.
	if m.UpperDir != "" {
		upperEntries, _ := os.ReadDir(m.resolveUpper(rel))
		for _, e := range upperEntries {
			// Whiteout hides lower entry.
			if strings.HasPrefix(e.Name(), ".wh.") {
				origName := strings.TrimPrefix(e.Name(), ".wh.")
				delete(seen, origName)
				continue
			}
			if !seen[e.Name()] {
				seen[e.Name()] = true
				entries = append(entries, e)
			}
		}
	}

	// Filter out whiteout-hidden entries from the result.
	filtered := entries[:0]
	for _, e := range entries {
		if seen[e.Name()] {
			filtered = append(filtered, e)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Name() < filtered[j].Name()
	})
	return filtered, nil
}

// Open opens a file for reading. Checks upper first, then lower.
func (m *MergedView) Open(path string) (*os.File, error) {
	rel, err := m.safePath(path)
	if err != nil {
		return nil, err
	}

	if m.UpperDir != "" {
		if f, err := os.Open(m.resolveUpper(rel)); err == nil {
			return f, nil
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
		if data, err := os.ReadFile(m.resolveUpper(rel)); err == nil {
			return data, nil
		}
	}
	return os.ReadFile(m.resolveLower(rel))
}

// WriteFile writes data to upper directory.
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

// Remove deletes a file. Upper takes priority; lower-only is reported as
// unwritable (whiteout not yet implemented).
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
			return os.Remove(upperPath)
		}
	}

	// File exists only in lower — cannot delete without whiteout.
	lowerPath := m.resolveLower(rel)
	if _, err := os.Stat(lowerPath); err == nil {
		return fmt.Errorf("cannot remove file from read-only workspace lower: %s", path)
	}

	return fs.ErrNotExist
}

// RemoveAll deletes a path recursively.
func (m *MergedView) RemoveAll(path string) error {
	if m.Mode == WorkspaceRO {
		return fmt.Errorf("remove denied: workspace is read-only")
	}

	rel, err := m.safePath(path)
	if err != nil {
		return err
	}

	if m.UpperDir != "" {
		return os.RemoveAll(m.resolveUpper(rel))
	}
	return fmt.Errorf("read-only workspace")
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
	return os.MkdirAll(m.resolveUpper(rel), perm)
}

// Rename moves a file within upper, or copies lower→upper then whites out.
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

	// If old file doesn't exist in upper, copy it up first.
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
	}

	if err := os.MkdirAll(filepath.Dir(newUpper), 0o755); err != nil {
		return err
	}
	return os.Rename(oldUpper, newUpper)
}

// Chmod changes permissions on a path. Upper takes priority.
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
	}
	return os.Chmod(m.resolveLower(rel), mode)
}

// Search walks the merged view and returns matching paths.
func (m *MergedView) Search(pattern string) ([]string, error) {
	var results []string
	seen := make(map[string]bool)

	// Walk lower.
	if m.LowerDir != "" {
		_ = filepath.WalkDir(m.LowerDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(m.LowerDir, p)
			if matched, _ := filepath.Match(pattern, filepath.Base(rel)); matched {
				seen[rel] = true
				results = append(results, rel)
			}
			return nil
		})
	}

	// Walk upper.
	if m.UpperDir != "" {
		_ = filepath.WalkDir(m.UpperDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Skip whiteout files.
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
