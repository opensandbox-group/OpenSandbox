//go:build !windows

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

// resetInteractiveSessions stops bash and PTY sessions (runtime init).
func (c *Controller) resetInteractiveSessions() {
	c.bashSessionClientMap.Range(func(key, _ any) bool {
		sessionID, ok := key.(string)
		if !ok {
			return true
		}
		// closeBashSession removes the map entry.
		if err := c.closeBashSession(sessionID); err != nil {
			log.Warn("runtime init: close bash session %s: %v", sessionID, err)
		}
		return true
	})

	c.ptySessionMap.Range(func(key, _ any) bool {
		sessionID, ok := key.(string)
		if !ok {
			return true
		}
		// DeletePTYSession removes the map entry.
		if err := c.DeletePTYSession(sessionID); err != nil {
			log.Warn("runtime init: delete pty session %s: %v", sessionID, err)
		}
		return true
	})
}
