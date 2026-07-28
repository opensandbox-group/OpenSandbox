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
	"testing"
)

// withCgroupRoot points the readers at a fake cgroupfs for the duration of a test.
func withCgroupRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	previous := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = previous })
	return root
}

func TestProcessMetricsReadCgroupV2(t *testing.T) {
	withCgroupRoot(t, map[string]string{
		"memory.current": "8036352\n",
		"cpu.stat":       "usage_usec 1500000\nuser_usec 1000000\n",
	})

	memory, ok := processMemoryUsageBytes()
	if !ok || memory != 8036352 {
		t.Errorf("memory = %d, %v; want 8036352, true", memory, ok)
	}
	seconds, ok := processCPUTimeSeconds()
	if !ok || seconds != 1.5 {
		t.Errorf("cpu = %v, %v; want 1.5, true", seconds, ok)
	}
}

func TestProcessMetricsFallBackToCgroupV1(t *testing.T) {
	withCgroupRoot(t, map[string]string{
		"memory/memory.usage_in_bytes": "4096\n",
		"cpuacct/cpuacct.usage":        "2500000000\n",
	})

	memory, ok := processMemoryUsageBytes()
	if !ok || memory != 4096 {
		t.Errorf("memory = %d, %v; want 4096, true", memory, ok)
	}
	seconds, ok := processCPUTimeSeconds()
	if !ok || seconds != 2.5 {
		t.Errorf("cpu = %v, %v; want 2.5, true", seconds, ok)
	}
}

// A runtime that does not expose cgroupfs must yield no reading at all. Publishing zero
// would be indistinguishable from an idle sidecar.
func TestProcessMetricsUnavailableWithoutCgroupfs(t *testing.T) {
	withCgroupRoot(t, nil)

	if _, ok := processMemoryUsageBytes(); ok {
		t.Error("memory reported available with no cgroup files")
	}
	if _, ok := processCPUTimeSeconds(); ok {
		t.Error("cpu reported available with no cgroup files")
	}
}

// End to end: with a readable cgroupfs the instruments are registered and observed; with
// none, they must be absent from the collection rather than present and zero.
func TestProcessMetricsRegistrationFollowsAvailability(t *testing.T) {
	t.Run("registered when readable", func(t *testing.T) {
		withCgroupRoot(t, map[string]string{
			"memory.current": "8036352\n",
			"cpu.stat":       "usage_usec 1500000\n",
		})

		collected := collectEgressMetrics(t)

		if got := collected["egress.process.memory.usage_bytes"]; got != float64(8036352) {
			t.Errorf("memory metric = %v, want 8036352", got)
		}
		if got := collected["egress.process.cpu.time"]; got != 1.5 {
			t.Errorf("cpu metric = %v, want 1.5", got)
		}
	})

	t.Run("absent when unreadable", func(t *testing.T) {
		withCgroupRoot(t, nil)

		collected := collectEgressMetrics(t)

		if _, present := collected["egress.process.memory.usage_bytes"]; present {
			t.Error("memory metric registered without a cgroup source")
		}
		if _, present := collected["egress.process.cpu.time"]; present {
			t.Error("cpu metric registered without a cgroup source")
		}
	})
}
