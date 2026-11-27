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
	"os"
	"strings"
	"sync"

	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/alibaba/opensandbox/internal/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/resources"
)

const (
	serviceName         = "opensandbox-execd"
	envSandboxID        = "OPENSANDBOX_ID"
	envMetricsExtraAttr = "OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS"
)

var (
	httpRequestDuration metric.Float64Histogram
	executionDuration   metric.Float64Histogram
	oomKills            metric.Int64Counter
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
		metric.WithDescription("Code and command duration by operation and result; background commands measure submission only"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(10, 100, 1000, 10000, 60000, 300000, 900000, 3600000),
	)
	if err != nil {
		return err
	}

	if err := registerSystemMetrics(meter); err != nil {
		return err
	}

	oomKills, err = meter.Int64Counter(
		"execd.system.oom.kills",
		metric.WithDescription("OOM kills observed in the sandbox cgroup"),
		metric.WithUnit("{kill}"),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"execd.isolation.session.count",
		metric.WithDescription("Current number of active isolated sessions"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			if isolationStatsProvider == nil {
				return nil
			}
			obs.Observe(isolationStatsProvider().ActiveSessions, metric.WithAttributes(sharedAttrs()...))
			return nil
		}),
	)
	return err
}

// registerSystemMetrics registers the periodic system-observability gauges
// and counters: process/CPU/memory/network plus disk usage and IO.
func registerSystemMetrics(meter metric.Meter) error {
	_, err := meter.Int64ObservableGauge(
		"execd.system.process.count",
		metric.WithDescription("Current number of processes in the system"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			count, err := systemProcessCount()
			if err != nil {
				log.Warn("telemetry: process count unavailable: %v", err)
				return nil
			}
			obs.Observe(count, metric.WithAttributes(sharedAttrs()...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Float64ObservableGauge(
		"execd.system.cpu.usage",
		metric.WithDescription("Sandbox CPU usage percentage (sandbox cgroup when limited, host fallback)"),
		metric.WithUnit("%"),
		metric.WithFloat64Callback(func(ctx context.Context, obs metric.Float64Observer) error {
			usage, err := systemCPUUsagePercent()
			if err != nil {
				log.Warn("telemetry: CPU usage unavailable: %v", err)
				return nil
			}
			obs.Observe(usage, metric.WithAttributes(sharedAttrs()...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	memoryUsage, err := meter.Int64ObservableGauge(
		"execd.system.memory.usage_bytes",
		metric.WithDescription("Sandbox memory working set bytes, excluding reclaimable file cache (sandbox cgroup when limited, host fallback)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}
	memoryLimit, err := meter.Int64ObservableGauge(
		"execd.system.memory.limit_bytes",
		metric.WithDescription("Effective sandbox memory capacity: cgroup limit or host/VM total"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, obs metric.Observer) error {
		stats, err := systemMemoryStats()
		if err != nil {
			log.Warn("telemetry: memory stats unavailable: %v", err)
			return nil
		}
		attrs := metric.WithAttributes(sharedAttrs()...)
		obs.ObserveInt64(memoryUsage, stats.UsedBytes, attrs)
		obs.ObserveInt64(memoryLimit, stats.TotalBytes, attrs)
		return nil
	}, memoryUsage, memoryLimit)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableCounter(
		"execd.system.network.io.bytes",
		metric.WithDescription("System network IO bytes by direction"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			inBytes, outBytes, err := systemNetworkIOBytes()
			if err != nil {
				log.Warn("telemetry: network IO unavailable: %v", err)
				return nil
			}
			base := append([]attribute.KeyValue{}, sharedAttrs()...)
			obs.Observe(inBytes, metric.WithAttributes(append(base, attribute.String("direction", "in"))...))
			obs.Observe(outBytes, metric.WithAttributes(append(base, attribute.String("direction", "out"))...))
			return nil
		}),
	)
	if err != nil {
		return err
	}

	return registerDiskMetrics(meter)
}

// registerDiskMetrics registers the per-mount disk usage gauges and the
// rebased cumulative disk IO counters.
func registerDiskMetrics(meter metric.Meter) error {
	_, err := meter.Int64ObservableGauge(
		"execd.system.disk.usage_bytes",
		metric.WithDescription("Per-mount filesystem used bytes (real mounts only; FUSE/remote values are as reported by the filesystem)"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			base := append([]attribute.KeyValue{}, sharedAttrs()...)
			for _, usage := range systemDiskUsages() {
				obs.Observe(usage.UsedBytes, metric.WithAttributes(diskUsageAttrs(base, usage)...))
			}
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"execd.system.disk.total_bytes",
		metric.WithDescription("Per-mount filesystem total capacity (real mounts only; FUSE/remote values are as reported by the filesystem)"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			base := append([]attribute.KeyValue{}, sharedAttrs()...)
			for _, usage := range systemDiskUsages() {
				obs.Observe(usage.TotalBytes, metric.WithAttributes(diskUsageAttrs(base, usage)...))
			}
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableCounter(
		"execd.system.disk.io.bytes",
		metric.WithDescription("Cumulative disk IO bytes by direction since the current binding (rebased at warm-rebind handoffs; sandbox cgroup IO accounting, /proc/diskstats fallback)"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			// Unavailable (e.g. non-Linux) must not be reported as zero.
			if io, ok := systemDiskIO(); ok {
				base := append([]attribute.KeyValue{}, sharedAttrs()...)
				obs.Observe(io.ReadBytes, metric.WithAttributes(append(base, attribute.String("direction", "read"))...))
				obs.Observe(io.WrittenBytes, metric.WithAttributes(append(base, attribute.String("direction", "write"))...))
			}
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

// bindingDynamicAttrs renders the current RuntimeBinding as metric
// attributes: the authoritative sandbox_id and generation, plus the extra
// attributes delivered by POST /internal/init.
func bindingDynamicAttrs(b *binding.RuntimeBinding) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(b.TelemetryAttrs)+2)
	if b.SandboxID != "" {
		attrs = append(attrs, attribute.String("sandbox_id", b.SandboxID))
	}
	attrs = append(attrs, attribute.Int64("generation", int64(b.Generation)))
	for k, v := range b.TelemetryAttrs {
		key := attribute.Key(k)
		if key == "sandbox_id" || key == "generation" {
			// Reserved: delivered structurally above.
			continue
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

func diskUsageAttrs(base []attribute.KeyValue, usage resources.DiskUsage) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(base)+3)
	attrs = append(attrs, base...)
	attrs = append(attrs,
		attribute.String("mountpoint", usage.Mountpoint),
		attribute.String("fstype", usage.FSType),
		attribute.String("source", usage.Source),
	)
	return attrs
}

// RuntimeBinding attributes override static environment attributes at record time.
func sharedAttrs() []attribute.KeyValue {
	staticAttrs := execdSharedAttrs()
	b := binding.Current()
	if b == nil {
		return staticAttrs
	}
	dynamic := bindingDynamicAttrs(b)
	seen := make(map[attribute.Key]struct{}, len(dynamic))
	for _, kv := range dynamic {
		seen[kv.Key] = struct{}{}
	}
	attrs := make([]attribute.KeyValue, 0, len(dynamic)+len(staticAttrs))
	attrs = append(attrs, dynamic...)
	for _, kv := range staticAttrs {
		if _, taken := seen[kv.Key]; taken {
			continue
		}
		attrs = append(attrs, kv)
	}
	return attrs
}
