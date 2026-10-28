//go:build linux

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

package telemetry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// smokePollInterval speeds the smoke up; production uses oomPollInterval.
const smokePollInterval = 300 * time.Millisecond

// TestOOMSmoke is an end-to-end check of the OOM watcher: it creates a
// memory-limited child cgroup, runs a helper process that joins the cgroup
// and allocates until the kernel OOM killer terminates it, and asserts the
// watcher emits the detection log line. It configures the real root cgroup
// (exempt from the no-internal-process rule), so it works on VMs/CI runners
// but not inside a private-cgroup-namespace container, where enabling
// +memory fails with EBUSY. Missing privileges or cgroup v2 skip the test;
// setup failures are fatal so a silent no-op cannot pass CI. CI runs it
// under sudo (see .github/workflows/execd-test.yml).
func TestOOMSmoke(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root is required to create a memory-limited cgroup")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroup v2 not available: %v", err)
	}

	dir := filepath.Join("/sys/fs/cgroup", "opensandbox-oom-smoke-"+strconv.Itoa(os.Getpid()))
	_ = os.RemoveAll(dir) // stale leftover from a crashed run
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create cgroup %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := enableMemorySubtreeControl(); err != nil {
		t.Fatalf("delegate memory controller to children: %v", err)
	}
	// Well above the test binary's own usage, far below the helper's runaway
	// allocation, so only the helper can hit the limit.
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("300M"), 0o644); err != nil {
		t.Fatalf("set memory.max: %v", err)
	}

	// Capture execd logs: the smoke asserts on the watcher's log line.
	// Register the logger restore before t.Setenv so the env is unset by the
	// time the restore runs (log.Init reads the env).
	t.Cleanup(func() { log.Init(6) })
	logFile := filepath.Join(t.TempDir(), "execd.log")
	t.Setenv("EXECD_LOG_FILE", logFile)
	log.Init(6)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchOOMSource(ctx, &oomSource{
			mode:       "cgroup-v2",
			cgroupPath: "/" + filepath.Base(dir),
			dir:        dir,
			eventsPath: filepath.Join(dir, "memory.events"),
			localPath:  filepath.Join(dir, "memory.events.local"),
		}, smokePollInterval)
	}()
	defer func() { cancel(); <-done }()

	// Let the watcher take its baseline sample first: a kill that lands
	// before the baseline is swallowed as part of it.
	time.Sleep(2 * smokePollInterval)

	cmd := exec.Command(os.Args[0], "-test.run=^TestOOMSmokeHelperProcess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		"OOM_SMOKE_HELPER=1",
		"OOM_SMOKE_CGROUP="+dir,
	)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("helper exited cleanly; OOM did not trigger\noutput:\n%s", out)
	} else {
		t.Logf("helper terminated as expected: %v\noutput:\n%s", err, out)
	}

	waitForOOMLog(t, logFile, 30*time.Second)
}

// TestOOMSmokeHelperProcess is the allocation bomb started by TestOOMSmoke as
// a separate test-binary process; it is inert in normal test runs.
func TestOOMSmokeHelperProcess(t *testing.T) {
	if os.Getenv("OOM_SMOKE_HELPER") == "" {
		t.Skip("helper process for TestOOMSmoke")
	}
	dir := os.Getenv("OOM_SMOKE_CGROUP")
	if dir == "" {
		t.Fatal("OOM_SMOKE_CGROUP is not set")
	}
	// Move this process into the limited cgroup before allocating, so the
	// very first allocations are already accounted against memory.max.
	procs := filepath.Join(dir, "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("join cgroup %s: %v", dir, err)
	}

	var sink [][]byte
	for i := 0; i < 4096; i++ {
		chunk := make([]byte, 16<<20)
		for j := 0; j < len(chunk); j += 4096 {
			chunk[j] = 1
		}
		sink = append(sink, chunk)
		// Stretch the allocation out over several watcher ticks so the pid
		// tracker observes this process alive before the kernel kills it.
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("helper survived %d chunks (%d MiB); OOM did not trigger", len(sink), len(sink)*16)
}

// waitForOOMLog polls the captured log until the watcher reports the kill,
// including the exited-process attribution: the helper lives for several
// ticks before dying, so the tracker must have its details on record.
func waitForOOMLog(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "oom: detected") {
			t.Logf("OOM detection log received:\n%s", data)
			if !strings.Contains(string(data), "OOM kill") {
				t.Fatalf("unexpected detection log format:\n%s", data)
			}
			if !strings.Contains(string(data), "exited=[pid=") {
				t.Fatalf("detection log is missing exited-process attribution:\n%s", data)
			}
			return
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(path)
			t.Fatalf("no OOM detection log within %s; captured logs:\n%s", timeout, data)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// enableMemorySubtreeControl enables the memory controller in the root
// cgroup's subtree when it is not already delegated to children.
func enableMemorySubtreeControl() error {
	data, err := os.ReadFile("/sys/fs/cgroup/cgroup.subtree_control")
	if err != nil {
		return err
	}
	if strings.Contains(string(data), "memory") {
		return nil
	}
	return os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory"), 0o644)
}
