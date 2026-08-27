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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"k8s.io/klog/v2"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/utils"
)

type fileStore struct {
	dataDir string
	locks   sync.Map // key: taskName, value: *sync.RWMutex
}

func NewFileStore(dataDir string) (TaskStore, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("dataDir cannot be empty")
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory %s: %w", dataDir, err)
	}

	testFile := filepath.Join(dataDir, ".test")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		return nil, fmt.Errorf("data directory %s is not writable: %w", dataDir, err)
	}
	os.Remove(testFile)

	klog.InfoS("initialized file store", "dataDir", dataDir)

	return &fileStore{
		dataDir: dataDir,
	}, nil
}

func (s *fileStore) getTaskLock(name string) *sync.RWMutex {
	val, _ := s.locks.LoadOrStore(name, &sync.RWMutex{})
	return val.(*sync.RWMutex)
}

func (s *fileStore) Create(ctx context.Context, task *types.Task) error {
	if task == nil {
		return fmt.Errorf("task cannot be nil")
	}
	if task.Name == "" {
		return fmt.Errorf("task name cannot be empty")
	}

	mu := s.getTaskLock(task.Name)
	mu.Lock()
	defer mu.Unlock()

	taskDir, err := utils.SafeJoin(s.dataDir, task.Name)
	if err != nil {
		return fmt.Errorf("invalid task name: %w", err)
	}

	if _, err := os.Stat(taskDir); err == nil {
		return fmt.Errorf("task %s already exists", task.Name)
	}

	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return fmt.Errorf("failed to create task directory: %w", err)
	}

	if err := s.writeTaskFile(taskDir, task); err != nil {
		os.RemoveAll(taskDir)
		return err
	}

	klog.InfoS("created task", "name", task.Name, "dir", taskDir)
	return nil
}

func (s *fileStore) Update(ctx context.Context, task *types.Task) error {
	if task == nil {
		return fmt.Errorf("task cannot be nil")
	}
	if task.Name == "" {
		return fmt.Errorf("task name cannot be empty")
	}

	mu := s.getTaskLock(task.Name)
	mu.Lock()
	defer mu.Unlock()

	taskDir, err := utils.SafeJoin(s.dataDir, task.Name)
	if err != nil {
		return fmt.Errorf("invalid task name: %w", err)
	}

	// Check if task exists
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		return fmt.Errorf("task %s does not exist", task.Name)
	}

	if err := s.writeTaskFile(taskDir, task); err != nil {
		return err
	}

	klog.V(2).InfoS("updated task", "name", task.Name, "state", task.Status.State)
	return nil
}

func (s *fileStore) Get(ctx context.Context, name string) (*types.Task, error) {
	if name == "" {
		return nil, fmt.Errorf("task name cannot be empty")
	}

	mu := s.getTaskLock(name)
	mu.RLock()
	defer mu.RUnlock()

	taskDir, err := utils.SafeJoin(s.dataDir, name)
	if err != nil {
		return nil, fmt.Errorf("invalid task name: %w", err)
	}

	// Check if task exists
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("task %s not found", name)
	}

	return s.readTaskFile(taskDir, name)
}

func (s *fileStore) List(ctx context.Context) ([]*types.Task, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read data directory: %w", err)
	}

	tasks := make([]*types.Task, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		taskName := entry.Name()
		taskDir, err := utils.SafeJoin(s.dataDir, taskName)
		if err != nil {
			klog.ErrorS(err, "invalid task directory, skipping", "name", taskName)
			continue
		}

		mu := s.getTaskLock(taskName)
		mu.RLock()
		task, err := s.readTaskFile(taskDir, taskName)
		mu.RUnlock()

		if err != nil {
			klog.ErrorS(err, "failed to read task, skipping", "name", taskName)
			continue
		}

		tasks = append(tasks, task)
	}

	return tasks, nil
}

func (s *fileStore) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("task name cannot be empty")
	}

	mu := s.getTaskLock(name)
	mu.Lock()
	defer mu.Unlock()

	taskDir, err := utils.SafeJoin(s.dataDir, name)
	if err != nil {
		return fmt.Errorf("invalid task name: %w", err)
	}

	// Check if task exists
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		klog.InfoS("task already deleted", "name", name)
		return nil
	}

	if err := os.RemoveAll(taskDir); err != nil {
		return fmt.Errorf("failed to delete task %s: %w", name, err)
	}

	klog.InfoS("deleted task", "name", name)
	return nil
}

func (s *fileStore) getTaskFilePath(taskDir string) string {
	return filepath.Join(taskDir, "task.json")
}

// writeTaskFile writes task data to disk atomically
func (s *fileStore) writeTaskFile(taskDir string, task *types.Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	taskFile := s.getTaskFilePath(taskDir)
	tmpFile := taskFile + ".tmp"

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Publish atomically: rename(2) is atomic, so a reader never observes a
	// partially written task file. That is the invariant callers rely on.
	//
	// Deliberately no fsync before the rename. fsync would only add durability
	// across a machine crash (power loss / kernel panic), and this runs on the
	// critical path of taskManager.Create, ahead of executor.Start -- on a
	// congested device it was measured blocking sandbox process start by more
	// than 6s, and it also ran every ReconcileInterval under the manager-wide
	// lock. That durability was not actually being obtained either: the parent
	// directory was never synced, so the rename itself was never durable. Losing
	// this file is recoverable -- the controller re-issues setTask on its next
	// reconcile. See #1613.
	if err := os.Rename(tmpFile, taskFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func (s *fileStore) readTaskFile(taskDir, taskName string) (*types.Task, error) {
	taskFile := s.getTaskFilePath(taskDir)

	data, err := os.ReadFile(taskFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read task file: %w", err)
	}

	var task types.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("failed to unmarshal task file: %w", err)
	}

	return &task, nil
}
