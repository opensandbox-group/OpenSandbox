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
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/runtime"
	store "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/storage"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
)

const (
	maxConcurrentTasks = 1

	reasonPostStopHookCompleted = "PostStopHookCompleted"
)

type taskManager struct {
	mu    sync.RWMutex
	tasks map[string]*types.Task // name -> task

	store    store.TaskStore
	executor runtime.Executor
	config   *config.Config

	stopping map[string]bool

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewTaskManager creates a new task manager instance.
func NewTaskManager(cfg *config.Config, taskStore store.TaskStore, exec runtime.Executor) (TaskManager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if taskStore == nil {
		return nil, fmt.Errorf("task store cannot be nil")
	}
	if exec == nil {
		return nil, fmt.Errorf("executor cannot be nil")
	}

	return &taskManager{
		tasks:    make(map[string]*types.Task),
		store:    taskStore,
		executor: exec,
		config:   cfg,
		stopping: make(map[string]bool),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}, nil
}

// isTaskActive checks if the task is counting towards the concurrency limit
func (m *taskManager) isTaskActive(task *types.Task) bool {
	if task == nil {
		return false
	}
	if task.DeletionTimestamp != nil {
		return false
	}
	state := task.Status.State
	return state == types.TaskStatePending || state == types.TaskStateRunning
}

// countActiveTasks counts tasks that are active
func (m *taskManager) countActiveTasks() int {
	count := 0
	for _, task := range m.tasks {
		if m.isTaskActive(task) {
			count++
		}
	}
	return count
}

func (m *taskManager) Create(ctx context.Context, task *types.Task) (*types.Task, error) {
	if task == nil {
		return nil, fmt.Errorf("task cannot be nil")
	}
	if task.Name == "" {
		return nil, fmt.Errorf("task name cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tasks[task.Name]; exists {
		return nil, fmt.Errorf("task %s already exists", task.Name)
	}

	if m.countActiveTasks() >= maxConcurrentTasks {
		return nil, fmt.Errorf("maximum concurrent tasks (%d) reached, cannot create new task", maxConcurrentTasks)
	}

	if err := m.store.Create(ctx, task); err != nil {
		return nil, fmt.Errorf("failed to persist task: %w", err)
	}

	if err := m.executor.Start(ctx, task); err != nil {
		// Persist the task in Failed state so the scheduler can observe the failure
		// and surface it in BatchSandbox status, rather than silently discarding it.
		reason := "StartFailed"
		var startErr *types.StartError
		if errors.As(err, &startErr) {
			reason = startErr.Reason
		}
		now := time.Now()
		task.Status = types.Status{
			State: types.TaskStateFailed,
			SubStatuses: []types.SubStatus{{
				Reason:     reason,
				Message:    err.Error(),
				ExitCode:   1,
				FinishedAt: &now,
			}},
		}
		if updateErr := m.store.Update(ctx, task); updateErr != nil {
			klog.ErrorS(updateErr, "failed to persist failed task status after Start error", "name", task.Name)
		}
		m.tasks[task.Name] = task
		return nil, fmt.Errorf("failed to start task: %w", err)
	}

	if status, err := m.executor.Inspect(ctx, task); err == nil {
		task.Status = *status
		// Persist the PID and initial status
		if err := m.store.Update(ctx, task); err != nil {
			klog.ErrorS(err, "failed to persist initial task status", "name", task.Name)
		}
	} else {
		klog.ErrorS(err, "failed to inspect task after start", "name", task.Name)
	}

	if task.Status.State == "" {
		task.Status.State = types.TaskStatePending
	}

	m.tasks[task.Name] = task

	klog.InfoS("task created successfully", "name", task.Name)
	return task, nil
}

// Sync synchronizes the current task list with the desired state
func (m *taskManager) Sync(ctx context.Context, desired []*types.Task) ([]*types.Task, error) {
	if desired == nil {
		return nil, fmt.Errorf("desired task list cannot be nil")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	desiredMap := make(map[string]*types.Task)
	for _, task := range desired {
		if task != nil && task.Name != "" {
			desiredMap[task.Name] = task
		}
	}

	var syncErrors []error

	for name, task := range m.tasks {
		if _, ok := desiredMap[name]; !ok {
			if err := m.softDeleteLocked(ctx, task); err != nil {
				klog.ErrorS(err, "failed to delete task during sync", "name", name)
				syncErrors = append(syncErrors, fmt.Errorf("failed to delete task %s: %w", name, err))
			}
		}
	}

	for name, task := range desiredMap {
		if _, exists := m.tasks[name]; !exists {
			if err := m.createTaskLocked(ctx, task); err != nil {
				klog.ErrorS(err, "failed to create task during sync", "name", name)
				syncErrors = append(syncErrors, fmt.Errorf("failed to create task %s: %w", name, err))
			}
		}
	}

	if len(syncErrors) > 0 {
		return m.listTasksLocked(), errors.Join(syncErrors...)
	}
	return m.listTasksLocked(), nil
}

func (m *taskManager) Get(ctx context.Context, name string) (*types.Task, error) {
	if name == "" {
		return nil, fmt.Errorf("task name cannot be empty")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	task, exists := m.tasks[name]
	if !exists {
		return nil, fmt.Errorf("task %s not found", name)
	}

	return task, nil
}

func (m *taskManager) List(ctx context.Context) ([]*types.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.listTasksLocked(), nil
}

// Delete removes a task by marking it for deletion
func (m *taskManager) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("task name cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	task, exists := m.tasks[name]
	if !exists {
		return nil
	}

	return m.softDeleteLocked(ctx, task)
}

// softDeleteLocked marks a task for deletion
func (m *taskManager) softDeleteLocked(ctx context.Context, task *types.Task) error {
	if task.DeletionTimestamp != nil {
		return nil
	}

	now := time.Now()
	task.DeletionTimestamp = &now

	if err := m.store.Update(ctx, task); err != nil {
		return fmt.Errorf("failed to mark task for deletion: %w", err)
	}

	klog.InfoS("task marked for deletion", "name", task.Name)
	return nil
}

// Start initializes the manager, loads tasks from store, and starts the reconcile loop
func (m *taskManager) Start(ctx context.Context) {
	klog.InfoS("starting task manager")

	if err := m.recoverTasks(ctx); err != nil {
		klog.ErrorS(err, "failed to recover tasks from store")
	}

	go m.reconcileLoop(ctx)

	klog.InfoS("task manager started")
}

func (m *taskManager) Stop() {
	klog.InfoS("stopping task manager")
	close(m.stopCh)
	<-m.doneCh
	klog.InfoS("task manager stopped")
}

// createTaskLocked creates a task without acquiring the lock
func (m *taskManager) createTaskLocked(ctx context.Context, task *types.Task) error {
	if task == nil || task.Name == "" {
		return fmt.Errorf("invalid task")
	}

	if _, exists := m.tasks[task.Name]; exists {
		return fmt.Errorf("task %s already exists", task.Name)
	}

	if m.countActiveTasks() >= maxConcurrentTasks {
		return fmt.Errorf("maximum concurrent tasks (%d) reached, cannot create new task", maxConcurrentTasks)
	}

	if err := m.store.Create(ctx, task); err != nil {
		return fmt.Errorf("failed to persist task: %w", err)
	}

	if err := m.executor.Start(ctx, task); err != nil {
		reason := "StartFailed"
		var startErr *types.StartError
		if errors.As(err, &startErr) {
			reason = startErr.Reason
		}
		now := time.Now()
		task.Status = types.Status{
			State: types.TaskStateFailed,
			SubStatuses: []types.SubStatus{{
				Reason:     reason,
				Message:    err.Error(),
				ExitCode:   1,
				FinishedAt: &now,
			}},
		}
		if updateErr := m.store.Update(ctx, task); updateErr != nil {
			klog.ErrorS(updateErr, "failed to persist failed task status after Start error", "name", task.Name)
		}
		m.tasks[task.Name] = task
		return fmt.Errorf("failed to start task: %w", err)
	}

	if status, err := m.executor.Inspect(ctx, task); err == nil {
		task.Status = *status
		// Persist the PID and initial status
		if err := m.store.Update(ctx, task); err != nil {
			klog.ErrorS(err, "failed to persist initial task status", "name", task.Name)
		}
	} else {
		klog.ErrorS(err, "failed to inspect task after start", "name", task.Name)
	}

	m.tasks[task.Name] = task
	return nil
}

// listTasksLocked returns all tasks without acquiring the lock
func (m *taskManager) listTasksLocked() []*types.Task {
	tasks := make([]*types.Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		if task != nil {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (m *taskManager) recoverTasks(ctx context.Context) error {
	klog.InfoS("recovering tasks from store")

	tasks, err := m.store.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list tasks from store: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, task := range tasks {
		if task == nil {
			continue
		}

		persistedState := task.Status.State
		status, err := m.executor.Inspect(ctx, task)
		if err != nil {
			klog.ErrorS(err, "failed to inspect task during recovery", "name", task.Name)
			continue
		}
		mergedStatus := mergePostStopFinishedStatus(task.Status, *status)
		status = &mergedStatus
		if shouldPreservePersistedStatus(task.Status, *status) {
			status = &task.Status
		}

		if shouldDropRecoveredTask(task, persistedState, status.State) {
			klog.InfoS("dropping recovered task with lost active runtime state",
				"name", task.Name, "persistedState", persistedState, "recoveredState", status.State)
			if err := m.store.Delete(ctx, task.Name); err != nil {
				klog.ErrorS(err, "failed to delete stale recovered task from store", "name", task.Name)
			}
			continue
		}

		task.Status = *status

		m.tasks[task.Name] = task

		klog.InfoS("recovered task", "name", task.Name, "state", task.Status.State, "deleting", task.DeletionTimestamp != nil)
	}

	klog.InfoS("task recovery completed", "count", len(m.tasks))
	return nil
}

func shouldDropRecoveredTask(task *types.Task, persistedState, recoveredState types.TaskState) bool {
	if task == nil || task.DeletionTimestamp != nil {
		return false
	}
	if persistedState != types.TaskStatePending && persistedState != types.TaskStateRunning {
		return false
	}
	switch recoveredState {
	case types.TaskStatePending, types.TaskStateFailed, types.TaskStateNotFound:
		return true
	default:
		return false
	}
}

func (m *taskManager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(m.config.ReconcileInterval)
	defer ticker.Stop()
	defer close(m.doneCh)

	for {
		select {
		case <-ticker.C:
			m.reconcileTasks(ctx)
		case <-m.stopCh:
			klog.InfoS("reconcile loop stopped")
			return
		case <-ctx.Done():
			klog.InfoS("reconcile loop context canceled")
			return
		}
	}
}

func (m *taskManager) reconcileTasks(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var tasksToDelete []string

	for name, task := range m.tasks {
		observed, ok := m.observeTaskLocked(ctx, name, task)
		if !ok {
			continue
		}

		if decision := decideTaskStop(task, observed, m.stopping[name]); decision.shouldStop {
			m.enqueueStopLocked(ctx, task, name, observed.state, decision.reason)
		}

		if shouldFinalizeTaskDeletion(task, observed, m.stopping[name]) {
			klog.InfoS("task terminated, finalizing deletion", "name", name)
			tasksToDelete = append(tasksToDelete, name)
		}

		m.persistObservedStatusLocked(ctx, name, task, observed, m.stopping[name])
	}

	m.deleteFinalizedTasksLocked(ctx, tasksToDelete)
}

type observedTaskStatus struct {
	status types.Status
	state  types.TaskState
}

type stopDecision struct {
	shouldStop bool
	reason     string
}

func (m *taskManager) observeTaskLocked(ctx context.Context, name string, task *types.Task) (observedTaskStatus, bool) {
	if task == nil {
		return observedTaskStatus{}, false
	}

	status, err := m.executor.Inspect(ctx, task)
	if err != nil {
		klog.ErrorS(err, "failed to inspect task", "name", name)
		return observedTaskStatus{}, false
	}

	mergedStatus := mergePostStopFinishedStatus(task.Status, *status)
	state := mergedStatus.State

	// Preserve terminal status that carries scheduler-visible failure details
	// or postStop completion markers. Inspect only reflects process state, so
	// blindly replacing persisted status can hide lifecycle failures or rerun
	// postStop after recovery.
	if shouldPreservePersistedStatus(task.Status, mergedStatus) {
		mergedStatus = task.Status
		state = task.Status.State
	}

	return observedTaskStatus{
		status: mergedStatus,
		state:  state,
	}, true
}

func decideTaskStop(task *types.Task, observed observedTaskStatus, alreadyStopping bool) stopDecision {
	if alreadyStopping {
		return stopDecision{}
	}

	needsPostStop := postStopRequired(task, observed.status)
	postStopDone := hasPostStopHook(task) && !needsPostStop

	if task.DeletionTimestamp != nil {
		if postStopDone {
			return stopDecision{}
		}
		if !isTerminalState(observed.state) {
			return stopDecision{
				shouldStop: true,
				reason:     "deletion requested",
			}
		}
		if needsPostStop {
			return stopDecision{
				shouldStop: true,
				reason:     "terminal task deletion requested",
			}
		}
		return stopDecision{}
	}

	if observed.state == types.TaskStateTimeout && !postStopDone {
		return stopDecision{
			shouldStop: true,
			reason:     "timeout exceeded",
		}
	}

	if isTerminalState(observed.state) && needsPostStop {
		return stopDecision{
			shouldStop: true,
			reason:     "terminal task completed",
		}
	}

	return stopDecision{}
}

func (m *taskManager) enqueueStopLocked(ctx context.Context, task *types.Task, name string, state types.TaskState, reason string) {
	klog.InfoS("stopping task", "name", name, "reason", reason, "current_state", state)
	m.stopping[name] = true

	go func(t *types.Task, taskName, stopReason string) {
		defer func() {
			m.mu.Lock()
			delete(m.stopping, taskName)
			m.mu.Unlock()
		}()

		klog.V(1).InfoS("task stop initiated", "name", taskName, "reason", stopReason)
		if err := m.executor.Stop(ctx, t); err != nil {
			klog.ErrorS(err, "failed to stop task", "name", taskName)
			m.mu.Lock()
			m.persistStopFailureLocked(ctx, taskName, err)
			m.mu.Unlock()
		} else if hasPostStopHook(t) {
			stoppedStatus := m.inspectAfterStop(ctx, t, taskName)

			m.mu.Lock()
			m.persistPostStopSuccessLocked(ctx, taskName, stoppedStatus)
			m.mu.Unlock()
		}
		klog.InfoS("task stopped", "name", taskName)
	}(task, name, reason)
}

func (m *taskManager) inspectAfterStop(ctx context.Context, task *types.Task, name string) *types.Status {
	status, err := m.executor.Inspect(ctx, task)
	if err != nil {
		klog.ErrorS(err, "failed to inspect task after postStop", "name", name)
		return nil
	}
	if status == nil {
		return nil
	}
	return status
}

func (m *taskManager) persistStopFailureLocked(ctx context.Context, name string, err error) {
	// Extract structured reason from StopError and annotate task status.
	reason := "StopFailed"
	var stopErr *types.StopError
	if errors.As(err, &stopErr) {
		reason = stopErr.Reason
	}

	if existingTask, ok := m.tasks[name]; ok {
		now := time.Now()
		exitCode := stopFailureExitCode(existingTask.Status)
		existingTask.Status.State = types.TaskStateFailed
		existingTask.Status.SubStatuses = []types.SubStatus{{
			Reason:     reason,
			Message:    err.Error(),
			ExitCode:   exitCode,
			FinishedAt: &now,
		}}
		if updateErr := m.store.Update(ctx, existingTask); updateErr != nil {
			klog.ErrorS(updateErr, "failed to persist stop error status", "name", name)
		}
	}
}

func (m *taskManager) persistPostStopSuccessLocked(ctx context.Context, name string, stoppedStatus *types.Status) {
	existingTask, ok := m.tasks[name]
	if !ok || statusHasPostStopFinished(existingTask.Status) {
		return
	}

	now := time.Now()
	if stoppedStatus != nil {
		mergedStatus := mergePostStopFinishedStatus(existingTask.Status, *stoppedStatus)
		if shouldPreservePersistedStatus(existingTask.Status, mergedStatus) {
			mergedStatus = existingTask.Status
		}
		existingTask.Status = mergedStatus
	}
	existingTask.Status.SubStatuses = append(existingTask.Status.SubStatuses, types.SubStatus{
		Reason:     reasonPostStopHookCompleted,
		FinishedAt: &now,
	})
	if updateErr := m.store.Update(ctx, existingTask); updateErr != nil {
		klog.ErrorS(updateErr, "failed to persist postStop completion status", "name", name)
	}
}

func shouldFinalizeTaskDeletion(task *types.Task, observed observedTaskStatus, stopping bool) bool {
	return task.DeletionTimestamp != nil &&
		isTerminalState(observed.state) &&
		!stopping &&
		!postStopRequired(task, observed.status)
}

func (m *taskManager) persistObservedStatusLocked(ctx context.Context, name string, task *types.Task, observed observedTaskStatus, stopping bool) {
	if stopping || reflect.DeepEqual(task.Status, observed.status) {
		return
	}

	oldState := task.Status.State
	task.Status = observed.status
	// Log state changes only
	if oldState != observed.status.State {
		klog.InfoS("task state changed", "name", name, "oldState", oldState, "newState", observed.status.State)
	}
	if err := m.store.Update(ctx, task); err != nil {
		klog.ErrorS(err, "failed to update task status in store", "name", name)
	}
}

func (m *taskManager) deleteFinalizedTasksLocked(ctx context.Context, tasksToDelete []string) {
	for _, name := range tasksToDelete {
		if _, exists := m.tasks[name]; !exists {
			continue
		}

		if err := m.store.Delete(ctx, name); err != nil {
			klog.ErrorS(err, "failed to delete task from store", "name", name)
			continue
		}

		delete(m.tasks, name)
		delete(m.stopping, name)
		klog.InfoS("task deleted successfully", "name", name)
	}
}

// isTerminalState returns true if the task will not transition to another state
func isTerminalState(state types.TaskState) bool {
	return state == types.TaskStateSucceeded ||
		state == types.TaskStateFailed ||
		state == types.TaskStateNotFound
}

func hasPostStopHook(task *types.Task) bool {
	return task != nil &&
		task.Process != nil &&
		task.Process.Lifecycle != nil &&
		task.Process.Lifecycle.PostStop != nil
}

func postStopRequired(task *types.Task, status types.Status) bool {
	return hasPostStopHook(task) && !statusHasPostStopFinished(status)
}

func postStopFinished(task *types.Task) bool {
	if task == nil {
		return false
	}
	return statusHasPostStopFinished(task.Status)
}

func statusHasPostStopFinished(status types.Status) bool {
	for _, subStatus := range status.SubStatuses {
		if isPostStopFinishedReason(subStatus.Reason) {
			return true
		}
	}
	return false
}

func mergePostStopFinishedStatus(persisted, recovered types.Status) types.Status {
	if statusHasPostStopFinished(recovered) || !statusHasPostStopFinished(persisted) {
		return recovered
	}

	merged := recovered
	for _, subStatus := range persisted.SubStatuses {
		if isPostStopFinishedReason(subStatus.Reason) {
			merged.SubStatuses = append(merged.SubStatuses, subStatus)
		}
	}
	return merged
}

func isPostStopFinishedReason(reason string) bool {
	switch reason {
	case reasonPostStopHookCompleted, types.ReasonPostStopHookFailed:
		return true
	default:
		return false
	}
}

func shouldPreservePersistedStatus(persisted, recovered types.Status) bool {
	if !isTerminalState(persisted.State) {
		return false
	}
	if !isTerminalState(recovered.State) {
		return true
	}
	return statusHasPostStopFinished(persisted)
}

func stopFailureExitCode(status types.Status) int {
	if status.State == types.TaskStateTimeout {
		return 137
	}
	if len(status.SubStatuses) > 0 && status.SubStatuses[0].ExitCode != 0 {
		return status.SubStatuses[0].ExitCode
	}
	return 1
}
