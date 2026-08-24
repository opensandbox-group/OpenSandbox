//go:build !linux

// Copyright 2026 Alibaba Group Holding Ltd.
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
	"os/exec"
	"sync/atomic"
)

type directManagedCommand struct {
	cmd    *exec.Cmd
	exited atomic.Bool
}

func (c *directManagedCommand) Wait() error {
	err := c.cmd.Wait()
	c.exited.Store(true)
	return err
}

func (c *directManagedCommand) ExitCode() int {
	if c.cmd.ProcessState == nil {
		return -1
	}
	return c.cmd.ProcessState.ExitCode()
}

func (c *directManagedCommand) Cancel(cancel func()) {
	if !c.exited.Load() {
		cancel()
	}
}

func startManagedCommand(cmd *exec.Cmd) (managedCommand, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &directManagedCommand{cmd: cmd}, nil
}
