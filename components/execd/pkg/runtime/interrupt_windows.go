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

//go:build windows
// +build windows

package runtime

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/internal/safego"
)

// Interrupt stops execution in the specified session.
func (c *Controller) Interrupt(sessionID string) error {
	switch {
	case c.getJupyterKernel(sessionID) != nil:
		kernel := c.getJupyterKernel(sessionID)
		log.Warning("Interrupting Jupyter kernel %s", kernel.kernelID)
		return kernel.client.InterruptKernel(kernel.kernelID)
	case c.getCommandKernel(sessionID) != nil:
		// Guard against a stale PID after the command has finished: the
		// kernel is retained in commandClientMap, so a late Interrupt could
		// otherwise terminate an unrelated process that reused the PID.
		snapshot := c.commandSnapshot(sessionID)
		if snapshot == nil || !snapshot.running || snapshot.pid <= 0 {
			return fmt.Errorf("command session %s is not running", sessionID)
		}
		return c.killPid(snapshot.pid)
	default:
		return errors.New("no such session")
	}
}

// killPid terminates a process on Windows.
func (c *Controller) killPid(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	log.Warning("Attempting to terminate process %d", pid)

	if err := process.Kill(); err != nil {
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
		log.Warning("Process %d kill wait timed out", pid)
	}

	return nil
}
