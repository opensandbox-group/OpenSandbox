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

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/utils"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func setupTestExecutor(t *testing.T) (Executor, string) {
	dataDir := t.TempDir()
	cfg := &config.Config{
		DataDir:           dataDir,
		EnableSidecarMode: false,
	}
	executor, err := NewProcessExecutor(cfg)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}
	return executor, dataDir
}

func TestProcessExecutor_Lifecycle(t *testing.T) {
	// Skip if not running on Linux/Unix-like systems where sh is available
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found, skipping process executor test")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	// 1. Create a task that runs for a while
	task := &types.Task{
		Name: "long-running",
		Process: &api.Process{
			Command: []string{"/bin/sh", "-c", "sleep 10"},
		},
	}

	// Create task directory manually (normally handled by store)

	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	// 2. Start
	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for process to fully start and PID file to be written
	time.Sleep(100 * time.Millisecond)

	// 3. Inspect (Running)
	status, err := executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	if status.State != types.TaskStateRunning {
		t.Errorf("Task should be running, got: %s", status.State)
	}

	// 4. Stop
	if err := executor.Stop(ctx, task); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// 5. Inspect (Terminated)
	// Wait for exit file to be written
	time.Sleep(200 * time.Millisecond)
	status, err = executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	// sleep command killed by signal results in non-zero exit code, so it's Failed
	if status.State != types.TaskStateFailed {
		t.Errorf("Task should be failed (terminated), got: %s", status.State)
	}
}

func TestProcessExecutor_ShortLived(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	task := &types.Task{
		Name: "short-lived",
		Process: &api.Process{
			Command: []string{"echo", "done"},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for process to finish
	time.Sleep(200 * time.Millisecond)

	status, err := executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	if status.State != types.TaskStateSucceeded {
		t.Errorf("Task should be succeeded, got: %s", status.State)
	}
	assert.NotEmpty(t, status.SubStatuses)
	if status.SubStatuses[0].ExitCode != 0 {
		t.Errorf("Exit code should be 0, got %d", status.SubStatuses[0].ExitCode)
	}
}

func TestProcessExecutor_Failure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	task := &types.Task{
		Name: "failing-task",
		Process: &api.Process{
			Command: []string{"/bin/sh", "-c", "exit 1"},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	status, err := executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	if status.State != types.TaskStateFailed {
		t.Errorf("Task should be failed")
	}
	assert.NotEmpty(t, status.SubStatuses)
	if status.SubStatuses[0].ExitCode != 1 {
		t.Errorf("Exit code should be 1, got %d", status.SubStatuses[0].ExitCode)
	}
}

func TestProcessExecutor_InvalidArgs(t *testing.T) {
	exec, _ := setupTestExecutor(t)
	ctx := context.Background()

	// Nil task
	if err := exec.Start(ctx, nil); err == nil {
		t.Error("Start should fail with nil task")
	}

	// Missing process spec
	task := &types.Task{
		Name:    "invalid",
		Process: &api.Process{},
	}
	if err := exec.Start(ctx, task); err == nil {
		t.Error("Start should fail with missing process spec")
	}
}

func TestShellEscape(t *testing.T) {
	tests := []struct {
		input    []string
		expected string
	}{
		{[]string{"echo", "hello"}, "'echo' 'hello'"},
		{[]string{"echo", "hello world"}, "'echo' 'hello world'"},
		{[]string{"foo'bar"}, "'foo'\\''bar'"},
	}

	for _, tt := range tests {
		got := shellEscape(tt.input)
		if got != tt.expected {
			t.Errorf("shellEscape(%v) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestNewExecutor(t *testing.T) {
	// 1. Container mode + Host Mode
	cfg := &config.Config{}
	e, err := NewExecutor(cfg)
	if err != nil {
		t.Fatalf("NewExecutor(container) failed: %v", err)
	}
	if _, ok := e.(*compositeExecutor); !ok {
		t.Error("NewExecutor should return CompositeExecutor")
	}

	// 2. Process mode only
	cfg = &config.Config{
		DataDir: t.TempDir(),
	}
	e, err = NewExecutor(cfg)
	if err != nil {
		t.Fatalf("NewExecutor(process) failed: %v", err)
	}
	if _, ok := e.(*compositeExecutor); !ok {
		t.Error("NewExecutor should return CompositeExecutor")
	}

	// 3. Nil config
	if _, err := NewExecutor(nil); err == nil {
		t.Error("NewExecutor should fail with nil config")
	}
}

func TestProcessExecutor_EnvInheritance(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	// 1. Setup Host Environment
	expectedHostVar := "HOST_TEST_VAR=host_value"
	os.Setenv("HOST_TEST_VAR", "host_value")
	defer os.Unsetenv("HOST_TEST_VAR")

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	// 2. Define Task with Custom Env
	task := &types.Task{
		Name: "env-test",
		Process: &api.Process{
			Command: []string{"env"},
			Env: []corev1.EnvVar{
				{Name: "TASK_TEST_VAR", Value: "task_value"},
			},
		},
	}
	expectedTaskVar := "TASK_TEST_VAR=task_value"

	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	// 3. Start Task
	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// 4. Wait for completion
	time.Sleep(200 * time.Millisecond)

	status, err := executor.Inspect(ctx, task)
	assert.Nil(t, err)
	assert.Equal(t, types.TaskStateSucceeded, status.State)

	// 5. Verify Output
	stdoutPath := filepath.Join(taskDir, StdoutFile)
	output, err := os.ReadFile(stdoutPath)
	assert.Nil(t, err)
	outputStr := string(output)

	assert.Contains(t, outputStr, expectedHostVar, "Should inherit host environment variables")
	assert.Contains(t, outputStr, expectedTaskVar, "Should include task-specific environment variables")
}

func TestProcessExecutor_TimeoutDetection(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	timeoutSec := int64(2)
	task := &types.Task{
		Name: "timeout-task",
		Process: &api.Process{
			Command:        []string{"sleep", "30"},
			TimeoutSeconds: &timeoutSec,
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for timeout to be detected (2 seconds + margin)
	time.Sleep(2500 * time.Millisecond)

	status, err := executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}

	// Should detect timeout
	assert.Equal(t, types.TaskStateTimeout, status.State, "Task should be in Timeout state")
	assert.NotEmpty(t, status.SubStatuses)
	assert.Equal(t, "TaskTimeout", status.SubStatuses[0].Reason)
	assert.Contains(t, status.SubStatuses[0].Message, "timeout of 2 seconds")

	// Cleanup
	executor.Stop(ctx, task)
}

func TestProcessExecutor_TimeoutNotExceeded(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	ctx := context.Background()

	timeoutSec := int64(10)
	task := &types.Task{
		Name: "quick-task",
		Process: &api.Process{
			Command:        []string{"echo", "done"},
			TimeoutSeconds: &timeoutSec,
		},
	}
	taskDir, err := utils.SafeJoin(executor.(*processExecutor).rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for process to complete
	time.Sleep(200 * time.Millisecond)

	status, err := executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}

	// Should be Succeeded, not Timeout
	assert.Equal(t, types.TaskStateSucceeded, status.State, "Task should be Succeeded, not Timeout")
}

func TestProcessExecutor_PreStartHook(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	// Create a marker file path to verify preStart ran
	markerFile := filepath.Join(pExecutor.rootDir, "prestart-marker")

	task := &types.Task{
		Name: "prestart-test",
		Process: &api.Process{
			Command: []string{"echo", "main-process"},
			Lifecycle: &api.ProcessLifecycle{
				PreStart: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "echo prestart-executed > " + markerFile},
					},
				},
			},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	// Start should execute preStart hook first, then main process
	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for completion
	time.Sleep(300 * time.Millisecond)

	// Verify preStart hook created the marker file
	data, err := os.ReadFile(markerFile)
	assert.Nil(t, err, "preStart hook should have created marker file")
	assert.Contains(t, string(data), "prestart-executed")

	// Verify main process ran successfully
	status, err := executor.Inspect(ctx, task)
	assert.Nil(t, err)
	assert.Equal(t, types.TaskStateSucceeded, status.State)
}

func TestProcessExecutor_PreStartHookFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	task := &types.Task{
		Name: "prestart-fail-test",
		Process: &api.Process{
			Command: []string{"echo", "should-not-run"},
			Lifecycle: &api.ProcessLifecycle{
				PreStart: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "exit 1"},
					},
				},
			},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	// Start should fail because preStart hook fails
	err = executor.Start(ctx, task)
	assert.NotNil(t, err, "Start should fail when preStart hook fails")
	assert.Contains(t, err.Error(), "preStart hook failed")

	// Verify the error is a StartError with correct Reason
	var startErr *types.StartError
	assert.True(t, errors.As(err, &startErr), "error should be *types.StartError")
	assert.Equal(t, types.ReasonPreStartHookFailed, startErr.Reason)

	// Main process should not have started (no pid file)
	pidPath := filepath.Join(taskDir, PidFile)
	_, err = os.ReadFile(pidPath)
	assert.NotNil(t, err, "PID file should not exist when preStart hook fails")
}

func TestProcessExecutor_PostStopHook(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	// Create a marker file path to verify postStop ran
	markerFile := filepath.Join(pExecutor.rootDir, "poststop-marker")

	task := &types.Task{
		Name: "poststop-test",
		Process: &api.Process{
			Command: []string{"/bin/sh", "-c", "sleep 10"},
			Lifecycle: &api.ProcessLifecycle{
				PostStop: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "echo poststop-executed > " + markerFile},
					},
				},
			},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	// Start the task
	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Verify it's running
	time.Sleep(100 * time.Millisecond)
	status, err := executor.Inspect(ctx, task)
	assert.Nil(t, err)
	assert.Equal(t, types.TaskStateRunning, status.State)

	// Stop should execute postStop hook after main process stops
	if err := executor.Stop(ctx, task); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// Verify postStop hook created the marker file
	time.Sleep(200 * time.Millisecond)
	data, err := os.ReadFile(markerFile)
	assert.Nil(t, err, "postStop hook should have created marker file")
	assert.Contains(t, string(data), "poststop-executed")
}

func TestProcessExecutor_LifecycleExecModeLocal(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	// Even with EnableSidecarMode=true, ExecModeLocal should run locally
	dataDir := t.TempDir()
	cfg := &config.Config{
		DataDir:           dataDir,
		EnableSidecarMode: false, // Can't test nsenter without /proc, but verify Local works
	}
	executor, err := NewProcessExecutor(cfg)
	assert.Nil(t, err)
	ctx := context.Background()

	markerFile := filepath.Join(dataDir, "local-hook-marker")

	task := &types.Task{
		Name: "execmode-local-test",
		Process: &api.Process{
			Command: []string{"echo", "main"},
			Lifecycle: &api.ProcessLifecycle{
				PreStart: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "echo local-hook > " + markerFile},
					},
					ExecMode: api.ExecModeLocal,
				},
			},
		},
	}

	taskDir, err := utils.SafeJoin(dataDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	// Verify hook ran locally
	data, err := os.ReadFile(markerFile)
	assert.Nil(t, err)
	assert.Contains(t, string(data), "local-hook")
}

func TestProcessExecutor_NoTimeout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	// Task without timeout setting
	task := &types.Task{
		Name: "no-timeout-task",
		Process: &api.Process{
			Command: []string{"sleep", "1"},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	if err := executor.Start(ctx, task); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Inspect immediately
	status, err := executor.Inspect(ctx, task)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}

	// Should be Running, not Timeout
	assert.Equal(t, types.TaskStateRunning, status.State, "Task should be Running when no timeout is set")

	// Cleanup
	executor.Stop(ctx, task)
}

func TestProcessExecutor_LifecycleHookTimeout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	timeoutSec := int64(1)
	task := &types.Task{
		Name: "hook-timeout-test",
		Process: &api.Process{
			Command: []string{"echo", "main"},
			Lifecycle: &api.ProcessLifecycle{
				PreStart: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "sleep 30"},
					},
					TimeoutSeconds: &timeoutSec,
				},
			},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	start := time.Now()
	err = executor.Start(ctx, task)
	elapsed := time.Since(start)

	assert.NotNil(t, err, "Start should fail when preStart hook times out")
	assert.Contains(t, err.Error(), "timed out", "Error should mention timeout")
	// Should not take much longer than the 1s timeout
	assert.Less(t, elapsed, 5*time.Second, "Should fail within a reasonable time after timeout")

	// Verify it's a StartError with PreStartHookFailed reason
	var startErr *types.StartError
	assert.True(t, errors.As(err, &startErr))
	assert.Equal(t, types.ReasonPreStartHookFailed, startErr.Reason)
}

func TestProcessExecutor_LifecycleHookStderrCaptured(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	task := &types.Task{
		Name: "hook-stderr-test",
		Process: &api.Process{
			Command: []string{"echo", "main"},
			Lifecycle: &api.ProcessLifecycle{
				PreStart: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "echo 'mount error: device busy' >&2; exit 1"},
					},
				},
			},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	err = executor.Start(ctx, task)
	assert.NotNil(t, err)
	// The stderr output should be included in the error message
	assert.Contains(t, err.Error(), "mount error: device busy",
		"stderr from failed hook should be included in error message")
}

func TestProcessExecutor_LifecycleHookTimeoutZero_NoDeadline(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	executor, _ := setupTestExecutor(t)
	pExecutor := executor.(*processExecutor)
	ctx := context.Background()

	// TimeoutSeconds = 0 means no timeout — hook that finishes quickly should succeed.
	zero := int64(0)
	markerFile := filepath.Join(pExecutor.rootDir, "zero-timeout-marker")
	task := &types.Task{
		Name: "hook-zero-timeout",
		Process: &api.Process{
			Command: []string{"echo", "main"},
			Lifecycle: &api.ProcessLifecycle{
				PreStart: &api.LifecycleHandler{
					Exec: &api.ExecAction{
						Command: []string{"/bin/sh", "-c", "echo ok > " + markerFile},
					},
					TimeoutSeconds: &zero,
				},
			},
		},
	}
	taskDir, err := utils.SafeJoin(pExecutor.rootDir, task.Name)
	assert.Nil(t, err)
	os.MkdirAll(taskDir, 0755)

	assert.Nil(t, executor.Start(ctx, task))

	time.Sleep(300 * time.Millisecond)
	data, err := os.ReadFile(markerFile)
	assert.Nil(t, err)
	assert.Contains(t, string(data), "ok")
}
