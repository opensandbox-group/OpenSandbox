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

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
	"github.com/alibaba/opensandbox/ingress/pkg/wake"
	internaltelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Other proxy tests can leave background flights or hijacked connections alive.
func runProxyMetricSubprocess(t *testing.T) bool {
	t.Helper()
	if os.Getenv("OPENSANDBOX_PROXY_METRICS_TEST") == t.Name() {
		return false
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	parts := strings.Split(t.Name(), "/")
	for i := range parts {
		parts[i] = "^" + regexp.QuoteMeta(parts[i]) + "$"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run="+strings.Join(parts, "/"), "-test.count=1")
	command.Env = append(os.Environ(), "OPENSANDBOX_PROXY_METRICS_TEST="+t.Name())
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return true
}

func collectProxyMetrics(t *testing.T) func() map[string]*metricspb.Metric {
	t.Helper()
	var mu sync.Mutex
	var exports []*collectormetrics.ExportMetricsServiceRequest
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read metrics export: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		export := new(collectormetrics.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, export); err != nil {
			t.Errorf("decode metrics export: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		exports = append(exports, export)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	for key, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":               collector.URL + "/v1/metrics",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL":               "http/protobuf",
		"OTEL_EXPORTER_OTLP_METRICS_COMPRESSION":            "none",
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "cumulative",
		"OTEL_METRIC_EXPORT_INTERVAL":                       "3600000",
		"OTEL_METRICS_EXPORTER":                             "otlp",
		"OTEL_SDK_DISABLED":                                 "false",
	} {
		t.Setenv(key, value)
	}
	previousMeter, previousTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	shutdown, err := telemetry.Init(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, shutdown(ctx))
		otel.SetMeterProvider(previousMeter)
		otel.SetTracerProvider(previousTracer)
	})

	return func() map[string]*metricspb.Metric {
		t.Helper()
		mu.Lock()
		exports = nil
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, internaltelemetry.ForceFlush(ctx))
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, exports, 1, "ForceFlush must synchronously export one snapshot")
		metrics := make(map[string]*metricspb.Metric)
		for _, resource := range exports[0].ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				if scope.Scope.GetName() != "opensandbox/ingress" {
					continue
				}
				for _, metric := range scope.Metrics {
					require.NotContains(t, metrics, metric.Name)
					metrics[metric.Name] = metric
				}
			}
		}
		return metrics
	}
}

func proxyMetricAttributes(attrs []*commonpb.KeyValue) map[string]any {
	result := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		switch value := attr.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			result[attr.Key] = value.StringValue
		case *commonpb.AnyValue_IntValue:
			result[attr.Key] = value.IntValue
		}
	}
	return result
}

func requireProxyActive(t *testing.T, metrics map[string]*metricspb.Metric, proxyType string, want int64) {
	t.Helper()
	points := metrics["ingress.requests.active"].GetSum().GetDataPoints()
	require.Len(t, points, 1)
	require.Equal(t, map[string]any{"proxy_type": proxyType}, proxyMetricAttributes(points[0].Attributes))
	require.Equal(t, want, points[0].GetAsInt())
}

func requireProxyRequestMetric(t *testing.T, metrics map[string]*metricspb.Metric, status int, proxyType, outcome string) {
	t.Helper()
	count := metrics["ingress.http.request.count"].GetSum().GetDataPoints()
	require.Len(t, count, 1, "one completed request must create exactly one counter series")
	require.Equal(t, int64(1), count[0].GetAsInt())
	attrs := map[string]any{
		"http_method": http.MethodGet, "http_status_code": int64(status),
		"proxy_type": proxyType, "outcome": outcome,
	}
	require.Equal(t, attrs, proxyMetricAttributes(count[0].Attributes))
	duration := metrics["ingress.http.request.duration"].GetHistogram().GetDataPoints()
	require.Len(t, duration, 1)
	require.Equal(t, uint64(1), duration[0].Count, "duration must be recorded exactly once")
	require.Greater(t, duration[0].GetSum(), float64(0))
	delete(attrs, "outcome")
	require.Equal(t, attrs, proxyMetricAttributes(duration[0].Attributes), "outcome must not multiply histogram series")
	requireProxyActive(t, metrics, proxyType, 0)
}

func requireProxyRoutingMetrics(t *testing.T, metrics map[string]*metricspb.Metric, want map[string]uint64) {
	t.Helper()
	require.NotContains(t, metrics, "ingress.routing.resolutions.count")
	points := metrics["ingress.routing.resolution.duration"].GetHistogram().GetDataPoints()
	require.Len(t, points, len(want))
	got := make(map[string]uint64)
	for _, point := range points {
		attrs := proxyMetricAttributes(point.Attributes)
		require.Len(t, attrs, 1)
		result, ok := attrs["routing_result"].(string)
		require.True(t, ok)
		got[result] = point.Count
	}
	require.Equal(t, want, got)
}

func metricHTTPResponse(status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, ContentLength: -1}
}

type metricBrokenBody struct{}

func (metricBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestServeHTTPMetricsTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport roundTripFunc
		status    int
		outcome   string
		abort     bool
	}{
		{
			name: "success", status: http.StatusOK, outcome: "upstream_response",
			transport: func(*http.Request) (*http.Response, error) {
				return metricHTTPResponse(http.StatusOK, io.NopCloser(strings.NewReader("served"))), nil
			},
		},
		{
			name: "application 502", status: http.StatusBadGateway, outcome: "upstream_response",
			transport: func(*http.Request) (*http.Response, error) {
				return metricHTTPResponse(http.StatusBadGateway, io.NopCloser(strings.NewReader("application error"))), nil
			},
		},
		{
			name: "transport failure", status: http.StatusBadGateway, outcome: "upstream_error",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("upstream connection refused")
			},
		},
		{
			name: "aborted stream retains 200", status: http.StatusOK, outcome: "stream_aborted", abort: true,
			transport: func(*http.Request) (*http.Response, error) {
				return metricHTTPResponse(http.StatusOK, io.NopCloser(io.MultiReader(strings.NewReader("partial"), metricBrokenBody{}))), nil
			},
		},
		{
			name: "unexpected panic", status: http.StatusBadGateway, outcome: "ingress_error",
			transport: func(*http.Request) (*http.Response, error) { panic("unexpected transport panic") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runProxyMetricSubprocess(t) {
				return
			}
			collect := collectProxyMetrics(t)
			p := NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil)
			p.httpTransport = tc.transport
			req := httptest.NewRequest(http.MethodGet, "http://sandbox-8080.example.com/", nil)
			// ReverseProxy only re-panics on copy errors when running in a server.
			req = req.WithContext(context.WithValue(req.Context(), http.ServerContextKey, &http.Server{}))
			rec := httptest.NewRecorder()
			if tc.abort {
				require.PanicsWithValue(t, http.ErrAbortHandler, func() { p.ServeHTTP(rec, req) })
				require.Equal(t, "partial", rec.Body.String(), "do not append an ingress error to the committed stream")
			} else {
				require.NotPanics(t, func() { p.ServeHTTP(rec, req) })
			}
			require.Equal(t, tc.status, rec.Code)
			metrics := collect()
			requireProxyRequestMetric(t, metrics, tc.status, "http", tc.outcome)
			requireProxyRoutingMetrics(t, metrics, map[string]uint64{"success": 1})
		})
	}
}

func TestServeHTTPMetricsActiveAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			if runProxyMetricSubprocess(t) {
				return
			}
			collect := collectProxyMetrics(t)
			p := NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			p.httpTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				close(entered)
				select {
				case <-release:
					return metricHTTPResponse(http.StatusOK, http.NoBody), nil
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			})
			req := httptest.NewRequest(http.MethodGet, "http://sandbox-8080.example.com/", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			go func() {
				defer close(done)
				p.ServeHTTP(rec, req)
			}()
			defer func() {
				cancel()
				<-done
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("transport did not start")
			}
			metrics := collect()
			requireProxyActive(t, metrics, "http", 1)
			require.NotContains(t, metrics, "ingress.http.request.count")
			require.NotContains(t, metrics, "ingress.http.request.duration")
			status, outcome := http.StatusOK, "upstream_response"
			if canceled {
				cancel()
				status, outcome = http.StatusBadGateway, "canceled"
			} else {
				close(release)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not complete")
			}
			require.Equal(t, status, rec.Code)
			requireProxyRequestMetric(t, collect(), status, "http", outcome)
		})
	}
}

type metricFailingProvider struct {
	stubNoSecureProvider
	err   error
	calls int
}

func (p *metricFailingProvider) ResolveEndpoint(context.Context, sandbox.EndpointTarget) (*sandbox.EndpointInfo, error) {
	p.calls++
	return nil, p.err
}

func TestServeHTTPMetricsRoutingOnlyMeasuresProviderCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		err    error
		status int
		want   map[string]uint64
	}{
		{"parse failure", "http://invalidhost/", nil, http.StatusBadRequest, map[string]uint64{}},
		{"provider not found", "http://sandbox-8080.example.com/", sandbox.ErrSandboxNotFound, http.StatusNotFound, map[string]uint64{"not_found": 1}},
		{"provider not ready", "http://sandbox-8080.example.com/", sandbox.ErrSandboxNotReady, http.StatusServiceUnavailable, map[string]uint64{"not_ready": 1}},
		{"provider error", "http://sandbox-8080.example.com/", errors.New("provider unavailable"), http.StatusBadGateway, map[string]uint64{"error": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runProxyMetricSubprocess(t) {
				return
			}
			collect := collectProxyMetrics(t)
			provider := &metricFailingProvider{err: tc.err}
			p := NewProxy(context.Background(), provider, ModeHeader, nil, nil, nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, len(tc.want), provider.calls)
			metrics := collect()
			requireProxyRequestMetric(t, metrics, tc.status, "http", "routing_error")
			requireProxyRoutingMetrics(t, metrics, tc.want)
		})
	}
}

func TestServeHTTPMetricsWakeRouting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []resolveAttempt
		stale     bool
		terminal  bool
		status    int
		outcome   string
		routing   map[string]uint64
		forwards  int
	}{
		{
			name:      "resolution retry after wake",
			responses: []resolveAttempt{{err: sandbox.ErrSandboxNotReady}, {info: fsbInfo("http://127.0.0.1:8080")}},
			status:    http.StatusOK, outcome: "upstream_response", forwards: 1,
			routing: map[string]uint64{"not_ready": 1, "success": 1},
		},
		{
			name:      "stale route replay",
			responses: []resolveAttempt{{info: fsbInfo("http://127.0.0.1:8080")}}, stale: true,
			status: http.StatusOK, outcome: "upstream_response", forwards: 2,
			routing: map[string]uint64{"success": 2},
		},
		{
			name:      "wake failure",
			responses: []resolveAttempt{{err: sandbox.ErrSandboxNotReady}}, terminal: true,
			status: http.StatusNotFound, outcome: "wake_error",
			routing: map[string]uint64{"not_ready": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runProxyMetricSubprocess(t) {
				return
			}
			collect := collectProxyMetrics(t)
			provider := &scriptedWakeProvider{responses: tc.responses}
			lifecycle := &fakeLifecycle{probes: []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseWakeable}, {Phase: sandbox.SandboxPhaseReady}}}
			if tc.terminal {
				lifecycle.probes = []sandbox.SandboxProbe{{Phase: sandbox.SandboxPhaseTerminal}}
			}
			p := newWakeTestProxy(t, provider, lifecycle, &recordingActivity{}, wake.Config{ParkBudget: time.Second, ParkMax: 8, RetryInterval: time.Millisecond})
			forwards := 0
			p.httpTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				forwards++
				response := metricHTTPResponse(http.StatusOK, http.NoBody)
				if tc.stale && forwards == 1 {
					response.StatusCode = http.StatusServiceUnavailable
					response.Header.Set(sandbox.FastSandboxProxyError, "stale_route")
				}
				return response, nil
			})
			req := httptest.NewRequest(http.MethodGet, "http://ingress/", nil)
			req.Header.Set(SandboxIngress, fsbScopeVector)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, tc.forwards, forwards)
			metrics := collect()
			requireProxyRequestMetric(t, metrics, tc.status, "http", tc.outcome)
			requireProxyRoutingMetrics(t, metrics, tc.routing)
			var calls uint64
			for _, count := range tc.routing {
				calls += count
			}
			require.Equal(t, int(calls), provider.calls)
		})
	}
}

func TestServeHTTPMetricsWebSocketLifetime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    websocket.StatusCode
		outcome string
	}{
		{"normal close", websocket.StatusNormalClosure, "upstream_response"},
		{"abnormal close", websocket.StatusInternalError, "stream_aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runProxyMetricSubprocess(t) {
				return
			}
			collect := collectProxyMetrics(t)
			closeBackend := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(closeBackend) }) }
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					t.Errorf("accept backend websocket: %v", err)
					return
				}
				defer conn.CloseNow()
				<-closeBackend
				_ = conn.Close(tc.code, "test finished")
			}))
			defer backend.Close()
			defer release()
			p := NewProxy(context.Background(), stubNoSecureProvider{}, ModeHeader, nil, nil, nil)
			done := make(chan struct{})
			ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				p.ServeHTTP(w, r)
			}))
			defer ingress.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			header := http.Header{SandboxIngress: []string{fmt.Sprintf("sandbox-%d", backend.Listener.Addr().(*net.TCPAddr).Port)}}
			conn, _, err := websocket.Dial(ctx, ingress.URL, &websocket.DialOptions{HTTPHeader: header})
			require.NoError(t, err)
			defer conn.CloseNow()
			defer func() {
				release()
				_ = conn.CloseNow()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("websocket handler did not complete")
				}
			}()
			metrics := collect()
			requireProxyActive(t, metrics, "websocket", 1)
			require.NotContains(t, metrics, "ingress.http.request.count", "an open websocket is not a completed request")
			require.NotContains(t, metrics, "ingress.http.request.duration")
			release()
			_, _, err = conn.Read(ctx)
			require.Equal(t, tc.code, websocket.CloseStatus(err))
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("websocket handler did not finish after close")
			}
			requireProxyRequestMetric(t, collect(), http.StatusSwitchingProtocols, "websocket", tc.outcome)
		})
	}
}
