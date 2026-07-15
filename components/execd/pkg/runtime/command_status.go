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
	"fmt"
	"io"
	"os"
	"time"
)

// CommandStatus describes the lifecycle state of a command.
type CommandStatus struct {
	Session    string     `json:"session"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Content    string     `json:"content,omitempty"`
}

// CommandOutput contains non-streamed stdout/stderr plus status.
type CommandOutput struct {
	CommandStatus
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

func (c *Controller) commandSnapshot(session string) *commandKernel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var kernel *commandKernel
	if v, ok := c.commandClientMap.Load(session); ok {
		kernel, _ = v.(*commandKernel)
	}
	if kernel == nil {
		return nil
	}

	cp := *kernel
	return &cp
}

// GetCommandStatus returns the execution status for a command session.
func (c *Controller) GetCommandStatus(session string) (*CommandStatus, error) {
	kernel := c.commandSnapshot(session)
	if kernel == nil {
		return nil, fmt.Errorf("command not found: %s", session)
	}

	status := &CommandStatus{
		Session:    session,
		Running:    kernel.running,
		ExitCode:   kernel.exitCode,
		Error:      kernel.errMsg,
		StartedAt:  kernel.startedAt,
		FinishedAt: kernel.finishedAt,
		Content:    kernel.content,
	}
	return status, nil
}

// SeekBackgroundCommandOutput returns accumulated stdout/stderr and status for a session.
func (c *Controller) SeekBackgroundCommandOutput(session string, cursor int64) ([]byte, int64, error) {
	kernel := c.commandSnapshot(session)
	if kernel == nil {
		return nil, -1, fmt.Errorf("command not found: %s", session)
	}

	if !kernel.isBackground {
		return nil, -1, fmt.Errorf("command %s is not running in background", session)
	}

	file, err := os.Open(kernel.stdoutPath)
	if err != nil {
		return nil, -1, fmt.Errorf("error open combined output file for command %s: %w", session, err)
	}
	defer file.Close()

	// Seek to the cursor position
	_, err = file.Seek(cursor, 0)
	if err != nil {
		return nil, -1, fmt.Errorf("error seek file: %w", err)
	}

	// Read all content from cursor to end
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, -1, fmt.Errorf("error read file: %w", err)
	}

	// Get current file position (end of file)
	currentPos, err := file.Seek(0, 1)
	if err != nil {
		return nil, -1, fmt.Errorf("error get current position: %w", err)
	}

	return data, currentPos, nil
}

// markCommandFinished updates bookkeeping when a command exits.
func (c *Controller) markCommandFinished(session string, exitCode int, errMsg string) {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	var kernel *commandKernel
	if v, ok := c.commandClientMap.Load(session); ok {
		kernel, _ = v.(*commandKernel)
	}
	if kernel == nil {
		return
	}

	kernel.exitCode = &exitCode
	kernel.errMsg = errMsg
	kernel.running = false
	kernel.finishedAt = &now
	// Clear the PID so a late or retried Interrupt cannot signal a recycled
	// process. Group-wide kill would otherwise amplify the impact of a
	// stale-PID hit to every process in the unrelated process group.
	kernel.pid = 0
}
