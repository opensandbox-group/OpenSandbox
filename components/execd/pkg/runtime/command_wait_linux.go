//go:build linux

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
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// waitCommandWithExitBarrier observes process exit with WNOWAIT before
// exec.Cmd.Wait reaps the group leader. markBeforeReap is therefore serialized
// with process-group signalling while the kernel still reserves the numeric
// PID/PGID for this child.
func waitCommandWithExitBarrier(
	cmd *exec.Cmd,
	markBeforeReap func(error),
) error {
	if cmd == nil || cmd.Process == nil {
		err := errors.New("isolated session command was not started")
		markBeforeReap(err)
		return err
	}

	var (
		info       unix.Siginfo
		barrierErr error
	)
	for {
		err := unix.Waitid(
			unix.P_PID,
			cmd.Process.Pid,
			&info,
			unix.WEXITED|unix.WNOWAIT,
			nil,
		)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		barrierErr = fmt.Errorf(
			"waitid WNOWAIT for pid %d: %w",
			cmd.Process.Pid,
			err,
		)
		break
	}

	markBeforeReap(barrierErr)
	return errors.Join(barrierErr, cmd.Wait())
}
