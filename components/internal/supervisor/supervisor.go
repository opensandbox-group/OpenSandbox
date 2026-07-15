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

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrBurstExceeded is returned by Run when the crashloop budget is exhausted
// and Spec.OnBurstExit is true.
var ErrBurstExceeded = errors.New("supervisor: crashloop budget exceeded")

// Run supervises the worker described by spec until ctx is cancelled or the
// crashloop budget is exhausted. It returns ctx.Err() on graceful shutdown,
// ErrBurstExceeded on burst exit, or a setup error if Spec is invalid.
func Run(ctx context.Context, spec Spec) error {
	if spec.Cmd == "" {
		return errors.New("supervisor: Spec.Cmd is required")
	}
	spec.applyDefaults()

	ew := newEventWriter(spec.EventLog, spec.Name, spec.Clock.Now)
	starts := newBurstTracker(spec.BurstMax, spec.BurstWindow, spec.Clock.Now)

	var (
		gen     uint64
		attempt int
		backoff time.Duration
	)

	shutdown := func() error {
		_ = ew.emit(Event{Event: EventShutdown, Reason: "ctx cancelled"})
		return ctx.Err()
	}

	for {
		if ctx.Err() != nil {
			return shutdown()
		}

		attempt++
		curGen := atomic.AddUint64(&gen, 1)

		// Pre-start hooks. Failure aborts the launch, counts toward burst.
		if hookErr := runHooks(ctx, spec.PreStart, spec.PreStartTimeout, spec.Env, ew, EventPreStart); hookErr != nil {
			spec.Logger.Warnf("supervisor: pre-start hook failed: %v", hookErr)
			starts.record()
			if exitOnBurst(starts, spec, ew, attempt) {
				return ErrBurstExceeded
			}
			backoff = sleepBackoff(ctx, spec, ew, backoff, attempt+1)
			if backoff < 0 {
				return shutdown()
			}
			continue
		}

		// Launch worker. Worker run-duration is measured in wall-clock time
		// because the child is a real process; fake clocks (used in tests
		// for backoff control) would otherwise report 0 and never trip the
		// StableAfter threshold.
		starts.record()
		runStart := time.Now()
		cmd := exec.Command(spec.Cmd, spec.Args...)
		cmd.Env = spec.Env
		cmd.Dir = spec.Dir
		cmd.Stdout = spec.WorkerStdout
		cmd.Stderr = spec.WorkerStderr
		applyChildPgid(cmd)

		if err := cmd.Start(); err != nil {
			spec.Logger.Errorf("supervisor: launch failed: %v", err)
			_ = ew.emit(Event{
				Event:   EventExit,
				Gen:     curGen,
				Attempt: attempt,
				Reason:  "launch_failed",
				Error:   err.Error(),
			})
			if exitOnBurst(starts, spec, ew, attempt) {
				return ErrBurstExceeded
			}
			backoff = sleepBackoff(ctx, spec, ew, backoff, attempt+1)
			if backoff < 0 {
				return shutdown()
			}
			continue
		}

		pid := cmd.Process.Pid
		spec.Logger.Infof("supervisor: worker started (pid=%d, gen=%d, attempt=%d)", pid, curGen, attempt)
		_ = ew.emit(Event{Event: EventStart, PID: pid, Gen: curGen, Attempt: attempt})

		// Graceful-shutdown goroutine: on ctx cancel, send SIGTERM then SIGKILL
		// unless the worker exits within GracePeriod on its own.
		stopped := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				gracefulStop(cmd.Process, spec.GracePeriod, stopped)
			case <-stopped:
			}
		}()

		waitErr := cmd.Wait()
		close(stopped)
		runDur := time.Since(runStart)

		exitCode, sigName, reason := classifyExit(cmd, waitErr, ctx.Err() != nil)
		spec.Logger.Infof("supervisor: worker exited (pid=%d, gen=%d, dur=%s, code=%d, signal=%s, reason=%s)",
			pid, curGen, runDur, exitCode, sigName, reason)
		_ = ew.emit(Event{
			Event:      EventExit,
			PID:        pid,
			Gen:        curGen,
			Attempt:    attempt,
			ExitCode:   intPtr(exitCode),
			Signal:     sigName,
			DurationMS: runDur.Milliseconds(),
			Reason:     reason,
		})

		// Stable: reset backoff if the worker stayed alive long enough.
		if runDur >= spec.StableAfter {
			if backoff > 0 {
				_ = ew.emit(Event{
					Event: EventStable, PID: pid, Gen: curGen, ResetBackoff: true,
					DurationMS: runDur.Milliseconds(),
				})
			}
			backoff = 0
		}

		// Post-exit hooks. Receive context env. Errors are logged, not fatal.
		// Build hookEnv into a fresh slice so we never mutate spec.Env's
		// underlying array (which `append(spec.Env, ...)` may do when
		// cap > len).
		hookEnv := make([]string, 0, len(spec.Env)+5)
		hookEnv = append(hookEnv, spec.Env...)
		hookEnv = append(hookEnv,
			"WORKER_EXIT_CODE="+strconv.Itoa(exitCode),
			"WORKER_SIGNAL="+sigName,
			"WORKER_DURATION_MS="+strconv.FormatInt(runDur.Milliseconds(), 10),
			"WORKER_PID="+strconv.Itoa(pid),
			"WORKER_ATTEMPT="+strconv.Itoa(attempt),
		)
		// Post-exit hooks must run to completion even during shutdown so
		// cleanup (iptables / nft / temp files) is not aborted. Use a
		// detached ctx bounded by PostExitTimeout instead of ctx.
		if hookErr := runHooks(context.Background(), spec.PostExit, spec.PostExitTimeout, hookEnv, ew, EventPostExit); hookErr != nil {
			spec.Logger.Warnf("supervisor: post-exit hook failed: %v", hookErr)
		}

		if ctx.Err() != nil {
			return shutdown()
		}

		if exitOnBurst(starts, spec, ew, attempt) {
			return ErrBurstExceeded
		}

		backoff = sleepBackoff(ctx, spec, ew, backoff, attempt+1)
		if backoff < 0 {
			return shutdown()
		}
	}
}

// sleepBackoff computes the next backoff, emits a backoff event, and sleeps.
// Returns the slept duration, or -1 if ctx was cancelled mid-sleep.
func sleepBackoff(ctx context.Context, spec Spec, ew *eventWriter, prev time.Duration, nextAttempt int) time.Duration {
	d := nextBackoff(prev, spec.BackoffMin, spec.BackoffMax, *spec.BackoffJitter, defaultRNG)
	_ = ew.emit(Event{Event: EventBackoff, SleepMS: d.Milliseconds(), NextAttempt: nextAttempt})
	select {
	case <-ctx.Done():
		return -1
	case <-spec.Clock.After(d):
	}
	return d
}

// exitOnBurst checks the burst tracker. Returns true if Run should bail out.
func exitOnBurst(b *burstTracker, spec Spec, ew *eventWriter, attempt int) bool {
	if !b.exceeded() {
		return false
	}
	_ = ew.emit(Event{
		Event:    EventBurstExit,
		Attempts: b.count(),
		Window:   spec.BurstWindow.String(),
		Attempt:  attempt,
		Reason:   "crashloop budget exceeded",
	})
	return *spec.OnBurstExit
}

// classifyExit extracts the worker's exit code and (if killed) the signal
// name, plus a coarse reason string for the event log.
func classifyExit(cmd *exec.Cmd, waitErr error, ctxCancelled bool) (exitCode int, sigName, reason string) {
	if cmd.ProcessState == nil {
		return -1, "", "no_processstate"
	}
	exitCode = cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sigName = ws.Signal().String()
	}
	switch {
	case ctxCancelled:
		reason = "shutdown"
	case waitErr == nil:
		reason = "exited"
	case sigName != "":
		reason = "signaled"
	default:
		reason = "crashed"
	}
	return
}

// runHooks invokes each hook sequentially. The first non-zero exit (or
// timeout / launch error) is recorded and returned; subsequent hooks still
// run so cleanup paths complete.
func runHooks(ctx context.Context, hooks []Hook, timeout time.Duration, env []string, ew *eventWriter, kind string) error {
	var firstErr error
	for _, h := range hooks {
		if len(h.Argv) == 0 {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(hctx, h.Argv[0], h.Argv[1:]...)
		cmd.Env = env
		start := time.Now()
		err := cmd.Run()
		dur := time.Since(start)
		cancel()

		ev := Event{
			Event:      kind,
			Hook:       h.Argv[0],
			DurationMS: dur.Milliseconds(),
		}
		exitCode := 0
		if err != nil {
			ev.Error = err.Error()
			if cmd.ProcessState != nil {
				exitCode = cmd.ProcessState.ExitCode()
			} else {
				exitCode = -1
			}
		} else if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		ev.ExitCode = intPtr(exitCode)
		_ = ew.emit(ev)

		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s hook %q: %w", kind, h.Argv[0], err)
		}
	}
	return firstErr
}
