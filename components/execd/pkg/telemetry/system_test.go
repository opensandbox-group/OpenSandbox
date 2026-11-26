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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/alibaba/opensandbox/execd/pkg/resources"
)

func TestDiskIORebaser(t *testing.T) {
	t.Parallel()

	r := &diskIORebaser{}

	// The first sample becomes the baseline: nothing is attributed yet.
	first := r.rebase(7, resources.DiskIO{ReadBytes: 100, WrittenBytes: 200, ReadOps: 1, WriteOps: 2})
	assert.Equal(t, resources.DiskIO{}, first)

	// Same binding: increments since the baseline.
	second := r.rebase(7, resources.DiskIO{ReadBytes: 250, WrittenBytes: 300, ReadOps: 3, WriteOps: 4})
	assert.Equal(t, resources.DiskIO{ReadBytes: 150, WrittenBytes: 100, ReadOps: 2, WriteOps: 2}, second)

	// Binding handoff: totals accumulated under the old binding are dropped,
	// so the new identity's series starts at zero instead of inheriting them.
	rebound := r.rebase(8, resources.DiskIO{ReadBytes: 900, WrittenBytes: 900, ReadOps: 9, WriteOps: 9})
	assert.Equal(t, resources.DiskIO{}, rebound)

	// Counters grow from the new baseline afterwards.
	after := r.rebase(8, resources.DiskIO{ReadBytes: 950, WrittenBytes: 900, ReadOps: 9, WriteOps: 10})
	assert.Equal(t, resources.DiskIO{ReadBytes: 50, WrittenBytes: 0, ReadOps: 0, WriteOps: 1}, after)

	// A counter that reset below the baseline clamps to zero, never negative.
	reset := r.rebase(8, resources.DiskIO{ReadBytes: 5})
	assert.Equal(t, resources.DiskIO{}, reset)
}

func TestCoreMetricsAndResourceFailures(t *testing.T) {
	originalProcess, originalCPU, originalMemory, originalNetwork := systemProcessCount, systemCPUUsagePercent, systemMemoryStats, systemNetworkIOBytes
	originalDiskUsage, originalDiskIO, originalIsolation := systemDiskUsages, systemDiskIO, isolationStatsProvider
	t.Cleanup(func() {
		systemProcessCount, systemCPUUsagePercent, systemMemoryStats, systemNetworkIOBytes = originalProcess, originalCPU, originalMemory, originalNetwork
		systemDiskUsages, systemDiskIO, isolationStatsProvider = originalDiskUsage, originalDiskIO, originalIsolation
	})

	var readErr error
	var memoryReads int
	systemProcessCount = func() (int64, error) { return 0, readErr }
	systemCPUUsagePercent = func() (float64, error) { return 0, readErr }
	systemMemoryStats = func() (resources.MemoryUsage, error) {
		memoryReads++
		return resources.MemoryUsage{UsedBytes: 0, TotalBytes: 1024}, readErr
	}
	systemNetworkIOBytes = func() (int64, int64, error) { return 0, 0, readErr }
	systemDiskUsages = func() []resources.DiskUsage {
		return []resources.DiskUsage{{Mountpoint: "/", FSType: "ext4", Source: "/dev/vda", UsedBytes: 256, TotalBytes: 1024}}
	}
	systemDiskIO = func() (resources.DiskIO, bool) { return resources.DiskIO{ReadBytes: 10, WrittenBytes: 20}, true }
	SetIsolationStatsProvider(func() IsolationStats { return IsolationStats{ActiveSessions: 1} })
	reader := setupMetricsReader(t)
	ctx := context.Background()
	RecordHTTPRequest(ctx, "GET", "/files", 200, 1)
	RecordExecutionDuration(ctx, "run_code", "success", 1)
	oomKills.Add(ctx, 1)

	metrics := collectMetrics(t, reader)
	wantNames := []string{
		"execd.http.request.duration", "execd.execution.duration",
		"execd.system.process.count", "execd.system.cpu.usage", "execd.system.memory.usage_bytes",
		"execd.system.memory.limit_bytes", "execd.system.network.io.bytes", "execd.system.disk.usage_bytes",
		"execd.system.disk.total_bytes", "execd.system.disk.io.bytes", "execd.system.oom.kills",
		"execd.isolation.session.count",
	}
	require.Len(t, metrics, len(wantNames))
	for _, name := range wantNames {
		require.Contains(t, metrics, name)
	}
	require.Equal(t, 1, memoryReads)

	affected := []string{"execd.system.process.count", "execd.system.cpu.usage", "execd.system.memory.usage_bytes", "execd.system.memory.limit_bytes", "execd.system.network.io.bytes"}
	readErr = errors.New("sample unavailable")
	metrics = collectMetrics(t, reader)
	for _, name := range affected {
		require.NotContains(t, metrics, name)
	}
	require.Contains(t, metrics, "execd.system.disk.usage_bytes")
	require.Equal(t, 2, memoryReads)

	readErr = nil
	metrics = collectMetrics(t, reader)
	for _, name := range affected {
		require.Contains(t, metrics, name)
	}
	require.Equal(t, 3, memoryReads)
	usage := metrics["execd.system.memory.usage_bytes"].Data.(metricdata.Gauge[int64])
	limit := metrics["execd.system.memory.limit_bytes"].Data.(metricdata.Gauge[int64])
	require.Len(t, usage.DataPoints, 1)
	require.Len(t, limit.DataPoints, 1)
	require.Zero(t, usage.DataPoints[0].Value)
	require.Equal(t, int64(1024), limit.DataPoints[0].Value)
	require.Equal(t, usage.DataPoints[0].Attributes, limit.DataPoints[0].Attributes)
}
