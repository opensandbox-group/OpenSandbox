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

package manager

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/runtime"
	store "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/storage"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type fakeExecutor struct {
	inspect map[string]*types.Status
	starts  int
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{inspect: make(map[string]*types.Status)}
}

func (f *fakeExecutor) Start(_ context.Context, task *types.Task) error {
	f.starts++
	f.inspect[task.Name] = &types.Status{
		State: types.TaskStateRunning,
		SubStatuses: []types.SubStatus{{
			Reason: "Running",
		}},
	}
	return nil
}

func (f *fakeExecutor) Inspect(_ context.Context, task *types.Task) (*types.Status, error) {
	if status, ok := f.inspect[task.Name]; ok {
		return status, nil
	}
	return &types.Status{
		State: types.TaskStateFailed,
		SubStatuses: []types.SubStatus{{
			Reason:  "ProcessCrashed",
			Message: "Process exited without writing exit code",
		}},
	}, nil
}

func (f *fakeExecutor) Stop(_ context.Context, _ *types.Task) error {
	return nil
}

func setupTestManager(t *testing.T) (TaskManager, *config.Config) {
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: 100 * time.Millisecond,
	}

	taskStore, err := store.NewFileStore(cfg.DataDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	exec, err := runtime.NewProcessExecutor(cfg)
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	mgr, err := NewTaskManager(cfg, taskStore, exec)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	return mgr, cfg
}

func cleanupTask(t *testing.T, mgr TaskManager, name string) {
	ctx := context.Background()
	mgr.Delete(ctx, name)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := mgr.Get(ctx, name)
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("Task %s not deleted within timeout during cleanup", name)
}

func TestNewTaskManager(t *testing.T) {
	cfg := &config.Config{
		DataDir: t.TempDir(),
	}
	taskStore, _ := store.NewFileStore(cfg.DataDir)
	exec, _ := runtime.NewProcessExecutor(cfg)

	tests := []struct {
		name     string
		cfg      *config.Config
		store    store.TaskStore
		executor runtime.Executor
		wantErr  bool
	}{
		{
			name:     "nil config",
			cfg:      nil,
			store:    taskStore,
			executor: exec,
			wantErr:  true,
		},
		{
			name:     "nil store",
			cfg:      cfg,
			store:    nil,
			executor: exec,
			wantErr:  true,
		},
		{
			name:     "nil executor",
			cfg:      cfg,
			store:    taskStore,
			executor: nil,
			wantErr:  true,
		},
		{
			name:     "valid parameters",
			cfg:      cfg,
			store:    taskStore,
			executor: exec,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := NewTaskManager(tt.cfg, tt.store, tt.executor)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewTaskManager() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && mgr == nil {
				t.Error("NewTaskManager() returned nil manager")
			}
		})
	}
}

func TestTaskManager_Create(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		task    *types.Task
		wantErr bool
	}{
		{
			name:    "nil task",
			task:    nil,
			wantErr: true,
		},
		{
			name: "empty task name",
			task: &types.Task{
				Name: "",
				Process: &api.Process{
					Command: []string{"echo", "test"},
				},
			},
			wantErr: true,
		},
		{
			name: "valid task",
			task: &types.Task{
				Name: "test-task",
				Process: &api.Process{
					Command: []string{"sh", "-c", "echo hello && exit 0"},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			created, err := mgr.Create(ctx, tt.task)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if created == nil {
					t.Error("Create() returned nil task")
				}
				if created != nil && created.Name != tt.task.Name {
					t.Errorf("Create() task name = %v, want %v", created.Name, tt.task.Name)
				}

				// Wait for task to complete naturally
				time.Sleep(200 * time.Millisecond)
				// Then clean up
				if tt.task != nil {
					mgr.Delete(ctx, tt.task.Name)
				}
			}
		})
	}
}

func TestTaskManager_CreateDuplicate(t *testing.T) {
	mgr, _ := setupTestManager(t)
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	task := &types.Task{
		Name: "duplicate-task",
		Process: &api.Process{
			Command: []string{"echo", "test"},
		},
	}

	// First create should succeed
	_, err := mgr.Create(ctx, task)
	if err != nil {
		t.Fatalf("First Create() failed: %v", err)
	}
	defer cleanupTask(t, mgr, task.Name)

	// Second create should fail
	_, err = mgr.Create(ctx, task)
	if err == nil {
		t.Error("Create() should fail for duplicate task")
	}
}

func TestTaskManager_CreateMaxConcurrentTasks(t *testing.T) {
	mgr, _ := setupTestManager(t)
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	task1 := &types.Task{
		Name: "task-1",
		Process: &api.Process{
			Command: []string{"sleep", "10"},
		},
	}

	// Create first task
	_, err := mgr.Create(ctx, task1)
	if err != nil {
		t.Fatalf("First Create() failed: %v", err)
	}
	defer cleanupTask(t, mgr, task1.Name)

	// Try to create second task - should fail due to max concurrent limit
	task2 := &types.Task{
		Name: "task-2",
		Process: &api.Process{
			Command: []string{"echo", "test"},
		},
	}

	_, err = mgr.Create(ctx, task2)
	if err == nil {
		t.Error("Create() should fail when max concurrent tasks reached")
		cleanupTask(t, mgr, task2.Name)
	}
}

func TestTaskManager_Get(t *testing.T) {
	mgr, _ := setupTestManager(t)
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	task := &types.Task{
		Name: "get-task",
		Process: &api.Process{
			Command: []string{"echo", "get"},
		},
	}

	// Create task
	_, err := mgr.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	defer cleanupTask(t, mgr, task.Name)

	// Get task
	got, err := mgr.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if got.Name != task.Name {
		t.Errorf("Get() name = %v, want %v", got.Name, task.Name)
	}
}

func TestTaskManager_GetNotFound(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	_, err := mgr.Get(ctx, "non-existent")
	if err == nil {
		t.Error("Get() should fail for non-existent task")
	}
}

func TestTaskManager_GetEmptyName(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	_, err := mgr.Get(ctx, "")
	if err == nil {
		t.Error("Get() should fail for empty name")
	}
}

func TestTaskManager_List(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	// Initially empty
	tasks, err := mgr.List(ctx)
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("List() initial count = %d, want 0", len(tasks))
	}

	// Create a task
	task := &types.Task{
		Name: "list-task",
		Process: &api.Process{
			Command: []string{"echo", "list"},
		},
	}

	_, err = mgr.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	defer mgr.Delete(ctx, task.Name)

	// List should return 1 task
	tasks, err = mgr.List(ctx)
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("List() count = %d, want 1", len(tasks))
	}
	if tasks[0].Name != task.Name {
		t.Errorf("List() task name = %v, want %v", tasks[0].Name, task.Name)
	}
}

func TestTaskManager_Delete(t *testing.T) {
	mgr, _ := setupTestManager(t)
	// Start the manager to enable the reconcile loop
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	task := &types.Task{
		Name: "delete-task",
		Process: &api.Process{
			Command: []string{"echo", "delete"},
		},
	}

	// Create task
	_, err := mgr.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	// Delete task (soft delete)
	err = mgr.Delete(ctx, task.Name)
	if err != nil {
		t.Errorf("Delete() failed: %v", err)
	}

	// Verify task is marked for deletion but still exists
	got, err := mgr.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get() should succeed after Delete() (soft delete): %v", err)
	}
	if got.DeletionTimestamp == nil {
		t.Error("DeletionTimestamp should be set after Delete()")
	}

	// Wait for task to be finalized
	timeout := 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := mgr.Get(ctx, task.Name)
		if err != nil {
			// Task is gone, success
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("Task was not finalized (deleted) within timeout")
}

func TestTaskManager_DeleteNonExistent(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	// Delete non-existent task should not error
	err := mgr.Delete(ctx, "non-existent")
	if err != nil {
		t.Errorf("Delete() should not fail for non-existent task: %v", err)
	}
}

func TestTaskManager_SyncRestartsRecoveredActiveTaskWhenRuntimeStateIsLost(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	persisted := &types.Task{
		Name: "resume-task",
		Process: &api.Process{
			Command: []string{"sleep", "3600"},
		},
		Status: types.Status{
			State: types.TaskStateRunning,
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	tasks, err := mgr.Sync(ctx, []*types.Task{{
		Name: "resume-task",
		Process: &api.Process{
			Command: []string{"sleep", "3600"},
		},
	}})
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, 1, exec.starts, "sync should recreate an active task whose recovered runtime state was lost")
	assert.Equal(t, types.TaskStateRunning, tasks[0].Status.State)
}

func TestTaskManager_SyncKeepsRecoveredSucceededTask(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	persisted := &types.Task{
		Name: "completed-task",
		Process: &api.Process{
			Command: []string{"echo", "done"},
		},
		Status: types.Status{
			State: types.TaskStateSucceeded,
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect["completed-task"] = &types.Status{
		State: types.TaskStateSucceeded,
		SubStatuses: []types.SubStatus{{
			Reason: "Succeeded",
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	tasks, err := mgr.Sync(ctx, []*types.Task{{
		Name: "completed-task",
		Process: &api.Process{
			Command: []string{"echo", "done"},
		},
	}})
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, 0, exec.starts, "sync should not recreate tasks that were already completed before recovery")
	assert.Equal(t, types.TaskStateSucceeded, tasks[0].Status.State)
}

func TestTaskManager_Sync(t *testing.T) {
	mgr, _ := setupTestManager(t)
	// Start the manager to enable the reconcile loop
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	// Create initial task
	task1 := &types.Task{
		Name: "sync-task-1",
		Process: &api.Process{
			Command: []string{"echo", "1"},
		},
	}

	_, err := mgr.Create(ctx, task1)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	// Sync with new desired state (task1 removed, task2 added)
	task2 := &types.Task{
		Name: "sync-task-2",
		Process: &api.Process{
			Command: []string{"echo", "2"},
		},
	}

	// Sync triggers soft delete for task1 and creation of task2
	current, err := mgr.Sync(ctx, []*types.Task{task2})
	if err != nil {
		t.Fatalf("Sync() failed: %v", err)
	}
	defer mgr.Delete(ctx, task2.Name)

	// Verify task1 is marked for deletion in the returned list
	var task1Found bool
	for _, t1 := range current {
		if t1.Name == task1.Name {
			task1Found = true
			if t1.DeletionTimestamp == nil {
				t.Error("task1 should be marked for deletion after Sync()")
			}
		}
	}
	if !task1Found {
		// It's possible it was deleted super fast, but unlikely
		t.Log("task1 not found in Sync result (maybe already deleted?)")
	}

	// Verify task2 is created
	var task2Found bool
	for _, t2 := range current {
		if t2.Name == task2.Name {
			task2Found = true
		}
	}
	if !task2Found {
		t.Error("task2 should be present after Sync()")
	}

	// Wait for task1 to be finalized
	timeout := 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := mgr.Get(ctx, task1.Name)
		if err != nil {
			// Task is gone, success
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("task1 should be deleted after Sync()")
}

func TestTaskManager_SyncNil(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	_, err := mgr.Sync(ctx, nil)
	if err == nil {
		t.Error("Sync() should fail for nil desired list")
	}
}

func TestTaskManager_AsyncStopOnDelete(t *testing.T) {
	mgr, _ := setupTestManager(t)
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	timeoutSec := int64(30)
	task := &types.Task{
		Name: "long-running-task",
		Process: &api.Process{
			Command:        []string{"sleep", "30"},
			TimeoutSeconds: &timeoutSec,
		},
	}

	// Create task
	created, err := mgr.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	defer cleanupTask(t, mgr, task.Name)

	// Verify task is running
	assert.Equal(t, types.TaskStateRunning, created.Status.State)

	// Record the time before delete
	beforeDelete := time.Now()

	// Delete task (should trigger async stop)
	err = mgr.Delete(ctx, task.Name)
	if err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	// Verify DeletionTimestamp is set immediately (soft delete)
	got, err := mgr.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get() after Delete failed: %v", err)
	}
	if got.DeletionTimestamp == nil {
		t.Error("DeletionTimestamp should be set immediately after Delete()")
	}

	// Verify Delete returned quickly (not blocked by Stop)
	deleteDuration := time.Since(beforeDelete)
	if deleteDuration > 500*time.Millisecond {
		t.Errorf("Delete() took too long (%v), should be fast (async stop)", deleteDuration)
	}

	// Wait for task to be finalized
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, err := mgr.Get(ctx, task.Name)
		if err != nil {
			// Task is gone, success
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("Task was not finalized within timeout after async stop")
}

func TestTaskManager_TimeoutHandling(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found, skipping timeout test")
	}

	mgr, _ := setupTestManager(t)
	mgr.Start(context.Background())
	defer mgr.Stop()

	ctx := context.Background()

	// Create task with short timeout
	timeoutSec := int64(2)
	task := &types.Task{
		Name: "timeout-task",
		Process: &api.Process{
			Command:        []string{"sleep", "30"},
			TimeoutSeconds: &timeoutSec,
		},
	}

	_, err := mgr.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	defer cleanupTask(t, mgr, task.Name)

	// Wait for timeout to be detected and async stop triggered
	time.Sleep(3 * time.Second)

	// Check task status - should be Timeout or Failed (after stop)
	got, err := mgr.Get(ctx, task.Name)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	// State should be Timeout (during stop) or Failed (after stop completes)
	if got.Status.State != types.TaskStateTimeout && got.Status.State != types.TaskStateFailed {
		t.Errorf("Expected Timeout or Failed state, got: %s", got.Status.State)
	}

	// If in Timeout state, verify reason
	if got.Status.State == types.TaskStateTimeout {
		assert.NotEmpty(t, got.Status.SubStatuses)
		assert.Equal(t, "TaskTimeout", got.Status.SubStatuses[0].Reason)
	}

	// Wait for final state
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := mgr.Get(ctx, task.Name)
		if err != nil {
			// Task was deleted, that's also acceptable
			return
		}
		if got.Status.State == types.TaskStateFailed {
			// Stop completed
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestTaskManager_CountActiveTasks(t *testing.T) {
	mgr, _ := setupTestManager(t)
	mgr.Start(context.Background())
	defer mgr.Stop()
	ctx := context.Background()

	// Initially empty
	activeCount := mgr.(*taskManager).countActiveTasks()
	if activeCount != 0 {
		t.Errorf("Initial active count = %d, want 0", activeCount)
	}

	// Create a short-lived task that will complete quickly
	task1 := &types.Task{
		Name: "quick-task-1",
		Process: &api.Process{
			Command: []string{"echo", "done"},
		},
	}
	_, err := mgr.Create(ctx, task1)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	defer mgr.Delete(ctx, task1.Name)

	// Wait for task1 to complete
	time.Sleep(500 * time.Millisecond)

	// Should have 0 active tasks after task1 completes
	activeCount = mgr.(*taskManager).countActiveTasks()
	if activeCount != 0 {
		t.Errorf("Active count after task1 completion = %d, want 0", activeCount)
	}

	// Create a running task
	task2 := &types.Task{
		Name: "active-task-2",
		Process: &api.Process{
			Command: []string{"sleep", "5"},
		},
	}
	_, err = mgr.Create(ctx, task2)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	defer mgr.Delete(ctx, task2.Name)

	// Should have 1 active task
	activeCount = mgr.(*taskManager).countActiveTasks()
	if activeCount != 1 {
		t.Errorf("Active count after create = %d, want 1", activeCount)
	}
}

func TestIsTerminalState(t *testing.T) {
	tests := []struct {
		name     string
		state    types.TaskState
		expected bool
	}{
		{"Succeeded is terminal", types.TaskStateSucceeded, true},
		{"Failed is terminal", types.TaskStateFailed, true},
		{"NotFound is terminal", types.TaskStateNotFound, true},
		{"Pending is not terminal", types.TaskStatePending, false},
		{"Running is not terminal", types.TaskStateRunning, false},
		{"Unknown is not terminal", types.TaskStateUnknown, false},
		{"Timeout is not terminal", types.TaskStateTimeout, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTerminalState(tt.state)
			if got != tt.expected {
				t.Errorf("isTerminalState(%v) = %v, want %v", tt.state, got, tt.expected)
			}
		})
	}
}
