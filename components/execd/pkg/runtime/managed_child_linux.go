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

//go:build linux

package runtime

import "os/exec"

// PrepareManagedChild applies the hardening-floor rewrite (launcher exec,
// capability drop, seccomp, Landlock — see hardenCmd) to a command that an
// external party will Start itself, such as the MCP SDK's CommandTransport.
// The returned release function must be called once the child has started
// (or failed to).
func PrepareManagedChild(cmd *exec.Cmd) (release func(), err error) {
	file, err := hardenCmd(cmd, false, nil)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return func() {}, nil
	}
	return func() { _ = file.Close() }, nil
}

// AdoptManagedChild registers an externally started direct child with the
// init reaper. No-op when init mode is off. Unlike launchManagedWith there
// is a small post-start window: a child dying inside it is reaped as an
// orphan and surfaces as a Wait error to the starter — teardown proceeds
// normally.
func AdoptManagedChild(cmd *exec.Cmd) {
	if initReaper == nil || cmd.Process == nil {
		return
	}
	mp := newManagedProcess(cmd)
	initReaper.mu.Lock()
	initReaper.owned[cmd.Process.Pid] = mp
	initReaper.mu.Unlock()
}

// ReleaseManagedChild drops an adoption once the starter (which owns
// exec.Cmd.Wait) consumed the exit status, so a starter-won wait cannot
// leak an owned entry that shutdown would signal. No-op if already reaped.
func ReleaseManagedChild(cmd *exec.Cmd) {
	if initReaper == nil || cmd.Process == nil {
		return
	}
	initReaper.mu.Lock()
	if mp, ok := initReaper.owned[cmd.Process.Pid]; ok && mp.cmd == cmd {
		delete(initReaper.owned, cmd.Process.Pid)
	}
	initReaper.mu.Unlock()
}
