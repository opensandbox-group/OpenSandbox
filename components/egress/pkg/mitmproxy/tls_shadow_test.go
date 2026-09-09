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

package mitmproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestShadowOutputReachesOTLP(t *testing.T) {
	received := make(chan []byte, 8)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		received <- body
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer endpoint.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", endpoint.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "600000")
	previousMeter, previousTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetMeterProvider(previousMeter); otel.SetTracerProvider(previousTracer) })
	shutdown, err := telemetry.Init(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(context.Background())) }()
	forwardMitmdumpOutput(io.NopCloser(strings.NewReader(
		"[12:34:56.000] credential proxy: tls-shadow binding_host\n" +
			"credential proxy: tls-shadow binding_host\n" +
			"credential proxy: tls-shadow lookup_failed\n" +
			"credential proxy: tls-shadow secret.example.com\n" +
			"10.0.0.1: GET /credential proxy: tls-shadow binding_host\n")))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, telemetry.ForceFlush(ctx))
	select {
	case body := <-received:
		var req collectormetrics.ExportMetricsServiceRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		counts := map[string]int64{}
		for _, resource := range req.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name != "egress.mitm.shadow.requests_total" {
						continue
					}
					for _, point := range metric.GetSum().DataPoints {
						for _, attr := range point.Attributes {
							if attr.Key == "reason" {
								counts[attr.Value.GetStringValue()] += point.GetAsInt()
							}
						}
					}
				}
			}
		}
		require.Equal(t, map[string]int64{"binding_host": 2, "lookup_failed": 1}, counts)
	case <-ctx.Done():
		t.Fatal("shadow samples never reached the OTLP receiver")
	}
}
