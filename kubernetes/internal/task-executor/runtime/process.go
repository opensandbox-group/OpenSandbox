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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/config"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/types"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/task-executor/utils"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

const (
	ExitFile   = "exit"
	PidFile    = "pid"
	StdoutFile = "stdout.log"
	StderrFile = "stderr.log"

	lifecycleHookOutputHeadBytes = 8 * 1024
	lifecycleHookOutputTailBytes = 8 * 1024
	lifecycleHookOutputMarker    = "\n... output truncated; showing first 8 KiB and last 8 KiB ...\n"
)

// processExecutor handles both Host and Sidecar modes as they share the same
// shim-based process execution model.
type processExecutor struct {
	config  *config.Config
	rootDir string
}

func NewProcessExecutor(config *config.Config) (Executor, error) {
	return &processExecutor{rootDir: config.DataDir, config: config}, nil
}

func (e *processExecutor) useNsenterForProcess(process *api.Process) bool {
	if process != nil {
		switch process.ExecMode {
		case api.ExecModeLocal:
			return false
		case api.ExecModeRemote:
			return true
		}
	}
	return e.config != nil && e.config.EnableSidecarMode
}

func (e *processExecutor) Start(ctx context.Context, task *types.Task) error {
	if task == nil {
		return fmt.Errorf("task cannot be nil")
	}

	// Execute preStart lifecycle hook if present
	if task.Process != nil && task.Process.Lifecycle != nil && task.Process.Lifecycle.PreStart != nil {
		klog.InfoS("Executing preStart lifecycle hook", "task", task.Name)
		if err := e.execLifecycleHook(ctx, task, task.Process.Lifecycle.PreStart); err != nil {
			return &types.StartError{
				Reason:  types.ReasonPreStartHookFailed,
				Message: fmt.Sprintf("preStart hook failed: %v", err),
			}
		}
		klog.InfoS("preStart lifecycle hook completed", "task", task.Name)
	}

	taskDir, err := utils.SafeJoin(e.rootDir, task.Name)
	if err != nil {
		return fmt.Errorf("invalid task name: %w", err)
	}
	pidPath := filepath.Join(taskDir, PidFile)
	exitPath := filepath.Join(taskDir, ExitFile)

	var cmdList []string
	if task.Process != nil {
		cmdList = append(task.Process.Command, task.Process.Args...)
	} else {
		return fmt.Errorf("process spec is required for process executor but task.Process is nil (task name: %s)", task.Name)
	}

	if len(cmdList) == 0 {
		return fmt.Errorf("no command specified in process spec (task name: %s)", task.Name)
	}

	safeCmdStr := shellEscape(cmdList)
	shimScript := e.buildShimScript(exitPath, safeCmdStr)

	var cmd *exec.Cmd

	if e.useNsenterForProcess(task.Process) {
		targetPID, err := e.findPidByEnvVar("SANDBOX_MAIN_CONTAINER", e.config.MainContainerName)
		if err != nil {
			return fmt.Errorf("failed to resolve target PID: %w", err)
		}

		targetEnv, err := getProcEnviron(targetPID)
		if err != nil {
			return fmt.Errorf("failed to read target process environment: %w", err)
		}

		nsenterArgs := []string{
			"-t", strconv.Itoa(targetPID),
			"--mount", "--uts", "--ipc", "--net", "--pid",
			"--",
			"/bin/sh", "-c", shimScript,
		}
		cmd = exec.Command("nsenter", nsenterArgs...)
		cmd.Env = targetEnv
		klog.InfoS("Starting sidecar task", "id", task.Name, "targetPID", targetPID)

	} else {
		cmd = exec.Command("/bin/sh", "-c", shimScript)
		cmd.Env = os.Environ()
		klog.InfoS("Starting host task", "name", task.Name, "cmd", safeCmdStr, "exitPath", exitPath)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	return e.executeCommand(task, cmd, pidPath)
}

// executeCommand handles log setup and process starting.
// Returns *types.StartError with ReasonProcessStartFailed on failure.
func (e *processExecutor) executeCommand(task *types.Task, cmd *exec.Cmd, pidPath string) error {
	if task == nil || cmd == nil {
		return fmt.Errorf("task and cmd cannot be nil")
	}

	taskDir, err := utils.SafeJoin(e.rootDir, task.Name)
	if err != nil {
		return fmt.Errorf("invalid task name: %w", err)
	}

	stdoutPath := filepath.Join(taskDir, StdoutFile)
	stderrPath := filepath.Join(taskDir, StderrFile)

	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open stdout: %w", err)
	}

	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		stdoutFile.Close()
		return fmt.Errorf("failed to open stderr: %w", err)
	}

	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	if task.Process != nil {
		for _, env := range task.Process.Env {
			if env.Name != "" {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", env.Name, env.Value))
			}
		}

		if task.Process.WorkingDir != "" {
			cmd.Dir = task.Process.WorkingDir
			klog.InfoS("Set working directory", "name", task.Name, "workingDir", task.Process.WorkingDir)
		}
	}

	if err := cmd.Start(); err != nil {
		klog.ErrorS(err, "failed to start command", "name", task.Name)
		stdoutFile.Close()
		stderrFile.Close()
		return &types.StartError{
			Reason:  types.ReasonProcessStartFailed,
			Message: fmt.Sprintf("failed to start cmd: %v", err),
		}
	}

	// Write PID to file immediately (Host-side PID)
	// This fixes the issue where sidecar tasks would write the container-internal PID
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		klog.ErrorS(err, "failed to write pid file", "name", task.Name)
		_ = cmd.Process.Kill()
		stdoutFile.Close()
		stderrFile.Close()
		return fmt.Errorf("failed to write pid file: %w", err)
	}

	klog.InfoS("Task command started successfully", "name", task.Name, "pid", pid)

	stdoutFile.Close()
	stderrFile.Close()

	go func() {
		if err := cmd.Wait(); err != nil {
			klog.ErrorS(err, "task process exited with error", "name", task.Name)
		} else {
			klog.InfoS("task process exited successfully", "name", task.Name)
		}
	}()
	return nil
}

func (e *processExecutor) buildShimScript(exitPath, cmdStr string) string {
	// The shim script acts as a mini-init process.
	// 1. It runs the user command in the background.
	// 2. It traps SIGTERM and forwards it to the child process.
	// 3. It waits for the child to exit and captures the exit code.
	// This ensures graceful shutdown propagation in sidecar/host modes.
	script := fmt.Sprintf(`
cleanup() {
    if [ -n "$CHILD_PID" ]; then
        kill -TERM "$CHILD_PID" 2>/dev/null
    fi
}
trap cleanup TERM

%s &
CHILD_PID=$!
wait "$CHILD_PID"
EXIT_CODE=$?

printf "%%d" $EXIT_CODE > %s
exit $EXIT_CODE
`, cmdStr, shellEscapePath(exitPath))
	klog.InfoS("Generated shim script", "exitPath", exitPath, "script", script)
	return script
}

func (e *processExecutor) Inspect(ctx context.Context, task *types.Task) (*types.Status, error) {
	taskDir, err := utils.SafeJoin(e.rootDir, task.Name)
	if err != nil {
		return nil, fmt.Errorf("invalid task name: %w", err)
	}
	exitPath := filepath.Join(taskDir, ExitFile)
	pidPath := filepath.Join(taskDir, PidFile)

	status := &types.Status{
		State: types.TaskStateUnknown,
	}
	subStatus := types.SubStatus{}
	var pid int
	if exitData, err := os.ReadFile(exitPath); err == nil {
		fileInfo, _ := os.Stat(exitPath)
		exitCode, _ := strconv.Atoi(string(exitData))

		subStatus.ExitCode = exitCode
		finishedAt := fileInfo.ModTime()
		subStatus.FinishedAt = &finishedAt

		if exitCode == 0 {
			status.State = types.TaskStateSucceeded
			subStatus.Reason = "Succeeded"
		} else {
			status.State = types.TaskStateFailed
			subStatus.Reason = "Failed"
		}

		if pidFileInfo, err := os.Stat(pidPath); err == nil {
			startedAt := pidFileInfo.ModTime()
			subStatus.StartedAt = &startedAt
		}

		status.SubStatuses = []types.SubStatus{subStatus}
		return status, nil
	}

	if pidData, err := os.ReadFile(pidPath); err == nil {
		pid, _ = strconv.Atoi(strings.TrimSpace(string(pidData)))
		fileInfo, _ := os.Stat(pidPath)
		startedAt := fileInfo.ModTime()
		subStatus.StartedAt = &startedAt

		if isProcessRunning(pid) {
			status.State = types.TaskStateRunning
			if task.Process != nil && task.Process.TimeoutSeconds != nil {
				timeout := time.Duration(*task.Process.TimeoutSeconds) * time.Second
				elapsed := time.Since(startedAt)
				if elapsed > timeout {
					status.State = types.TaskStateTimeout
					subStatus.Reason = "TaskTimeout"
					subStatus.Message = fmt.Sprintf("Task exceeded timeout of %d seconds", *task.Process.TimeoutSeconds)
				}
			}
		} else {
			status.State = types.TaskStateFailed
			subStatus.ExitCode = 137
			subStatus.Reason = "ProcessCrashed"
			subStatus.Message = "Process exited without writing exit code"
			subStatus.FinishedAt = &startedAt
		}
		status.SubStatuses = []types.SubStatus{subStatus}
		return status, nil
	}

	status.State = types.TaskStatePending
	subStatus.Reason = "Pending"
	status.SubStatuses = []types.SubStatus{subStatus}

	return status, nil
}

func (e *processExecutor) Stop(ctx context.Context, task *types.Task) error {
	var processErr, postStopErr error

	// Stop main process first
	if err := e.stopMainProcess(ctx, task); err != nil {
		klog.ErrorS(err, "Failed to stop main process", "task", task.Name)
		processErr = &types.StopError{
			Reason:  types.ReasonProcessStopFailed,
			Message: fmt.Sprintf("failed to stop main process: %v", err),
		}
	}

	// Execute postStop lifecycle hook if present
	if task.Process != nil && task.Process.Lifecycle != nil && task.Process.Lifecycle.PostStop != nil {
		klog.InfoS("Executing postStop lifecycle hook", "task", task.Name)
		if err := e.execLifecycleHook(ctx, task, task.Process.Lifecycle.PostStop); err != nil {
			klog.ErrorS(err, "postStop hook failed", "task", task.Name)
			postStopErr = &types.StopError{
				Reason:  types.ReasonPostStopHookFailed,
				Message: fmt.Sprintf("postStop hook failed: %v", err),
			}
		} else {
			klog.InfoS("postStop lifecycle hook completed", "task", task.Name)
		}
	}

	// Return the first error encountered (process stop takes priority)
	if processErr != nil {
		return processErr
	}
	return postStopErr
}

func (e *processExecutor) stopMainProcess(ctx context.Context, task *types.Task) error {
	taskDir, err := utils.SafeJoin(e.rootDir, task.Name)
	if err != nil {
		return fmt.Errorf("invalid task name: %w", err)
	}
	exitPath := filepath.Join(taskDir, ExitFile)
	if _, err := os.ReadFile(exitPath); err == nil {
		klog.V(1).InfoS("Skipping process signal because task already exited", "name", task.Name)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to inspect exit marker: %w", err)
	}

	pidPath := filepath.Join(taskDir, PidFile)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		return nil
	}
	var pid int
	pid, err = strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || pid == 0 {
		return nil
	}
	klog.InfoS("Read PID from pid file", "name", task.Name, "pid", pid)

	pgid := -pid

	targetPID := 0
	if e.useNsenterForProcess(task.Process) {
		children, err := getChildrenPIDs(pid)
		if err == nil && len(children) > 0 {
			targetPID = children[0]
			klog.InfoS("Sidecar mode: targeted Shim process via /proc/children", "nsenterPID", pid, "shimPID", targetPID)
		} else {
			klog.Warning("Sidecar mode: failed to find child process via /proc/children, falling back to PGID", "pid", pid, "err", err)
		}
	} else {
		targetPID = pid
	}

	killedShim := false
	if targetPID > 0 {
		if err := syscall.Kill(targetPID, syscall.SIGTERM); err == nil {
			killedShim = true
		} else if err != syscall.ESRCH {
			klog.ErrorS(err, "Failed to send SIGTERM to target process", "targetPID", targetPID)
		}
	}

	if !killedShim {
		_ = syscall.Kill(pgid, syscall.SIGTERM)
	}

	timeout := 10 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isProcessRunning(pid) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	klog.InfoS("Process did not exit after timeout, sending SIGKILL", "pgid", pgid)
	if targetPID > 0 {
		_ = syscall.Kill(targetPID, syscall.SIGKILL)
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)

	return nil
}

// getChildrenPIDs reads /proc/<pid>/task/<pid>/children to find direct children
func getChildrenPIDs(pid int) ([]int, error) {
	path := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var pids []int
	for _, field := range strings.Fields(string(data)) {
		if id, err := strconv.Atoi(field); err == nil {
			pids = append(pids, id)
		}
	}
	return pids, nil
}

func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// execLifecycleHook executes a lifecycle hook (preStart or postStop).
// It returns a descriptive error that includes the stderr output on failure.
func (e *processExecutor) execLifecycleHook(ctx context.Context, task *types.Task, hook *api.LifecycleHandler) error {
	if hook == nil || hook.Exec == nil {
		return nil
	}

	cmdList := hook.Exec.Command
	if len(cmdList) == 0 {
		return fmt.Errorf("empty command in lifecycle hook")
	}

	safeCmdStr := shellEscape(cmdList)

	// Apply per-hook timeout when specified.
	hookCtx := ctx
	if hook.TimeoutSeconds != nil && *hook.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		hookCtx, cancel = context.WithTimeout(ctx, time.Duration(*hook.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	// Lifecycle hooks default to Local so executor-only mounts remain usable
	// even when the main process runs in sidecar mode.
	useNsenter := false
	if hook.ExecMode == api.ExecModeRemote {
		useNsenter = true
	}

	var cmd *exec.Cmd
	if useNsenter {
		targetPID, err := e.findPidByEnvVar("SANDBOX_MAIN_CONTAINER", e.config.MainContainerName)
		if err != nil {
			return fmt.Errorf("failed to resolve target PID for lifecycle hook: %w", err)
		}

		targetEnv, err := getProcEnviron(targetPID)
		if err != nil {
			return fmt.Errorf("failed to read target process environment: %w", err)
		}

		nsenterArgs := []string{
			"-t", strconv.Itoa(targetPID),
			"--mount", "--uts", "--ipc", "--net", "--pid",
			"--",
			"/bin/sh", "-c", safeCmdStr,
		}
		cmd = exec.CommandContext(hookCtx, "nsenter", nsenterArgs...)
		cmd.Env = targetEnv
		klog.InfoS("Executing lifecycle hook via nsenter", "task", task.Name, "targetPID", targetPID, "cmd", safeCmdStr)
	} else {
		cmd = exec.CommandContext(hookCtx, "/bin/sh", "-c", safeCmdStr)
		cmd.Env = os.Environ()
		klog.InfoS("Executing lifecycle hook locally", "task", task.Name, "cmd", safeCmdStr)
	}

	// Create a new process group so that on timeout cancellation we can kill
	// the entire tree (shell + children like sleep). Without this,
	// Run blocks waiting for orphaned child I/O pipes to close.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second

	output := utils.NewHeadTailBuffer(
		lifecycleHookOutputHeadBytes,
		lifecycleHookOutputTailBytes,
		lifecycleHookOutputMarker,
	)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	capturedOutput := strings.TrimSpace(output.String())
	if err != nil {
		// When a per-hook timeout is configured and the hook context has been
		// canceled (either DeadlineExceeded or Canceled propagated from the
		// timeout), treat it as a timeout error.  We avoid comparing with
		// context.DeadlineExceeded directly because the Go exec package may
		// wrap or race with the context error after cmd.Cancel fires.
		if hook.TimeoutSeconds != nil && *hook.TimeoutSeconds > 0 && hookCtx.Err() != nil {
			return fmt.Errorf("lifecycle hook timed out after %ds: %w; output: %s",
				*hook.TimeoutSeconds, context.DeadlineExceeded, capturedOutput)
		}
		return fmt.Errorf("lifecycle hook failed: %w; output: %s", err, capturedOutput)
	}

	klog.InfoS("Lifecycle hook executed successfully", "task", task.Name, "output", capturedOutput)
	return nil
}

// shellEscape quotes arguments for safe shell execution
func shellEscape(args []string) string {
	quoted := make([]string, len(args))
	for i, s := range args {
		quoted[i] = shellEscapePath(s)
	}
	return strings.Join(quoted, " ")
}

// shellEscapePath escapes a single string for safe shell execution.
// It wraps the string in single quotes and escapes any embedded single quotes.
// e.g., foo'bar -> 'foo'\”bar'
func shellEscapePath(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// findPidByEnvVar finds a process by checking for a specific environment variable
func (e *processExecutor) findPidByEnvVar(envName, expectedValue string) (int, error) {
	procDir, err := os.Open("/proc")
	if err != nil {
		return 0, fmt.Errorf("failed to open /proc: %w", err)
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(-1)
	if err != nil {
		return 0, fmt.Errorf("failed to read /proc entries: %w", err)
	}

	selfPID := os.Getpid()
	targetEnv := fmt.Sprintf("%s=%s", envName, expectedValue)

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry)
		if err != nil {
			continue
		}
		if pid == selfPID {
			continue
		}

		// Read process environment
		envPath := filepath.Join("/proc", entry, "environ")
		envData, err := os.ReadFile(envPath)
		if err != nil {
			continue
		}

		// Environment variables are null-separated
		envVars := strings.Split(string(envData), "\x00")
		for _, env := range envVars {
			if env == targetEnv {
				klog.InfoS("Found main container by environment variable", "pid", pid, "env", targetEnv)
				return pid, nil
			}
		}
	}

	return 0, fmt.Errorf("no process found with environment variable %s=%s", envName, expectedValue)
}

// getProcEnviron reads environment variables from /proc/<pid>/environ
func getProcEnviron(pid int) ([]string, error) {
	envPath := filepath.Join("/proc", strconv.Itoa(pid), "environ")
	data, err := os.ReadFile(envPath)
	if err != nil {
		return nil, err
	}

	// Environment variables in /proc/<pid>/environ are separated by null bytes
	var envs []string
	for _, env := range strings.Split(string(data), "\x00") {
		if len(env) > 0 {
			envs = append(envs, env)
		}
	}
	return envs, nil
}
