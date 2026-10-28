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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/cgroupfs"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// Test-overridable input paths.
var (
	procSelfCgroupFile  = "/proc/self/cgroup"
	cgroupV2Dir         = "/sys/fs/cgroup"
	cgroupV1MemoryDir   = "/sys/fs/cgroup/memory"
	cgroupV1CPUAcctDirs = []string{"/sys/fs/cgroup/cpu,cpuacct", "/sys/fs/cgroup/cpuacct"}
	cgroupV1QuotaDirs   = []string{"/sys/fs/cgroup/cpu,cpuacct", "/sys/fs/cgroup/cpu"}
)

// errNoLimitedCgroup: no visible cgroup carries a configured resource limit,
// so the cgroup cannot describe a sandbox resource boundary.
var errNoLimitedCgroup = errors.New("no sandbox cgroup with configured resource limits")

// newBackend prefers the sandbox cgroup and falls back to the host view:
// without a configured limit the cgroup does not represent the sandbox
// boundary, and in a microVM guest the host view is the sandbox. The
// fallback is logged: a wrongly selected host view reports host-wide
// numbers, which is exactly what this package exists to avoid.
func newBackend() backend {
	b, err := newCgroupBackend()
	if err == nil {
		return b
	}
	log.Warn("resources: sandbox cgroup unavailable, using host /proc stats: %v", err)
	return hostBackend{}
}

func newCgroupBackend() (backend, error) {
	data, err := os.ReadFile(procSelfCgroupFile)
	if err != nil {
		return nil, err
	}
	entries := cgroupfs.ParseEntries(data)
	if rel := cgroupfs.V2RelPath(entries); rel != "" {
		if b, err := newCgroupV2Backend(rel); err == nil {
			log.Info("resources: reading sandbox stats from cgroup v2 %s", rel)
			return b, nil
		}
	}
	memRel := cgroupfs.V1RelPath(entries, "memory")
	cpuRel := cgroupfs.V1RelPath(entries, "cpu")
	cpuacctRel := cgroupfs.V1RelPath(entries, "cpuacct")
	if b, err := newCgroupV1Backend(memRel, cpuRel, cpuacctRel); err == nil {
		log.Info("resources: reading sandbox stats from cgroup v1 (memory=%q cpu=%q cpuacct=%q)",
			memRel, cpuRel, cpuacctRel)
		return b, nil
	}
	return nil, errNoLimitedCgroup
}

// cgroupBackend reads the sandbox's own cgroup interface files: the sandbox
// subtree's real CPU time, CPU quota, and memory usage/limit. CPU and
// memory degrade independently: whichever resource has no readable cgroup
// view falls back to the host numbers (see cpuPercent/memoryStats).
type cgroupBackend struct {
	usagePath    string  // cumulative CPU time file; empty when unavailable
	usageNS      bool    // usagePath counts nanoseconds (cgroup v1)
	quotaCores   float64 // >0 when a CPU quota is configured
	memUsedPath  string  // empty when unavailable
	memStatPath  string  // memory.stat, for the working-set computation
	memLimitPath string  // empty when no limit file is available
	memV1        bool    // memLimitPath uses the v1 huge-value encoding

	lastMu   sync.Mutex
	lastUSec uint64
	lastWall time.Time
	haveLast bool
}

func newCgroupV2Backend(relPath string) (backend, error) {
	mount, err := cgroupV2Mount()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(mount, filepath.FromSlash(relPath))
	b := &cgroupBackend{}

	// CPU side: cpu.stat is the usage source, cpu.max carries the quota.
	usagePath := filepath.Join(dir, "cpu.stat")
	if fileExists(usagePath) {
		if quotaData, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
			if quota, period, limited, ok := parseCPUMax(quotaData); ok {
				b.usagePath = usagePath
				if limited {
					b.quotaCores = quotaCores(quota, period)
				}
			}
		}
	}

	// Memory side: memory.stat backs the working-set computation.
	memUsedPath := filepath.Join(dir, "memory.current")
	memLimitPath := filepath.Join(dir, "memory.max")
	if fileExists(memUsedPath) && fileExists(memLimitPath) {
		b.memUsedPath, b.memLimitPath = memUsedPath, memLimitPath
		if statPath := filepath.Join(dir, "memory.stat"); fileExists(statPath) {
			b.memStatPath = statPath
		}
	}

	if err := b.validate(); err != nil {
		return nil, err
	}
	b.seedCPU()
	return b, nil
}

func newCgroupV1Backend(memRel, cpuRel, cpuacctRel string) (backend, error) {
	b := &cgroupBackend{}

	if memRel != "" {
		if dir, err := cgroupV1MountDir([]string{cgroupV1MemoryDir}, "memory"); err == nil {
			memDir := filepath.Join(dir, filepath.FromSlash(memRel))
			usedPath := filepath.Join(memDir, "memory.usage_in_bytes")
			limitPath := filepath.Join(memDir, "memory.limit_in_bytes")
			if fileExists(usedPath) && fileExists(limitPath) {
				b.memUsedPath, b.memLimitPath, b.memV1 = usedPath, limitPath, true
				if statPath := filepath.Join(memDir, "memory.stat"); fileExists(statPath) {
					b.memStatPath = statPath
				}
			}
		}
	}

	if cpuacctRel != "" {
		// cgroup v1 may mount cpuacct separately from cpu; discover the
		// accounting and quota files independently so a split-mount host
		// still reports cgroup-scoped CPU (same approach as the egress
		// sidecar's cgroup reader).
		if dir, err := cgroupV1MountDir(cgroupV1CPUAcctDirs, "cpuacct"); err == nil {
			usagePath := filepath.Join(dir, filepath.FromSlash(cpuacctRel), "cpuacct.usage")
			if fileExists(usagePath) {
				b.usagePath, b.usageNS = usagePath, true
				if cpuRel != "" {
					if quotaDir, err := cgroupV1MountDir(cgroupV1QuotaDirs, "cpu"); err == nil {
						b.quotaCores = v1QuotaCores(filepath.Join(quotaDir, filepath.FromSlash(cpuRel)))
					}
				}
			}
		}
	}

	if err := b.validate(); err != nil {
		return nil, err
	}
	b.seedCPU()
	return b, nil
}

// validate requires at least one readable resource side and at least one
// real limit. Without any limit the cgroup does not bound the sandbox: it
// cannot distinguish a constrained container from an unconstrained VM
// guest, so the caller should use the host view instead.
func (b *cgroupBackend) validate() error {
	if b.usagePath == "" && b.memUsedPath == "" {
		return errNoLimitedCgroup
	}
	if !b.hasAnyLimit() {
		return errNoLimitedCgroup
	}
	return nil
}

// hasAnyLimit reports whether a real CPU quota or memory limit is
// configured.
func (b *cgroupBackend) hasAnyLimit() bool {
	if b.quotaCores > 0 {
		return true
	}
	if b.memLimitPath == "" {
		return false
	}
	data, err := os.ReadFile(b.memLimitPath)
	if err != nil {
		return false
	}
	if b.memV1 {
		_, limited, _ := parseV1MemoryLimit(data)
		return limited
	}
	_, limited, _ := parseMemoryMax(data)
	return limited
}

// seedCPU primes the since-last-call baseline so the first non-blocking
// sample reports the interval since construction instead of 0.
func (b *cgroupBackend) seedCPU() {
	if b.usagePath == "" {
		return
	}
	uSec, err := b.cpuUSec()
	if err != nil {
		return
	}
	b.lastMu.Lock()
	defer b.lastMu.Unlock()
	b.lastUSec, b.lastWall, b.haveLast = uSec, time.Now(), true
}

func (b *cgroupBackend) cpuPercent(interval time.Duration) (float64, error) {
	if b.usagePath == "" {
		// Per-resource degradation: this backend is memory-only (e.g. a v1
		// host with a memory limit but no visible cpu/cpuacct hierarchy).
		return hostBackend{}.cpuPercent(interval)
	}
	capacity := b.cpuCount()
	if interval > 0 {
		// Measure the real window: time.Sleep only guarantees a lower
		// bound, and a saturated machine (exactly what these metrics
		// observe) can delay the wake-up. A nominal denominator would
		// inflate percentages past 100%.
		start := time.Now()
		prev, err := b.cpuUSec()
		if err != nil {
			return hostBackend{}.cpuPercent(interval)
		}
		time.Sleep(interval)
		cur, err := b.cpuUSec()
		if err != nil {
			// The window is already spent; take a cheap degraded sample
			// instead of sleeping again.
			return hostBackend{}.cpuPercent(0)
		}
		return cpuDeltaPercent(prev, cur, time.Since(start), capacity), nil
	}
	b.lastMu.Lock()
	defer b.lastMu.Unlock()
	cur, err := b.cpuUSec()
	if err != nil {
		// Degrade to the host view (e.g. cgroup files gone during sandbox
		// teardown) and drop the stale baseline so a recovered cgroup view
		// starts a fresh interval.
		b.haveLast = false
		return hostBackend{}.cpuPercent(0)
	}
	now := time.Now()
	if !b.haveLast {
		b.lastUSec, b.lastWall, b.haveLast = cur, now, true
		return 0, nil
	}
	percent := cpuDeltaPercent(b.lastUSec, cur, now.Sub(b.lastWall), capacity)
	b.lastUSec, b.lastWall = cur, now
	return percent, nil
}

func (b *cgroupBackend) cpuCount() float64 {
	if b.quotaCores > 0 {
		return b.quotaCores
	}
	return float64(runtime.GOMAXPROCS(-1))
}

func (b *cgroupBackend) memoryStats() (MemoryUsage, error) {
	if b.memUsedPath == "" {
		return hostBackend{}.memoryStats()
	}
	data, err := os.ReadFile(b.memUsedPath)
	if err != nil {
		// Degrade to the host view instead of erroring: the cgroup view can
		// disappear while the sandbox is being torn down.
		return hostBackend{}.memoryStats()
	}
	used, ok := parseSingleInt(data)
	if !ok || used < 0 {
		return hostBackend{}.memoryStats()
	}
	used = b.workingSet(used)
	if b.memLimitPath != "" {
		if limitData, err := os.ReadFile(b.memLimitPath); err == nil {
			var limit int64
			var limited bool
			if b.memV1 {
				limit, limited, _ = parseV1MemoryLimit(limitData)
			} else {
				limit, limited, _ = parseMemoryMax(limitData)
			}
			if limited {
				return MemoryUsage{UsedBytes: used, TotalBytes: max(limit, 0)}, nil
			}
		}
	}
	// No configured limit: capacity is the host (or VM) total.
	host, err := hostBackend{}.memoryStats()
	if err != nil {
		return MemoryUsage{}, err
	}
	return MemoryUsage{UsedBytes: used, TotalBytes: host.TotalBytes}, nil
}

// workingSet subtracts reclaimable file cache from raw cgroup usage:
// memory.current charges page cache that the kernel reclaims before OOM, so
// an I/O-heavy sandbox (package installs, builds, clones) would otherwise
// read as memory pressure. v2 exposes inactive_file, v1 the hierarchical
// total_inactive_file — the same approximation kubelet uses for
// working-set. When the stat file is unreadable the raw usage is kept
// (conservative).
func (b *cgroupBackend) workingSet(used int64) int64 {
	if b.memStatPath == "" {
		return used
	}
	data, err := os.ReadFile(b.memStatPath)
	if err != nil {
		return used
	}
	field := "inactive_file"
	if b.memV1 {
		field = "total_inactive_file"
	}
	cache, ok := parseMemoryStatField(data, field)
	if !ok || cache <= 0 {
		return used
	}
	return max(used-cache, 0)
}

// cpuUSec reads the cumulative sandbox CPU time in microseconds.
func (b *cgroupBackend) cpuUSec() (uint64, error) {
	data, err := os.ReadFile(b.usagePath)
	if err != nil {
		return 0, err
	}
	var raw uint64
	var ok bool
	if b.usageNS {
		if raw, ok = parseSingleUint(data); !ok {
			return 0, fmt.Errorf("unparseable CPU usage file %s", b.usagePath)
		}
		raw /= 1000 // ns -> µs
		return raw, nil
	}
	if raw, ok = parseCPUStatUsageUSec(data); !ok {
		return 0, fmt.Errorf("unparseable CPU usage file %s", b.usagePath)
	}
	return raw, nil
}

// v1QuotaCores reads the v1 CFS quota files of a cpu controller directory
// and returns the configured core count (0 when unlimited or unreadable).
func v1QuotaCores(cpuDir string) float64 {
	quotaData, err := os.ReadFile(filepath.Join(cpuDir, "cpu.cfs_quota_us"))
	if err != nil {
		return 0
	}
	periodData, err := os.ReadFile(filepath.Join(cpuDir, "cpu.cfs_period_us"))
	if err != nil {
		return 0
	}
	quota, limited, ok := parseV1QuotaUS(quotaData)
	if !ok || !limited {
		return 0
	}
	period, ok := parseSingleInt(periodData)
	if !ok || period <= 0 {
		return 0
	}
	return quotaCores(quota, period)
}

// cgroupV2Mount locates the cgroup v2 filesystem mount.
func cgroupV2Mount() (string, error) {
	if fileExists(filepath.Join(cgroupV2Dir, "cgroup.controllers")) {
		return cgroupV2Dir, nil
	}
	return cgroupfs.ScanMounts(func(_ string, fstype string, _ []string) bool {
		return fstype == "cgroup2"
	})
}

// cgroupV1MountDir locates a v1 controller hierarchy mount: the default
// directories first, then /proc/self/mounts by controller name.
func cgroupV1MountDir(defaultDirs []string, controllers ...string) (string, error) {
	for _, dir := range defaultDirs {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}
	return cgroupfs.ScanMounts(func(_ string, fstype string, options []string) bool {
		if fstype != "cgroup" {
			return false
		}
		for _, controller := range controllers {
			if cgroupfs.OptionsHave(options, controller) {
				return true
			}
		}
		return false
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
