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

// ConfinedView serves an attacker-controlled session directory (the hosted
// private /tmp) through os.Root: operations stay beneath the root and
// symlink escapes are rejected — including absolute link targets, which the
// namespace would follow. BindView's source is operator-allowlisted and
// trusted to mirror the namespace; this directory is written by the
// workload, so a planted symlink (e.g. /tmp/p -> /etc/passwd) must not
// redirect a files-API write onto the host.
//
// Requires openat2 (Linux 5.6+); os.OpenRoot fails on older kernels.
//
// Compile-time check: ConfinedView satisfies vfs.FS.
var _ vfs.FS = (*ConfinedView)(nil)

// ConfinedView is a root-confined vfs.FS over one host directory.
type ConfinedView struct {
	Root string
	// Uid/Gid own newly created files, mirroring the BindView chown so
	// in-namespace identities can access what the files API creates.
	Uid, Gid uint32
}

// NewConfinedView creates a confined view over root.
func NewConfinedView(root string, uid, gid uint32) *ConfinedView {
	return &ConfinedView{Root: root, Uid: uid, Gid: gid}
}

// rel converts a namespace-or-host path to a root-relative name; the router
// forwards host paths after its OverlayView root rewrite. The staging prefix is
// reserved, so every operation treats one as absent: a rename would otherwise
// expose an in-flight upload under a user-visible name.
func (c *ConfinedView) rel(path string) (string, error) {
	name, err := c.rawRel(path)
	if err != nil {
		return "", err
	}
	if err := denyStagingName(name, path); err != nil {
		return "", err
	}
	return name, nil
}

// rawRel is rel without the staging-name check.
func (c *ConfinedView) rawRel(path string) (string, error) {
	cleaned := filepath.Clean(path)
	switch {
	case cleaned == c.Root || cleaned == "/tmp":
		return ".", nil
	case strings.HasPrefix(cleaned, c.Root+"/"):
		return strings.TrimPrefix(cleaned, c.Root+"/"), nil
	case strings.HasPrefix(cleaned, "/tmp/"):
		return strings.TrimPrefix(cleaned, "/tmp/"), nil
	}
	return "", fmt.Errorf("path %q is outside the confined view root %q", path, c.Root)
}

func (c *ConfinedView) openRoot() (*os.Root, error) {
	return os.OpenRoot(c.Root)
}

// chownNew moves ownership of a created entry to the session identity.
func (c *ConfinedView) chownNew(root *os.Root, name string) error {
	return root.Chown(name, int(c.Uid), int(c.Gid))
}

func (c *ConfinedView) Stat(path string) (os.FileInfo, error) {
	name, err := c.rel(path)
	if err != nil {
		return nil, err
	}
	root, err := c.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Stat(name)
}

func (c *ConfinedView) ReadFile(path string) ([]byte, error) {
	name, err := c.rel(path)
	if err != nil {
		return nil, err
	}
	root, err := c.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}

func (c *ConfinedView) WriteFile(path string, data []byte, perm os.FileMode) error {
	name, err := c.rel(path)
	if err != nil {
		return err
	}
	root, err := c.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return writeFileConfinedStaged(root, name, data, perm, newFileOwner(c.Uid, c.Gid))
}

func (c *ConfinedView) WriteFileReader(path string, r io.Reader, perm os.FileMode) (int64, error) {
	name, err := c.rel(path)
	if err != nil {
		return 0, err
	}
	root, err := c.openRoot()
	if err != nil {
		return 0, err
	}
	defer root.Close()
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	return writeReaderConfinedStaged(root, name, r, perm, newFileOwner(c.Uid, c.Gid))
}

func (c *ConfinedView) Remove(path string) error {
	name, err := c.rel(path)
	if err != nil {
		return err
	}
	if name == "." {
		return fmt.Errorf("cannot remove the confined view root %q", c.Root)
	}
	root, err := c.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(name)
}

func (c *ConfinedView) RemoveAll(path string) error {
	name, err := c.rel(path)
	if err != nil {
		return err
	}
	if name == "." {
		return fmt.Errorf("cannot remove the confined view root %q", c.Root)
	}
	root, err := c.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(name)
}

func (c *ConfinedView) MkdirAll(path string, perm os.FileMode) error {
	name, err := c.rel(path)
	if err != nil {
		return err
	}
	root, err := c.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(name, perm); err != nil {
		return err
	}
	return c.chownNew(root, name)
}

func (c *ConfinedView) Rename(oldPath, newPath string) error {
	oldName, err := c.rel(oldPath)
	if err != nil {
		return err
	}
	newName, err := c.rel(newPath)
	if err != nil {
		return err
	}
	root, err := c.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := filepath.Dir(newName); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return root.Rename(oldName, newName)
}

func (c *ConfinedView) Chmod(path string, mode os.FileMode) error {
	name, err := c.rel(path)
	if err != nil {
		return err
	}
	root, err := c.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Chmod(name, mode)
}

func (c *ConfinedView) ReadDir(path string) ([]os.DirEntry, error) {
	name, err := c.rel(path)
	if err != nil {
		return nil, err
	}
	root, err := c.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	return hideStagingEntries(entries), nil
}

func (c *ConfinedView) Open(path string) (*os.File, error) {
	name, err := c.rel(path)
	if err != nil {
		return nil, err
	}
	root, err := c.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(name)
}

// Search walks the confined tree (escaping symlinks surface as walk errors
// and are skipped) and returns root-relative paths; the router rebases
// rewritten-route results onto the namespace mount path.
func (c *ConfinedView) Search(rootPath, pattern string) ([]string, error) {
	base, err := c.rel(rootPath)
	if err != nil {
		return nil, err
	}
	root, err := c.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()

	fsys := root.FS()
	var results []string
	_ = fs.WalkDir(fsys, base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable or escaping entries
		}
		if d.IsDir() {
			return nil
		}
		if isStagingName(d.Name()) {
			return nil
		}
		if matched, _ := filepath.Match(pattern, filepath.Base(p)); matched {
			results = append(results, p)
		}
		return nil
	})
	sort.Strings(results)
	return results, nil
}

func (c *ConfinedView) ReplaceContent(path, old, newStr string) error {
	data, err := c.ReadFile(path)
	if err != nil {
		return err
	}
	content := strings.ReplaceAll(string(data), old, newStr)
	info, err := c.Stat(path)
	if err != nil {
		return err
	}
	return c.WriteFile(path, []byte(content), info.Mode().Perm())
}
