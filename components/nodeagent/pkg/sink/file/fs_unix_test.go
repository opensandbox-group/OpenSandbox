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

//go:build linux

package file

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"golang.org/x/sys/unix"
)

func TestStructuralFileErrorsAreNonRetryable(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	directorySymlink := filepath.Join(root, "directory-link")
	if err := os.Symlink(root, directorySymlink); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "final symlink", path: symlink},
		{name: "fifo", path: fifo},
		{name: "symlinked directory", path: filepath.Join(directorySymlink, "target")},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := openNoFollow(test.path)
			if file != nil {
				_ = file.Close()
			}
			if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("error=%v retryable=%v, want permanent error containing %q", err, api.IsRetryableError(err), test.path)
			}
		})
	}

	for _, errno := range []error{unix.EACCES, unix.EPERM, unix.EROFS, unix.ENOTDIR, unix.EINVAL} {
		err := classifyPathError("open", regular, errno)
		if api.IsRetryableError(err) || !errors.Is(err, errno) {
			t.Fatalf("error=%v retryable=%v, want permanent wrapped %v", err, api.IsRetryableError(err), errno)
		}
	}
}

func TestCreateNoFollowExclusiveRejectsExistingSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "marker.tmp")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}

	file, err := createNoFollowExclusive(symlink)
	if file != nil {
		_ = file.Close()
	}
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("exclusive marker creation error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	raw, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != "unchanged" {
		t.Fatalf("symlink target changed to %q", raw)
	}
}

func TestMkdirAllNoFollowSyncsNewParentEntriesAndRetriesEINTR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first", "second")
	syncCalls := 0
	err := mkdirAllNoFollowWithSync(path, 0o700, func(fd int) error {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		syncCalls++
		if syncCalls == 1 {
			return unix.EINTR
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if syncCalls != 3 {
		t.Fatalf("sync calls=%d, want one per new directory plus one EINTR retry", syncCalls)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("created path mode=%v, want directory", info.Mode())
	}
}

func TestRenameNoReplaceRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "marker.tmp")
	if err := os.WriteFile(source, []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "target")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkedParent := filepath.Join(root, "target-link")
	if err := os.Symlink(targetDir, symlinkedParent); err != nil {
		t.Fatal(err)
	}

	err := renameNoReplace(source, filepath.Join(symlinkedParent, "marker.json"))
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("rename error=%v retryable=%v, want permanent error", err, api.IsRetryableError(err))
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source changed after rejected rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "marker.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists after rejected rename: %v", err)
	}
}

func TestRenameNoReplaceRejectsSymlinkedSourceParent(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.Mkdir(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "marker.tmp")
	if err := os.WriteFile(source, []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkedParent := filepath.Join(root, "source-link")
	if err := os.Symlink(sourceDir, symlinkedParent); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "marker.json")

	err := renameNoReplace(filepath.Join(symlinkedParent, "marker.tmp"), target)
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("rename error=%v retryable=%v, want permanent error", err, api.IsRetryableError(err))
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source changed after rejected rename: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists after rejected rename: %v", err)
	}
}

func TestSyncDirRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	targetParent := filepath.Join(root, "target")
	targetDir := filepath.Join(targetParent, "data")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkedParent := filepath.Join(root, "target-link")
	if err := os.Symlink(targetParent, symlinkedParent); err != nil {
		t.Fatal(err)
	}

	err := syncDir(filepath.Join(symlinkedParent, "data"))
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("sync error=%v retryable=%v, want permanent error", err, api.IsRetryableError(err))
	}
}

func TestFamilyDirRejectsUnsafeContainer(t *testing.T) {
	_, err := familyDir(t.TempDir(), api.Resource{
		ClusterName: "cluster",
		Namespace:   "namespace",
		SandboxID:   "sandbox",
		PodUID:      "pod",
		Container:   "../../escape",
	})
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}
