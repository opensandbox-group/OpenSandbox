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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stagingPrefix names the private temp file a staged write creates next to its
// destination. The listing and search paths hide these names so an in-flight
// write, or one left behind by a killed daemon, is never returned as a user
// file.
const stagingPrefix = ".osb-staging-"

// stagingSuffixLen is the number of hex characters appended to stagingPrefix.
const stagingSuffixLen = 16

// maxSymlinkDepth bounds the final-component symlink walk; it mirrors the
// kernel's ELOOP threshold closely enough to reject a link cycle.
const maxSymlinkDepth = 40

// destinationModeMask keeps the permission bits an in-place write preserved for
// free, including setuid/setgid/sticky: a rename replaces the inode, so the
// staged file has to carry them explicitly.
const destinationModeMask = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// errNotRegularFile rejects a destination a rename cannot replace.
var errNotRegularFile = errors.New("not a regular file")

// fileOwner is the ownership a session view applies to a written file. set is
// false for a caller that must leave ownership alone.
type fileOwner struct {
	uid, gid int
	set      bool
}

// newFileOwner returns the ownership a session view assigns to a written file.
func newFileOwner(uid, gid uint32) fileOwner {
	return fileOwner{uid: int(uid), gid: int(gid), set: true}
}

// stagingFS is the subset of os and *os.Root a staged write needs, so one
// implementation serves both the plain and the root-confined views.
type stagingFS interface {
	Stat(name string) (os.FileInfo, error)
	Lstat(name string) (os.FileInfo, error)
	Readlink(name string) (string, error)
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Rename(oldName, newName string) error
	Remove(name string) error
}

// osStagingFS adapts the package-level os functions to stagingFS.
type osStagingFS struct{}

func (osStagingFS) Stat(name string) (os.FileInfo, error)  { return os.Stat(name) }
func (osStagingFS) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }
func (osStagingFS) Readlink(name string) (string, error)   { return os.Readlink(name) }
func (osStagingFS) Open(name string) (*os.File, error)     { return os.Open(name) }
func (osStagingFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}
func (osStagingFS) Rename(oldName, newName string) error { return os.Rename(oldName, newName) }
func (osStagingFS) Remove(name string) error             { return os.Remove(name) }

// isStagingName reports whether name is one of the staged-write temp files.
func isStagingName(name string) bool {
	suffix, ok := strings.CutPrefix(name, stagingPrefix)
	if !ok || len(suffix) != stagingSuffixLen {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

// IsStagingName reports whether name is a staged-write temp file. Callers
// outside this package filter listings with it.
func IsStagingName(name string) bool { return isStagingName(name) }

// hideStagingEntries removes staged-write temp files from a directory listing.
// It filters in place and returns the shortened slice, so the caller must pass
// a slice it owns.
func hideStagingEntries(entries []os.DirEntry) []os.DirEntry {
	filtered := entries[:0]
	for _, entry := range entries {
		if isStagingName(entry.Name()) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// stagedWrite replaces target with the bytes produced by write.
//
// The bytes are staged in this directory and renamed into place only once they
// are fully written and synced, so a write that fails partway - ENOSPC, an I/O
// error, a short read, the daemon being killed mid-copy - leaves the file it
// was replacing intact. An in-place O_TRUNC write destroyed it, and a session
// workspace usually holds the only copy of the file a write replaces.
//
// followLink resolves a symlink in the final path component so the rename lands
// on the file an in-place write would have written through, instead of
// replacing the link itself.
//
// owner is applied to the staging inode before the rename. It has to happen
// here rather than on the committed path: a chown clears setuid/setgid, so
// chowning after the rename would drop the mode the write is supposed to keep.
func stagedWrite(fsys stagingFS, target string, perm os.FileMode, followLink bool, owner fileOwner, write func(io.Writer) (int64, error)) (int64, error) {
	if followLink {
		resolved, err := resolveFinalSymlink(fsys, target)
		if err != nil {
			return 0, err
		}
		target = resolved
	}

	existing, statErr := fsys.Stat(target)
	switch {
	case statErr == nil && !existing.Mode().IsRegular():
		// A device or a FIFO cannot be staged and renamed over without
		// replacing the node with a regular file, so refuse them.
		return 0, &os.PathError{Op: "write", Path: target, Err: errNotRegularFile}
	case statErr == nil:
		// A rename only needs a writable directory, so probe the file itself to
		// keep rejecting a read-only destination the way an in-place write did.
		probe, err := fsys.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			return 0, err
		}
		_ = probe.Close()
	case !errors.Is(statErr, fs.ErrNotExist):
		return 0, statErr
	}

	// An existing destination keeps its content private while it is copied
	// instead of exposing it through the staging file's mode; a new one takes
	// perm, exactly as the plain create did.
	stagingPerm := perm
	if statErr == nil {
		stagingPerm = 0o600
	}
	staging, err := createStagingFile(fsys, filepath.Dir(target), stagingPerm)
	if err != nil {
		// A directory that is only traversable (0555, with a writable file in
		// it) was writable in place before staging existed. Keep that write
		// working through the legacy path; its tradeoff is documented on the
		// route.
		if statErr == nil && errors.Is(err, fs.ErrPermission) {
			return writeInPlace(fsys, target, existing.Mode(), owner, write)
		}
		// Name the destination the caller asked for, not the staging entry.
		return 0, writeError(target, err)
	}
	committed := false
	descriptorClosed := false
	defer func() {
		if committed || descriptorClosed {
			return
		}
		// cleanupStaging checks ownership before unlinking, while the
		// descriptor still pins the staging inode on POSIX, so the allocator
		// cannot recycle the number and make a session's fresh file look like
		// ours. Only an entry this write still owns is removed: a session that
		// renamed its own file onto the name would otherwise lose it.
		cleanupStaging(fsys, staging)
	}()

	n, err := write(staging.file)
	if err != nil {
		return n, writeError(target, err)
	}
	if owner.set {
		// Ownership goes on before the mode: chown clears setuid/setgid.
		if err := staging.file.Chown(owner.uid, owner.gid); err != nil {
			return n, writeError(target, err)
		}
	}
	if statErr == nil {
		// Restore the destination mode after the copy and after the chown, so
		// setuid/setgid/sticky survive into the renamed inode.
		if err := staging.file.Chmod(existing.Mode() & destinationModeMask); err != nil {
			return n, writeError(target, err)
		}
	}
	if err := verifyStagingEntry(fsys, staging, target); err != nil {
		return n, err
	}
	if err := staging.file.Sync(); err != nil {
		return n, writeError(target, err)
	}
	if closeStagingBeforeCleanup() {
		// Windows cannot rename over or unlink an open file, so its descriptor
		// has to go before the rename. Mark it closed so cleanup does not close
		// it a second time.
		if err := staging.file.Close(); err != nil {
			return n, writeError(target, err)
		}
		descriptorClosed = true
	}
	if err := fsys.Rename(staging.name, target); err != nil {
		// On POSIX the descriptor is still open here, so the deferred cleanup
		// can still prove ownership before unlinking. On Windows it is closed
		// already and cleanupStaging reopens nothing.
		return n, writeError(target, err)
	}
	committed = true
	if !closeStagingBeforeCleanup() {
		// The content is already synced and renamed into place, so a delayed
		// close error now describes durability of an applied write, not a
		// failed one. Reporting it would tell the caller a mutation failed
		// after it succeeded, and invite a retry that replaces the file again.
		descriptorClosed = true
		_ = staging.file.Close()
	}

	// Best-effort directory fsync so the renamed entry is durable and visible
	// on weakly-coherent filesystems (virtio-fs, 9pfs, etc.). Some filesystems
	// return ENOTSUP.
	syncDir(fsys, filepath.Dir(target))
	return n, nil
}

// writeError reports a failed staged write against the destination the caller
// asked for, so the staging temp file never appears in an API response.
func writeError(target string, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return &os.PathError{Op: pathErr.Op, Path: target, Err: pathErr.Err}
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return &os.LinkError{Op: linkErr.Op, Old: target, New: target, Err: linkErr.Err}
	}
	return fmt.Errorf("write %s: %w", target, err)
}

// writeInPlace is the legacy path for a destination whose directory is not
// writable but whose file is. Unlike the staged path it truncates first, so a
// copy that fails partway loses the previous content.
func writeInPlace(fsys stagingFS, target string, perm os.FileMode, owner fileOwner, write func(io.Writer) (int64, error)) (int64, error) {
	file, err := fsys.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return 0, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	n, err := write(file)
	if err != nil {
		return n, err
	}
	if owner.set {
		if err := file.Chown(owner.uid, owner.gid); err != nil {
			return n, err
		}
		if err := file.Chmod(perm & destinationModeMask); err != nil {
			return n, err
		}
	}
	if err := file.Sync(); err != nil {
		return n, err
	}
	if err := file.Close(); err != nil {
		return n, err
	}
	closed = true
	return n, nil
}

// verifyStagingEntry narrows the swap window: it checks the staging name still
// refers to the inode this write filled, immediately before the rename. A
// session that can write the destination directory can still swap the name
// between this check and the rename, so this is not a closed hole; it turns an
// unbounded window into two adjacent syscalls.
func verifyStagingEntry(fsys stagingFS, staging *stagingFile, target string) error {
	if staging.ownsEntry(fsys) {
		return nil
	}
	return fmt.Errorf("write %s: the staging file was replaced during the write", target)
}

// stagingFile is an open staging inode plus the name it was created under.
//
// The descriptor is kept from the exclusive create through the write, the
// metadata changes and the rename. Reopening the name instead would hand the
// sandbox workload a window to replace the staging entry with a symlink and
// redirect the payload, in the very directories a session is allowed to write.
type stagingFile struct {
	file *os.File
	name string
	// created identifies the inode this write owns, so cleanup can tell its
	// own staging file from one a session has since renamed onto the same name.
	created os.FileInfo
}

// ownsEntry reports whether name still refers to this staging inode.
func (s *stagingFile) ownsEntry(fsys stagingFS) bool {
	onDisk, err := fsys.Lstat(s.name)
	if err != nil {
		return false
	}
	return os.SameFile(onDisk, s.created)
}

// createStagingFile atomically creates a new staging file in dir. The caller
// owns the returned descriptor.
func createStagingFile(fsys stagingFS, dir string, perm os.FileMode) (*stagingFile, error) {
	for range 10 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, err
		}
		name := stagingPrefix + hex.EncodeToString(suffix[:])
		path := name
		if dir != "." {
			path = filepath.Join(dir, name)
		}
		file, err := fsys.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			created, statErr := file.Stat()
			if statErr != nil {
				// Leave the entry alone rather than removing it by name: a
				// session could have swapped the name in the meantime, and an
				// orphaned staging file is hidden from listings anyway.
				_ = file.Close()
				return nil, statErr
			}
			return &stagingFile{file: file, name: path, created: created}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, errors.New("could not allocate a unique staging file name")
}

// resolveFinalSymlink follows a symlink in the final path component. Because
// every call goes through fsys, an os.Root-confined view keeps its confinement:
// an absolute target is left as-is and rejected by the root on the next step.
func resolveFinalSymlink(fsys stagingFS, path string) (string, error) {
	resolved := path
	for range maxSymlinkDepth {
		info, err := fsys.Lstat(resolved)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// A new file, or the dangling target of a link.
				return resolved, nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return resolved, nil
		}
		link, err := fsys.Readlink(resolved)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(resolved), link)
		}
		resolved = link
	}
	return "", fmt.Errorf("write to %s: too many levels of symbolic links", path)
}

// syncDir best-effort fsyncs dir so a renamed entry is durable.
func syncDir(fsys stagingFS, dir string) {
	d, err := fsys.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// writeFileStaged replaces target with data, applying owner.
func writeFileStaged(target string, data []byte, perm os.FileMode, followLink bool, owner fileOwner) error {
	_, err := stagedWrite(osStagingFS{}, target, perm, followLink, owner, func(w io.Writer) (int64, error) {
		n, err := w.Write(data)
		return int64(n), err
	})
	return err
}

// writeReaderStaged replaces target with the bytes read from r, applying owner.
func writeReaderStaged(target string, r io.Reader, perm os.FileMode, followLink bool, owner fileOwner) (int64, error) {
	return stagedWrite(osStagingFS{}, target, perm, followLink, owner, func(w io.Writer) (int64, error) {
		return io.Copy(w, r)
	})
}

// writeFileConfinedStaged replaces name (root-relative) with data inside root.
func writeFileConfinedStaged(root *os.Root, name string, data []byte, perm os.FileMode, owner fileOwner) error {
	_, err := stagedWrite(root, name, perm, true, owner, func(w io.Writer) (int64, error) {
		n, err := w.Write(data)
		return int64(n), err
	})
	return err
}

// writeReaderConfinedStaged replaces name (root-relative) with r inside root.
func writeReaderConfinedStaged(root *os.Root, name string, r io.Reader, perm os.FileMode, owner fileOwner) (int64, error) {
	return stagedWrite(root, name, perm, true, owner, func(w io.Writer) (int64, error) {
		return io.Copy(w, r)
	})
}
