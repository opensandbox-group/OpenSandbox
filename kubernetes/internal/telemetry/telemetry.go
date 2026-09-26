// Copyright 2026 The OpenSandbox Authors
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

// Package telemetry provides OpenTelemetry setup for the sandbox controller.
// Export is configured through standard OTEL_* environment variables; see
// docs/telemetry.md.
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	// DefaultServiceName is used when OTEL_SERVICE_NAME is not set.
	DefaultServiceName = "opensandbox-controller"

	// MetricsEndpointEnv and EndpointEnv are the standard OTLP endpoint
	// variables, kept exported for the startup log.
	MetricsEndpointEnv = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	EndpointEnv        = "OTEL_EXPORTER_OTLP_ENDPOINT"

	serviceNameEnv = "OTEL_SERVICE_NAME"
	sdkDisabledEnv = "OTEL_SDK_DISABLED"
	exporterEnv    = "OTEL_METRICS_EXPORTER"
)

// Setup installs the global meter provider backed by an OTLP/HTTP exporter
// configured through standard OTEL_* environment variables, parsed by the OTel
// SDK. It reports whether export is enabled, and the returned shutdown flushes
// pending data. When disabled, the no-op provider stays installed and shutdown
// is a no-op.
func Setup(ctx context.Context) (enabled bool, shutdown func(context.Context) error, err error) {
	if exportDisabled() || (strings.TrimSpace(os.Getenv(MetricsEndpointEnv)) == "" &&
		strings.TrimSpace(os.Getenv(EndpointEnv)) == "") {
		return false, func(context.Context) error { return nil }, nil
	}

	// No exporter options: the SDK applies the standard OTEL_* env contract.
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return false, noopShutdown(), fmt.Errorf("failed to create OTLP metric exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName())),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return false, noopShutdown(), fmt.Errorf("failed to build telemetry resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	otel.SetMeterProvider(provider)
	return true, provider.Shutdown, nil
}

func noopShutdown() func(context.Context) error {
	return func(context.Context) error { return nil }
}

// SanitizeEndpoint strips userinfo, query, and fragment so an endpoint is safe to log.
func SanitizeEndpoint(endpoint string) string {
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

// serviceName honors OTEL_SERVICE_NAME with a controller default.
func serviceName() string {
	if name := strings.TrimSpace(os.Getenv(serviceNameEnv)); name != "" {
		return name
	}
	return DefaultServiceName
}

// exportDisabled reports whether OTEL_SDK_DISABLED=true or OTEL_METRICS_EXPORTER
// (a comma-separated list) does not include otlp, the only exporter implemented.
func exportDisabled() bool {
	if strings.EqualFold(os.Getenv(sdkDisabledEnv), "true") {
		return true
	}
	raw := strings.TrimSpace(os.Getenv(exporterEnv))
	if raw == "" {
		return false
	}
	for _, exporter := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(exporter), "otlp") {
			return false
		}
	}
	return true
}
