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
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

func TestInitGRPCExportsMetrics(t *testing.T) {
	for _, preference := range []string{"", "delta", "cumulative"} {
		t.Run(preference, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			endpoint, exported := newTestGRPCCollector(t)
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "grpc")
			t.Setenv(envOTLPEndpoint, endpoint)
			t.Setenv(envOTLPMetricsEndpoint, "")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "x-test-header=grpc")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
			t.Setenv(envTemporality, preference)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{
				ServiceName:        "grpc-test",
				ResourceAttributes: []attribute.KeyValue{attribute.String("sandbox_id", "test-sandbox")},
			})
			if err != nil {
				t.Fatal(err)
			}
			stopped := false
			defer func() {
				if !stopped {
					if err := shutdown(ctx); err != nil {
						t.Error(err)
					}
				}
			}()
			counter, err := otel.Meter("test").Int64Counter("grpc.counter")
			if err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 3; round++ {
				counter.Add(ctx, 2)
				if round == 2 {
					stopped = true
					err = shutdown(ctx)
				} else {
					err = ForceFlush(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				var received grpcMetricsExport
				select {
				case received = <-exported:
				case <-ctx.Done():
					t.Fatal("missing gRPC metrics export")
				}
				if got := received.headers.Get("x-test-header"); len(got) != 1 || got[0] != "grpc" {
					t.Fatalf("gRPC export headers = %v", received.headers)
				}
				resources := received.payload.GetResourceMetrics()
				if len(resources) != 1 {
					t.Fatalf("exported %d resources, want 1", len(resources))
				}
				attributes := make(map[string]string)
				for _, attr := range resources[0].GetResource().GetAttributes() {
					attributes[attr.Key] = attr.Value.GetStringValue()
				}
				if attributes["service.name"] != "grpc-test" || attributes["sandbox_id"] != "test-sandbox" {
					t.Fatalf("gRPC resource attributes = %v", attributes)
				}
				scopes := resources[0].GetScopeMetrics()
				if len(scopes) != 1 || len(scopes[0].GetMetrics()) != 1 {
					t.Fatalf("unexpected exported metrics: %v", scopes)
				}
				exportedMetric := scopes[0].GetMetrics()[0]
				sum := exportedMetric.GetSum()
				wantTemporality := metrics.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
				wantValue := int64(2)
				if preference == "cumulative" {
					wantTemporality = metrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
					wantValue *= int64(round + 1)
				}
				if exportedMetric.Name != "grpc.counter" || sum.GetAggregationTemporality() != wantTemporality || len(sum.GetDataPoints()) != 1 || sum.DataPoints[0].GetAsInt() != wantValue {
					t.Fatalf("unexpected gRPC counter round %d: %v", round, exportedMetric)
				}
			}
		})
	}
}

func TestInitGRPCDisabledOrRegistrationFailed(t *testing.T) {
	registrationErr := errors.New("registration failed")
	for _, mode := range []string{"sdk disabled", "metrics disabled", "registration failed"} {
		t.Run(mode, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			endpoint, exported := newTestGRPCCollector(t)
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
			t.Setenv(envOTLPEndpoint, endpoint)
			t.Setenv(envOTLPMetricsEndpoint, "")
			if mode == "sdk disabled" {
				t.Setenv(envSDKDisabled, "true")
			} else if mode == "metrics disabled" {
				t.Setenv(envMetricsExporter, "none")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{
				ServiceName: "grpc-disabled-test",
				RegisterMetrics: func() error {
					if mode != "registration failed" {
						t.Error("disabled initialization registered metrics")
					}
					return registrationErr
				},
			})
			if mode == "registration failed" {
				if !errors.Is(err, registrationErr) || shutdown != nil {
					t.Fatalf("Init = (%v, %v), want nil shutdown and registration error", shutdown == nil, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := otel.GetMeterProvider().(noop.MeterProvider); !ok || meterProvider.Load() != nil {
				t.Fatal("disabled or failed gRPC Init left an active provider")
			}
			if err := ForceFlush(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case received := <-exported:
				// Failed registration still shuts down the reader, which may export an empty batch.
				if mode != "registration failed" {
					t.Fatal("disabled gRPC Init sent an export request")
				}
				for _, resource := range received.payload.GetResourceMetrics() {
					for _, scope := range resource.GetScopeMetrics() {
						if len(scope.GetMetrics()) != 0 {
							t.Fatalf("failed gRPC Init exported metrics: %v", received.payload)
						}
					}
				}
			default:
			}
		})
	}
}

func TestInitGRPCPreservesCollectorHostname(t *testing.T) {
	preserveTelemetryGlobals(t)
	endpoint, exported := newTestGRPCCollector(t)
	dns := resolver.Get("dns")
	testDNS := manual.NewBuilderWithScheme("dns")
	// Resolve the test hostname locally and inspect the target gRPC parsed.
	testDNS.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: strings.TrimPrefix(endpoint, "http://")}}})
	testDNS.BuildCallback = func(target resolver.Target, _ resolver.ClientConn, _ resolver.BuildOptions) {
		if got := target.Endpoint(); got != "dns:4317" {
			t.Errorf("gRPC resolver endpoint = %q, want dns:4317", got)
		}
	}
	resolver.Register(testDNS)
	t.Cleanup(func() { resolver.Register(dns) })

	t.Setenv(envSDKDisabled, "")
	t.Setenv(envMetricsExporter, "")
	t.Setenv(envOTLPMetricsProtocol, "grpc")
	t.Setenv(envOTLPMetricsEndpoint, "http://dns:4317")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdown, err := Init(ctx, Config{ServiceName: "grpc-hostname-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	counter, err := otel.Meter("test").Int64Counter("grpc.hostname.counter")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 1)
	if err := ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exported:
	case <-ctx.Done():
		t.Fatal("missing gRPC export")
	}
}

func TestInitGRPCURLSchemeOverridesSecurityEnv(t *testing.T) {
	for _, tc := range []struct {
		name, insecure string
	}{
		{"https", "true"},
		{"http", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			exported := make(chan grpcMetricsExport, 8)
			grpcServer := grpc.NewServer()
			collector.RegisterMetricsServiceServer(grpcServer, &testGRPCMetricsService{exported: exported})
			server := httptest.NewUnstartedServer(grpcServer)
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(grpcServer.Stop)
			t.Cleanup(server.Close)
			certificatePath := filepath.Join(t.TempDir(), "collector-ca.pem")
			certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
			endpoint := server.URL
			var receivedExports <-chan grpcMetricsExport = exported
			if tc.name == "http" {
				endpoint, receivedExports = newTestGRPCCollector(t)
			}
			t.Setenv(envOTLPEndpoint, endpoint+"/collector/path")
			t.Setenv(envOTLPMetricsEndpoint, "")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", certificatePath)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_INSECURE", tc.insecure)
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{ServiceName: "grpc-tls-test"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := shutdown(ctx); err != nil {
					t.Error(err)
				}
			}()
			counter, err := otel.Meter("test").Int64Counter("grpc.tls.counter")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(ctx, 1)
			if err := ForceFlush(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case received := <-receivedExports:
				if len(received.payload.GetResourceMetrics()) != 1 {
					t.Fatalf("missing metrics over gRPC: %v", received.payload)
				}
			case <-ctx.Done():
				t.Fatal("missing gRPC export")
			}
		})
	}
}

type grpcMetricsExport struct {
	payload *collector.ExportMetricsServiceRequest
	headers metadata.MD
}

type testGRPCMetricsService struct {
	collector.UnimplementedMetricsServiceServer
	exported chan grpcMetricsExport
}

func (server *testGRPCMetricsService) Export(ctx context.Context, payload *collector.ExportMetricsServiceRequest) (*collector.ExportMetricsServiceResponse, error) {
	headers, _ := metadata.FromIncomingContext(ctx)
	select {
	case server.exported <- grpcMetricsExport{payload: payload, headers: headers.Copy()}:
		return &collector.ExportMetricsServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newTestGRPCCollector(t *testing.T) (string, <-chan grpcMetricsExport) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exported := make(chan grpcMetricsExport, 8)
	server := grpc.NewServer()
	collector.RegisterMetricsServiceServer(server, &testGRPCMetricsService{exported: exported})
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Error(err)
		}
	}()
	t.Cleanup(server.Stop)
	return "http://" + listener.Addr().String(), exported
}
