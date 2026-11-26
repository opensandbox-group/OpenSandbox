// Copyright 2025 The OpenSandbox Authors
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

//go:build windows
// +build windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/internal/safego"
)

const taskkillTimeout = 2 * time.Second

// killProcessTree terminates process and the processes it started. Windows
// has no process groups, so killing only cmd.exe would leave its children
// running. taskkill /T finds children by parent PID, so it must run while
// the root process is still alive. If taskkill fails, kill the root only.
func killProcessTree(commandID string, process *os.Process) error {
	// exec.Cmd can call Cancel after Wait has reaped the process, and then
	// the PID can belong to another process. Open a handle first, then check
	// that the process is not done: if it is not, the handle refers to it
	// and keeps the PID from being reused while taskkill runs.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(process.Pid))
	if err == nil {
		defer func() { _ = windows.CloseHandle(handle) }()
	}
	// On Windows, Wait marks the process released, so Signal reports EINVAL.
	if sigErr := process.Signal(syscall.Signal(0)); errors.Is(sigErr, os.ErrProcessDone) || errors.Is(sigErr, syscall.EINVAL) {
		return os.ErrProcessDone
	}
	var systemDirectory string
	if err == nil {
		systemDirectory, err = windows.GetSystemDirectory()
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
		defer cancel()
		err = exec.CommandContext(
			ctx,
			filepath.Join(systemDirectory, "taskkill.exe"),
			"/T",
			"/F",
			"/PID",
			strconv.Itoa(process.Pid),
		).Run()
		if err == nil {
			return nil
		}
	}
	log.WarnWithCommandID(commandID, "kill process tree of pid %d: %v; killing the process only", process.Pid, err)
	return process.Kill()
}

// Interrupt stops execution in the specified session.
func (c *Controller) Interrupt(sessionID string) error {
	switch {
	case c.getJupyterKernel(sessionID) != nil:
		kernel := c.getJupyterKernel(sessionID)
		log.Warn("jupyter: interrupt kernel %s", kernel.kernelID)
		return kernel.client.InterruptKernel(kernel.kernelID)
	case c.getCommandKernel(sessionID) != nil:
		// Guard against a stale PID after the command has finished: the
		// kernel is retained in commandClientMap, so a late Interrupt could
		// otherwise terminate an unrelated process that reused the PID.
		snapshot := c.commandSnapshot(sessionID)
		if snapshot == nil || !snapshot.running || snapshot.pid <= 0 {
			return fmt.Errorf("command session %s is not running", sessionID)
		}
		return c.killPid(sessionID, snapshot.pid)
	default:
		return errors.New("no such session")
	}
}

// killPid terminates a process on Windows.
func (c *Controller) killPid(commandID string, pid int) error {
	// Prefer the process that execd started. It knows when Wait has reaped
	// the command, so a PID reused after the command exits is never killed.
	if snapshot := c.commandSnapshot(commandID); snapshot != nil && snapshot.process != nil && snapshot.process.Pid == pid {
		log.WarnWithCommandID(commandID, "interrupt: terminating process %d", pid)
		if err := killProcessTree(commandID, snapshot.process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("failed to kill process %d: %w", pid, err)
		}
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	log.WarnWithCommandID(commandID, "interrupt: terminating process %d", pid)

	if err := killProcessTree(commandID, process); err != nil {
		return fmt.Errorf("failed to kill process %d: %w", pid, err)
	}

	// Best-effort wait to reduce zombies; os.Process.Wait only works for child processes.
	done := make(chan error, 1)
	safego.Go(func() {
		_, err := process.Wait()
		done <- err
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.WarnWithCommandID(commandID, "interrupt: process %d kill wait timed out", pid)
	}

	return nil
}
