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
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	resetMetricState()
	previousProvider := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		SetConnectivitySnapshotProvider(nil)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
		resetMetricState()
		otel.SetMeterProvider(previousProvider)
	})
	if err := registerIngressMetrics(); err != nil {
		t.Fatalf("registerIngressMetrics() error = %v", err)
	}
	return reader
}

func TestIngressMetricInventoryAndAttributes(t *testing.T) {
	reader := newMetricReader(t)
	RecordHTTPRequest("GET", 200, "http", "upstream_response", 125)
	RecordHTTPRequest("GET", 200, "http", "canceled", 25)
	RecordRequestDelta("http", 2)
	RecordRequestDelta("http", -1)
	RecordRouting("success", 3)
	RecordUpstreamConnect("timeout", "http", 125)
	RecordWakeDuration("ready", 250*time.Millisecond)
	RecordParkDelta(2)
	RecordParkDelta(-1)
	RecordParkWait("served", 500*time.Millisecond)
	RecordParkShed()
	RecordRestoreDelta(2)
	RecordRestoreDelta(-1)
	RecordPolicyRead("allowed")
	RecordActivityWriteDropped(3)
	providerCalls := 0
	SetConnectivitySnapshotProvider(func() ConnectivitySnapshot {
		providerCalls++
		return ConnectivitySnapshot{Qualified: true, Degraded: true}
	})

	metrics := collectMetrics(t, reader)
	wantNames := []string{
		"ingress.http.request.count",
		"ingress.http.request.duration",
		"ingress.requests.active",
		"ingress.routing.resolution.duration",
		"ingress.upstream.connect.duration",
		"ingress.wake.duration",
		"ingress.park.active",
		"ingress.park.wait.duration",
		"ingress.park.shed",
		"ingress.wake.restore.active",
		"ingress.idlep.policy.reads",
		"ingress.activity.writes_dropped",
		"ingress.network.shadow.qualified",
		"ingress.network.shadow.degraded",
	}
	if len(metrics) != 14 {
		t.Errorf("collected %d metrics, want exactly 14: %v", len(metrics), metrics)
	}
	for _, name := range wantNames {
		if _, exists := metrics[name]; !exists {
			t.Errorf("metric %q was not collected", name)
		}
	}
	for _, name := range []string{
		"ingress.routing.resolutions.count",
		"ingress.upstream.connect.count",
		"ingress.wake.flights",
		"ingress.system.cpu.usage",
		"ingress.system.memory.usage_bytes",
		"ingress.connections.active",
		"ingress.network.shadow.attempts",
		"ingress.network.shadow.signal_failures",
		"ingress.network.shadow.distinct_targets",
		"ingress.network.shadow.distinct_signal_targets",
	} {
		if _, exists := metrics[name]; exists {
			t.Errorf("removed metric %q was collected", name)
		}
	}
	if providerCalls != 1 {
		t.Fatalf("connectivity snapshot provider calls = %d, want 1", providerCalls)
	}

	httpAttrs := []attribute.KeyValue{
		attribute.String("http_method", "GET"),
		attribute.Int("http_status_code", 200),
		attribute.String("proxy_type", "http"),
	}
	count, ok := metrics["ingress.http.request.count"].Data.(metricdata.Sum[int64])
	if !ok || !count.IsMonotonic || len(count.DataPoints) != 2 {
		t.Fatalf("unexpected HTTP request count: %#v", metrics["ingress.http.request.count"])
	}
	outcomes := map[string]bool{}
	for _, point := range count.DataPoints {
		outcome, _ := point.Attributes.Value("outcome")
		outcomes[outcome.AsString()] = true
		assertAttributes(t, point.Attributes, append(httpAttrs, attribute.String("outcome", outcome.AsString()))...)
		if point.Value != 1 {
			t.Errorf("HTTP count = %d, want 1 per outcome", point.Value)
		}
	}
	if len(outcomes) != 2 || !outcomes["upstream_response"] || !outcomes["canceled"] {
		t.Errorf("HTTP outcomes = %v, want upstream_response and canceled", outcomes)
	}
	// Different outcomes must share the same histogram series.
	assertHistogram(t, metrics, "ingress.http.request.duration", "ms", 2, 150, httpAttrs...)
	assertHistogram(t, metrics, "ingress.routing.resolution.duration", "ms", 1, 3, attribute.String("routing_result", "success"))
	assertHistogram(t, metrics, "ingress.upstream.connect.duration", "ms", 1, 125,
		attribute.String("connect_result", "timeout"), attribute.String("proxy_type", "http"))
	assertHistogram(t, metrics, "ingress.wake.duration", "s", 1, 0.25, attribute.String("outcome", "ready"))
	assertHistogram(t, metrics, "ingress.park.wait.duration", "s", 1, 0.5, attribute.String("outcome", "served"))
	assertInt64Sum(t, metrics, "ingress.requests.active", 1, false, attribute.String("proxy_type", "http"))
	assertInt64Sum(t, metrics, "ingress.park.active", 1, false)
	assertInt64Sum(t, metrics, "ingress.park.shed", 1, true)
	assertInt64Sum(t, metrics, "ingress.wake.restore.active", 1, false)
	assertInt64Sum(t, metrics, "ingress.idlep.policy.reads", 1, true, attribute.String("outcome", "allowed"))
	assertInt64Sum(t, metrics, "ingress.activity.writes_dropped", 3, true)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.qualified", 1)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.degraded", 1)
}

func TestConnectivityMetricsWithoutProvider(t *testing.T) {
	reader := newMetricReader(t)
	if metrics := collectMetrics(t, reader); len(metrics) != 0 {
		t.Fatalf("metrics without observations or snapshot provider = %v, want none", metrics)
	}
	SetConnectivitySnapshotProvider(func() ConnectivitySnapshot { return ConnectivitySnapshot{} })
	metrics := collectMetrics(t, reader)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.qualified", 0)
	assertInt64Gauge(t, metrics, "ingress.network.shadow.degraded", 0)
	SetConnectivitySnapshotProvider(nil)
	if metrics := collectMetrics(t, reader); len(metrics) != 0 {
		t.Fatalf("metrics after removing snapshot provider = %v, want none", metrics)
	}
}

func TestRecordMetricsBeforeInitialization(t *testing.T) {
	resetMetricState()
	t.Cleanup(resetMetricState)
	RecordHTTPRequest("GET", 200, "http", "upstream_response", 1)
	RecordRequestDelta("http", 1)
	RecordRouting("success", 1)
	RecordUpstreamConnect("success", "http", 1)
	RecordWakeDuration("ready", time.Second)
	RecordParkDelta(1)
	RecordParkWait("served", time.Second)
	RecordParkShed()
	RecordRestoreDelta(1)
	RecordPolicyRead("allowed")
	RecordActivityWriteDropped(1)
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var resourceMetrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resourceMetrics); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	metrics := make(map[string]metricdata.Metrics)
	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, collectedMetric := range scopeMetrics.Metrics {
			metrics[collectedMetric.Name] = collectedMetric
		}
	}
	return metrics
}

func assertInt64Gauge(t *testing.T, metrics map[string]metricdata.Metrics, name string, want int64) {
	t.Helper()
	gauge, ok := metrics[name].Data.(metricdata.Gauge[int64])
	if !ok || len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != want {
		t.Fatalf("metric %q data = %#v, want one gauge data point with value %d", name, metrics[name], want)
	}
	assertAttributes(t, gauge.DataPoints[0].Attributes)
}

func assertInt64Sum(t *testing.T, metrics map[string]metricdata.Metrics, name string, want int64, monotonic bool, attrs ...attribute.KeyValue) {
	t.Helper()
	sum, ok := metrics[name].Data.(metricdata.Sum[int64])
	if !ok || sum.IsMonotonic != monotonic || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != want {
		t.Fatalf("metric %q data = %#v, want one sum data point with value %d, monotonic %v", name, metrics[name], want, monotonic)
	}
	assertAttributes(t, sum.DataPoints[0].Attributes, attrs...)
}

func assertHistogram(t *testing.T, metrics map[string]metricdata.Metrics, name, unit string, count uint64, sum float64, attrs ...attribute.KeyValue) {
	t.Helper()
	histogram, ok := metrics[name].Data.(metricdata.Histogram[float64])
	if !ok || len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != count || histogram.DataPoints[0].Sum != sum || metrics[name].Unit != unit {
		t.Fatalf("metric %q data = %#v, want one histogram data point with count %d, sum %v, unit %q", name, metrics[name], count, sum, unit)
	}
	assertAttributes(t, histogram.DataPoints[0].Attributes, attrs...)
}

func assertAttributes(t *testing.T, got attribute.Set, attrs ...attribute.KeyValue) {
	t.Helper()
	want := attribute.NewSet(attrs...)
	if !got.Equals(&want) {
		t.Fatalf("attributes = %v, want %v", got.ToSlice(), want.ToSlice())
	}
}

func resetMetricState() {
	SetConnectivitySnapshotProvider(nil)
	meter = nil
	httpRequestCount = nil
	httpRequestDuration = nil
	requestsActive = nil
	routingResolutionDuration = nil
	upstreamConnectDuration = nil
	wakeDuration = nil
	parkActive = nil
	parkWaitDuration = nil
	parkShed = nil
	restoreActive = nil
	policyReads = nil
	activityWritesDropped = nil
}
