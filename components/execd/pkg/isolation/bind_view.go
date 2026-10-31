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
	"path"
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
// session starts). However, because session.run writes land live in a rw bind
// source, sandbox code can plant (or race in) a symlink pointing outside the
// source. Every operation therefore goes through an os.Root opened on the
// source: path resolution happens component by component relative to that
// directory fd, symlinks that stay inside the source are followed, and any
// that would escape it are refused atomically with the operation (#2332).
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

// resolve maps a namespace path to a path relative to b.Source, suitable
// for the os.Root returned by openRoot.
func (b *BindView) resolve(path string) (string, error) {
	cleaned := filepath.Clean(path)
	// Strip the source prefix so absolute namespace paths (which may arrive
	// with either the dest or the source prefix) resolve identically.
	if strings.HasPrefix(cleaned, b.Source+"/") {
		cleaned = strings.TrimPrefix(cleaned, b.Source+"/")
	} else if cleaned == b.Source {
		cleaned = "."
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		cleaned = "."
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path traversal denied: %s", path)
	}
	return cleaned, nil
}

// openRoot resolves path and opens the bind source as an os.Root. The caller
// must Close the returned root.
func (b *BindView) openRoot(path string) (*os.Root, string, error) {
	rel, err := b.resolve(path)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(b.Source)
	if err != nil {
		return nil, "", err
	}
	return root, b.rebaseLinks(root, rel), nil
}

// maxLinkHops bounds rebaseLinks' symlink expansion, like the kernel's
// MAXSYMLINKS.
const maxLinkHops = 40

// rebaseLinks rewrites absolute symlinks that point back into b.Source into
// their source-relative form, since os.Root refuses every absolute link while
// such links legitimately resolve for the files API. It is purely a path
// rewrite: the caller still performs the operation through root, so a link
// swapped after this walk is resolved (and contained) by os.Root itself.
// Anything it cannot rewrite is returned unchanged for os.Root to refuse.
func (b *BindView) rebaseLinks(root *os.Root, rel string) string {
	src := filepath.Clean(b.Source)
	done := "."
	rest := strings.Split(rel, "/")
	for hops := 0; len(rest) > 0; {
		seg := rest[0]
		rest = rest[1:]
		if seg == "" || seg == "." {
			continue
		}
		next := filepath.Join(done, seg)
		if strings.HasPrefix(next, "..") {
			return rel
		}
		info, err := root.Lstat(next)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			done = next
			continue
		}
		if hops++; hops > maxLinkHops {
			return rel
		}
		target, err := root.Readlink(next)
		if err != nil {
			return rel
		}
		if filepath.IsAbs(target) {
			t := filepath.Clean(target)
			if t != src && !strings.HasPrefix(t, src+"/") {
				return rel
			}
			done = "."
			target = strings.TrimPrefix(strings.TrimPrefix(t, src), "/")
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return done
}

func (b *BindView) denyWrite(op string) error {
	return &os.PathError{Op: op, Path: b.Source, Err: fmt.Errorf("bind is read-only")}
}

func (b *BindView) Stat(path string) (os.FileInfo, error) {
	root, rel, err := b.openRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Stat(rel)
}

func (b *BindView) ReadFile(path string) ([]byte, error) {
	root, rel, err := b.openRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(rel)
}

func (b *BindView) WriteFile(path string, data []byte, perm os.FileMode) error {
	if b.ReadOnly {
		return b.denyWrite("write")
	}
	root, rel, err := b.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := root.WriteFile(rel, data, perm); err != nil {
		return err
	}
	return root.Chown(rel, int(b.Uid), int(b.Gid))
}

func (b *BindView) WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error) {
	if b.ReadOnly {
		return 0, b.denyWrite("write")
	}
	root, rel, err := b.openRoot(path)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return 0, err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, r)
	if err != nil {
		return n, err
	}
	// Chown the open fd so a concurrent swap of the path cannot redirect it.
	return n, f.Chown(int(b.Uid), int(b.Gid))
}

func (b *BindView) Remove(path string) error {
	if b.ReadOnly {
		return b.denyWrite("remove")
	}
	root, rel, err := b.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(rel)
}

func (b *BindView) RemoveAll(path string) error {
	if b.ReadOnly {
		return b.denyWrite("remove")
	}
	root, rel, err := b.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(rel)
}

func (b *BindView) MkdirAll(path string, perm os.FileMode) error {
	if b.ReadOnly {
		return b.denyWrite("mkdir")
	}
	root, rel, err := b.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(rel, perm); err != nil {
		return err
	}
	return root.Chown(rel, int(b.Uid), int(b.Gid))
}

func (b *BindView) Rename(oldPath, newPath string) error {
	if b.ReadOnly {
		return b.denyWrite("rename")
	}
	root, oldRel, err := b.openRoot(oldPath)
	if err != nil {
		return err
	}
	defer root.Close()
	newRel, err := b.resolve(newPath)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(newRel), 0o755); err != nil {
		return err
	}
	return root.Rename(oldRel, newRel)
}

func (b *BindView) Chmod(path string, mode os.FileMode) error {
	if b.ReadOnly {
		return b.denyWrite("chmod")
	}
	root, rel, err := b.openRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Chmod(rel, mode)
}

func (b *BindView) ReadDir(path string) ([]os.DirEntry, error) {
	root, rel, err := b.openRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return fs.ReadDir(root.FS(), rel)
}

func (b *BindView) Open(path string) (*os.File, error) {
	root, rel, err := b.openRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(rel)
}

// Search walks the source directory (no whiteout masking) and returns paths
// relative to the source root, matching MergedView.Search's convention; the
// router rebases rewritten-route results onto the namespace mount path.
// The walk runs inside the source's os.Root, so a search root that descends
// through an escaping symlink is refused instead of walked on the host.
func (b *BindView) Search(root, pattern string) ([]string, error) {
	r, rel, err := b.openRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var results []string
	_ = fs.WalkDir(r.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable dirs
		}
		if d.IsDir() {
			return nil
		}
		if matched, _ := filepath.Match(pattern, path.Base(p)); matched {
			results = append(results, p)
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
