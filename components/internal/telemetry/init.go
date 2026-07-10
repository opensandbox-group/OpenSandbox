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

// Package telemetry provides shared OpenTelemetry OTLP metrics setup for OpenSandbox binaries.
package telemetry

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config controls OTLP metrics export. Endpoints follow standard OTEL env vars; see metricsEnabled.
type Config struct {
	ServiceName        string
	ResourceAttributes []attribute.KeyValue
	RegisterMetrics    func() error
}

const (
	envOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envHostIP              = "HOST_IP"
	otlpHTTPPort           = "4318"
	hostInfoPath           = "/etc/hostinfo"
)

// Init sets a noop TracerProvider, optionally MeterProvider with OTLP HTTP exporter.
// Shutdown must be called on exit.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return nil, errors.New("telemetry: ServiceName is required")
	}

	otel.SetTracerProvider(tracenoop.NewTracerProvider())

	res, err := buildResource(ctx, cfg.ServiceName, cfg.ResourceAttributes)
	if err != nil {
		return nil, err
	}

	var (
		mp            *sdkmetric.MeterProvider
		shutdownFuncs []func(context.Context) error
	)

	if metricsEnabled() {
		opts := append(metricsClientOptions(),
			otlpmetrichttp.WithTemporalitySelector(deltaTemporalitySelector),
		)
		mexp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, err
		}
		reader := sdkmetric.NewPeriodicReader(mexp)
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
		)
		otel.SetMeterProvider(mp)
		shutdownFuncs = append(shutdownFuncs, mp.Shutdown)
		if cfg.RegisterMetrics != nil {
			if err := cfg.RegisterMetrics(); err != nil {
				_ = mp.Shutdown(ctx)
				otel.SetMeterProvider(noop.NewMeterProvider())
				return nil, err
			}
		}
	}

	shutdown = func(ctx context.Context) error {
		var errs []error
		for i := len(shutdownFuncs) - 1; i >= 0; i-- {
			if err := shutdownFuncs[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	return shutdown, nil
}

func buildResource(ctx context.Context, serviceName string, extra []attribute.KeyValue) (*resource.Resource, error) {
	opts := []resource.Option{
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	}
	if len(extra) > 0 {
		opts = append(opts, resource.WithAttributes(extra...))
	}
	return resource.New(ctx, opts...)
}

// Endpoint precedence: OTEL_EXPORTER_OTLP_*_ENDPOINT -> HOST_IP -> /etc/hostinfo.
func metricsEnabled() bool {
	if otlpEndpointFromEnv() != "" {
		return true
	}
	_, ok := resolveNodeIP()
	return ok
}

func metricsClientOptions() []otlpmetrichttp.Option {
	if otlpEndpointFromEnv() != "" {
		return nil
	}
	ip, ok := resolveNodeIP()
	if !ok {
		return nil
	}
	return []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(net.JoinHostPort(ip, otlpHTTPPort)),
		otlpmetrichttp.WithInsecure(),
	}
}

func otlpEndpointFromEnv() string {
	return firstEndpoint(os.Getenv(envOTLPMetricsEndpoint), os.Getenv(envOTLPEndpoint))
}

func resolveNodeIP() (string, bool) {
	if ip := strings.TrimSpace(os.Getenv(envHostIP)); ip != "" && net.ParseIP(ip) != nil {
		return ip, true
	}
	return readHostInfoIP(hostInfoPath)
}

func readHostInfoIP(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if net.ParseIP(line) != nil {
			return line, true
		}
	}
	return "", false
}

func firstEndpoint(primary, fallback string) string {
	if s := strings.TrimSpace(primary); s != "" {
		return s
	}
	return strings.TrimSpace(fallback)
}

// deltaTemporalitySelector returns delta temporality for monotonic instruments
// (Counter, Histogram, ObservableCounter). Gauges and UpDownCounters keep
// the default cumulative semantics.
func deltaTemporalitySelector(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	switch kind {
	case sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindHistogram:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}
