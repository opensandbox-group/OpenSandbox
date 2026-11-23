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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
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
		{" \t\n", delta, cumulative},
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

func TestInitRegistrationFailureClearsProvider(t *testing.T) {
	preserveTelemetryGlobals(t)
	endpoint, _ := newTestCollector(t)
	t.Setenv(envSDKDisabled, "")
	t.Setenv(envMetricsExporter, "")
	t.Setenv(envOTLPEndpoint, endpoint)
	t.Setenv(envOTLPMetricsEndpoint, "")
	registrationErr := errors.New("registration failed")
	shutdown, err := Init(context.Background(), Config{
		ServiceName:     "registration-failure-test",
		RegisterMetrics: func() error { return registrationErr },
	})
	if !errors.Is(err, registrationErr) || shutdown != nil {
		t.Fatalf("Init returned (%v, %v), want nil shutdown and registration error", shutdown == nil, err)
	}
	if _, ok := otel.GetMeterProvider().(noop.MeterProvider); !ok || meterProvider.Load() != nil {
		t.Fatal("failed Init left an active metrics provider")
	}
	if err := ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush after failed Init: %v", err)
	}
}

func TestInitAppliesDialControl(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
	}{
		{name: "http", protocol: otlpHTTPProtocol},
		{name: "grpc", protocol: otlpGRPCProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			t.Setenv(envSDKDisabled, "")
			t.Setenv(envMetricsExporter, "")
			t.Setenv(envOTLPMetricsEndpoint, "")
			t.Setenv(envOTLPProtocol, "")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")

			var waitExported func()
			if tc.protocol == otlpGRPCProtocol {
				endpoint, exported := newTestGRPCCollector(t)
				t.Setenv(envOTLPMetricsProtocol, otlpGRPCProtocol)
				t.Setenv(envOTLPEndpoint, endpoint)
				waitExported = func() {
					select {
					case <-exported:
					case <-time.After(3 * time.Second):
						t.Fatal("timed out waiting for OTLP gRPC export")
					}
				}
			} else {
				endpoint, exported := newTestCollector(t)
				t.Setenv(envOTLPMetricsProtocol, otlpHTTPProtocol)
				t.Setenv(envOTLPEndpoint, endpoint)
				waitExported = func() {
					select {
					case <-exported:
					case <-time.After(3 * time.Second):
						t.Fatal("timed out waiting for OTLP HTTP export")
					}
				}
			}

			var dialed atomic.Int32
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{
				ServiceName: "dial-control-test",
				DialControl: func(network, address string, c syscall.RawConn) error {
					dialed.Add(1)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = shutdown(context.Background())
			}()

			counter, err := otel.Meter("test").Int64Counter("dial.control.count")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(ctx, 1)
			if err := ForceFlush(ctx); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			waitExported()
			if dialed.Load() == 0 {
				t.Fatal("expected DialControl to be invoked on OTLP dial")
			}
		})
	}
}

func TestInitAppliesDialControlWithMTLS(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	issue := func(serial int64, isServer bool) (certPEM, keyPEM []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "test-leaf"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if isServer {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}

	serverCertPEM, serverKeyPEM := issue(2, true)
	serverPair, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM, clientKeyPEM := issue(3, false)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	clientCertFile := filepath.Join(dir, "client.pem")
	clientKeyFile := filepath.Join(dir, "client-key.pem")
	for path, data := range map[string][]byte{
		caFile:         caPEM,
		clientCertFile: clientCertPEM,
		clientKeyFile:  clientKeyPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name    string
		caEnv   string
		certEnv string
		keyEnv  string
	}{
		{"general", envOTLPCertificate, envOTLPClientCert, envOTLPClientKey},
		{"metrics-specific", envOTLPMetricsCertificate, envOTLPMetricsClientCert, envOTLPMetricsClientKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveTelemetryGlobals(t)
			exported := make(chan *collector.ExportMetricsServiceRequest, 4)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			server.TLS = &tls.Config{
				Certificates: []tls.Certificate{serverPair},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    caPool,
				MinVersion:   tls.VersionTLS12,
			}
			server.StartTLS()
			t.Cleanup(server.Close)

			for _, k := range []string{
				envSDKDisabled, envMetricsExporter, envOTLPMetricsEndpoint,
				envOTLPProtocol, envOTLPMetricsProtocol,
				envOTLPCertificate, envOTLPMetricsCertificate,
				envOTLPClientCert, envOTLPMetricsClientCert,
				envOTLPClientKey, envOTLPMetricsClientKey,
			} {
				t.Setenv(k, "")
			}
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
			t.Setenv(envOTLPEndpoint, server.URL)
			t.Setenv(tc.caEnv, caFile)
			t.Setenv(tc.certEnv, clientCertFile)
			t.Setenv(tc.keyEnv, clientKeyFile)

			var dialed atomic.Int32
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdown, err := Init(ctx, Config{
				ServiceName: "mtls-dial-control-test",
				DialControl: func(network, address string, c syscall.RawConn) error {
					dialed.Add(1)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = shutdown(context.Background()) }()

			counter, err := otel.Meter("test").Int64Counter("mtls.dial.control.count")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(ctx, 1)
			if err := ForceFlush(ctx); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			select {
			case <-exported:
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for mTLS OTLP export")
			}
			if dialed.Load() == 0 {
				t.Fatal("expected DialControl to be invoked on mTLS OTLP dial")
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
	t.Setenv(envOTLPMetricsProtocol, "")
	t.Setenv(envOTLPProtocol, "")
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
