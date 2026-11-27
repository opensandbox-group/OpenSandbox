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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

func TestNormalizeRoute(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "unknown", normalizeRoute(""))
	assert.Equal(t, "/code/contexts/:contextId", normalizeRoute("/code/contexts/:contextId"))
}

func TestRecordHTTPRequestWithoutInit(t *testing.T) {
	t.Parallel()

	RecordHTTPRequest(context.Background(), "GET", "/ping", 200, 0.01)
}

func TestSystemMetricsReaders(t *testing.T) {
	t.Parallel()

	processCount, err := systemProcessCount()
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, processCount, int64(0))
	cpuUsage, err := systemCPUUsagePercent()
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, cpuUsage, 0.0)
	memory, err := systemMemoryStats()
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, memory.UsedBytes, int64(0))
	assert.Greater(t, memory.TotalBytes, int64(0))
	inBytes, outBytes, err := systemNetworkIOBytes()
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, inBytes, int64(0))
	assert.GreaterOrEqual(t, outBytes, int64(0))
	for _, usage := range systemDiskUsages() {
		assert.NotEmpty(t, usage.Mountpoint)
		assert.GreaterOrEqual(t, usage.UsedBytes, int64(0))
		assert.GreaterOrEqual(t, usage.TotalBytes, int64(0))
	}
	if io, ok := systemDiskIO(); ok {
		assert.GreaterOrEqual(t, io.ReadBytes, int64(0))
		assert.GreaterOrEqual(t, io.WrittenBytes, int64(0))
		assert.GreaterOrEqual(t, io.ReadOps, int64(0))
		assert.GreaterOrEqual(t, io.WriteOps, int64(0))
	}
}

func TestExecdSharedAttrs(t *testing.T) {
	t.Setenv(envSandboxID, "sb-123")
	t.Setenv(envMetricsExtraAttr, "tenant=t1,env=dev")

	orig := execdSharedAttrs
	execdSharedAttrs = sync.OnceValue(func() []attribute.KeyValue {
		return inttelemetry.SharedAttrsFromEnv(inttelemetry.SharedAttrsEnvConfig{
			SandboxIDEnv:  envSandboxID,
			ExtraAttrsEnv: envMetricsExtraAttr,
			SandboxAttr:   "sandbox_id",
		})
	})
	t.Cleanup(func() { execdSharedAttrs = orig })

	attrs := sharedAttrs()
	assert.Len(t, attrs, 3)
}

func setupMetricsReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	originalProvider := otel.GetMeterProvider()
	originalHTTP, originalDuration, originalOOM := httpRequestDuration, executionDuration, oomKills
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(originalProvider)
		httpRequestDuration, executionDuration, oomKills = originalHTTP, originalDuration, originalOOM
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	require.NoError(t, registerExecdMetrics())
	return reader
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	metrics := make(map[string]metricdata.Metrics)
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			metrics[m.Name] = m
		}
	}
	return metrics
}

func TestExecutionDurationIncludesIsolatedRuns(t *testing.T) {
	reader := setupMetricsReader(t)
	RecordExecutionDuration(context.Background(), "run_isolated", "failure", 60000)
	metrics := collectMetrics(t, reader)
	require.NotContains(t, metrics, "execd.isolation.run.duration")
	duration := metrics["execd.execution.duration"].Data.(metricdata.Histogram[float64])
	require.Len(t, duration.DataPoints, 1)
	point := duration.DataPoints[0]
	require.Equal(t, uint64(1), point.Count)
	require.Equal(t, float64(60000), point.Sum)
	require.Equal(t, []float64{10, 100, 1000, 10000, 60000, 300000, 900000, 3600000}, point.Bounds)
	operation, ok := point.Attributes.Value("operation")
	require.True(t, ok)
	require.Equal(t, "run_isolated", operation.AsString())
	result, ok := point.Attributes.Value("result")
	require.True(t, ok)
	require.Equal(t, "failure", result.AsString())
}
