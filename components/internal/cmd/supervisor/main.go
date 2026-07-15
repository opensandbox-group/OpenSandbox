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

// Command opensandbox-supervisor wraps a single worker process with restart
// backoff, lifecycle hooks, and a structured event log. It is designed to
// run as a container ENTRYPOINT or as a child of another process; it does
// not assume PID 1 and performs no zombie reaping.
//
// Usage:
//
//	opensandbox-supervisor [flags] -- <worker-cmd> [worker-args...]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/supervisor"
	"github.com/alibaba/opensandbox/internal/version"
	"gopkg.in/natefinch/lumberjack.v2"
)

// multiFlag collects a repeatable flag into a string slice.
type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprintf("%v", *m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	version.EchoVersion("OpenSandbox Supervisor")

	var (
		preStart      multiFlag
		postExit      multiFlag
		eventLog      string
		backoffMin    time.Duration
		backoffMax    time.Duration
		backoffJitter float64
		stableAfter   time.Duration
		burstWindow   time.Duration
		burstMax      int
		onBurst       bool
		grace         time.Duration
		preTimeout    time.Duration
		postTimeout   time.Duration
		name          string
		logLevel      string
	)

	fs := flag.NewFlagSet("opensandbox-supervisor", flag.ExitOnError)
	fs.Var(&preStart, "pre-start", "Executable to run before each worker launch (repeatable). No shell expansion; wrap in a script if needed.")
	fs.Var(&postExit, "post-exit", "Executable to run after each worker exit (repeatable). Receives WORKER_* env. Failures are logged, not fatal.")
	fs.StringVar(&eventLog, "event-log", "", "Path to JSONL event log. Empty = stderr.")
	fs.DurationVar(&backoffMin, "backoff-min", time.Second, "Minimum restart backoff.")
	fs.DurationVar(&backoffMax, "backoff-max", 30*time.Second, "Maximum restart backoff (exponential capped here).")
	fs.Float64Var(&backoffJitter, "backoff-jitter", 0.1, "Backoff jitter fraction (0 disables, e.g. 0.1 = ±10%). Negative clamped to 0.")
	fs.DurationVar(&stableAfter, "stable-after", 60*time.Second, "Worker uptime after which backoff resets.")
	fs.DurationVar(&burstWindow, "burst-window", 5*time.Minute, "Crashloop budget sliding window.")
	fs.IntVar(&burstMax, "burst-max", 10, "Max launches inside burst-window before tripping the breaker.")
	fs.BoolVar(&onBurst, "on-burst-exit", true, "true: supervisor exits non-zero when the burst budget trips, so a higher-level supervisor (e.g. kubelet) reacts. false: keep retrying.")
	fs.DurationVar(&grace, "grace-period", 10*time.Second, "Time between SIGTERM and SIGKILL when shutting the worker down.")
	fs.DurationVar(&preTimeout, "pre-start-timeout", 30*time.Second, "Timeout for each pre-start hook.")
	fs.DurationVar(&postTimeout, "post-exit-timeout", 30*time.Second, "Timeout for each post-exit hook.")
	fs.StringVar(&name, "name", "", "Worker name shown in logs and events (default: basename of the worker cmd).")
	fs.StringVar(&logLevel, "log-level", "info", "Supervisor diagnostic log level (debug|info|warn|error).")

	args := os.Args[1:]
	workerArgs := splitOnDoubleDash(&args)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if len(workerArgs) == 0 {
		fmt.Fprintln(os.Stderr, "opensandbox-supervisor: missing worker command after `--`")
		fs.Usage()
		os.Exit(2)
	}

	log := logger.MustNew(logger.Config{Level: logLevel}).Named("supervisor")
	defer log.Sync()

	eventWriter, closer, err := openEventLog(eventLog)
	if err != nil {
		log.Errorf("event log: %v", err)
		os.Exit(2)
	}
	defer closer()

	spec := supervisor.Spec{
		Name:            name,
		Cmd:             workerArgs[0],
		Args:            workerArgs[1:],
		PreStart:        toHooks(preStart),
		PostExit:        toHooks(postExit),
		BackoffMin:      backoffMin,
		BackoffMax:      backoffMax,
		BackoffJitter:   &backoffJitter,
		StableAfter:     stableAfter,
		BurstWindow:     burstWindow,
		BurstMax:        burstMax,
		OnBurstExit:     &onBurst,
		GracePeriod:     grace,
		PreStartTimeout: preTimeout,
		PostExitTimeout: postTimeout,
		EventLog:        eventWriter,
		Logger:          log,
	}
	if spec.Name == "" {
		spec.Name = filepath.Base(spec.Cmd)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Infof("supervising %q (event-log=%s)", spec.Cmd, eventLogDest(eventLog))
	err = supervisor.Run(ctx, spec)
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		os.Exit(0)
	case errors.Is(err, supervisor.ErrBurstExceeded):
		log.Errorf("supervisor: %v", err)
		os.Exit(1)
	default:
		log.Errorf("supervisor: %v", err)
		os.Exit(2)
	}
}

// splitOnDoubleDash takes everything after the first "--" as the worker
// argv and trims the supervisor flag slice in place.
func splitOnDoubleDash(args *[]string) []string {
	for i, a := range *args {
		if a == "--" {
			worker := append([]string(nil), (*args)[i+1:]...)
			*args = (*args)[:i]
			return worker
		}
	}
	return nil
}

func toHooks(paths []string) []supervisor.Hook {
	if len(paths) == 0 {
		return nil
	}
	out := make([]supervisor.Hook, 0, len(paths))
	for _, p := range paths {
		out = append(out, supervisor.Hook{Argv: []string{p}})
	}
	return out
}

func openEventLog(path string) (io.Writer, func(), error) {
	if path == "" {
		return os.Stderr, func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	lj := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    logger.DefaultRotateMaxSize,
		MaxAge:     logger.DefaultRotateMaxAge,
		MaxBackups: logger.DefaultRotateMaxBackups,
		Compress:   true,
	}
	return lj, func() { _ = lj.Close() }, nil
}

func eventLogDest(path string) string {
	if path == "" {
		return "stderr"
	}
	return path
}
