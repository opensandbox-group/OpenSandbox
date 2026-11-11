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

// Package resources reports sandbox-scoped CPU and memory usage.
//
// On Linux, /proc/stat and /proc/meminfo are not virtualized per cgroup:
// inside a container they describe the host. When the sandbox runs in a
// cgroup with configured resource limits, this package therefore reads the
// sandbox's own cgroup instead, so the numbers reflect what the sandbox can
// actually use. When no such cgroup is visible — for example execd running
// directly in a microVM guest, where /proc already describes the whole
// machine — it falls back to /proc through gopsutil.
package resources

import (
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
)

// MemoryUsage is a sandbox memory snapshot.
type MemoryUsage struct {
	// UsedBytes is the memory currently charged to the sandbox.
	UsedBytes int64
	// TotalBytes is the sandbox memory capacity: the cgroup limit when one
	// is configured, otherwise the host (or VM) total.
	TotalBytes int64
}

// backend is the active data source, selected once per process: Linux
// prefers the sandbox cgroup when it carries a real resource limit,
// everything else reads /proc through gopsutil.
type backend interface {
	// cpuPercent reports utilization percent relative to cpuCount().
	cpuPercent(interval time.Duration) (float64, error)
	cpuCount() float64
	memoryStats() (MemoryUsage, error)
}

var defaultBackend = sync.OnceValue(newBackend)

// CPUPercent returns the sandbox CPU utilization in percent, relative to the
// capacity reported by CPUCount. With interval > 0 the call blocks and
// samples usage at both ends of the window; with interval <= 0 it returns
// the usage accumulated since the previous call (the first call reports 0).
func CPUPercent(interval time.Duration) (float64, error) {
	return defaultBackend().cpuPercent(interval)
}

// CPUCount returns the sandbox's effective CPU capacity in cores: the cgroup
// quota rounded up when a quota is configured, otherwise the number of cores
// visible to the process.
func CPUCount() float64 {
	return defaultBackend().cpuCount()
}

// MemoryStats returns a sandbox memory snapshot.
func MemoryStats() (MemoryUsage, error) {
	return defaultBackend().memoryStats()
}

// hostBackend reads /proc through gopsutil. In a microVM guest /proc already
// describes the whole machine, which is exactly the sandbox scope.
type hostBackend struct{}

func (hostBackend) cpuPercent(interval time.Duration) (float64, error) {
	usage, err := cpu.Percent(interval, false)
	if err != nil {
		return 0, err
	}
	if len(usage) == 0 {
		return 0, nil
	}
	return usage[0], nil
}

func (hostBackend) cpuCount() float64 {
	return float64(runtime.GOMAXPROCS(-1))
}

func (hostBackend) memoryStats() (MemoryUsage, error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return MemoryUsage{}, err
	}
	return MemoryUsage{UsedBytes: int64(stats.Used), TotalBytes: int64(stats.Total)}, nil
}

// cpuDeltaPercent converts a cumulative CPU-time delta (microseconds)
// against a wall-clock window into a percent of the sandbox CPU capacity.
func cpuDeltaPercent(prevUSec, curUSec uint64, wall time.Duration, capacity float64) float64 {
	if wall <= 0 || capacity <= 0 || curUSec <= prevUSec {
		return 0
	}
	usedCores := float64(curUSec-prevUSec) / 1e6 / wall.Seconds()
	return usedCores / capacity * 100
}
