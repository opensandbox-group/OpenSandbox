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
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookedReader yields one chunk, then behaves as instructed. It stands in for a
// client that disconnects mid-copy, a short read, or an I/O error - and lets a
// test act at the moment the staging file exists, which is how the races below
// are driven.
type hookedReader struct {
	data   []byte
	err    error
	before func() // runs once, before the first chunk is delivered
	done   bool
}

func (h *hookedReader) Read(p []byte) (int, error) {
	if h.done {
		if h.err == nil {
			return 0, io.EOF
		}
		return 0, h.err
	}
	h.done = true
	if h.before != nil {
		h.before()
	}
	if h.data == nil {
		h.data = []byte("partial")
	}
	return copy(p, h.data), nil
}

// TestMergedView_WriteFileReaderFailureKeepsTheReplacedFile is the regression
// for the isolated-session file API: the upload used to open the destination
// with O_TRUNC, so a copy that failed partway left the file it was replacing
// empty or half-written.
func TestMergedView_WriteFileReaderFailureKeepsTheReplacedFile(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	const previous = "previous content\n"
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, "notes.md"), []byte(previous), 0o644))

	boom := errors.New("simulated read failure")
	_, err := mv.WriteFileReader("notes.md", &hookedReader{data: []byte("partial"), err: boom}, 0o644)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)

	data, readErr := os.ReadFile(filepath.Join(mv.UpperDir, "notes.md"))
	require.NoError(t, readErr)
	assert.Equal(t, previous, string(data), "a failed write must leave the replaced file intact")
}

// TestMergedView_WriteFileReaderFailureLeavesNoNewFile covers the other half:
// a destination that did not exist before must not be left behind as an empty
// or partial file.
func TestMergedView_WriteFileReaderFailureLeavesNoNewFile(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	boom := errors.New("simulated read failure")
	_, err := mv.WriteFileReader("fresh.md", &hookedReader{data: []byte("partial"), err: boom}, 0o644)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(mv.UpperDir, "fresh.md"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a failed write must not create the destination")
}

// TestMergedView_WriteFileReaderNoStagingLeftBehind asserts no staging file
// survives either path, so a workspace does not accumulate hidden artifacts.
func TestMergedView_WriteFileReaderNoStagingLeftBehind(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	_, err := mv.WriteFileReader("ok.md", strings.NewReader("content"), 0o644)
	if runtime.GOOS != "windows" {
		// chown is unsupported on Windows; the staging cleanup still holds.
		require.NoError(t, err)
	}

	boom := errors.New("simulated read failure")
	_, err = mv.WriteFileReader("bad.md", &hookedReader{data: []byte("x"), err: boom}, 0o644)
	require.Error(t, err)

	entries, err := os.ReadDir(mv.UpperDir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, isStagingName(entry.Name()), "staging file left behind: %s", entry.Name())
	}
}

// TestMergedView_WriteFilePreservesExistingMode keeps the permission an
// in-place truncating write preserved for free.
func TestMergedView_WriteFilePreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	mv := newTestMergedView(t, WorkspaceRW)

	target := filepath.Join(mv.UpperDir, "script.sh")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Chmod(target, 0o755))

	// WriteFile takes the caller's perm, so pass the destination's own mode as
	// the isolated-session controller does after stat'ing it.
	require.NoError(t, mv.WriteFile("script.sh", []byte("#!/bin/sh\necho hi\n"), 0o755))

	info, statErr := os.Stat(target)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// TestMergedView_StagingNamesHiddenFromListings ensures an in-flight or
// crashed staging file is not surfaced by the directory and search APIs.
func TestMergedView_StagingNamesHiddenFromListings(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)

	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, "visible.txt"), []byte("v"), 0o644))
	stagingName := stagingPrefix + strings.Repeat("a", stagingSuffixLen)
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, stagingName), []byte("s"), 0o600))

	entries, err := mv.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Contains(t, names, "visible.txt")
	assert.NotContains(t, names, stagingName)

	results, err := mv.Search(".", "*")
	require.NoError(t, err)
	for _, result := range results {
		assert.NotEqual(t, stagingName, filepath.Base(result), "staging file leaked into search")
	}
}

// TestStagedWriteRejectsNonRegularDestination keeps the rejection an in-place
// write had: a directory must not be replaced by a regular file.
func TestStagedWriteRejectsNonRegularDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(target, 0o755))

	err := writeFileStaged(target, []byte("x"), 0o644, false, fileOwner{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotRegularFile)

	info, statErr := os.Stat(target)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir(), "the directory must survive a rejected write")
}

// TestStagedWriteNewFileUsesRequestedMode keeps the mode a plain create used.
func TestStagedWriteNewFileUsesRequestedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "new.txt")

	require.NoError(t, writeFileStaged(target, []byte("data"), 0o640, false, fileOwner{}))

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

// TestStagedWriteFollowsFinalSymlink keeps the bind/confined view semantics: a
// symlinked destination is written through, not replaced by a regular file.
func TestStagedWriteFollowsFinalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, writeFileStaged(link, []byte("new"), 0o644, true, fileOwner{}))

	linkInfo, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink, "the symlink must survive")

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data), "the link target must receive the content")
}

// TestStagedWriteCreatesRestrictiveMode covers the other half of retaining the
// create descriptor: reopening the staging name for writing would apply the
// final mode, so creating a new 0444 or 0000 file would fail where the plain
// create succeeded.
func TestStagedWriteCreatesRestrictiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()

	for _, perm := range []os.FileMode{0o444, 0o000} {
		target := filepath.Join(dir, fmt.Sprintf("new-%o.txt", perm))
		require.NoError(t, writeFileStaged(target, []byte("data"), perm, false, fileOwner{}), "perm %o", perm)

		info, err := os.Stat(target)
		require.NoError(t, err)
		assert.Equal(t, perm, info.Mode().Perm())
	}
}

var _ io.Reader = (*hookedReader)(nil)

// TestStagedWriteCleanupChecksOwnershipWhileTheDescriptorIsOpen pins the order
// of the cleanup: the ownership check has to run while the descriptor still
// holds the staging inode open. Closing first frees the inode number, and the
// allocator can hand it to a file the session creates in the gap, which then
// looks like ours and gets deleted.
// TestStagedWriteCleanupKeepsASessionFileOnTheStagingName covers cleanup when the
// session replaces the staging entry with a plain file (not a symlink) before the
// write fails. The ordering itself is pinned by
// TestStagedWriteCleanupDoesNotTrustARecycledInode; this one guards the outcome.
func TestStagedWriteCleanupKeepsASessionFileOnTheStagingName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode reuse is not observable this way on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dest.txt")

	var staged string
	// The session reacts to the create: it takes the staging name away and
	// puts its own file in that slot, so the inode the writer owns is no longer
	// reachable by the name.
	reader := &hookedReader{
		before: func() {
			matches, err := filepath.Glob(filepath.Join(dir, stagingPrefix+"*"))
			require.NoError(t, err)
			require.Len(t, matches, 1)
			staged = matches[0]
			require.NoError(t, os.Remove(staged))
			require.NoError(t, os.WriteFile(staged, []byte("session slot"), 0o644))
		},
		err: errors.New("simulated read failure"),
	}

	_, err := writeReaderStaged(target, reader, 0o644, false, fileOwner{})
	require.Error(t, err)

	// Whatever the inode numbers look like, the session's file must be intact.
	data, readErr := os.ReadFile(staged)
	require.NoError(t, readErr, "cleanup deleted the session's file at the staging name")
	assert.Equal(t, "session slot", string(data))
}

// TestStagedWriteRenameFailureKeepsTheInodePinned drives a real rename failure
// through stagedWrite: the message must name the destination, and on POSIX the
// descriptor must still be open when cleanup unlinks, so the check cannot be
// fooled by a recycled inode.
func TestStagedWriteRenameFailureKeepsTheInodePinned(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dest.txt")
	renameErr := errors.New("simulated rename failure")

	fsys := &recordingStagingFS{renameErr: renameErr}
	_, err := stagedWrite(fsys, target, 0o644, false, fileOwner{}, func(w io.Writer) (int64, error) {
		return io.Copy(w, strings.NewReader("payload"))
	})
	require.ErrorIs(t, err, renameErr)
	assert.NotContains(t, err.Error(), stagingPrefix, "the rename error leaked the staging path")
	assert.Contains(t, err.Error(), target)

	if runtime.GOOS == "windows" {
		// Windows closes the descriptor before the rename, so there is nothing
		// left for cleanup to unlink safely; the entry is an orphan.
		t.Skip("Windows closes the staging descriptor before the rename")
	}
	require.True(t, fsys.removeSeen, "cleanup never ran after a rename failure")
	assert.True(t, fsys.descriptorOpenAtRemove,
		"the ownership check must run while the descriptor still pins the inode")
}

// TestStagedWriteCleanupDoesNotTrustARecycledInode drives the ordering defect
// directly: the recording filesystem observes the unlink, and asserts the
// staging descriptor is still open at that moment. Closing first would free the
// inode number for a session file to take over.
func TestStagedWriteCleanupDoesNotTrustARecycledInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX inode semantics")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dest.txt")

	fsys := &recordingStagingFS{}
	_, err := stagedWrite(fsys, target, 0o644, false, fileOwner{}, func(io.Writer) (int64, error) {
		return 0, errors.New("simulated write failure")
	})
	require.Error(t, err)

	require.True(t, fsys.removeSeen, "cleanup never ran")
	assert.True(t, fsys.descriptorOpenAtRemove, "the ownership check must run while the descriptor still pins the inode")
}

// recordingStagingFS records whether the staging descriptor was still open when
// the cleanup unlinked the name, and can fail the rename.
type recordingStagingFS struct {
	osStagingFS
	renameErr              error
	removeSeen             bool
	descriptorOpenAtRemove bool
	stagingFile            *os.File
}

func (r *recordingStagingFS) Rename(oldName, newName string) error {
	if r.renameErr != nil {
		return &os.LinkError{Op: "rename", Old: oldName, New: newName, Err: r.renameErr}
	}
	return r.osStagingFS.Rename(oldName, newName)
}

func (r *recordingStagingFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := r.osStagingFS.OpenFile(name, flag, perm)
	if err == nil && flag&os.O_EXCL != 0 {
		r.stagingFile = f
	}
	return f, err
}

func (r *recordingStagingFS) Remove(name string) error {
	if isStagingName(filepath.Base(name)) {
		r.removeSeen = true
		if r.stagingFile != nil {
			// A live descriptor can still stat its own inode.
			_, statErr := r.stagingFile.Stat()
			r.descriptorOpenAtRemove = statErr == nil
		}
	}
	return r.osStagingFS.Remove(name)
}

// TestStagedWriteErrorsDoNotNameTheStagingFile keeps the temp path out of API
// responses: the controller returns err.Error() verbatim, so every failure path
// has to name the destination the caller asked for.
func TestStagedWriteErrorsDoNotNameTheStagingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dest.txt")
	stagedPath := filepath.Join(dir, stagingPrefix+"0123456789abcdef")
	boom := errors.New("simulated failure")

	paths := map[string]func(io.Writer) (int64, error){
		"write": func(io.Writer) (int64, error) {
			return 0, &os.PathError{Op: "write", Path: stagedPath, Err: boom}
		},
	}
	for name, write := range paths {
		_, err := stagedWrite(osStagingFS{}, target, 0o644, false, fileOwner{}, write)
		require.ErrorIs(t, err, boom, name)
		assert.NotContains(t, err.Error(), stagingPrefix, "%s error leaked the staging path", name)
		assert.Contains(t, err.Error(), target, "%s error must name the destination", name)
	}

	// A rename failure surfaces as *os.LinkError, the most likely error and the
	// one an errors.As on *os.PathError alone would miss.
	linkErr := &os.LinkError{Op: "rename", Old: stagedPath, New: target, Err: boom}
	wrapped := writeError(target, linkErr)
	assert.ErrorIs(t, wrapped, boom)
	assert.NotContains(t, wrapped.Error(), stagingPrefix, "rename error leaked the staging path")
	assert.Contains(t, wrapped.Error(), target)
}

// TestViewsRejectStagingNamesOnRead closes the read paths: hiding a staging file
// from listings is not enough if a client can still stat or download it by name.
func TestViewsRejectStagingNamesOnRead(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	stagingName := stagingPrefix + strings.Repeat("c", stagingSuffixLen)
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, stagingName), []byte("body"), 0o600))

	_, err := mv.Stat(stagingName)
	assert.ErrorIs(t, err, os.ErrNotExist, "Stat must not reach a staging file")
	_, err = mv.ReadFile(stagingName)
	assert.ErrorIs(t, err, os.ErrNotExist, "ReadFile must not reach a staging file")
	f, err := mv.Open(stagingName)
	if err == nil {
		_ = f.Close()
		t.Fatal("Open must not reach a staging file")
	}
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestViewsRejectStagingNamesOnMutate closes the other escape: a rename would
// publish an in-flight upload under a user-visible name, and a remove would cut
// a concurrent one short.
func TestViewsRejectStagingNamesOnMutate(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	stagingName := stagingPrefix + strings.Repeat("d", stagingSuffixLen)
	require.NoError(t, os.WriteFile(filepath.Join(mv.UpperDir, stagingName), []byte("body"), 0o600))

	require.Error(t, mv.Rename(stagingName, "stolen.txt"), "Rename must not publish a staging file")
	_, statErr := os.Stat(filepath.Join(mv.UpperDir, "stolen.txt"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	require.Error(t, mv.Remove(stagingName), "Remove must not unlink a staging file")
	_, statErr = os.Stat(filepath.Join(mv.UpperDir, stagingName))
	require.NoError(t, statErr, "the staging file must survive a rejected remove")
}

// TestStagedWriteFailureDoesNotDeleteASwappedEntry covers the cleanup path: the
// payload fails after a session renamed its own file onto the staging name, and
// the deferred removal must not delete that file.
func TestStagedWriteFailureDoesNotDeleteASwappedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dest.txt")

	var staged string
	sessionFile := filepath.Join(dir, "session.bin")
	require.NoError(t, os.WriteFile(sessionFile, []byte("session data"), 0o644))

	boom := errors.New("simulated read failure")
	reader := &hookedReader{
		before: func() {
			matches, err := filepath.Glob(filepath.Join(dir, stagingPrefix+"*"))
			require.NoError(t, err)
			require.Len(t, matches, 1)
			staged = matches[0]
			// Rename one of the session's own files onto the staging name, the
			// way a workload racing the copy can.
			require.NoError(t, os.Remove(staged))
			require.NoError(t, os.Rename(sessionFile, staged))
		},
		err: boom,
	}

	_, err := writeReaderStaged(target, reader, 0o644, false, fileOwner{})
	require.ErrorIs(t, err, boom)

	data, readErr := os.ReadFile(staged)
	require.NoError(t, readErr, "the session's file at the staging name must survive the failed write")
	assert.Equal(t, "session data", string(data))
}

// TestMergedView_StagingNamesHiddenInRWMode covers the shape the runtime
// builds for rw sessions: LowerDir and UpperDir are the same directory, so the
// lower pass of a listing reads the very entries the upper pass filters.
func TestMergedView_StagingNamesHiddenInRWMode(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	resolved, err := filepath.EvalSymlinks(ws)
	require.NoError(t, err)
	mv := NewMergedView(resolved, resolved, WorkspaceRW, uint32(os.Getuid()), uint32(os.Getgid()))

	require.NoError(t, os.WriteFile(filepath.Join(resolved, "real.txt"), []byte("r"), 0o644))
	stagingName := stagingPrefix + strings.Repeat("a", stagingSuffixLen)
	require.NoError(t, os.WriteFile(filepath.Join(resolved, stagingName), []byte("s"), 0o600))

	entries, err := mv.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, isStagingName(entry.Name()), "rw listing leaked %s", entry.Name())
	}

	results, err := mv.Search(".", "*")
	require.NoError(t, err)
	for _, result := range results {
		assert.False(t, isStagingName(filepath.Base(result)), "rw search leaked %s", result)
	}
}

// TestStagedWriteInPlaceFallback covers the documented safety valve: when the
// directory is not writable but the file is, the write still succeeds in place.
// Its tradeoff - a partial failure truncates - is the documented one.
func TestStagedWriteInPlaceFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission check this test depends on")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(target, []byte("previous"), 0o666))
	require.NoError(t, os.Chmod(target, 0o666))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	require.NoError(t, writeFileStaged(target, []byte("new content"), 0o666, false, fileOwner{}))

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new content", string(data))

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o666), info.Mode().Perm(), "the in-place path keeps the destination mode")
}

// TestStagedWriteRejectsSwappedStagingEntry covers a workload that unlinks the
// staging entry mid-copy and points its name at another file. The rename must
// be refused rather than carrying the wrong inode into the destination.
//
// This is the same-entry check, not the descriptor-retention proof: a build that
// reopened the staging name by path would also fail here, because the check
// fires first. TestStagedWriteCreatesRestrictiveMode is what pins the
// descriptor down.
func TestStagedWriteRejectsSwappedStagingEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dest.txt")
	decoy := filepath.Join(dir, "decoy.txt")
	require.NoError(t, os.WriteFile(decoy, []byte("untouched"), 0o644))

	var staged string
	reader := &hookedReader{before: func() {
		matches, err := filepath.Glob(filepath.Join(dir, stagingPrefix+"*"))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		staged = matches[0]
		// A workload that can write this directory can do the same: drop the
		// staging entry and point its name at another file.
		require.NoError(t, os.Remove(staged))
		require.NoError(t, os.Symlink(decoy, staged))
	}}

	_, err := writeReaderStaged(target, reader, 0o644, false, fileOwner{})
	require.Error(t, err, "a swapped staging entry must not be renamed into place")

	data, readErr := os.ReadFile(decoy)
	require.NoError(t, readErr)
	assert.Equal(t, "untouched", string(data), "the payload must not be redirected through a swapped staging name")

	_, statErr := os.Stat(target)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the destination must not appear from a swapped staging entry")

	// The name belongs to the session now, so cleanup must leave it alone.
	linkInfo, linkErr := os.Lstat(staged)
	require.NoError(t, linkErr)
	assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink, "cleanup must not delete the entry the session put there")
}

// TestStagedWriteRejectsNonWritableDestination keeps the permission check an
// in-place write performed by opening the destination for writing. A rename
// only needs a writable directory, so without the probe a 0444 file inside a
// writable directory would silently become replaceable.
func TestStagedWriteRejectsNonWritableDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission check this test asserts")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "readonly.txt")
	require.NoError(t, os.WriteFile(target, []byte("locked"), 0o444))

	err := writeFileStaged(target, []byte("new"), 0o444, false, fileOwner{})
	require.ErrorIs(t, err, os.ErrPermission)

	data, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "locked", string(data), "a rejected write must leave the file intact")
}

// TestMergedView_WriteFilePreservesSetuid covers the chown ordering: the
// session views chown the written file, and a chown after the rename would
// clear the setuid/setgid bits the staged write is supposed to keep.
func TestMergedView_WriteFilePreservesSetuid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the setuid clearing this test depends on")
	}
	mv := newTestMergedView(t, WorkspaceRW)

	target := filepath.Join(mv.UpperDir, "helper")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Chmod(target, 0o755|os.ModeSetuid|os.ModeSetgid))

	require.NoError(t, mv.WriteFile("helper", []byte("#!/bin/sh\necho hi\n"), 0o755))

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSetuid, "setuid must survive the session chown")
	assert.NotZero(t, info.Mode()&os.ModeSetgid, "setgid must survive the session chown")
}
