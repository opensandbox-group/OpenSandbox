// Copyright 2026 Alibaba Group Holding Ltd.
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
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	controllerServiceName          = "opensandbox-controller"
	otelMetricsEndpointEnvironment = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	otelEndpointEnvironment        = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

type capacityMetricsRunner struct {
	reader      client.Reader
	allocations poolAllocationReader
}

func SetupCapacityMetricsWithManager(mgr manager.Manager, allocations Allocator) error {
	return mgr.Add(&capacityMetricsRunner{reader: mgr.GetCache(), allocations: allocations})
}

func (r *capacityMetricsRunner) NeedLeaderElection() bool {
	return true
}

func (r *capacityMetricsRunner) Start(ctx context.Context) error {
	endpointEnvironment, endpoint, enabled := capacityMetricsEndpoint()
	if !enabled {
		return nil
	}
	logger := logf.FromContext(ctx).WithName("capacity-metrics")
	safeEndpoint := sanitizeOTLPEndpoint(endpoint)
	if err := validateOTLPEndpoint(endpoint); err != nil {
		logCapacityMetricsDisabled(logger, err, "configuration", endpointEnvironment, safeEndpoint)
		return nil
	}
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		logCapacityMetricsDisabled(logger, err, "exporter", endpointEnvironment, safeEndpoint)
		return nil
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(controllerServiceName)),
	)
	if err != nil {
		logCapacityMetricsDisabled(logger, err, "resource", endpointEnvironment, safeEndpoint)
		shutdownMetricExporter(logger, exporter)
		return nil
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	registration, err := registerCapacityMetrics(provider.Meter(capacityMeterName), r.reader, r.allocations)
	if err != nil {
		logCapacityMetricsDisabled(logger, err, "registration", endpointEnvironment, safeEndpoint)
		shutdownMetricProvider(logger, provider)
		return nil
	}
	logger.Info("Capacity metrics enabled", "endpointEnvironment", endpointEnvironment, "endpoint", safeEndpoint)

	<-ctx.Done()
	if err := registration.Unregister(); err != nil {
		logger.Error(err, "Unable to unregister capacity metrics")
	}
	shutdownMetricProvider(logger, provider)
	return nil
}

func capacityMetricsEndpoint() (environment, endpoint string, enabled bool) {
	if endpoint := strings.TrimSpace(os.Getenv(otelMetricsEndpointEnvironment)); endpoint != "" {
		return otelMetricsEndpointEnvironment, endpoint, true
	}
	if endpoint := strings.TrimSpace(os.Getenv(otelEndpointEnvironment)); endpoint != "" {
		return otelEndpointEnvironment, endpoint, true
	}
	return "", "", false
}

func validateOTLPEndpoint(endpoint string) error {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("endpoint scheme must be http or https")
	}
	return nil
}

func sanitizeOTLPEndpoint(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<invalid>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func logCapacityMetricsDisabled(logger logr.Logger, err error, stage, endpointEnvironment, endpoint string) {
	logger.Error(err, "Capacity metrics disabled after OTLP setup failure",
		"stage", stage, "endpointEnvironment", endpointEnvironment, "endpoint", endpoint)
}

func shutdownMetricExporter(logger logr.Logger, exporter *otlpmetrichttp.Exporter) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exporter.Shutdown(ctx); err != nil {
		logger.Error(err, "Unable to shut down OTLP metrics exporter")
	}
}

func shutdownMetricProvider(logger logr.Logger, provider *sdkmetric.MeterProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provider.Shutdown(ctx); err != nil {
		logger.Error(err, "Unable to shut down OTLP metrics provider")
	}
}
