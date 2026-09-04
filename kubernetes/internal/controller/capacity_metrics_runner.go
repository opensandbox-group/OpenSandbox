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
	return mgr.Add(&capacityMetricsRunner{reader: mgr.GetClient(), allocations: allocations})
}

func (r *capacityMetricsRunner) NeedLeaderElection() bool {
	return true
}

func (r *capacityMetricsRunner) Start(ctx context.Context) error {
	if !capacityMetricsEnabled() {
		return nil
	}
	logger := logf.FromContext(ctx).WithName("capacity-metrics")
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		logger.Error(err, "Unable to initialize OTLP metrics exporter")
		return nil
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(controllerServiceName)),
	)
	if err != nil {
		logger.Error(err, "Unable to initialize OTLP resource")
		return nil
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	registration, err := registerCapacityMetrics(provider.Meter(capacityMeterName), r.reader, r.allocations)
	if err != nil {
		logger.Error(err, "Unable to register capacity metrics")
		shutdownMetricProvider(logger, provider)
		return nil
	}

	<-ctx.Done()
	if err := registration.Unregister(); err != nil {
		logger.Error(err, "Unable to unregister capacity metrics")
	}
	shutdownMetricProvider(logger, provider)
	return nil
}

func capacityMetricsEnabled() bool {
	return strings.TrimSpace(os.Getenv(otelMetricsEndpointEnvironment)) != "" ||
		strings.TrimSpace(os.Getenv(otelEndpointEnvironment)) != ""
}

func shutdownMetricProvider(logger logr.Logger, provider *sdkmetric.MeterProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provider.Shutdown(ctx); err != nil {
		logger.Error(err, "Unable to shut down OTLP metrics provider")
	}
}
