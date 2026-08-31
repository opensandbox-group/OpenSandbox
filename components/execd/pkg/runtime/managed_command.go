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
	"context"
	"errors"
	"os/exec"
	"time"
)

const managedCommandCancelWait = 5 * time.Second

var ErrManagedCommandCancelTimeout = errors.New("timed out waiting for canceled command to exit")

type managedCommand interface {
	Wait() error
	ExitCode() int
	Cancel(func())
}

// RunManagedCommand runs cmd through execd's init-mode-aware child tracker.
// cancel must promptly terminate cmd and any descendants when ctx is canceled;
// it may be called while the init reaper lock is held.
func RunManagedCommand(ctx context.Context, cmd *exec.Cmd, cancel func()) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	process, err := startManagedCommand(cmd)
	if err != nil {
		return -1, err
	}

	done := make(chan error, 1)
	go func() { done <- process.Wait() }()

	select {
	case err := <-done:
		return process.ExitCode(), err
	case <-ctx.Done():
		// Prefer a result that completed concurrently with cancellation.
		select {
		case err := <-done:
			return process.ExitCode(), err
		default:
		}
		process.Cancel(cancel)
		select {
		case err := <-done:
			return process.ExitCode(), errors.Join(ctx.Err(), err)
		case <-time.After(managedCommandCancelWait):
			return -1, errors.Join(ctx.Err(), ErrManagedCommandCancelTimeout)
		}
	}
}
