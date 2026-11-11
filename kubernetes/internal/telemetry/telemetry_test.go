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

package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func TestSanitizeEndpoint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "http://collector:4318", want: "http://collector:4318"},
		{name: "drops query and user", in: "http://user:pass@collector:4318/v1/metrics?token=secret", want: "http://collector:4318/v1/metrics"},
		{name: "invalid", in: "::::", want: "<invalid>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeEndpoint(tt.in); got != tt.want {
				t.Fatalf("SanitizeEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestServiceName(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(serviceNameEnv, "")
		if got := serviceName(); got != DefaultServiceName {
			t.Fatalf("serviceName() = %q, want %q", got, DefaultServiceName)
		}
	})
	t.Run("env override", func(t *testing.T) {
		t.Setenv(serviceNameEnv, "custom-controller")
		if got := serviceName(); got != "custom-controller" {
			t.Fatalf("serviceName() = %q, want custom-controller", got)
		}
	})
}

func TestSetupDisabledWithoutEndpoint(t *testing.T) {
	t.Setenv(MetricsEndpointEnv, "")
	t.Setenv(EndpointEnv, "")

	enabled, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if enabled {
		t.Fatal("Setup() reported enabled without endpoint")
	}
	if shutdown == nil {
		t.Fatal("Setup() returned nil shutdown for disabled telemetry")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestSetupDisabledByStandardEnv(t *testing.T) {
	t.Run("OTEL_SDK_DISABLED", func(t *testing.T) {
		t.Setenv(MetricsEndpointEnv, "http://collector:4318")
		t.Setenv(EndpointEnv, "")
		t.Setenv(sdkDisabledEnv, "true")
		enabled, _, err := Setup(context.Background())
		if err != nil {
			t.Fatalf("Setup() error = %v", err)
		}
		if enabled {
			t.Fatal("Setup() reported enabled with OTEL_SDK_DISABLED=true")
		}
	})
	disabledExporters := []string{"none", "prometheus", "console,foo"}
	for _, exporter := range disabledExporters {
		t.Run("OTEL_METRICS_EXPORTER="+exporter, func(t *testing.T) {
			t.Setenv(MetricsEndpointEnv, "http://collector:4318")
			t.Setenv(EndpointEnv, "")
			t.Setenv(exporterEnv, exporter)
			enabled, _, err := Setup(context.Background())
			if err != nil {
				t.Fatalf("Setup() error = %v", err)
			}
			if enabled {
				t.Fatalf("Setup() reported enabled with OTEL_METRICS_EXPORTER=%s", exporter)
			}
		})
	}
	t.Run("OTEL_METRICS_EXPORTER=otlp stays enabled", func(t *testing.T) {
		captureMeterProvider(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		t.Setenv(MetricsEndpointEnv, "")
		t.Setenv(EndpointEnv, server.URL)
		t.Setenv(exporterEnv, "otlp")
		enabled, shutdown, err := Setup(context.Background())
		if err != nil {
			t.Fatalf("Setup() error = %v", err)
		}
		if !enabled {
			t.Fatal("Setup() reported disabled with OTEL_METRICS_EXPORTER=otlp")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			t.Fatalf("shutdown() error = %v", err)
		}
	})
}

// TestSetupExportsMetrics verifies env-only configuration exports over
// OTLP/HTTP (generic endpoint is a base URL: /v1/metrics is appended) and
// flushes on shutdown.
func TestSetupExportsMetrics(t *testing.T) {
	captureMeterProvider(t)

	requests := make(chan *http.Request, 4)
	bodies := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(MetricsEndpointEnv, "")
	t.Setenv(EndpointEnv, server.URL)

	enabled, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if !enabled {
		t.Fatal("Setup() reported disabled with endpoint configured")
	}

	counter, err := otel.GetMeterProvider().Meter("telemetry_test").Int64Counter("telemetry.test.counter")
	if err != nil {
		t.Fatalf("failed to create counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	select {
	case r := <-requests:
		if got := r.URL.Path; got != "/v1/metrics" {
			t.Errorf("export path = %q, want /v1/metrics", got)
		}
		if ct := r.Header.Get("Content-Type"); ct == "" {
			t.Error("export request missing Content-Type")
		}
		if body := <-bodies; len(body) == 0 {
			t.Error("export request has empty body")
		}
	default:
		t.Fatal("no OTLP export request reached the test server")
	}
}

// TestSetupKeepsCustomExportPath verifies a per-signal endpoint URL path is used as-is.
func TestSetupKeepsCustomExportPath(t *testing.T) {
	captureMeterProvider(t)

	pathCh := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		pathCh <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(EndpointEnv, "")
	t.Setenv(MetricsEndpointEnv, server.URL+"/custom/v1/metrics")

	enabled, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if !enabled {
		t.Fatal("Setup() reported disabled with endpoint configured")
	}
	counter, err := otel.GetMeterProvider().Meter("telemetry_test").Int64Counter("telemetry.test.counter")
	if err != nil {
		t.Fatalf("failed to create counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	select {
	case p := <-pathCh:
		if p != "/custom/v1/metrics" {
			t.Fatalf("export path = %q, want /custom/v1/metrics", p)
		}
	default:
		t.Fatal("no OTLP export request reached the test server")
	}
}

// captureMeterProvider restores the previous global meter provider on cleanup.
func captureMeterProvider(t *testing.T) {
	t.Helper()
	previous := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(previous) })
}
