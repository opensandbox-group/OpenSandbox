// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// swapGlobalMeterProvider installs an SDK provider backed by a ManualReader so
// tests can assert metric emission, and restores the previous provider on cleanup.
func swapGlobalMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })
	return reader
}

func collectHistogram(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Histogram[float64] {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %s has unexpected data type %T", name, metric.Data)
			}
			if metric.Unit != "s" {
				t.Errorf("metric %s unit = %q, want %q", name, metric.Unit, "s")
			}
			return histogram
		}
	}
	t.Fatalf("metric %s not found in collected data", name)
	return metricdata.Histogram[float64]{}
}

func TestAllocatorMetricsEmission(t *testing.T) {
	reader := swapGlobalMeterProvider(t)
	ctx := context.Background()

	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool-1"}}
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sbx-1"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool-1"},
	}

	recordAllocatorScheduleDuration(ctx, pool, 5*time.Millisecond, nil)
	recordAllocatorScheduleDuration(ctx, pool, 5*time.Millisecond, context.DeadlineExceeded)
	recordAllocatorPersistAllocStateDuration(ctx, sandbox, 8*time.Millisecond, nil)
	recordAllocatorSyncAllocResultDuration(ctx, pool, 20*time.Millisecond, nil)
	recordAllocatorSyncSingleAllocResultDuration(ctx, sandbox, 10*time.Millisecond, nil)
	recordAllocatorSyncSingleAllocResultDuration(ctx, sandbox, 10*time.Millisecond, context.Canceled)

	schedule := collectHistogram(t, reader, allocatorScheduleDurationMetricName)
	if len(schedule.DataPoints) != 2 {
		t.Fatalf("schedule metric: expected 2 data points (success/error), got %d", len(schedule.DataPoints))
	}
	successDP := assertSingleDataPointAttrs(t, schedule, map[string]string{"namespace": "default", "pool_name": "pool-1"}, true)
	if successDP.Sum < 0.005 {
		t.Errorf("schedule success sum = %v, want >= 0.005", successDP.Sum)
	}
	errorDP := assertSingleDataPointAttrs(t, schedule, map[string]string{"namespace": "default", "pool_name": "pool-1"}, false)
	if errorDP.Sum < 0.005 {
		t.Errorf("schedule error sum = %v, want >= 0.005", errorDP.Sum)
	}

	persist := collectHistogram(t, reader, allocatorPersistAllocStateDurationMetricName)
	assertSingleDataPointAttrs(t, persist, map[string]string{"namespace": "default", "pool_name": "pool-1"}, true)

	syncResult := collectHistogram(t, reader, allocatorSyncAllocResultDurationMetricName)
	assertSingleDataPointAttrs(t, syncResult, map[string]string{"namespace": "default", "pool_name": "pool-1"}, true)

	syncSingle := collectHistogram(t, reader, allocatorSyncSingleAllocResultDurationName)
	if len(syncSingle.DataPoints) != 2 {
		t.Fatalf("single sync metric: expected 2 data points (success/error), got %d", len(syncSingle.DataPoints))
	}
	assertSingleDataPointAttrs(t, syncSingle, map[string]string{"namespace": "default", "pool_name": "pool-1"}, true)
	assertSingleDataPointAttrs(t, syncSingle, map[string]string{"namespace": "default", "pool_name": "pool-1"}, false)
}

// assertSingleDataPointAttrs finds the unique data point matching the given
// attribute set (ignoring extra attributes) and validates count/sum/attributes.
func assertSingleDataPointAttrs(t *testing.T, histogram metricdata.Histogram[float64], wantAttrs map[string]string, wantSuccess bool) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	matches := make([]metricdata.HistogramDataPoint[float64], 0, 1)
	for _, dp := range histogram.DataPoints {
		namespace, hasNamespace := dp.Attributes.Value(attribute.Key("namespace"))
		poolName, hasPoolName := dp.Attributes.Value(attribute.Key("pool_name"))
		success, hasSuccess := dp.Attributes.Value(attribute.Key("success"))
		if hasNamespace && namespace.AsString() == wantAttrs["namespace"] &&
			hasPoolName && poolName.AsString() == wantAttrs["pool_name"] &&
			hasSuccess && success == attribute.BoolValue(wantSuccess) {
			matches = append(matches, dp)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 data point with attrs %v success=%v, got %d", wantAttrs, wantSuccess, len(matches))
	}
	dp := matches[0]
	if dp.Count != 1 {
		t.Errorf("data point count = %d, want 1", dp.Count)
	}
	if dp.Sum <= 0 {
		t.Errorf("data point sum = %v, want > 0", dp.Sum)
	}
	if _, ok := dp.Attributes.Value(attribute.Key("sandbox_name")); ok {
		t.Error("sandbox_name attribute must not be emitted")
	}
	return dp
}
