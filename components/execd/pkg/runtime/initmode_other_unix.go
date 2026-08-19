//go:build !linux && !windows

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

// managedProcess fallback for non-Linux Unix platforms: degrades to plain
// Cmd.Start/Cmd.Wait so the shared launch paths keep compiling. Windows does
// not compile these — nothing there uses the managed-process launch paths.

package runtime

import "os/exec"

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

// initModeActive is always false off Linux: init mode never runs there.
func initModeActive() bool {
	return false
}
