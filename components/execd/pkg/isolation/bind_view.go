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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

// BindView serves a bwrap bind mount's namespace paths directly from the
// bind's host source directory.
//
// Unlike MergedView, a bind is a live 1:1 mount with no copy-on-write
// layers, so the view has no overlay semantics: there are no whiteout
// conventions (a host file named ".wh.x" is a real file, exactly as the
// namespace sees it) and symlinked paths are followed (the kernel resolves
// them the same way for processes inside the namespace — bubblewrap bind
// mounts do not redirect symlink traversal). Read-only binds reject every
// mutation, matching --ro-bind.
//
// The bind source is an operator-allowlisted trusted host directory
// (validateBinds resolves symlinks and enforces allowed_writable before the
// session starts), so this view intentionally applies no per-path symlink
// policy of its own.
//
// Compile-time check: BindView satisfies vfs.FS.
var _ vfs.FS = (*BindView)(nil)

// BindView is a passthrough vfs.FS over a bind mount's host source.
type BindView struct {
	Source   string
	ReadOnly bool
	// Uid/Gid own newly created files and directories, mirroring the
	// MergedView rw chown so in-namespace identities can access what the
	// files API creates.
	Uid, Gid uint32
}

// NewBindView creates a passthrough view over source.
func NewBindView(source string, readOnly bool, uid, gid uint32) *BindView {
	return &BindView{Source: source, ReadOnly: readOnly, Uid: uid, Gid: gid}
}

func (b *BindView) resolve(path string) (string, error) {
	p, err := b.rawResolve(path)
	if err != nil {
		return "", err
	}
	// The staging prefix is reserved, so every operation treats one as absent:
	// a rename would otherwise expose an in-flight upload under a user-visible
	// name, and a remove would cut short a concurrent one.
	if err := denyStagingName(filepath.Base(p), path); err != nil {
		return "", err
	}
	return p, nil
}

// rawResolve is resolve without the staging-name check.
func (b *BindView) rawResolve(path string) (string, error) {
	cleaned := filepath.Clean(path)
	// Strip the source prefix so absolute namespace paths (which may arrive
	// with either the dest or the source prefix) resolve identically.
	if strings.HasPrefix(cleaned, b.Source+"/") {
		cleaned = strings.TrimPrefix(cleaned, b.Source+"/")
	} else if cleaned == b.Source {
		cleaned = "."
	}
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path traversal denied: %s", path)
	}
	return filepath.Join(b.Source, cleaned), nil
}

func (b *BindView) denyWrite(op string) error {
	return &os.PathError{Op: op, Path: b.Source, Err: fmt.Errorf("bind is read-only")}
}

func (b *BindView) Stat(path string) (os.FileInfo, error) {
	p, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (b *BindView) ReadFile(path string) ([]byte, error) {
	p, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (b *BindView) WriteFile(path string, data []byte, perm os.FileMode) error {
	if b.ReadOnly {
		return b.denyWrite("write")
	}
	p, err := b.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeFileStaged(p, data, perm, true, newFileOwner(b.Uid, b.Gid))
}

func (b *BindView) WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error) {
	if b.ReadOnly {
		return 0, b.denyWrite("write")
	}
	p, err := b.resolve(path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	return writeReaderStaged(p, r, perm, true, newFileOwner(b.Uid, b.Gid))
}

func (b *BindView) Remove(path string) error {
	if b.ReadOnly {
		return b.denyWrite("remove")
	}
	p, err := b.resolve(path)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (b *BindView) RemoveAll(path string) error {
	if b.ReadOnly {
		return b.denyWrite("remove")
	}
	p, err := b.resolve(path)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

func (b *BindView) MkdirAll(path string, perm os.FileMode) error {
	if b.ReadOnly {
		return b.denyWrite("mkdir")
	}
	p, err := b.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p, perm); err != nil {
		return err
	}
	return os.Chown(p, int(b.Uid), int(b.Gid))
}

func (b *BindView) Rename(oldPath, newPath string) error {
	if b.ReadOnly {
		return b.denyWrite("rename")
	}
	oldP, err := b.resolve(oldPath)
	if err != nil {
		return err
	}
	newP, err := b.resolve(newPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(newP), 0o755); err != nil {
		return err
	}
	return os.Rename(oldP, newP)
}

func (b *BindView) Chmod(path string, mode os.FileMode) error {
	if b.ReadOnly {
		return b.denyWrite("chmod")
	}
	p, err := b.resolve(path)
	if err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

func (b *BindView) ReadDir(path string) ([]os.DirEntry, error) {
	p, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	return hideStagingEntries(entries), nil
}

func (b *BindView) Open(path string) (*os.File, error) {
	p, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// Search walks the source directory (no whiteout masking) and returns paths
// relative to the source root, matching MergedView.Search's convention; the
// router rebases rewritten-route results onto the namespace mount path.
func (b *BindView) Search(root, pattern string) ([]string, error) {
	rootP, err := b.resolve(root)
	if err != nil {
		return nil, err
	}

	var results []string
	_ = filepath.WalkDir(rootP, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable dirs
		}
		if d.IsDir() {
			return nil
		}
		if isStagingName(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(b.Source, p)
		if relErr != nil {
			return nil //nolint:nilerr // skip paths outside the source
		}
		if matched, _ := filepath.Match(pattern, filepath.Base(rel)); matched {
			results = append(results, rel)
		}
		return nil
	})
	sort.Strings(results)
	return results, nil
}

func (b *BindView) ReplaceContent(path, old, newStr string) error {
	data, err := b.ReadFile(path)
	if err != nil {
		return err
	}
	content := strings.ReplaceAll(string(data), old, newStr)
	return b.WriteFile(path, []byte(content), 0o644) //nolint:gosec
}
