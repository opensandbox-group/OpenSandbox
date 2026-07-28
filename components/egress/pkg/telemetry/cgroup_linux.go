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

//go:build linux

package telemetry

import (
	"os"
	"path/filepath"
)

// cgroupRoot is where the container's own cgroup is mounted. With a cgroup namespace —
// the default for containerd and CRI-O on cgroup v2 — this path is the container's cgroup
// root, so the values below describe the sidecar and not the node. Overridden in tests.
var cgroupRoot = "/sys/fs/cgroup"

// processMemoryUsageBytes returns the sidecar's own memory usage, trying cgroup v2 before
// v1. The bool is false when neither layout is readable, which is a real possibility: a
// sandbox pod may run under a runtime that does not expose cgroupfs (see secure_runtime),
// and the caller must then skip the metric rather than publish a zero.
func processMemoryUsageBytes() (int64, bool) {
	if data, err := os.ReadFile(filepath.Join(cgroupRoot, "memory.current")); err == nil {
		if value, ok := cgroupSingleValueBytes(data); ok {
			return value, true
		}
	}
	if data, err := os.ReadFile(filepath.Join(cgroupRoot, "memory", "memory.usage_in_bytes")); err == nil {
		if value, ok := cgroupSingleValueBytes(data); ok {
			return value, true
		}
	}
	return 0, false
}

// processCPUTimeSeconds returns the CPU time the sidecar has consumed, cgroup v2 first.
// Cumulative on purpose: a counter of consumed seconds composes with rate() and does not
// depend on the exporter's sampling interval, unlike a sampled utilisation ratio.
func processCPUTimeSeconds() (float64, bool) {
	if data, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.stat")); err == nil {
		if seconds, ok := cgroupCPUSecondsFromStat(data); ok {
			return seconds, true
		}
	}
	if data, err := os.ReadFile(filepath.Join(cgroupRoot, "cpuacct", "cpuacct.usage")); err == nil {
		if seconds, ok := cgroupCPUSecondsFromNanos(data); ok {
			return seconds, true
		}
	}
	return 0, false
}
