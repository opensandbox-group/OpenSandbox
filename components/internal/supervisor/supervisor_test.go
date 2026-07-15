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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary re-execs itself to act as a fake child process. The mode
// is selected by env so callers do not need a separate helper binary on disk.
const childModeEnv = "SUPERVISOR_TEST_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(childModeEnv) {
	case "":
		os.Exit(m.Run())
	case "exit0":
		os.Exit(0)
	case "exit1":
		os.Exit(1)
	case "crash-after-100ms":
		time.Sleep(100 * time.Millisecond)
		os.Exit(2)
	case "sleep-then-exit0":
		dur, _ := time.ParseDuration(os.Getenv("CHILD_SLEEP"))
		time.Sleep(dur)
		os.Exit(0)
	case "hang-until-sigterm":
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		<-c
		os.Exit(0)
	case "hang-ignore-sigterm":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
		os.Exit(99)
	case "write-stdout":
		fmt.Println("hello from child")
		os.Exit(0)
	default:
		os.Exit(99)
	}
}

// childCmd builds the args needed to re-invoke the test binary in a given
// child mode. The supervisor sees this as a normal external command.
func childCmd(mode string, extraEnv ...string) (cmd string, args []string, env []string) {
	env = append(os.Environ(), childModeEnv+"="+mode)
	env = append(env, extraEnv...)
	return os.Args[0], []string{"-test.run=TestMain"}, env
}

// fakeClock implements Clock with controllable time advancement. After is
// implemented as a real-time short sleep so we don't have to build a full
// scheduler; tests use sub-second backoffs.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func parseEvents(t *testing.T, buf *bytes.Buffer) []Event {
	t.Helper()
	var out []Event
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad event JSON: %v\nline=%q", err, sc.Text())
		}
		out = append(out, e)
	}
	return out
}

func filterEvents(events []Event, kind string) []Event {
	var out []Event
	for _, e := range events {
		if e.Event == kind {
			out = append(out, e)
		}
	}
	return out
}

// baseSpec returns a Spec with short timeouts suitable for tests.
func baseSpec(buf *bytes.Buffer, clk Clock) Spec {
	false_ := false
	jitter := 0.01
	return Spec{
		Name:          "test-worker",
		BackoffMin:    10 * time.Millisecond,
		BackoffMax:    20 * time.Millisecond,
		BackoffJitter: &jitter,
		StableAfter:   50 * time.Millisecond,
		BurstWindow:   time.Second,
		BurstMax:      1000, // disabled by default for most tests
		OnBurstExit:   &false_,
		GracePeriod:   200 * time.Millisecond,
		EventLog:      buf,
		WorkerStdout:  &bytes.Buffer{},
		WorkerStderr:  &bytes.Buffer{},
		Clock:         clk,
	}
}

func TestRun_CrashRestartsThenShutdown(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("exit1")

	ctx, cancel := context.WithCancel(context.Background())
	// Let it crash a couple of times then cancel.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx, spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	events := parseEvents(t, &buf)
	starts := filterEvents(events, EventStart)
	exits := filterEvents(events, EventExit)
	if len(starts) < 2 {
		t.Fatalf("expected ≥2 starts, got %d", len(starts))
	}
	if len(exits) < 2 {
		t.Fatalf("expected ≥2 exits, got %d", len(exits))
	}
	// The last exit may be a SIGTERM-on-shutdown race where the child gets
	// killed before its os.Exit(1) runs. Only assert on completed exits.
	for i, e := range exits {
		if e.Reason == "shutdown" || e.Reason == "signaled" {
			continue
		}
		if e.ExitCode == nil || *e.ExitCode != 1 {
			code := "<nil>"
			if e.ExitCode != nil {
				code = strconv.Itoa(*e.ExitCode)
			}
			t.Errorf("exit[%d] code = %s reason=%q, want exit code 1", i, code, e.Reason)
		}
	}
	if len(filterEvents(events, EventBackoff)) < 1 {
		t.Errorf("expected backoff events")
	}
	if len(filterEvents(events, EventShutdown)) != 1 {
		t.Errorf("expected one shutdown event")
	}
}

func TestRun_GracefulShutdownOnSIGTERM(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("hang-until-sigterm")
	spec.GracePeriod = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx, spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	events := parseEvents(t, &buf)
	exits := filterEvents(events, EventExit)
	if len(exits) != 1 {
		t.Fatalf("expected 1 exit, got %d", len(exits))
	}
	if exits[0].ExitCode == nil || *exits[0].ExitCode != 0 {
		t.Errorf("expected clean exit 0 after SIGTERM, got code=%v signal=%q", exits[0].ExitCode, exits[0].Signal)
	}
}

func TestRun_SIGKILLAfterGracePeriod(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("hang-ignore-sigterm")
	spec.GracePeriod = 250 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx, spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	events := parseEvents(t, &buf)
	exits := filterEvents(events, EventExit)
	if len(exits) != 1 {
		t.Fatalf("expected 1 exit, got %d", len(exits))
	}
	if exits[0].Signal != syscall.SIGKILL.String() {
		t.Errorf("expected SIGKILL, got %q", exits[0].Signal)
	}
}

func TestRun_StableResetsBackoff(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	// Sleep child for longer than StableAfter so we mark stable.
	spec.Cmd, spec.Args, spec.Env = childCmd("sleep-then-exit0", "CHILD_SLEEP=100ms")
	spec.StableAfter = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = Run(ctx, spec)

	events := parseEvents(t, &buf)
	if len(filterEvents(events, EventStable)) == 0 {
		t.Fatalf("expected at least one stable event\nall events: %+v", events)
	}
}

func TestRun_BurstExitTriggers(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("exit1")
	spec.BurstMax = 3
	spec.BurstWindow = 5 * time.Second
	true_ := true
	spec.OnBurstExit = &true_

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, spec)
	if !errors.Is(err, ErrBurstExceeded) {
		t.Fatalf("want ErrBurstExceeded, got %v", err)
	}
	events := parseEvents(t, &buf)
	if len(filterEvents(events, EventBurstExit)) != 1 {
		t.Fatalf("expected 1 burst_exit event\nevents: %+v", events)
	}
}

func TestRun_PreStartFailureCountsAsBurst(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("exit0")
	// A pre-start hook that always fails (false on POSIX).
	spec.PreStart = []Hook{{Argv: []string{"/bin/false"}}}
	spec.BurstMax = 2
	spec.BurstWindow = 5 * time.Second
	true_ := true
	spec.OnBurstExit = &true_

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, spec)
	if !errors.Is(err, ErrBurstExceeded) {
		t.Fatalf("want ErrBurstExceeded, got %v", err)
	}
	events := parseEvents(t, &buf)
	ps := filterEvents(events, EventPreStart)
	if len(ps) < 2 {
		t.Fatalf("expected ≥2 prestart events\nevents: %+v", events)
	}
	for i, e := range ps {
		if e.ExitCode == nil || *e.ExitCode == 0 {
			t.Errorf("prestart[%d] expected non-zero exit, got %v", i, e.ExitCode)
		}
	}
	// No worker should have launched.
	if got := filterEvents(events, EventStart); len(got) != 0 {
		t.Errorf("expected zero start events, got %d", len(got))
	}
}

func TestRun_PostExitHookRunsWithWorkerEnv(t *testing.T) {
	var buf bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("exit0")

	// Use a hook that writes its env to a file we can inspect.
	tmp, err := os.CreateTemp(t.TempDir(), "posthook-*.env")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	script := fmt.Sprintf("#!/bin/sh\nenv | grep ^WORKER_ > %s\n", tmp.Name())
	hookPath := tmp.Name() + ".sh"
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	spec.PostExit = []Hook{{Argv: []string{hookPath}}}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = Run(ctx, spec)

	got, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	// The script truncates the file each invocation, so we see only the
	// last hook run's env. Assert the env var keys are present; the value
	// may be the shutdown iteration's (e.g. WORKER_EXIT_CODE=-1).
	needles := []string{"WORKER_EXIT_CODE=", "WORKER_PID=", "WORKER_ATTEMPT=", "WORKER_DURATION_MS="}
	for _, n := range needles {
		if !strings.Contains(string(got), n) {
			t.Errorf("hook env missing %q\nfile=\n%s", n, string(got))
		}
	}
}

func TestRun_WorkerStdoutForwarded(t *testing.T) {
	var buf bytes.Buffer
	var stdout bytes.Buffer
	spec := baseSpec(&buf, newFakeClock())
	spec.Cmd, spec.Args, spec.Env = childCmd("write-stdout")
	spec.WorkerStdout = &stdout

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = Run(ctx, spec)

	if !strings.Contains(stdout.String(), "hello from child") {
		t.Errorf("expected child stdout, got %q", stdout.String())
	}
}

func TestRun_RejectsEmptyCmd(t *testing.T) {
	err := Run(context.Background(), Spec{})
	if err == nil {
		t.Fatal("expected error for empty Cmd")
	}
	if !strings.Contains(err.Error(), "Cmd") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Sanity: parse a duration into a string and back to verify the suite runs
// when triggered by the real go test entry (not the re-exec path).
func TestSanity(t *testing.T) {
	_, err := strconv.Atoi("1")
	if err != nil {
		t.Fatal(err)
	}
}
