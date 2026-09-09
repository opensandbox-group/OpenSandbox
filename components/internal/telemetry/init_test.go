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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestMetricsEnabledCanDisableEndpointFallback(t *testing.T) {
	t.Setenv(envSDKDisabled, "")
	t.Setenv(envMetricsExporter, "")
	t.Setenv(envOTLPMetricsEndpoint, "")
	t.Setenv(envOTLPEndpoint, "")
	t.Setenv(envHostIP, "192.0.2.10")

	if !metricsEnabled(false) {
		t.Fatal("expected historical HOST_IP fallback to enable metrics")
	}
	if metricsEnabled(true) {
		t.Fatal("expected endpoint fallback to be disabled")
	}
}

func TestExplicitEndpointWinsWhenFallbackDisabled(t *testing.T) {
	t.Setenv(envSDKDisabled, "")
	t.Setenv(envMetricsExporter, "")
	t.Setenv(envOTLPMetricsEndpoint, "https://collector.example/v1/metrics")
	t.Setenv(envOTLPEndpoint, "")
	t.Setenv(envHostIP, "")

	if !metricsEnabled(true) {
		t.Fatal("expected explicit endpoint to enable metrics")
	}
}

func TestMetricsDisableSwitches(t *testing.T) {
	for _, tc := range []struct {
		name, disabled, exporter string
		wantEnabled              bool
	}{
		{"unset", "", "", true},
		{"sdk disabled", "true", "otlp", false},
		{"sdk mixed case", "TrUe", "", false},
		{"metrics disabled", "false", "none", false},
		{"metrics mixed case", "", "NoNe", false},
		{"sdk false", "false", "otlp", true},
		{"invalid boolean", "1", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envSDKDisabled, tc.disabled)
			t.Setenv(envMetricsExporter, tc.exporter)
			t.Setenv(envOTLPMetricsEndpoint, "")
			t.Setenv(envOTLPEndpoint, "")
			t.Setenv(envHostIP, "192.0.2.10")
			if got := metricsEnabled(false); got != tc.wantEnabled {
				t.Fatalf("node fallback enabled = %v, want %v", got, tc.wantEnabled)
			}
			t.Setenv(envOTLPEndpoint, "http://collector.example:4318")
			t.Setenv(envOTLPMetricsEndpoint, "http://collector.example:4318/v1/metrics")
			for _, disableFallback := range []bool{false, true} {
				if got := metricsEnabled(disableFallback); got != tc.wantEnabled {
					t.Fatalf("explicit endpoint enabled = %v, want %v", got, tc.wantEnabled)
				}
			}
		})
	}
}

func TestDisabledInitDoesNotExport(t *testing.T) {
	for _, key := range []string{envSDKDisabled, envMetricsExporter} {
		t.Run(key, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			endpoint, exported := newTestCollector(t)
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv(envOTLPEndpoint, endpoint)
			t.Setenv(envOTLPMetricsEndpoint, endpoint+"/v1/metrics")
			t.Setenv(envHostIP, "192.0.2.10")
			if key == envSDKDisabled {
				t.Setenv(key, "true")
			} else {
				t.Setenv(key, "none")
			}
			// Disabled initialization must replace any previously installed provider
			// and clear the separate ForceFlush target as well.
			old := sdkmetric.NewMeterProvider()
			otel.SetMeterProvider(old)
			meterProvider.Store(old)
			t.Cleanup(func() { _ = old.Shutdown(context.Background()) })
			shutdown, err := Init(context.Background(), Config{
				ServiceName: "disabled-test",
				RegisterMetrics: func() error {
					t.Error("disabled Init registered metrics")
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := otel.GetMeterProvider().(noop.MeterProvider); !ok || meterProvider.Load() != nil {
				t.Fatal("disabled Init left an active metrics provider")
			}
			counter, err := otel.Meter("test").Int64Counter("disabled.count")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(context.Background(), 1)
			if err := ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exported:
				t.Fatal("disabled metrics sent an OTLP request")
			default:
			}
		})
	}
}

func TestInitExportsConfiguredTemporality(t *testing.T) {
	const cumulative = metrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	const delta = metrics.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	for _, tc := range []struct {
		preference       string
		sync, observable metrics.AggregationTemporality
	}{
		{"", delta, cumulative},
		{"cumulative", cumulative, cumulative},
		{"delta", delta, delta},
		{"lowmemory", delta, cumulative},
		{"CuMuLaTiVe", cumulative, cumulative},
		{"invalid", cumulative, cumulative},
	} {
		t.Run(tc.preference, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			endpoint, exported := newTestCollector(t)
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv(envTemporality, tc.preference)
			t.Setenv(envOTLPEndpoint, endpoint)
			t.Setenv(envOTLPMetricsEndpoint, "")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{ServiceName: "temporality-test"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := shutdown(ctx); err != nil {
					t.Error(err)
				}
			}()
			meter := otel.Meter("test")
			counter, err := meter.Int64Counter("counter")
			if err != nil {
				t.Fatal(err)
			}
			histogram, err := meter.Int64Histogram("histogram")
			if err != nil {
				t.Fatal(err)
			}
			upDown, err := meter.Int64UpDownCounter("updown")
			if err != nil {
				t.Fatal(err)
			}
			observableValue := int64(10)
			_, err = meter.Int64ObservableCounter("observable", metric.WithInt64Callback(
				func(_ context.Context, observer metric.Int64Observer) error {
					observer.Observe(observableValue)
					return nil
				},
			))
			if err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 2; round++ {
				counter.Add(ctx, 2)
				histogram.Record(ctx, 5)
				upDown.Add(ctx, 1)
				observableValue = 10 + int64(round)*5
				if err := ForceFlush(ctx); err != nil {
					t.Fatal(err)
				}
				var payload *collector.ExportMetricsServiceRequest
				select {
				case payload = <-exported:
				case <-ctx.Done():
					t.Fatal("missing OTLP export")
				}
				seen := 0
				for _, resource := range payload.ResourceMetrics {
					for _, scope := range resource.ScopeMetrics {
						for _, m := range scope.Metrics {
							seen++
							if m.Name == "histogram" {
								h := m.GetHistogram()
								wantCount := uint64(1)
								if tc.sync == cumulative {
									wantCount += uint64(round)
								}
								if h.GetAggregationTemporality() != tc.sync || len(h.DataPoints) != 1 || h.DataPoints[0].Count != wantCount {
									t.Fatalf("unexpected histogram round %d: %v", round, h)
								}
								continue
							}
							wantTempo, wantValue := tc.sync, int64(2)
							switch m.Name {
							case "counter":
								if tc.sync == cumulative {
									wantValue *= int64(round + 1)
								}
							case "observable":
								wantTempo, wantValue = tc.observable, observableValue
								if tc.observable == delta && round == 1 {
									wantValue = 5
								}
							case "updown":
								wantTempo, wantValue = cumulative, int64(round+1)
							default:
								t.Fatalf("unexpected metric %q", m.Name)
							}
							sum := m.GetSum()
							if sum.GetAggregationTemporality() != wantTempo || len(sum.DataPoints) != 1 || sum.DataPoints[0].GetAsInt() != wantValue {
								t.Fatalf("unexpected %s round %d: %v", m.Name, round, sum)
							}
						}
					}
				}
				if seen != 4 {
					t.Fatalf("exported %d metrics, want 4", seen)
				}
			}
		})
	}
}

func preserveTelemetryGlobals(t *testing.T) {
	t.Helper()
	mp, tp, flush := otel.GetMeterProvider(), otel.GetTracerProvider(), meterProvider.Load()
	t.Cleanup(func() {
		otel.SetMeterProvider(mp)
		otel.SetTracerProvider(tp)
		meterProvider.Store(flush)
	})
}

func newTestCollector(t *testing.T) (string, <-chan *collector.ExportMetricsServiceRequest) {
	t.Helper()
	exported := make(chan *collector.ExportMetricsServiceRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		payload := &collector.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(data, payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		exported <- payload
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server.URL, exported
}
