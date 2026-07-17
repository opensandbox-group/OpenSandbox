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
	"sync"
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
	mu      sync.Mutex
	inspect map[string]*types.Status
	starts  int
	stops   int
	stopErr error
	stopCh  chan string
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		inspect: make(map[string]*types.Status),
		stopCh:  make(chan string, 10),
	}
}

func (f *fakeExecutor) Start(_ context.Context, task *types.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeExecutor) Stop(_ context.Context, task *types.Task) error {
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
	if task != nil {
		f.stopCh <- task.Name
	}
	return f.stopErr
}

func (f *fakeExecutor) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeExecutor) StopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func activeTaskCount(m *taskManager) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.countActiveTasks()
}

func taskStatusSnapshot(m *taskManager, name string) (types.Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[name]
	if !ok {
		return types.Status{}, false
	}
	status := task.Status
	status.SubStatuses = append([]types.SubStatus(nil), task.Status.SubStatuses...)
	return status, true
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
	assert.Equal(t, 1, exec.StartCount(), "sync should recreate an active task whose recovered runtime state was lost")
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
	assert.Equal(t, 0, exec.StartCount(), "sync should not recreate tasks that were already completed before recovery")
	assert.Equal(t, types.TaskStateSucceeded, tasks[0].Status.State)
}

func TestTaskManager_RecoverPreservesPersistedFailedStartStatus(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	finishedAt := time.Now()
	persisted := &types.Task{
		Name: "prestart-failed-task",
		Process: &api.Process{
			Command: []string{"echo", "should-not-run"},
		},
		Status: types.Status{
			State: types.TaskStateFailed,
			SubStatuses: []types.SubStatus{{
				Reason:     types.ReasonPreStartHookFailed,
				Message:    "preStart hook failed",
				ExitCode:   1,
				FinishedAt: &finishedAt,
			}},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStatePending,
		SubStatuses: []types.SubStatus{{
			Reason: "Pending",
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	got, err := mgr.Get(ctx, persisted.Name)
	require.NoError(t, err)
	assert.Equal(t, types.TaskStateFailed, got.Status.State)
	require.NotEmpty(t, got.Status.SubStatuses)
	assert.Equal(t, types.ReasonPreStartHookFailed, got.Status.SubStatuses[0].Reason)
}

func TestTaskManager_DeleteTerminalTaskRunsPostStopBeforeFinalizing(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	now := time.Now()
	persisted := &types.Task{
		Name:              "terminal-with-poststop",
		DeletionTimestamp: &now,
		Process: &api.Process{
			Command: []string{"echo", "done"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"true"},
					},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateSucceeded,
			SubStatuses: []types.SubStatus{{
				Reason:     "Completed",
				ExitCode:   0,
				FinishedAt: &now,
			}},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStateSucceeded,
		SubStatuses: []types.SubStatus{{
			Reason:     "Completed",
			ExitCode:   0,
			FinishedAt: &now,
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	mgr.reconcileTasks(ctx)
	select {
	case name := <-exec.stopCh:
		assert.Equal(t, persisted.Name, name)
	case <-time.After(time.Second):
		t.Fatal("expected postStop to run before terminal task deletion")
	}

	require.Eventually(t, func() bool {
		mgr.reconcileTasks(ctx)
		_, err = mgr.Get(ctx, persisted.Name)
		return err != nil
	}, time.Second, 10*time.Millisecond, "task should be finalized after postStop succeeds")
	assert.Equal(t, 1, exec.StopCount(), "postStop should only run once")
}

func TestTaskManager_RetainedTerminalTaskRunsPostStopOnce(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	now := time.Now()
	persisted := &types.Task{
		Name: "retained-terminal-with-poststop",
		Process: &api.Process{
			Command: []string{"echo", "done"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"true"},
					},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateSucceeded,
			SubStatuses: []types.SubStatus{{
				Reason:     "Completed",
				ExitCode:   0,
				FinishedAt: &now,
			}},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStateSucceeded,
		SubStatuses: []types.SubStatus{{
			Reason:     "Completed",
			ExitCode:   0,
			FinishedAt: &now,
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	mgr.reconcileTasks(ctx)
	select {
	case name := <-exec.stopCh:
		assert.Equal(t, persisted.Name, name)
	case <-time.After(time.Second):
		t.Fatal("expected retained terminal task to run postStop")
	}

	require.Eventually(t, func() bool {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		got, ok := mgr.tasks[persisted.Name]
		return ok && postStopFinished(got) && !mgr.stopping[persisted.Name]
	}, time.Second, 10*time.Millisecond, "postStop completion should be persisted before the next reconcile")

	mgr.reconcileTasks(ctx)

	got, err := mgr.Get(ctx, persisted.Name)
	require.NoError(t, err, "retained terminal task should not be finalized")
	assert.True(t, postStopFinished(got))
	assert.Equal(t, 1, exec.StopCount(), "postStop should only run once")
}

func TestDecideTaskStop_TerminalTaskWithPostStop(t *testing.T) {
	task := &types.Task{
		Process: &api.Process{
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{Command: []string{"true"}},
				},
			},
		},
	}

	for _, state := range []types.TaskState{
		types.TaskStateSucceeded,
		types.TaskStateFailed,
		types.TaskStateNotFound,
	} {
		t.Run(string(state), func(t *testing.T) {
			decision := decideTaskStop(task, observedTaskStatus{
				status: types.Status{State: state},
				state:  state,
			}, false)

			assert.True(t, decision.shouldStop)
			assert.Equal(t, "terminal task completed", decision.reason)
		})
	}
}

func TestDeletingTaskWithFinishedPostStopWaitsForTerminalState(t *testing.T) {
	now := time.Now()
	task := &types.Task{
		DeletionTimestamp: &now,
		Process: &api.Process{
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{Command: []string{"true"}},
				},
			},
		},
	}
	running := observedTaskStatus{
		status: types.Status{
			State: types.TaskStateRunning,
			SubStatuses: []types.SubStatus{{
				Reason: reasonPostStopHookCompleted,
			}},
		},
		state: types.TaskStateRunning,
	}

	assert.False(t, decideTaskStop(task, running, false).shouldStop,
		"a completed postStop hook must not be executed again while runtime status converges")
	assert.False(t, shouldFinalizeTaskDeletion(task, running, false))

	terminal := running
	terminal.status.State = types.TaskStateFailed
	terminal.state = types.TaskStateFailed
	assert.False(t, decideTaskStop(task, terminal, false).shouldStop)
	assert.True(t, shouldFinalizeTaskDeletion(task, terminal, false))
}

func TestTaskManager_InspectAfterStopReturnsExecutorStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	task := &types.Task{
		Name: "poststop-inspect",
		Status: types.Status{
			State: types.TaskStateRunning,
			SubStatuses: []types.SubStatus{{
				Reason:     reasonPostStopHookCompleted,
				FinishedAt: &now,
			}},
		},
	}
	expected := &types.Status{
		State: types.TaskStateSucceeded,
		SubStatuses: []types.SubStatus{{
			Reason:     "Succeeded",
			FinishedAt: &now,
		}},
	}
	exec := newFakeExecutor()
	exec.inspect[task.Name] = expected
	mgr := &taskManager{executor: exec}

	status := mgr.inspectAfterStop(ctx, task, task.Name)

	require.NotNil(t, status)
	assert.Equal(t, *expected, *status)
}

func TestTaskManager_RetainedFailedTaskPreservesFailureAfterPostStop(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	now := time.Now()
	persisted := &types.Task{
		Name: "retained-prestart-failed-with-poststop",
		Process: &api.Process{
			Command: []string{"echo", "should-not-run"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{Command: []string{"true"}},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateFailed,
			SubStatuses: []types.SubStatus{{
				Reason:     types.ReasonPreStartHookFailed,
				Message:    "preStart hook failed",
				ExitCode:   1,
				FinishedAt: &now,
			}},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStatePending,
		SubStatuses: []types.SubStatus{{
			Reason: "Pending",
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)
	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	mgr.reconcileTasks(ctx)
	select {
	case name := <-exec.stopCh:
		assert.Equal(t, persisted.Name, name)
	case <-time.After(time.Second):
		t.Fatal("expected failed retained task to run postStop")
	}

	require.Eventually(t, func() bool {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		task, ok := mgr.tasks[persisted.Name]
		return ok && statusHasPostStopFinished(task.Status) && !mgr.stopping[persisted.Name]
	}, time.Second, 10*time.Millisecond)

	status, ok := taskStatusSnapshot(mgr, persisted.Name)
	require.True(t, ok)
	assert.Equal(t, types.TaskStateFailed, status.State)
	require.NotEmpty(t, status.SubStatuses)
	assert.Equal(t, types.ReasonPreStartHookFailed, status.SubStatuses[0].Reason)
}

func TestTaskManager_StopFailureReplacesVisibleSubStatus(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	startedAt := time.Now().Add(-time.Minute)
	persisted := &types.Task{
		Name: "timeout-poststop-fails",
		Process: &api.Process{
			Command: []string{"sleep", "30"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"false"},
					},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateTimeout,
			SubStatuses: []types.SubStatus{{
				Reason:    "TaskTimeout",
				Message:   "Task exceeded timeout",
				StartedAt: &startedAt,
			}},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.stopErr = &types.StopError{
		Reason:  types.ReasonPostStopHookFailed,
		Message: "postStop hook failed: copy failed",
	}
	exec.inspect[persisted.Name] = &persisted.Status
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	mgr.reconcileTasks(ctx)
	select {
	case <-exec.stopCh:
	case <-time.After(time.Second):
		t.Fatal("expected timeout task to be stopped")
	}

	require.Eventually(t, func() bool {
		status, ok := taskStatusSnapshot(mgr, persisted.Name)
		if !ok || status.State != types.TaskStateFailed || len(status.SubStatuses) == 0 {
			return false
		}
		return status.SubStatuses[0].Reason == types.ReasonPostStopHookFailed
	}, time.Second, 10*time.Millisecond)

	status, ok := taskStatusSnapshot(mgr, persisted.Name)
	require.True(t, ok)
	require.NotEmpty(t, status.SubStatuses)
	assert.Equal(t, 137, status.SubStatuses[0].ExitCode)
}

func TestTaskManager_RecoverPreservesPostStopCompletionMarker(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	now := time.Now()
	persisted := &types.Task{
		Name:              "poststop-completed-task",
		DeletionTimestamp: &now,
		Process: &api.Process{
			Command: []string{"echo", "done"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"true"},
					},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateSucceeded,
			SubStatuses: []types.SubStatus{
				{
					Reason:     "Completed",
					ExitCode:   0,
					FinishedAt: &now,
				},
				{
					Reason:     reasonPostStopHookCompleted,
					FinishedAt: &now,
				},
			},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStateSucceeded,
		SubStatuses: []types.SubStatus{{
			Reason:     "Completed",
			ExitCode:   0,
			FinishedAt: &now,
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	got, err := mgr.Get(ctx, persisted.Name)
	require.NoError(t, err)
	assert.True(t, postStopFinished(got), "recovery should preserve persisted postStop completion marker")

	mgr.reconcileTasks(ctx)
	_, err = mgr.Get(ctx, persisted.Name)
	assert.Error(t, err, "task should be finalized without rerunning postStop")
	assert.Equal(t, 0, exec.StopCount(), "postStop should not run again after recovery")
}

func TestTaskManager_RecoverMergesPostStopMarkerFromNonTerminalStatus(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	now := time.Now()
	persisted := &types.Task{
		Name:              "deleting-running-poststop-completed",
		DeletionTimestamp: &now,
		Process: &api.Process{
			Command: []string{"sleep", "30"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"true"},
					},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateRunning,
			SubStatuses: []types.SubStatus{
				{
					Reason: "Running",
				},
				{
					Reason:     reasonPostStopHookCompleted,
					FinishedAt: &now,
				},
			},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStateSucceeded,
		SubStatuses: []types.SubStatus{{
			Reason:     "Completed",
			ExitCode:   0,
			FinishedAt: &now,
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))

	got, err := mgr.Get(ctx, persisted.Name)
	require.NoError(t, err)
	assert.Equal(t, types.TaskStateSucceeded, got.Status.State)
	assert.True(t, postStopFinished(got), "recovery should merge postStop marker onto recovered status")

	mgr.reconcileTasks(ctx)

	_, err = mgr.Get(ctx, persisted.Name)
	assert.Error(t, err, "deleting terminal task should finalize without rerunning postStop")
	assert.Equal(t, 0, exec.StopCount(), "postStop should not run again after recovery")
}

func TestTaskManager_ReconcilePreservesPostStopFailureStatus(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		DataDir:           t.TempDir(),
		EnableSidecarMode: false,
		ReconcileInterval: time.Hour,
	}
	taskStore, err := store.NewFileStore(cfg.DataDir)
	require.NoError(t, err)

	now := time.Now()
	persisted := &types.Task{
		Name: "poststop-failed-visible",
		Process: &api.Process{
			Command: []string{"sleep", "30"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"false"},
					},
				},
			},
		},
		Status: types.Status{
			State: types.TaskStateFailed,
			SubStatuses: []types.SubStatus{{
				Reason:     types.ReasonPostStopHookFailed,
				Message:    "postStop hook failed: copy failed",
				ExitCode:   1,
				FinishedAt: &now,
			}},
		},
	}
	require.NoError(t, taskStore.Create(ctx, persisted))

	exec := newFakeExecutor()
	exec.inspect[persisted.Name] = &types.Status{
		State: types.TaskStateFailed,
		SubStatuses: []types.SubStatus{{
			Reason:     "ProcessCrashed",
			Message:    "process exited",
			ExitCode:   137,
			FinishedAt: &now,
		}},
	}
	mgrIface, err := NewTaskManager(cfg, taskStore, exec)
	require.NoError(t, err)

	mgr := mgrIface.(*taskManager)
	require.NoError(t, mgr.recoverTasks(ctx))
	mgr.reconcileTasks(ctx)

	got, err := mgr.Get(ctx, persisted.Name)
	require.NoError(t, err)
	require.NotEmpty(t, got.Status.SubStatuses)
	assert.Equal(t, types.ReasonPostStopHookFailed, got.Status.SubStatuses[0].Reason)
	assert.Contains(t, got.Status.SubStatuses[0].Message, "copy failed")
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
	activeCount := activeTaskCount(mgr.(*taskManager))
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
	activeCount = activeTaskCount(mgr.(*taskManager))
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
	activeCount = activeTaskCount(mgr.(*taskManager))
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
