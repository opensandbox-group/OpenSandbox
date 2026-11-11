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
	"os"
	"path/filepath"
	"testing"
)

func TestUpperManager_PartialDeletionRecoveryAndAdmission(t *testing.T) {
	root := t.TempDir()
	owner, err := NewUpperManager(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	id, upper, work, err := owner.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(upper)
	if err := os.WriteFile(filepath.Join(work, "residue.bin"), []byte("work residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	// RemoveAll can delete upper and then fail on work, leaving a session
	// parent without the upper/ directory that the old startup predicate used
	// as its only ownership signal.
	if err := os.RemoveAll(upper); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewUpperManager(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart did not finish partial cleanup for %s: %v", parent, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".execd-cleanup", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart retained retired cleanup identity %s: %v", id, err)
	}

	freed, err := restarted.CollectWithErrors()
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 0 {
		t.Fatalf("repeated cleanup freed %v, want no work", freed)
	}

	newID, newUpper, newWork, err := restarted.Allocate()
	if err != nil {
		t.Fatalf("allocation after cleanup: %v", err)
	}
	if newID == "" || newUpper == "" || newWork == "" {
		t.Fatalf("allocation after cleanup returned empty identity: id=%q upper=%q work=%q", newID, newUpper, newWork)
	}
}

func TestUpperManager_CleanupFailureRetainsIdentityAcrossRestart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged UID for the real permission failure")
	}

	root := t.TempDir()
	owner, err := NewUpperManager(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	id, upper, work, err := owner.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(upper)
	residue := filepath.Join(work, "residue.bin")
	if err := os.WriteFile(residue, []byte("work residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(work, 0o700)
	})

	restarted, err := NewUpperManager(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected partial cleanup to remove upper, got %v", err)
	}
	if _, err := os.Stat(residue); err != nil {
		t.Fatalf("expected permission failure to retain work residue: %v", err)
	}
	restarted.mu.Lock()
	entry := restarted.entries[id]
	restarted.mu.Unlock()
	if entry == nil || entry.InUse {
		t.Fatalf("restart old-session state = %#v, want a released retry entry", entry)
	}
	if _, err := os.Stat(filepath.Join(root, ".execd-cleanup", id)); err != nil {
		t.Fatalf("partial cleanup lost durable identity: %v", err)
	}

	if err := os.Chmod(work, 0o700); err != nil {
		t.Fatal(err)
	}
	freed, err := restarted.CollectWithErrors()
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 1 || freed[0] != id {
		t.Fatalf("retry freed %v, want [%s]", freed, id)
	}
	freed, err = restarted.CollectWithErrors()
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 0 {
		t.Fatalf("repeated cleanup freed %v, want no work", freed)
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry retained old session parent %s: %v", parent, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".execd-cleanup", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry retained cleanup identity %s: %v", id, err)
	}
	restarted.mu.Lock()
	_, tracked := restarted.entries[id]
	restarted.mu.Unlock()
	if tracked {
		t.Fatal("successful cleanup retained old-session state")
	}
}

func TestUpperManager_RecordRetirementFailureRetainsReleasedState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged UID for the real permission failure")
	}

	root := t.TempDir()
	mgr, err := NewUpperManager(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	id, upper, _, err := mgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(upper)
	registry := filepath.Join(root, ".execd-cleanup")
	record := filepath.Join(registry, id)
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("allocation did not publish cleanup identity: %v", err)
	}
	if err := os.Chmod(registry, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(registry, 0o700)
	})

	if err := mgr.Remove(id); err == nil {
		t.Fatal("Remove succeeded despite cleanup-record retirement failure")
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session parent still exists after successful deletion: %v", err)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("record must survive failed retirement: %v", err)
	}
	mgr.mu.Lock()
	entry := mgr.entries[id]
	mgr.mu.Unlock()
	if entry == nil || entry.InUse {
		t.Fatalf("entry after retirement failure = %#v, want a released retry entry", entry)
	}

	freed, err := mgr.CollectWithErrors()
	if err == nil || len(freed) != 0 {
		t.Fatalf("cleanup before retirement unblock = (%v, %v), want no freed entries and an error", freed, err)
	}

	if err := os.Chmod(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	freed, err = mgr.CollectWithErrors()
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 1 || freed[0] != id {
		t.Fatalf("retry freed %v, want [%s]", freed, id)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry retained cleanup identity %s: %v", id, err)
	}
}

func TestUpperManager_PreservesUnrecordedDirectories(t *testing.T) {
	root := t.TempDir()
	sharedPayload := filepath.Join(root, "shared-data", "upper", "important.bin")
	if err := os.MkdirAll(filepath.Dir(sharedPayload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sharedPayload, []byte("unrelated data"), 0o600); err != nil {
		t.Fatal(err)
	}

	allocatorShaped := "00112233445566778899aabbccddeeff"
	allocatorPayload := filepath.Join(root, allocatorShaped, "upper", "important.bin")
	if err := os.MkdirAll(filepath.Dir(allocatorPayload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, allocatorShaped, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allocatorPayload, []byte("also unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewUpperManager(root, 1024); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sharedPayload, allocatorPayload} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrecorded data %s was removed: %v", path, err)
		}
	}
}

func TestUpperManager_RefusesTrustingWritableRegistry(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, cleanupRegistryName)
	if err := os.Mkdir(registry, 0o777); err != nil {
		t.Fatal(err)
	}
	// Mkdir is filtered through the process umask, so set the bits explicitly to
	// reproduce the pre-created group/other-writable registry from the review.
	if err := os.Chmod(registry, 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := NewUpperManager(root, 1024); err == nil {
		t.Fatal("NewUpperManager adopted a group/other-writable cleanup registry")
	}
}

func TestUpperManager_AcceptsPrivateRegistry(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, cleanupRegistryName)
	if err := os.Mkdir(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(registry, 0o700); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewUpperManager(root, 1024)
	if err != nil {
		t.Fatalf("NewUpperManager rejected a 0700 registry created by execd: %v", err)
	}
	if _, _, _, err := mgr.Allocate(); err != nil {
		t.Fatalf("Allocate on a reused private registry: %v", err)
	}
}
