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

// Package supervisor runs a single child process under a restart loop with
// exponential backoff, pre-start / post-exit hooks, a crashloop circuit
// breaker, and a structured event log. It is intentionally scoped to one
// worker per supervisor; multi-process supervision is delegated to higher
// layers (e.g. Kubernetes pods).
//
// Signal handling. Callers control the supervisor's lifecycle via the
// context passed to Run: cancelling ctx triggers a SIGTERM to the worker
// followed by SIGKILL after GracePeriod. This package does NOT install any
// signal.Notify itself; the caller (e.g. cmd/supervisor) is responsible for
// translating OS signals into context cancellation. As a result:
//
//   - SIGINT and SIGTERM, when wired to ctx by the caller, both result in
//     SIGTERM being sent to the worker (the supervisor does not preserve
//     which signal triggered shutdown).
//   - SIGHUP, SIGUSR1, SIGUSR2, SIGWINCH, SIGQUIT, and similar
//     application-level signals are NOT forwarded to the worker. If the
//     worker needs them (e.g. config reload, log rotate, tty resize), the
//     caller must add forwarding around Run.
//
// Process group. The worker is started with Setpgid=true on Unix so that
// signals delivered to the supervisor's process group do not reach the
// worker by side channel. The supervisor signals the worker explicitly via
// its PID.
//
// PID 1 / reaping. The supervisor does not call PR_SET_CHILD_SUBREAPER and
// does not reap arbitrary children, only the worker it launched. If the
// worker spawns its own descendants and is killed, those descendants are
// reparented per usual kernel rules. Run this supervisor as PID 1 only when
// the worker itself does not orphan grandchildren.
package supervisor

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
)

// Hook describes an auxiliary process invoked around the worker lifecycle.
// Argv[0] is the executable; remaining entries are arguments. Hooks are
// invoked directly (no shell); wrap in a shell script if expansion is needed.
type Hook struct {
	Argv []string
}

// Spec configures a supervisor run.
type Spec struct {
	// Name identifies the supervised worker in logs and events. Defaults to
	// basename(Cmd).
	Name string

	// Cmd is the worker executable path. Required.
	Cmd  string
	Args []string
	Env  []string // defaults to os.Environ()
	Dir  string   // working directory; empty = inherit

	// PreStart hooks run before each worker launch. A non-zero exit aborts
	// the launch and counts toward the crashloop budget.
	PreStart []Hook
	// PostExit hooks run after the worker has been reaped. Failures are
	// logged but do not block the restart loop.
	PostExit        []Hook
	PreStartTimeout time.Duration // default 30s
	PostExitTimeout time.Duration // default 30s

	// Backoff controls inter-restart sleep. Sleep grows exponentially from
	// BackoffMin to BackoffMax with ±*BackoffJitter*prev jitter. After the
	// worker has been alive at least StableAfter, the backoff resets.
	BackoffMin time.Duration // default 1s
	BackoffMax time.Duration // default 30s
	// BackoffJitter is a *float64 so callers can distinguish "unset"
	// (defaults to 0.1) from "explicitly disabled" (pass &zero). Negative
	// values are clamped to 0.
	BackoffJitter *float64
	StableAfter   time.Duration // default 60s

	// Crashloop circuit breaker. If more than BurstMax launches occur
	// within BurstWindow, the supervisor either returns (OnBurstExit=true,
	// default) so the surrounding runtime can react, or continues looping.
	BurstWindow time.Duration // default 5m
	BurstMax    int           // default 10
	// OnBurstExit selects burst behavior. Default true.
	// A *bool lets callers override the non-zero default; nil means default.
	OnBurstExit *bool

	// GracePeriod is how long SIGTERM is given to the worker on shutdown
	// before SIGKILL. Default 10s.
	GracePeriod time.Duration

	// EventLog receives one JSON object per line. nil => os.Stderr.
	EventLog io.Writer

	// WorkerStdout / WorkerStderr forward the worker's standard streams.
	// nil defaults to the supervisor's own streams.
	WorkerStdout io.Writer
	WorkerStderr io.Writer

	// Logger receives free-form supervisor diagnostics. nil => a no-op logger.
	Logger logger.Logger

	// Clock is injected for tests; nil => real clock.
	Clock Clock
}

// Clock abstracts time for tests. Implementations must be goroutine-safe.
type Clock interface {
	Now() time.Time
	// After is identical to time.After.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Defaults applied to zero-valued fields.
const (
	defaultBackoffMin      = time.Second
	defaultBackoffMax      = 30 * time.Second
	defaultBackoffJitter   = 0.1
	defaultStableAfter     = 60 * time.Second
	defaultBurstWindow     = 5 * time.Minute
	defaultBurstMax        = 10
	defaultGracePeriod     = 10 * time.Second
	defaultPreStartTimeout = 30 * time.Second
	defaultPostExitTimeout = 30 * time.Second
)

func (s *Spec) applyDefaults() {
	if s.Name == "" && s.Cmd != "" {
		s.Name = filepath.Base(s.Cmd)
	}
	if s.Env == nil {
		s.Env = os.Environ()
	}
	if s.BackoffMin <= 0 {
		s.BackoffMin = defaultBackoffMin
	}
	if s.BackoffMax <= 0 {
		s.BackoffMax = defaultBackoffMax
	}
	if s.BackoffMax < s.BackoffMin {
		s.BackoffMax = s.BackoffMin
	}
	if s.BackoffJitter == nil {
		v := defaultBackoffJitter
		s.BackoffJitter = &v
	} else if *s.BackoffJitter < 0 {
		v := 0.0
		s.BackoffJitter = &v
	}
	if s.StableAfter <= 0 {
		s.StableAfter = defaultStableAfter
	}
	if s.BurstWindow <= 0 {
		s.BurstWindow = defaultBurstWindow
	}
	if s.BurstMax <= 0 {
		s.BurstMax = defaultBurstMax
	}
	if s.OnBurstExit == nil {
		v := true
		s.OnBurstExit = &v
	}
	if s.GracePeriod <= 0 {
		s.GracePeriod = defaultGracePeriod
	}
	if s.PreStartTimeout <= 0 {
		s.PreStartTimeout = defaultPreStartTimeout
	}
	if s.PostExitTimeout <= 0 {
		s.PostExitTimeout = defaultPostExitTimeout
	}
	if s.EventLog == nil {
		s.EventLog = os.Stderr
	}
	if s.WorkerStdout == nil {
		s.WorkerStdout = os.Stdout
	}
	if s.WorkerStderr == nil {
		s.WorkerStderr = os.Stderr
	}
	if s.Logger == nil {
		s.Logger = noopLogger{}
	}
	if s.Clock == nil {
		s.Clock = realClock{}
	}
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...any)              {}
func (noopLogger) Infof(string, ...any)               {}
func (noopLogger) Warnf(string, ...any)               {}
func (noopLogger) Errorf(string, ...any)              {}
func (noopLogger) With(...logger.Field) logger.Logger { return noopLogger{} }
func (noopLogger) Named(string) logger.Logger         { return noopLogger{} }
func (noopLogger) Sync() error                        { return nil }
