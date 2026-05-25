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

//go:build !windows
// +build !windows

package runtime

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// Interrupt stops execution in the specified session.
func (c *Controller) Interrupt(sessionID string) error {
	switch {
	case c.getJupyterKernel(sessionID) != nil:
		kernel := c.getJupyterKernel(sessionID)
		log.Warning("Interrupting Jupyter kernel %s", kernel.kernelID)
		return kernel.client.InterruptKernel(kernel.kernelID)
	case c.getCommandKernel(sessionID) != nil:
		// Snapshot under c.mu so running/pid are observed consistently with
		// markCommandFinished. killPid signals the entire process group, so
		// guarding against a stale PID is critical: a late Interrupt on a
		// finished session must not blast SIGTERM/SIGKILL at an unrelated
		// process group that has reused the PID.
		snapshot := c.commandSnapshot(sessionID)
		if snapshot == nil || !snapshot.running || snapshot.pid <= 0 {
			return fmt.Errorf("command session %s is not running", sessionID)
		}
		return c.killPid(snapshot.pid)
	case c.getBashSession(sessionID) != nil:
		return c.closeBashSession(sessionID)
	default:
		return errors.New("no such session")
	}
}

// killPid sends SIGTERM followed by SIGKILL if needed.
//
// Commands are launched with Setpgid: true, so pid is also the process group
// id. We signal the entire group via syscall.Kill(-pid, sig) so child and
// grandchild processes are terminated, not just the group leader.
//
// kill(2) on a process group only guarantees delivery to at least one
// member, and kill(-pid, 0) keeps reporting the group as observable while
// any unreaped zombie lingers. The probe loops below are therefore
// best-effort logging — once a kill signal has been delivered, a slow or
// asynchronous teardown is not treated as a hard failure that would
// surface as a 500 from Interrupt.
func (c *Controller) killPid(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	log.Warning("Attempting to terminate process group %d", pid)

	sigtermDelivered := false
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		log.Warning("SIGTERM failed for pgroup %d: %v, trying SIGKILL", pid, err)
	} else {
		sigtermDelivered = true
		// Probe the group for liveness. os.Process.Wait() doesn't apply
		// because the leader is not a child of this goroutine.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(-pid, 0); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					log.Info("Process group %d terminated gracefully", pid)
					return nil
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		log.Warning("Process group %d did not exit after SIGTERM, escalating to SIGKILL", pid)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if sigtermDelivered {
			// SIGTERM was already delivered to at least one member, so the
			// kill is in flight. SIGKILL failure here is commonly EPERM on
			// a group reduced to zombies — the kernel will reap them once
			// the parent runs Wait(). Surface as a warning rather than a
			// hard error.
			log.Warning("SIGKILL on pgroup %d failed: %v; teardown likely already in progress", pid, err)
			return nil
		}
		return fmt.Errorf("failed to kill process group %d: %w", pid, err)
	}

	for range 3 {
		if err := syscall.Kill(-pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				log.Info("Process group %d confirmed terminated", pid)
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Warning("Process group %d still observable after SIGKILL; teardown may complete asynchronously", pid)
	return nil
}
