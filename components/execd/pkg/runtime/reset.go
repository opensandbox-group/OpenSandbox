// Copyright 2026 The OpenSandbox Authors
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
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// Reset terminates and clears every user-facing session: jupyter contexts
// and kernels, foreground/background commands, bash sessions, and PTY
// sessions. Best effort — teardown errors are logged, not returned. Used by
// POST /internal/init to stop pre-init user workloads (the legacy fallback
// path may have run the template-driven startup already) before applying
// the RuntimeBinding. Isolated sessions are owned by the IsolatedRunner and
// are reset separately.
func (c *Controller) Reset() {
	c.jupyterClientMap.Range(func(key, _ any) bool {
		sessionID, ok := key.(string)
		if !ok {
			return true
		}
		if err := c.deleteSessionAndCleanup(sessionID); err != nil {
			log.Warn("runtime init: delete jupyter session %s: %v", sessionID, err)
		}
		return true
	})
	c.defaultLanguageSessions.Range(func(key, _ any) bool {
		c.defaultLanguageSessions.Delete(key)
		return true
	})
	c.resetSessionProcesses()
}

// resetSessionProcesses stops the OS processes behind command, bash, and PTY
// sessions (platform split).
func (c *Controller) resetSessionProcesses() {
	c.commandClientMap.Range(func(key, _ any) bool {
		sessionID, ok := key.(string)
		if !ok {
			return true
		}
		// Snapshot under c.mu so running/pid are observed consistently with
		// markCommandFinished; killPid signals the whole process group.
		snapshot := c.commandSnapshot(sessionID)
		if snapshot != nil && snapshot.running && snapshot.pid > 0 {
			if err := c.killPid(snapshot.pid); err != nil {
				log.Warn("runtime init: kill command session %s (pid %d): %v", sessionID, snapshot.pid, err)
			}
		}
		c.commandClientMap.Delete(key)
		return true
	})

	c.resetInteractiveSessions()
}
