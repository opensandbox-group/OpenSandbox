// Copyright 2025 Alibaba Group Holding Ltd.
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

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func TestNewFileStore(t *testing.T) {
	// Test case 1: Valid directory
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("NewFileStore failed: %v", err)
	}
	if store == nil {
		t.Fatal("NewFileStore returned nil store")
	}

	// Test case 2: Empty directory
	_, err = NewFileStore("")
	if err == nil {
		t.Fatal("NewFileStore should fail with empty dir")
	}
}

func TestFileStore_CRUD(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	ctx := context.Background()
	task := &types.Task{
		Name: "test-task",
		Process: &api.Process{
			Command: []string{"echo", "hello"},
		},
	}

	// 1. Create
	if err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify file exists
	taskDir := filepath.Join(tmpDir, task.Name)
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		t.Error("Task directory was not created")
	}

	// 2. Get
	got, err := store.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Name != task.Name {
		t.Errorf("Get returned wrong name: got %s, want %s", got.Name, task.Name)
	}

	// 3. Update
	now := time.Now()
	got.DeletionTimestamp = &now

	if err := store.Update(ctx, got); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	updated, err := store.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get after update failed: %v", err)
	}
	if updated.DeletionTimestamp == nil {
		t.Error("Update failed to persist DeletionTimestamp")
	}

	// 4. List
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("List returned %d tasks, want 1", len(tasks))
	}
	if tasks[0].Name != task.Name {
		t.Errorf("List returned wrong task: %s", tasks[0].Name)
	}

	// 5. Delete
	if err := store.Delete(ctx, task.Name); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify deletion
	if _, err := store.Get(ctx, task.Name); err == nil {
		t.Error("Get should fail after delete")
	}

	tasks, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("List returned %d tasks after delete, want 0", len(tasks))
	}

	// Verify directory gone
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Error("Task directory still exists after delete")
	}
}

func TestFileStore_EdgeCases(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewFileStore(tmpDir)
	ctx := context.Background()

	// Create with nil task
	if err := store.Create(ctx, nil); err == nil {
		t.Error("Create should fail with nil task")
	}

	// Create with empty name
	if err := store.Create(ctx, &types.Task{}); err == nil {
		t.Error("Create should fail with empty name")
	}

	// Create duplicate
	task := &types.Task{Name: "dup"}
	store.Create(ctx, task)
	if err := store.Create(ctx, task); err == nil {
		t.Error("Create should fail for duplicate task")
	}

	// Update non-existent
	if err := store.Update(ctx, &types.Task{Name: "missing"}); err == nil {
		t.Error("Update should fail for non-existent task")
	}

	// Get non-existent
	if _, err := store.Get(ctx, "missing"); err == nil {
		t.Error("Get should fail for non-existent task")
	}

	// Delete non-existent
	if err := store.Delete(ctx, "missing"); err != nil {
		t.Errorf("Delete should not fail for non-existent task, got %v", err)
	}
}

func TestFileStore_CorruptedData(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewFileStore(tmpDir)
	ctx := context.Background()

	// Manually create a corrupted task file
	taskDir := filepath.Join(tmpDir, "corrupted")
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, "task.json"), []byte("{invalid-json"), 0644)

	// List should skip corrupted task
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("List should skip corrupted task, got %d", len(tasks))
	}

	// Get should fail for corrupted task
	if _, err := store.Get(ctx, "corrupted"); err == nil {
		t.Error("Get should fail for corrupted task")
	}
}

// TestConcurrency verifies thread safety
func TestFileStore_Concurrency(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := NewFileStore(tmpDir)
	ctx := context.Background()
	taskName := "concurrent-task"

	store.Create(ctx, &types.Task{Name: taskName})

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			store.Update(ctx, &types.Task{
				Name: taskName,
				Process: &api.Process{
					Args: []string{time.Now().String()},
				},
			})
			store.Get(ctx, taskName)
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestFileStore_AtomicPublish locks in the guarantee writeTaskFile actually owes
// its callers: task.json is published by an atomic rename, so a reader never sees
// a partial file and no temp file is left behind. This is the invariant that must
// survive independently of whether the write is fsynced.
func TestFileStore_AtomicPublish(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	ctx := context.Background()
	task := &types.Task{
		Name: "atomic-task",
		Process: &api.Process{
			Command: []string{"echo", "hello"},
		},
	}

	taskDir := filepath.Join(tmpDir, task.Name)
	taskFile := filepath.Join(taskDir, "task.json")
	tmpFile := taskFile + ".tmp"

	if err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if _, err := os.Stat(taskFile); err != nil {
		t.Fatalf("task.json missing after Create: %v", err)
	}
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Error("temp file left behind after Create")
	}

	// Update must publish through the same rename, leaving no temp file either.
	now := time.Now()
	task.DeletionTimestamp = &now
	if err := store.Update(ctx, task); err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Error("temp file left behind after Update")
	}
	got, err := store.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get after update failed: %v", err)
	}
	if got.DeletionTimestamp == nil {
		t.Error("Update did not persist DeletionTimestamp")
	}

	// A temp file stranded by a writer that died mid-write must not be visible to
	// readers: they only ever resolve task.json.
	if err := os.WriteFile(tmpFile, []byte("{ truncated"), 0644); err != nil {
		t.Fatalf("failed to plant stale temp file: %v", err)
	}
	if _, err := store.Get(ctx, task.Name); err != nil {
		t.Errorf("Get must ignore a stale temp file, got: %v", err)
	}
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Name != task.Name {
		t.Errorf("List must ignore a stale temp file, got %d tasks", len(tasks))
	}
}
