// Copyright 2025 The OpenSandbox Authors
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
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// partialThenFail yields prefix and then fails, standing in for a multipart
// body whose stream breaks partway through the copy.
func partialThenFail(prefix string) io.Reader {
	return io.MultiReader(
		strings.NewReader(prefix),
		iotest.ErrReader(errors.New("simulated stream failure")),
	)
}

// A failed upload must leave a replaced destination byte-for-byte intact
// instead of truncating it first (#2432).
func TestWriteUploadStreamFailureKeepsTheReplacedFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(dest, []byte("previous content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	uerr := writeUploadStream(dest, partialThenFail("new but doomed contents\n"))
	if uerr == nil {
		t.Fatal("expected an upload error")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous content\n" {
		t.Fatalf("replaced file was damaged: want %q, got %q", "previous content\n", string(got))
	}
}

// A failed upload must not leave a new destination (or a staging file) behind.
func TestWriteUploadStreamFailureLeavesNoNewFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "created.md")

	uerr := writeUploadStream(dest, partialThenFail("doomed contents\n"))
	if uerr == nil {
		t.Fatal("expected an upload error")
	}

	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no destination, stat error: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging leftovers: %v", entries)
	}
}

// A successful upload replaces the destination content and keeps its mode.
func TestWriteUploadStreamReplacesDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(dest, []byte("old contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if uerr := writeUploadStream(dest, strings.NewReader("new contents\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new contents\n" {
		t.Fatalf("want %q, got %q", "new contents\n", string(got))
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("destination mode changed: want 0600, got %o", fi.Mode().Perm())
		}
	}
}

// An upload through a final-component symlink must replace the target, not
// the link, exactly as the in-place write did.
func TestWriteUploadStreamFollowsFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.md")
	if err := os.WriteFile(real, []byte("old contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink("real.md", link); err != nil {
		t.Fatal(err)
	}

	if uerr := writeUploadStream(link, strings.NewReader("through the link\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "through the link\n" {
		t.Fatalf("symlink target not replaced: want %q, got %q", "through the link\n", string(got))
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was clobbered: %v", err)
	}
}

// A dangling final symlink must create its target, as the old O_CREATE open
// did, instead of replacing the link itself.
func TestWriteUploadStreamDanglingSymlinkCreatesTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.md")
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink("target.md", link); err != nil {
		t.Fatal(err)
	}

	if uerr := writeUploadStream(link, strings.NewReader("created through the link\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "created through the link\n" {
		t.Fatalf("want %q, got %q", "created through the link\n", string(got))
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was clobbered: %v", err)
	}
}

// A relative symlink target must resolve against the physical directory
// that contains the link, not the lexical parent of the requested path.
// Uploading <root>/dir/file where dir -> a/b and file -> ../target.md must
// update <root>/a/target.md and must not touch <root>/target.md.
func TestWriteUploadStreamResolvesRelativeTargetThroughSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "target.md"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target.md", filepath.Join(root, "a", "b", "file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b", filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}

	if uerr := writeUploadStream(filepath.Join(root, "dir", "file"), strings.NewReader("resolved physically\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	got, err := os.ReadFile(filepath.Join(root, "a", "target.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "resolved physically\n" {
		t.Fatalf("physical target not updated: want %q, got %q", "resolved physically\n", string(got))
	}
	if _, err := os.Lstat(filepath.Join(root, "target.md")); !os.IsNotExist(err) {
		t.Fatalf("lexical sibling was created or clobbered: %v", err)
	}
}

// A directory destination keeps being rejected instead of being renamed over.
func TestWriteUploadStreamRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "sub")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	uerr := writeUploadStream(dest, strings.NewReader("nope\n"))
	if uerr == nil {
		t.Fatal("expected an upload error")
	}
	if !strings.Contains(uerr.message, "is a directory") {
		t.Fatalf("unexpected error message: %q", uerr.message)
	}
	if fi, err := os.Lstat(dest); err != nil || !fi.IsDir() {
		t.Fatalf("directory destination was clobbered: %v", err)
	}
}

// A destination this process cannot open for writing keeps being rejected,
// as the in-place write's open error did.
func TestWriteUploadStreamRejectsReadOnlyDestination(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("read-only file bits do not block root or the Windows readonly attribute mapping")
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "locked.md")
	if err := os.WriteFile(dest, []byte("previous content\n"), 0o400); err != nil {
		t.Fatal(err)
	}

	uerr := writeUploadStream(dest, strings.NewReader("nope\n"))
	if uerr == nil {
		t.Fatal("expected an upload error")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous content\n" {
		t.Fatalf("read-only destination was replaced: got %q", string(got))
	}
}

// A destination rename(2) refuses to replace - a writable single-file bind
// mount answers EBUSY/EXDEV - still receives the upload, published in place
// from the staged copy, exactly as the old O_TRUNC write served it.
func TestWriteUploadStreamFallsBackWhenRenameRefuses(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "mounted.md")
	if err := os.WriteFile(dest, []byte("old contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := renameUpload
	renameUpload = func(oldpath, newpath string) error { return errors.New("simulated EBUSY") }
	t.Cleanup(func() { renameUpload = restore })

	if uerr := writeUploadStream(dest, strings.NewReader("mounted destination contents\n")); uerr != nil {
		t.Fatalf("upload through an unrenamable destination failed: %v", uerr.message)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mounted destination contents\n" {
		t.Fatalf("destination not updated: %q", string(got))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("staging leftovers after the in-place publish: %v", entries)
	}
}
