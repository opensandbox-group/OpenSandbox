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

package nftables

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNftRunnerProcessHelper(t *testing.T) {
	mode := os.Getenv("OPENSANDBOX_RUNNER_HELPER")
	if mode == "" {
		return
	}
	fmt.Fprint(os.Stderr, "runner diagnostic")
	switch mode {
	case "success":
		os.Exit(0)
	case "exit":
		os.Exit(9)
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "input":
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "wait":
		if err := os.WriteFile(os.Getenv("OPENSANDBOX_RUNNER_STARTED"), []byte("started"), 0600); err != nil {
			os.Exit(10)
		}
		time.Sleep(time.Minute)
	}
	os.Exit(11)
}

type failedNftInput struct{ err error }

func (r failedNftInput) Read([]byte) (int, error) { return 0, r.err }

func TestNftProductionRunnerEffects(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		cmd := exec.CommandContext(context.Background(), filepath.Join(t.TempDir(), "missing-nft"))
		_, err := runNftCommand(context.Background(), cmd)
		require.Error(t, err)
		require.Nil(t, cmd.Process)
		require.Equal(t, ApplyUnchanged, ApplyEffectOf(err))
	})
	t.Run("pre-cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNftRunnerProcessHelper$")
		_, err := runNftCommand(ctx, cmd)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, cmd.Process)
		require.Equal(t, ApplyUnchanged, ApplyEffectOf(err))
	})
	for _, mode := range []string{"success", "exit", "signal", "input"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNftRunnerProcessHelper$")
			cmd.Env = append(os.Environ(), "OPENSANDBOX_RUNNER_HELPER="+mode)
			copyErr := errors.New("input copy failed")
			if mode == "input" {
				cmd.Stdin = failedNftInput{copyErr}
			}
			out, err := runNftCommand(ctx, cmd)
			require.NotNil(t, cmd.Process)
			require.Contains(t, string(out), "runner diagnostic")
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, ApplyCommitted, ApplyEffectOf(err))
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), "runner diagnostic")
				require.Equal(t, ApplyUnknown, ApplyEffectOf(err))
				if mode == "input" {
					require.ErrorIs(t, err, copyErr)
				}
			}
		})
	}
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("post-start-deadline=%v", deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Cancellation follows child startup acknowledgement. The deadline
			// case also requires that acknowledgement before observing expiry.
			var stop context.CancelFunc
			if deadline {
				ctx, stop = context.WithTimeout(ctx, 5*time.Second)
				defer stop()
			}
			marker := filepath.Join(t.TempDir(), "started")
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNftRunnerProcessHelper$")
			cmd.Env = append(os.Environ(), "OPENSANDBOX_RUNNER_HELPER=wait", "OPENSANDBOX_RUNNER_STARTED="+marker)
			result := make(chan error, 1)
			go func() { _, err := runNftCommand(ctx, cmd); result <- err }()
			require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 3*time.Second, time.Millisecond)
			if !deadline {
				cancel()
			}
			err := <-result
			require.NotNil(t, cmd.Process)
			require.Error(t, err)
			require.Equal(t, ApplyUnknown, ApplyEffectOf(err))
			if deadline {
				require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
			} else {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			}
		})
	}
}
