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
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
		mexp, err := otlpmetrichttp.New(ctx)
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

func metricsEnabled() bool {
	return firstEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

func firstEndpoint(primary, fallback string) string {
	if s := strings.TrimSpace(primary); s != "" {
		return s
	}
	return strings.TrimSpace(fallback)
}
