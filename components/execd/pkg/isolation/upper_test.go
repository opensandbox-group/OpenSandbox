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
	"os"
	"path/filepath"
	"testing"
)

func TestNewUpperManager(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "isolation")

	mgr, err := NewUpperManager(root, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Root() != root {
		t.Errorf("Root() = %q, want %q", mgr.Root(), root)
	}
	if mgr.MaxBytes() != 8<<30 {
		t.Errorf("MaxBytes() = %d, want %d", mgr.MaxBytes(), 8<<30)
	}

	// Verify directory was created.
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Error("root dir not created")
	}
}

func TestNewUpperManager_EmptyRoot(t *testing.T) {
	_, err := NewUpperManager("", 0)
	if err == nil {
		t.Error("expected error for empty root")
	}
}

func TestUpperManager_Allocate(t *testing.T) {
	mgr := newTestUpperManager(t)

	id1, upper1, work1, err := mgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if id1 == "" {
		t.Error("empty session ID")
	}
	if upper1 == "" || work1 == "" {
		t.Error("empty directories")
	}

	// Verify directories exist.
	for _, p := range []string{upper1, work1} {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("directory %s not created", p)
		}
	}

	// Verify entries tracked.
	mgr.mu.Lock()
	e := mgr.entries[id1]
	mgr.mu.Unlock()
	if e == nil {
		t.Fatal("entry not tracked")
	}
	if !e.InUse {
		t.Error("entry should be InUse after allocation")
	}
}

func TestUpperManager_AllocateUnique(t *testing.T) {
	mgr := newTestUpperManager(t)

	id1, _, _, _ := mgr.Allocate()
	id2, _, _, _ := mgr.Allocate()
	if id1 == id2 {
		t.Error("session IDs should be unique")
	}
}

func TestUpperManager_Release(t *testing.T) {
	mgr := newTestUpperManager(t)
	id, upper, work, _ := mgr.Allocate()

	mgr.Release(id)

	mgr.mu.Lock()
	e := mgr.entries[id]
	mgr.mu.Unlock()
	if e.InUse {
		t.Error("entry should not be InUse after release")
	}

	// Directories should still exist (GC removes them).
	for _, p := range []string{upper, work} {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("directory %s removed prematurely", p)
		}
	}
}

func TestUpperManager_Remove(t *testing.T) {
	mgr := newTestUpperManager(t)
	id, upper, _, _ := mgr.Allocate()

	if err := mgr.Remove(id); err != nil {
		t.Fatal(err)
	}

	// Upper parent should be gone.
	upperParent := filepath.Dir(upper)
	if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
		t.Error("upper parent should be removed")
	}

	mgr.mu.Lock()
	_, ok := mgr.entries[id]
	mgr.mu.Unlock()
	if ok {
		t.Error("entry should be removed from map")
	}
}

func TestUpperManager_RemoveMissing(t *testing.T) {
	mgr := newTestUpperManager(t)
	if err := mgr.Remove("nonexistent"); err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestUpperManager_Collect(t *testing.T) {
	mgr := newTestUpperManager(t)

	id1, upper1, _, _ := mgr.Allocate()
	_, upper2, _, _ := mgr.Allocate()

	mgr.Release(id1)
	// id2 stays InUse.

	freed := mgr.Collect()
	if len(freed) != 1 {
		t.Fatalf("Collect freed %d entries, want 1", len(freed))
	}
	if freed[0] != id1 {
		t.Errorf("freed id = %q, want %q", freed[0], id1)
	}

	// Freed directory should be gone.
	upperParent1 := filepath.Dir(upper1)
	if _, err := os.Stat(upperParent1); !os.IsNotExist(err) {
		t.Error("freed upper should be removed")
	}

	// Non-freed directory should still exist.
	if _, err := os.Stat(upper2); os.IsNotExist(err) {
		t.Error("in-use upper should not be removed")
	}
}

func TestUpperManager_Usage(t *testing.T) {
	mgr := newTestUpperManager(t)

	// Empty usage.
	usage, err := mgr.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage != 0 {
		t.Errorf("empty usage = %d, want 0", usage)
	}

	// Allocate and write a file.
	_, upper, _, _ := mgr.Allocate()
	if err := os.WriteFile(filepath.Join(upper, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	usage, err = mgr.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage < 5 {
		t.Errorf("usage = %d, want at least 5", usage)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	size, err := dirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if size < 10 { // 5 + 5
		t.Errorf("dirSize = %d, want at least 10", size)
	}
}

func TestNewSessionID(t *testing.T) {
	id := newSessionID()
	if len(id) != 32 {
		t.Errorf("session ID length = %d, want 32", len(id))
	}
}

// Helpers

func newTestUpperManager(t *testing.T) *UpperManager {
	t.Helper()
	root := filepath.Join(t.TempDir(), "isolation")
	mgr, err := NewUpperManager(root, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}
