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
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestConnectivityMetricsUseLowCardinalityAttributes(t *testing.T) {
	resetMetricState()
	previousProvider := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		SetConnectivitySnapshotProvider(nil)
		_ = provider.Shutdown(context.Background())
		resetMetricState()
		otel.SetMeterProvider(previousProvider)
	})

	if err := registerIngressMetrics(); err != nil {
		t.Fatalf("registerIngressMetrics() error = %v", err)
	}
	RecordUpstreamConnect("timeout", "http", 125)
	providerCalls := 0
	SetConnectivitySnapshotProvider(func() ConnectivitySnapshot {
		providerCalls++
		return ConnectivitySnapshot{
			Attempts:              25,
			SignalFailures:        5,
			DistinctTargets:       7,
			DistinctSignalTargets: 3,
			Qualified:             true,
			Degraded:              true,
		}
	})

	metrics := collectMetrics(t, reader)
	if providerCalls != 1 {
		t.Fatalf("connectivity snapshot provider calls = %d, want 1", providerCalls)
	}
	assertInt64Gauge(t, metrics, "ingress.network.shadow.attempts", 25)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.signal_failures", 5)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.distinct_targets", 7)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.distinct_signal_targets", 3)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.qualified", 1)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.degraded", 1)
	assertConnectMetrics(t, metrics)
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var resourceMetrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resourceMetrics); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	metrics := make(map[string]metricdata.Aggregation)
	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, collectedMetric := range scopeMetrics.Metrics {
			metrics[collectedMetric.Name] = collectedMetric.Data
		}
	}
	return metrics
}

func assertInt64Gauge(t *testing.T, metrics map[string]metricdata.Aggregation, name string, want int64) {
	t.Helper()
	data, exists := metrics[name]
	if !exists {
		t.Fatalf("metric %q was not collected", name)
	}
	gauge, ok := data.(metricdata.Gauge[int64])
	if !ok || len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != want {
		t.Fatalf("metric %q data = %#v, want one data point with value %d", name, data, want)
	}
}

func assertConnectMetrics(t *testing.T, metrics map[string]metricdata.Aggregation) {
	t.Helper()
	count, ok := metrics["ingress.upstream.connect.count"].(metricdata.Sum[int64])
	if !ok || len(count.DataPoints) != 1 || count.DataPoints[0].Value != 1 {
		t.Fatalf("unexpected connect count: %#v", metrics["ingress.upstream.connect.count"])
	}
	duration, ok := metrics["ingress.upstream.connect.duration"].(metricdata.Histogram[float64])
	if !ok || len(duration.DataPoints) != 1 || duration.DataPoints[0].Count != 1 || duration.DataPoints[0].Sum != 125 {
		t.Fatalf("unexpected connect duration: %#v", metrics["ingress.upstream.connect.duration"])
	}
	assertConnectAttributes(t, count.DataPoints[0].Attributes)
	assertConnectAttributes(t, duration.DataPoints[0].Attributes)
}

func assertConnectAttributes(t *testing.T, attributes attribute.Set) {
	t.Helper()
	for key, want := range map[attribute.Key]string{
		"connect_result": "timeout",
		"proxy_type":     "http",
	} {
		value, exists := attributes.Value(key)
		if !exists || value.AsString() != want {
			t.Fatalf("attribute %q = %q, want %q", key, value.AsString(), want)
		}
	}
	for _, forbidden := range []attribute.Key{"target", "target_ip", "sandbox_id"} {
		if _, exists := attributes.Value(forbidden); exists {
			t.Fatalf("metric contains forbidden high-cardinality attribute %q", forbidden)
		}
	}
}

func resetMetricState() {
	SetConnectivitySnapshotProvider(nil)
	meter = nil
	httpRequestCount = nil
	httpRequestDuration = nil
	routingResolutions = nil
	routingResolutionDuration = nil
	upstreamConnectCount = nil
	upstreamConnectDuration = nil
}
