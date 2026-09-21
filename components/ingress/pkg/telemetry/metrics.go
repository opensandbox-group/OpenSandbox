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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	meter metric.Meter

	httpRequestCount    metric.Int64Counter
	httpRequestDuration metric.Float64Histogram

	routingResolutions        metric.Int64Counter
	routingResolutionDuration metric.Float64Histogram

	upstreamConnectCount    metric.Int64Counter
	upstreamConnectDuration metric.Float64Histogram

	connectivityProviderMu sync.RWMutex
	connectivityProvider   func() ConnectivitySnapshot
)

// ConnectivitySnapshot contains bounded, low-cardinality shadow aggregates.
type ConnectivitySnapshot struct {
	Attempts              int64
	SignalFailures        int64
	DistinctTargets       int64
	DistinctSignalTargets int64
	Qualified             bool
	Degraded              bool
}

func registerIngressMetrics() error {
	meter = otel.Meter("opensandbox/ingress")

	var err error
	httpRequestCount, err = meter.Int64Counter(
		"ingress.http.request.count",
		metric.WithDescription("Ingress HTTP request count"),
	)
	if err != nil {
		return err
	}

	httpRequestDuration, err = meter.Float64Histogram(
		"ingress.http.request.duration",
		metric.WithDescription("Ingress HTTP request duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	routingResolutions, err = meter.Int64Counter(
		"ingress.routing.resolutions.count",
		metric.WithDescription("Routing resolution count by result"),
	)
	if err != nil {
		return err
	}

	routingResolutionDuration, err = meter.Float64Histogram(
		"ingress.routing.resolution.duration",
		metric.WithDescription("Routing resolution duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	upstreamConnectCount, err = meter.Int64Counter(
		"ingress.upstream.connect.count",
		metric.WithDescription("Ingress upstream TCP connection attempts by result"),
	)
	if err != nil {
		return err
	}

	upstreamConnectDuration, err = meter.Float64Histogram(
		"ingress.upstream.connect.duration",
		metric.WithDescription("Ingress upstream TCP connection duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	_, err = meter.Float64ObservableGauge(
		"ingress.system.cpu.usage",
		metric.WithDescription("System CPU utilization ratio 0-1"),
		metric.WithUnit("1"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			obs.Observe(cpuUtilizationRatio())
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"ingress.system.memory.usage_bytes",
		metric.WithDescription("System memory used bytes"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(systemMemoryUsedBytes())
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"ingress.connections.active",
		metric.WithDescription("Current active network connections (TCP ESTABLISHED)"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(activeNetworkConnections())
			return nil
		}),
	)
	if err != nil {
		return err
	}

	return registerConnectivityMetrics()
}

func registerConnectivityMetrics() error {
	attempts, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.attempts",
		metric.WithDescription("TCP connection attempts in the most recent complete shadow window"),
	)
	if err != nil {
		return err
	}
	signalFailures, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.signal_failures",
		metric.WithDescription("Timeout and unreachable results in the most recent complete shadow window"),
	)
	if err != nil {
		return err
	}
	distinctTargets, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.distinct_targets",
		metric.WithDescription("Bounded distinct upstream targets in the most recent complete shadow window"),
	)
	if err != nil {
		return err
	}
	distinctSignalTargets, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.distinct_signal_targets",
		metric.WithDescription("Bounded distinct upstream targets with timeout or unreachable results in the most recent complete shadow window"),
	)
	if err != nil {
		return err
	}
	qualified, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.qualified",
		metric.WithDescription("Whether the most recent complete shadow window has enough samples"),
	)
	if err != nil {
		return err
	}
	degraded, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.degraded",
		metric.WithDescription("Whether the most recent complete qualified shadow window is degraded"),
	)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, observer metric.Observer) error {
			snapshot, ok := connectivitySnapshot()
			if !ok {
				return nil
			}
			observer.ObserveInt64(attempts, snapshot.Attempts)
			observer.ObserveInt64(signalFailures, snapshot.SignalFailures)
			observer.ObserveInt64(distinctTargets, snapshot.DistinctTargets)
			observer.ObserveInt64(distinctSignalTargets, snapshot.DistinctSignalTargets)
			observer.ObserveInt64(qualified, boolToInt64(snapshot.Qualified))
			observer.ObserveInt64(degraded, boolToInt64(snapshot.Degraded))
			return nil
		},
		attempts,
		signalFailures,
		distinctTargets,
		distinctSignalTargets,
		qualified,
		degraded,
	)
	return err
}

// SetConnectivitySnapshotProvider installs the callback used by shadow gauges.
func SetConnectivitySnapshotProvider(provider func() ConnectivitySnapshot) {
	connectivityProviderMu.Lock()
	defer connectivityProviderMu.Unlock()
	connectivityProvider = provider
}

func connectivitySnapshot() (ConnectivitySnapshot, bool) {
	connectivityProviderMu.RLock()
	provider := connectivityProvider
	connectivityProviderMu.RUnlock()
	if provider == nil {
		return ConnectivitySnapshot{}, false
	}
	return provider(), true
}

func boolToInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func RecordHTTPRequest(method string, statusCode int, proxyType string, durationMs float64) {
	if httpRequestCount == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("http_method", method),
		attribute.Int("http_status_code", statusCode),
		attribute.String("proxy_type", proxyType),
	)
	httpRequestCount.Add(context.Background(), 1, attrs)
	httpRequestDuration.Record(context.Background(), durationMs, attrs)
}

func RecordRouting(result string, durationMs float64) {
	if routingResolutions == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("routing_result", result))
	routingResolutions.Add(context.Background(), 1, attrs)
	routingResolutionDuration.Record(context.Background(), durationMs, attrs)
}

// RecordUpstreamConnect records only low-cardinality connection attributes.
// The target address is intentionally excluded because Sandbox endpoints are
// high-cardinality and short-lived.
func RecordUpstreamConnect(result, proxyType string, durationMs float64) {
	if upstreamConnectCount == nil || upstreamConnectDuration == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("connect_result", result),
		attribute.String("proxy_type", proxyType),
	)
	upstreamConnectCount.Add(context.Background(), 1, attrs)
	upstreamConnectDuration.Record(context.Background(), durationMs, attrs)
}
