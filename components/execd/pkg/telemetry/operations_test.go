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

package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOperationMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	operationMetrics.Lock()
	previousCounter, previousProvider := operationMetrics.requests, operationMetrics.provider
	operationMetrics.Unlock()
	t.Cleanup(func() {
		operationMetrics.Lock()
		operationMetrics.requests, operationMetrics.provider = previousCounter, previousProvider
		operationMetrics.Unlock()
	})
	SetOperationStatsProvider(func() OperationStats {
		return OperationStats{Records: [2][3]int64{{1, 2, 3}, {4, 5, 6}}, Capacity: 32, OldestCreatingAge: 17}
	})
	require.NoError(t, registerOperationMetrics(provider.Meter("operation-test")))
	RecordOperationResult("command", "create", "claimed")
	RecordOperationResult("caller-secret", "lookup", "invalid")
	var result metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &result))
	metrics := map[string]metricdata.Metrics{}
	for _, scope := range result.ScopeMetrics {
		for _, value := range scope.Metrics {
			metrics[value.Name] = value
		}
	}
	requests := metrics["execd.operation.requests"].Data.(metricdata.Sum[int64])
	require.Len(t, requests.DataPoints, 2)
	for _, point := range requests.DataPoints {
		require.Equal(t, int64(1), point.Value)
		kind, _ := point.Attributes.Value(attribute.Key("kind"))
		require.Contains(t, []string{"command", "unknown"}, kind.AsString())
		for _, attr := range point.Attributes.ToSlice() {
			require.NotContains(t, attr.Value.AsString(), "caller-secret")
		}
	}
	records := metrics["execd.operation.records"].Data.(metricdata.Gauge[int64])
	require.Len(t, records.DataPoints, 6)
	var total int64
	for _, point := range records.DataPoints {
		total += point.Value
	}
	require.Equal(t, int64(21), total)
	capacity := metrics["execd.operation.capacity"].Data.(metricdata.Gauge[int64])
	require.Equal(t, int64(32), capacity.DataPoints[0].Value)
	age := metrics["execd.operation.creating.oldest_age"].Data.(metricdata.Gauge[float64])
	require.Equal(t, float64(17), age.DataPoints[0].Value)
}
