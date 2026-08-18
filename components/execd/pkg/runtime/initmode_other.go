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

// Init mode is Linux-only (OSEP-0018). On other platforms the managedProcess
// abstraction degrades to plain Cmd.Start/Cmd.Wait so the shared launch paths
// keep compiling unchanged.

package runtime

import (
	"os/exec"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

type managedProcess struct {
	cmd *exec.Cmd
}

func newManagedProcess(cmd *exec.Cmd) *managedProcess {
	return &managedProcess{cmd: cmd}
}

func (mp *managedProcess) Wait() error {
	return mp.cmd.Wait()
}

func (mp *managedProcess) ExitCode() int {
	return mp.cmd.ProcessState.ExitCode()
}

type launchOption func(*managedProcess)

func withPreReap(fn func()) launchOption {
	return func(*managedProcess) {}
}

// withoutHardening is a no-op off Linux (the floor never applies there).
func withoutHardening() launchOption {
	return func(*managedProcess) {}
}

func launchManagedWith(cmd *exec.Cmd, startFn func() error, opts ...launchOption) (*managedProcess, error) {
	if err := startFn(); err != nil {
		return nil, err
	}
	return newManagedProcess(cmd), nil
}

func launchManaged(cmd *exec.Cmd, opts ...launchOption) (*managedProcess, error) {
	return launchManagedWith(cmd, cmd.Start, opts...)
}

// StartInitMode is unsupported off Linux; execd keeps today's behavior.
func StartInitMode(entryArgs []string) {
	log.Warn("init mode is unsupported on this platform; continuing without init duties")
}

// InitModeReport reports the init mode actually in effect for the
// capabilities endpoint.
func InitModeReport() (mode string, signalShield bool) {
	return "none", false
}

// initModeActive is always false off Linux: init mode never runs there.
func initModeActive() bool {
	return false
}
