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
	"os"
	"strings"
	"sync"

	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/alibaba/opensandbox/internal/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	serviceName         = "opensandbox-execd"
	envSandboxID        = "OPENSANDBOX_ID"
	envMetricsExtraAttr = "OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS"
)

var (
	httpRequestDuration      metric.Float64Histogram
	executionDuration        metric.Float64Histogram
	filesystemOperationDurMs metric.Float64Histogram
)

func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	var resourceAttrs []attribute.KeyValue
	if id := strings.TrimSpace(os.Getenv(envSandboxID)); id != "" {
		resourceAttrs = append(resourceAttrs, attribute.String("sandbox_id", id))
	}

	return inttelemetry.Init(ctx, inttelemetry.Config{
		ServiceName:        serviceName + "-" + version.Version,
		ResourceAttributes: resourceAttrs,
		RegisterMetrics:    registerExecdMetrics,
	})
}

func registerExecdMetrics() error {
	meter := otel.Meter("opensandbox/execd")

	var err error
	httpRequestDuration, err = meter.Float64Histogram(
		"execd.http.request.duration",
		metric.WithDescription("HTTP request duration by method and route template"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	executionDuration, err = meter.Float64Histogram(
		"execd.execution.duration",
		metric.WithDescription("Duration per execution"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	filesystemOperationDurMs, err = meter.Float64Histogram(
		"execd.filesystem.operations.duration",
		metric.WithDescription("Filesystem operation duration by type"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"execd.system.process.count",
		metric.WithDescription("Current number of processes in the system"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			obs.Observe(systemProcessCount(), metric.WithAttributes(execdSharedAttrs()...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Float64ObservableGauge(
		"execd.system.cpu.usage",
		metric.WithDescription("System-wide CPU usage percentage"),
		metric.WithUnit("%"),
		metric.WithFloat64Callback(func(ctx context.Context, obs metric.Float64Observer) error {
			obs.Observe(systemCPUUsagePercent(), metric.WithAttributes(execdSharedAttrs()...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"execd.system.memory.usage_bytes",
		metric.WithDescription("System memory used bytes"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			obs.Observe(systemMemoryUsageBytes(), metric.WithAttributes(execdSharedAttrs()...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableCounter(
		"execd.system.network.io.bytes",
		metric.WithDescription("System network IO bytes by direction"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			inBytes, outBytes := systemNetworkIOBytes()
			base := append([]attribute.KeyValue{}, execdSharedAttrs()...)
			obs.Observe(inBytes, metric.WithAttributes(append(base, attribute.String("direction", "in"))...))
			obs.Observe(outBytes, metric.WithAttributes(append(base, attribute.String("direction", "out"))...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"execd.system.network.connections.active",
		metric.WithDescription("Current active network connections by protocol"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			tcpCount, udpCount := systemNetworkConnectionCounts()
			base := append([]attribute.KeyValue{}, execdSharedAttrs()...)
			obs.Observe(tcpCount, metric.WithAttributes(append(base, attribute.String("protocol", "tcp"))...))
			obs.Observe(udpCount, metric.WithAttributes(append(base, attribute.String("protocol", "udp"))...))
			return nil
		}),
	)
	return err
}

var execdSharedAttrs = sync.OnceValue(func() []attribute.KeyValue {
	return inttelemetry.SharedAttrsFromEnv(inttelemetry.SharedAttrsEnvConfig{
		SandboxIDEnv:  envSandboxID,
		ExtraAttrsEnv: envMetricsExtraAttr,
		SandboxAttr:   "sandbox_id",
	})
})
