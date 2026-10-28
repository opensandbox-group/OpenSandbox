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

//go:build !windows

package controller

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// A new destination must land with the umask-filtered creation mode, exactly
// as the old O_CREATE open produced it - not with the raw staging mode that
// an explicit chmod would set.
func TestWriteUploadStreamAppliesUmaskToNewDestinations(t *testing.T) {
	prev := syscall.Umask(0o022)
	defer syscall.Umask(prev)

	dir := t.TempDir()
	dest := filepath.Join(dir, "fresh.md")

	if uerr := writeUploadStream(dest, strings.NewReader("contents\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("new destination mode: want 0755 (0777 through umask 022), got %o", got)
	}
}

// A writable destination under an unwritable parent directory always worked
// with the in-place write; staging is impossible there, so the upload keeps
// the historical semantics instead of failing.
func TestWriteUploadStreamFallsBackInPlaceWhenParentUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits, so the fallback path cannot be reached")
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "shared.md")
	if err := os.WriteFile(dest, []byte("old contents\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if uerr := writeUploadStream(dest, strings.NewReader("new contents\n")); uerr != nil {
		t.Fatalf("upload that always worked now fails: %v", uerr.message)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new contents\n" {
		t.Fatalf("want %q, got %q", "new contents\n", string(got))
	}
}

// Extended attributes - the POSIX access ACL lives in one - must survive the
// publish: swapping in a new inode would drop an ACL and can silently widen
// access, while the in-place write kept it.
func TestWriteUploadStreamKeepsExtendedAttributes(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "attributes.md")
	if err := os.WriteFile(dest, []byte("old contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const name = "user.opensandbox.upload-test"
	if err := unix.Setxattr(dest, name, []byte("keep-me"), 0); err != nil {
		t.Skipf("extended attributes unsupported here: %v", err)
	}

	if uerr := writeUploadStream(dest, strings.NewReader("new contents\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	buf := make([]byte, 64)
	n, err := unix.Getxattr(dest, name, buf)
	if err != nil {
		t.Fatalf("extended attribute lost by the upload: %v", err)
	}
	if string(buf[:n]) != "keep-me" {
		t.Fatalf("extended attribute value changed: %q", string(buf[:n]))
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new contents\n" {
		t.Fatalf("want %q, got %q", "new contents\n", string(got))
	}
}

// A hard-linked destination keeps every alias in sync: a rename would repoint
// only the requested name and leave the others exposing the old bytes.
func TestWriteUploadStreamKeepsHardLinkAliases(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "shared.md")
	alias := filepath.Join(dir, "alias.md")
	if err := os.WriteFile(dest, []byte("old contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(dest, alias); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	if uerr := writeUploadStream(dest, strings.NewReader("new contents\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	got, err := os.ReadFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new contents\n" {
		t.Fatalf("hard-link alias kept stale bytes: %q", string(got))
	}
	fi, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().IsRegular() == false {
		t.Fatalf("alias stopped being a regular file: %v", fi.Mode())
	}
}

// Special files were never replaceable by a rename and keep being streamed
// into in place, including fsync failures a device reports.
func TestWriteUploadStreamWritesSpecialFiles(t *testing.T) {
	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Skip("/dev/null is unavailable")
	}

	if uerr := writeUploadStream("/dev/null", strings.NewReader("discarded\n")); uerr != nil {
		t.Fatalf("upload to /dev/null failed: %v", uerr.message)
	}
}
