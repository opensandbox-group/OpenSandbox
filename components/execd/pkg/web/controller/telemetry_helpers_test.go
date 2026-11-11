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

package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestDownloadFilesystemMetricResult(t *testing.T) {
	exported := make(chan *collector.ExportMetricsServiceRequest, 16)
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
	}))
	defer server.Close()
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	oldMeter, oldTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	defer otel.SetMeterProvider(oldMeter)
	defer otel.SetTracerProvider(oldTracer)
	shutdown, err := telemetry.Init(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(context.Background())) }()
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o600))
	for _, tc := range []struct {
		name, query, rangeHeader string
		status                   int
		result                   string
	}{
		{"whole file", "", "", 200, "success"},
		{"valid lines", "&offset=1&limit=1", "", 200, "success"},
		{"invalid offset", "&offset=0", "", 400, "failure"},
		{"invalid limit", "&limit=0", "", 400, "failure"},
		{"valid range", "", "bytes=0-1", 206, "success"},
		{"range past EOF", "", "bytes=99-", 416, "failure"},
		{"missing file", "", "", 404, "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := path
			if tc.name == "missing file" {
				target += ".missing"
			}
			ctrl, rec := newFilesystemController(t, http.MethodGet, "/files/download?path="+url.QueryEscape(target)+tc.query, nil)
			if tc.rangeHeader != "" {
				ctrl.ctx.Request.Header.Set("Range", tc.rangeHeader)
			}
			ctrl.DownloadFile()
			require.Equal(t, tc.status, rec.Code)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, inttelemetry.ForceFlush(ctx))
			var payload *collector.ExportMetricsServiceRequest
			select {
			case payload = <-exported:
			case <-ctx.Done():
				t.Fatal("no exported metrics")
			}
			results := map[string]uint64{}
			for _, resource := range payload.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					for _, metric := range scope.Metrics {
						if metric.Name != "execd.filesystem.operations.duration" {
							continue
						}
						for _, point := range metric.GetHistogram().GetDataPoints() {
							for _, attr := range point.Attributes {
								if attr.Key == "result" {
									results[attr.Value.GetStringValue()] += point.Count
								}
							}
						}
					}
				}
			}
			require.Equal(t, map[string]uint64{tc.result: 1}, results)
		})
	}
}
