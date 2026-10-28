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

package resources

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// overridePaths points the discovery inputs at a fake cgroup tree and
// restores the originals after the test.
func overridePaths(t *testing.T, selfCgroup string, v2Dir, v1MemoryDir string, v1CPUAcctDirs, v1QuotaDirs []string) {
	t.Helper()
	origCgroup := procSelfCgroupFile
	origV2 := cgroupV2Dir
	origV1Mem := cgroupV1MemoryDir
	origV1Acct, origV1Quota := cgroupV1CPUAcctDirs, cgroupV1QuotaDirs
	cgroupFile := filepath.Join(t.TempDir(), "self-cgroup")
	if err := os.WriteFile(cgroupFile, []byte(selfCgroup), 0o644); err != nil {
		t.Fatal(err)
	}
	procSelfCgroupFile = cgroupFile
	cgroupV2Dir, cgroupV1MemoryDir = v2Dir, v1MemoryDir
	cgroupV1CPUAcctDirs, cgroupV1QuotaDirs = v1CPUAcctDirs, v1QuotaDirs
	t.Cleanup(func() {
		procSelfCgroupFile = origCgroup
		cgroupV2Dir, cgroupV1MemoryDir = origV2, origV1Mem
		cgroupV1CPUAcctDirs, cgroupV1QuotaDirs = origV1Acct, origV1Quota
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewCgroupBackendV2(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox")
	writeFile(t, filepath.Join(root, "cgroup.controllers"), "")
	writeFile(t, filepath.Join(sandbox, "cpu.stat"), "usage_usec 1000000\nuser_usec 10\nsystem_usec 10\n")
	writeFile(t, filepath.Join(sandbox, "cpu.max"), "100000 100000\n")
	writeFile(t, filepath.Join(sandbox, "memory.current"), "52428800\n")
	writeFile(t, filepath.Join(sandbox, "memory.max"), "209715200\n")
	writeFile(t, filepath.Join(sandbox, "memory.stat"), "anon 8388608\nfile 44040192\ninactive_file 4194304\n")
	overridePaths(t, "0::/sandbox\n", root, "", nil, nil)

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}
	cg, ok := backend.(*cgroupBackend)
	if !ok {
		t.Fatalf("backend = %T, want *cgroupBackend", backend)
	}
	if got := cg.cpuCount(); got != 1 {
		t.Fatalf("cpuCount = %v, want 1 (quota 100000/100000)", got)
	}
	// Working set: raw usage minus reclaimable file cache (kubelet-style).
	stats, err := backend.memoryStats()
	if err != nil {
		t.Fatalf("memoryStats: %v", err)
	}
	if want := int64(52428800 - 4194304); stats.UsedBytes != want || stats.TotalBytes != 209715200 {
		t.Fatalf("memoryStats = %+v, want used=%d total=209715200", stats, want)
	}

	// Advance the cumulative CPU time by 0.05 core-seconds in the middle of
	// the 100ms sampling window: half a core on a 1-core quota = ~50%.
	statPath := filepath.Join(sandbox, "cpu.stat")
	bumped := make(chan struct{})
	go func() {
		defer close(bumped)
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(statPath, []byte("usage_usec 1050000\nuser_usec 10\nsystem_usec 10\n"), 0o644)
	}()
	percent, err := backend.cpuPercent(100 * time.Millisecond)
	<-bumped
	if err != nil {
		t.Fatalf("cpuPercent: %v", err)
	}
	if percent < 25 || percent > 150 {
		t.Fatalf("cpuPercent = %v, want ~50 (tolerance for scheduling jitter)", percent)
	}
}

func TestNewCgroupBackendV2UnlimitedFallsBackToHost(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox")
	writeFile(t, filepath.Join(root, "cgroup.controllers"), "")
	writeFile(t, filepath.Join(sandbox, "cpu.stat"), "usage_usec 1000000\n")
	writeFile(t, filepath.Join(sandbox, "cpu.max"), "max 100000\n")
	writeFile(t, filepath.Join(sandbox, "memory.current"), "52428800\n")
	writeFile(t, filepath.Join(sandbox, "memory.max"), "max\n")
	overridePaths(t, "0::/sandbox\n", root, "", nil, nil)

	if _, err := newCgroupBackend(); err != errNoLimitedCgroup {
		t.Fatalf("newCgroupBackend err = %v, want %v", err, errNoLimitedCgroup)
	}
	host := newBackend()
	if _, ok := host.(hostBackend); !ok {
		t.Fatalf("newBackend = %T, want hostBackend", host)
	}
}

func TestNewCgroupBackendV1(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.usage_in_bytes"), "104857600\n")
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.limit_in_bytes"), "2147483648\n")
	writeFile(t, filepath.Join(root, "cpu,cpuacct", "sandbox", "cpuacct.usage"), "50000000\n")
	writeFile(t, filepath.Join(root, "cpu,cpuacct", "sandbox", "cpu.cfs_quota_us"), "200000\n")
	writeFile(t, filepath.Join(root, "cpu,cpuacct", "sandbox", "cpu.cfs_period_us"), "100000\n")
	overridePaths(t, "12:memory:/sandbox\n3:cpu,cpuacct:/sandbox\n",
		"", filepath.Join(root, "memory"),
		[]string{filepath.Join(root, "cpu,cpuacct")}, []string{filepath.Join(root, "cpu,cpuacct")})

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}
	if got := backend.cpuCount(); got != 2 {
		t.Fatalf("cpuCount = %v, want 2 (quota 200000/100000)", got)
	}
	stats, err := backend.memoryStats()
	if err != nil {
		t.Fatalf("memoryStats: %v", err)
	}
	if stats.UsedBytes != 104857600 || stats.TotalBytes != 2147483648 {
		t.Fatalf("memoryStats = %+v, want used=104857600 total=2147483648", stats)
	}
	cg, ok := backend.(*cgroupBackend)
	if !ok {
		t.Fatalf("backend = %T, want *cgroupBackend", backend)
	}
	uSec, err := cg.cpuUSec()
	if err != nil {
		t.Fatalf("cpuUSec: %v", err)
	}
	if uSec != 50000 { // 50000000 ns -> 50000 µs
		t.Fatalf("cpuUSec = %d, want 50000", uSec)
	}
}

func TestNewCgroupBackendV1SplitCPUMounts(t *testing.T) {
	// Some v1 hosts mount cpu and cpuacct as separate hierarchies; the
	// accounting file and the quota files must be discovered independently.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cpu", "sandbox", "cpu.cfs_quota_us"), "200000\n")
	writeFile(t, filepath.Join(root, "cpu", "sandbox", "cpu.cfs_period_us"), "100000\n")
	writeFile(t, filepath.Join(root, "cpuacct", "sandbox", "cpuacct.usage"), "50000000\n")
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.usage_in_bytes"), "104857600\n")
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.limit_in_bytes"), "2147483648\n")
	overridePaths(t, "3:cpu:/sandbox\n4:cpuacct:/sandbox\n12:memory:/sandbox\n",
		"", filepath.Join(root, "memory"),
		[]string{filepath.Join(root, "cpuacct")}, []string{filepath.Join(root, "cpu")})

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}
	if got := backend.cpuCount(); got != 2 {
		t.Fatalf("cpuCount = %v, want 2 (quota read from the cpu mount)", got)
	}
	cg, ok := backend.(*cgroupBackend)
	if !ok {
		t.Fatalf("backend = %T, want *cgroupBackend", backend)
	}
	uSec, err := cg.cpuUSec()
	if err != nil {
		t.Fatalf("cpuUSec: %v", err)
	}
	if uSec != 50000 { // 50000000 ns -> 50000 µs, read from the cpuacct mount
		t.Fatalf("cpuUSec = %d, want 50000", uSec)
	}
}

func TestNewCgroupBackendV1UnlimitedFallsBackToHost(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.usage_in_bytes"), "104857600\n")
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.limit_in_bytes"), "9223372036854771712\n")
	writeFile(t, filepath.Join(root, "cpu,cpuacct", "sandbox", "cpuacct.usage"), "50000000\n")
	writeFile(t, filepath.Join(root, "cpu,cpuacct", "sandbox", "cpu.cfs_quota_us"), "-1\n")
	writeFile(t, filepath.Join(root, "cpu,cpuacct", "sandbox", "cpu.cfs_period_us"), "100000\n")
	overridePaths(t, "12:memory:/sandbox\n3:cpu,cpuacct:/sandbox\n",
		"", filepath.Join(root, "memory"),
		[]string{filepath.Join(root, "cpu,cpuacct")}, []string{filepath.Join(root, "cpu,cpuacct")})

	if _, err := newCgroupBackend(); err != errNoLimitedCgroup {
		t.Fatalf("newCgroupBackend err = %v, want %v", err, errNoLimitedCgroup)
	}
	if _, ok := newBackend().(hostBackend); !ok {
		t.Fatal("newBackend = cgroup backend, want hostBackend")
	}
}

func TestNewCgroupBackendV2WithoutMemoryStatKeepsRawUsage(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox")
	writeFile(t, filepath.Join(root, "cgroup.controllers"), "")
	writeFile(t, filepath.Join(sandbox, "cpu.stat"), "usage_usec 1000000\n")
	writeFile(t, filepath.Join(sandbox, "cpu.max"), "max 100000\n")
	writeFile(t, filepath.Join(sandbox, "memory.current"), "52428800\n")
	writeFile(t, filepath.Join(sandbox, "memory.max"), "209715200\n")
	overridePaths(t, "0::/sandbox\n", root, "", nil, nil)

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}
	stats, err := backend.memoryStats()
	if err != nil {
		t.Fatalf("memoryStats: %v", err)
	}
	if stats.UsedBytes != 52428800 {
		t.Fatalf("memoryStats.UsedBytes = %d, want raw 52428800", stats.UsedBytes)
	}
}

func TestV1MemoryOnlyBackendDegradesCPUToHost(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.usage_in_bytes"), "104857600\n")
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.stat"), "total_inactive_file 1048576\n")
	writeFile(t, filepath.Join(root, "memory", "sandbox", "memory.limit_in_bytes"), "2147483648\n")
	// The cpu/cpuacct hierarchy is not visible: the backend becomes
	// memory-only instead of failing construction.
	missing := filepath.Join(root, "missing-cpu")
	overridePaths(t, "12:memory:/sandbox\n",
		"", filepath.Join(root, "memory"), []string{missing}, []string{missing})

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}
	stats, err := backend.memoryStats()
	if err != nil {
		t.Fatalf("memoryStats: %v", err)
	}
	if want := int64(104857600 - 1048576); stats.UsedBytes != want {
		t.Fatalf("memoryStats.UsedBytes = %d, want working set %d", stats.UsedBytes, want)
	}
	percent, err := backend.cpuPercent(0)
	if err != nil {
		t.Fatalf("cpuPercent(0) should degrade to the host view, got error: %v", err)
	}
	if percent < 0 {
		t.Fatalf("cpuPercent(0) = %v, want >= 0", percent)
	}
	if _, err := backend.cpuPercent(10 * time.Millisecond); err != nil {
		t.Fatalf("cpuPercent(interval) should degrade to the host view, got error: %v", err)
	}
}

func TestCgroupTeardownDegradesToHost(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox")
	writeFile(t, filepath.Join(root, "cgroup.controllers"), "")
	writeFile(t, filepath.Join(sandbox, "cpu.stat"), "usage_usec 1000000\n")
	writeFile(t, filepath.Join(sandbox, "cpu.max"), "100000 100000\n")
	writeFile(t, filepath.Join(sandbox, "memory.current"), "52428800\n")
	writeFile(t, filepath.Join(sandbox, "memory.max"), "209715200\n")
	overridePaths(t, "0::/sandbox\n", root, "", nil, nil)

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}

	// Sandbox teardown removes the cgroup mid-run; the metrics endpoints
	// must keep answering with the host view instead of erroring forever.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove cgroup tree: %v", err)
	}
	percent, err := backend.cpuPercent(0)
	if err != nil {
		t.Fatalf("cpuPercent(0) after teardown = %v, want host fallback", err)
	}
	if percent < 0 {
		t.Fatalf("cpuPercent(0) = %v, want >= 0", percent)
	}
	if _, err := backend.cpuPercent(10 * time.Millisecond); err != nil {
		t.Fatalf("cpuPercent(interval) after teardown = %v, want host fallback", err)
	}
	if _, err := backend.memoryStats(); err != nil {
		t.Fatalf("memoryStats after teardown = %v, want host fallback", err)
	}
}

func TestCPUUSecSinceLastInterval(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox")
	writeFile(t, filepath.Join(root, "cgroup.controllers"), "")
	writeFile(t, filepath.Join(sandbox, "cpu.stat"), "usage_usec 1000000\n")
	writeFile(t, filepath.Join(sandbox, "cpu.max"), "100000 100000\n")
	writeFile(t, filepath.Join(sandbox, "memory.current"), "1\n")
	writeFile(t, filepath.Join(sandbox, "memory.max"), "4096\n")
	overridePaths(t, "0::/sandbox\n", root, "", nil, nil)

	backend, err := newCgroupBackend()
	if err != nil {
		t.Fatalf("newCgroupBackend: %v", err)
	}
	// First since-last-call reports 0, then the delta against the previous
	// call: 0.1 core-seconds in a 100ms window = 1 saturated core on a
	// 1-core quota.
	first, err := backend.cpuPercent(0)
	if err != nil {
		t.Fatalf("cpuPercent(0): %v", err)
	}
	if first != 0 {
		t.Fatalf("first cpuPercent(0) = %v, want 0", first)
	}
	writeFile(t, filepath.Join(sandbox, "cpu.stat"), "usage_usec 1100000\n")
	time.Sleep(100 * time.Millisecond)
	second, err := backend.cpuPercent(0)
	if err != nil {
		t.Fatalf("cpuPercent(0): %v", err)
	}
	if second < 50 || second > 200 {
		t.Fatalf("cpuPercent(0) = %v, want ~100 (tolerance for scheduling jitter)", second)
	}
}
