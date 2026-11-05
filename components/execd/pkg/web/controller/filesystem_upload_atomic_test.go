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

package controller

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/iotest"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
	"github.com/stretchr/testify/require"
)

type uploadObservingReader func([]byte) (int, error)

func (f uploadObservingReader) Read(p []byte) (int, error) {
	return f(p)
}

type uploadListingFS struct {
	vfs.FS
	entries []os.DirEntry
}

func (f uploadListingFS) ReadDir(string) ([]os.DirEntry, error) {
	return f.entries, nil
}

// requireNoUploadStaging asserts the staging file of a failed upload is gone.
func requireNoUploadStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContainsf(t, entry.Name(), uploadTempPattern, "staging file %s was left behind", entry.Name())
	}
}

// A copy that fails after the destination was opened must not cost the sandbox
// the file it was replacing: the upload fails and the old content survives.
func TestWriteUploadStreamFailureKeepsTheReplacedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.md")
	original := []byte("previous content\n")
	require.NoError(t, os.WriteFile(target, original, 0o640))

	uerr := writeUploadStream(target, iotest.ErrReader(errors.New("source failed mid-copy")))

	require.NotNil(t, uerr)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, string(original), string(got))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
			"an in-place write kept the destination mode; a staged replace must keep it too")
	}
	requireNoUploadStaging(t, dir)
}

func TestWriteUploadStreamFailureLeavesNoNewFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "absent.txt")

	uerr := writeUploadStream(target, iotest.ErrReader(errors.New("source failed mid-copy")))

	require.NotNil(t, uerr)
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
	requireNoUploadStaging(t, dir)
}

func TestWriteUploadStreamReplacesContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))

	uerr := writeUploadStream(target, bytes.NewReader([]byte("new content")))

	require.Nil(t, uerr)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new content", string(got))
	requireNoUploadStaging(t, dir)
}

// A successful replacement keeps the destination mode an in-place write kept:
// the staging file starts at the create default, so only explicit preservation
// makes this pass.
func TestWriteUploadStreamPreservesDestinationMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows only models the read-only bit")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o640))

	uerr := writeUploadStream(target, bytes.NewReader([]byte("new")))

	require.Nil(t, uerr)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

// Replacing a restrictive file must not expose or let a co-tenant modify the
// in-flight content through the staging file's default permissions.
func TestWriteUploadStreamStagesRestrictiveDestinationPrivately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not model Unix permission bits")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))

	var stagingPerm os.FileMode
	found := false
	uerr := writeUploadStream(target, uploadObservingReader(func(p []byte) (int, error) {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, entry := range entries {
			if !isUploadStagingName(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			require.NoError(t, err)
			stagingPerm = info.Mode().Perm()
			found = true
			break
		}
		return copy(p, []byte("new")), io.EOF
	}))

	require.Nil(t, uerr)
	require.True(t, found, "upload did not create a staging file before reading content")
	require.Equal(t, os.FileMode(0o600), stagingPerm)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// A destination that cannot be written keeps failing: the rename alone would
// only need a writable directory.
func TestWriteUploadStreamRejectsReadOnlyDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "locked.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))
	require.NoError(t, os.Chmod(target, 0o444))
	if probe, err := os.OpenFile(target, os.O_WRONLY, 0); err == nil {
		// root can write a read-only file; the rejection is unobservable here.
		_ = probe.Close()
		t.Skip("process bypasses file permissions")
	}

	uerr := writeUploadStream(target, bytes.NewReader([]byte("new")))

	require.NotNil(t, uerr)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "old", string(got))
}

func TestWriteUploadStreamRejectsNonRegularDestination(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "a-directory")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	uerr := writeUploadStream(subdir, bytes.NewReader([]byte("new")))

	require.NotNil(t, uerr)
	info, err := os.Stat(subdir)
	require.NoError(t, err)
	require.True(t, info.IsDir(), "the destination itself must be untouched")
	requireNoUploadStaging(t, dir)
}

// An upload to a symlinked path keeps writing the link target, as an in-place
// write did, instead of replacing the link with a regular file.
func TestWriteUploadStreamFollowsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "current")
	require.NoError(t, os.WriteFile(realFile, []byte("old"), 0o644))
	if err := os.Symlink(realFile, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	uerr := writeUploadStream(link, bytes.NewReader([]byte("through the link")))

	require.Nil(t, uerr)
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "the symlink itself must survive the replace")
	got, err := os.ReadFile(realFile)
	require.NoError(t, err)
	require.Equal(t, "through the link", string(got))
}

func TestResolveUploadTargetPathRejectsSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.Symlink("second", first); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("first", second); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resolved, uerr := resolveUploadTargetPath(first)

	require.Empty(t, resolved)
	require.NotNil(t, uerr)
	require.Contains(t, uerr.message, "too many levels of symbolic links")
}

// A dangling final-component link keeps the old O_CREATE behavior: the upload
// creates the link target and leaves the link itself in place.
func TestWriteUploadStreamCreatesDanglingSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "created.txt")
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Base(target), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	uerr := writeUploadStream(link, bytes.NewReader([]byte("through the link")))

	require.Nil(t, uerr)
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "the dangling symlink must survive the upload")
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "through the link", string(got))
}

func TestIsUploadStagingName(t *testing.T) {
	require.True(t, isUploadStagingName(uploadTempPattern+"0123456789abcdef"))
	require.False(t, isUploadStagingName(uploadTempPattern+"not-a-valid-name"))
	require.False(t, isUploadStagingName("notes.md"))
}

// TestOrdinaryRoutesRejectIsolatedStagingNames covers the ordinary filesystem
// routes reaching an isolated session's workspace or a bind source by host
// path: they create no .osb-staging- files themselves, but they must not serve,
// rename or delete one another route is still writing.
func TestOrdinaryRoutesRejectIsolatedStagingNames(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, ".osb-staging-0123456789abcdef")
	require.NoError(t, os.WriteFile(staging, []byte("in flight"), 0o600))

	require.True(t, isAnyStagingName(filepath.Base(staging)))
	require.True(t, isAnyStagingName(uploadTempPattern+"0123456789abcdef"))
	require.False(t, isAnyStagingName("notes.md"))

	require.ErrorIs(t, rejectStagingPath(staging), fs.ErrNotExist)
	require.ErrorIs(t, rejectStagingPath(staging+string(os.PathSeparator)+"."), fs.ErrNotExist)
	require.ErrorIs(t, rejectStagingPath(filepath.Join(dir, uploadTempPattern+"0123456789abcdef")+string(os.PathSeparator)+"."), fs.ErrNotExist)
	require.NoError(t, rejectStagingPath(filepath.Join(dir, "notes.md")))
}

func TestListIsolatedDirHidesUploadStaging(t *testing.T) {
	dir := t.TempDir()
	visible := filepath.Join(dir, "visible.txt")
	require.NoError(t, os.WriteFile(visible, []byte("visible"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, uploadTempPattern+"0123456789abcdef"), []byte("in flight"), 0o600))
	dirEntries, err := os.ReadDir(dir)
	require.NoError(t, err)

	ctrl := &IsolatedSessionController{}
	entries, err := ctrl.listIsolatedDir(uploadListingFS{entries: dirEntries}, ".", 1)

	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, filepath.Base(visible), entries[0].Path)
}

// The multipart entry point keeps honoring the metadata path and mode.
func TestUploadFileWritesThroughMultipart(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "uploaded.txt")

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	metadata, err := writer.CreateFormFile("metadata", "metadata")
	require.NoError(t, err)
	_, err = metadata.Write([]byte(`{"path":"` + filepath.ToSlash(target) + `","mode":644}`))
	require.NoError(t, err)
	filePart, err := writer.CreateFormFile("file", "uploaded.txt")
	require.NoError(t, err)
	_, err = filePart.Write([]byte("multipart payload"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/upload", body.Bytes())
	ctrl.ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	ctrl.UploadFile()

	require.Equal(t, http.StatusOK, rec.Code)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "multipart payload", string(got))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	}
	requireNoUploadStaging(t, dir)
}

// A rename replaces the inode, so a setuid/setgid bit the old in-place write
// kept would silently disappear unless the staged file carries it too.
func TestUploadDestinationModeKeepsSpecialBits(t *testing.T) {
	mode := os.FileMode(0o750) | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	require.Equal(t, mode, uploadDestinationMode(mode))
	require.Equal(t, os.FileMode(0o640), uploadDestinationMode(0o640))
}

func TestWriteUploadStreamPreservesSpecialModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows only models the read-only bit")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "helper")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o750))
	require.NoError(t, os.Chmod(target, 0o750|os.ModeSetuid))
	before, err := os.Stat(target)
	require.NoError(t, err)
	if before.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not keep setuid bits")
	}

	uerr := writeUploadStream(target, bytes.NewReader([]byte("new")))

	require.Nil(t, uerr)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
	require.NotZero(t, info.Mode()&os.ModeSetuid, "the replacement must keep setuid")
}

// A destination the caller can write but whose directory it cannot must keep
// working: the staged path cannot create its sibling, so it falls back to the
// legacy in-place write.
func TestWriteUploadStreamFallsBackWithoutDirectoryWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not model directory write permission this way")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o666))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// root and some ACL setups still allow the staging file; then there is no
	// fallback to exercise.
	probe, probeErr := os.CreateTemp(dir, ".probe-")
	if probeErr == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("directory stays writable for this process")
	}

	uerr := writeUploadStream(target, bytes.NewReader([]byte("new")))

	require.Nil(t, uerr)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new", string(got))
	requireNoUploadStaging(t, dir)
}
