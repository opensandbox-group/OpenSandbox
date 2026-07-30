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
)

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
	return err
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
