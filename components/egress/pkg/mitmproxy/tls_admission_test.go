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

package mitmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestTLSAdmissionEventStrictStructure(t *testing.T) {
	for _, tc := range []struct {
		name, line, decision, reason string
		valid                        bool
	}{
		{"decrypt", "credential proxy: tls-admission-v1 decrypt binding_host", "decrypt", "binding_host", true},
		{"passthrough", "credential proxy: tls-admission-v1 passthrough ech", "passthrough", "ech", true},
		{"deny", "credential proxy: tls-admission-v1 deny invalid_sni", "deny", "invalid_sni", true},
		// The recorder, not this structural parser, owns the closed vocabulary.
		{"unknown labels", "credential proxy: tls-admission-v1 future unknown", "future", "unknown", true},
		{"empty", "", "", "", false},
		{"no labels", "credential proxy: tls-admission-v1", "", "", false},
		{"one label", "credential proxy: tls-admission-v1 decrypt", "", "", false},
		{"extra field", "credential proxy: tls-admission-v1 decrypt binding_host host=private.example.com", "", "", false},
		{"wrong version", "credential proxy: tls-admission-v2 decrypt binding_host", "", "", false},
		{"version suffix", "credential proxy: tls-admission-v10 decrypt binding_host", "", "", false},
		{"flow prefix", "10.0.0.1: GET /credential proxy: tls-admission-v1 decrypt binding_host", "", "", false},
		{"leading space", " credential proxy: tls-admission-v1 decrypt binding_host", "", "", false},
		{"double separator", "credential proxy: tls-admission-v1 decrypt  binding_host", "", "", false},
		{"tab separator", "credential proxy: tls-admission-v1 decrypt\tbinding_host", "", "", false},
		{"trailing field separator", "credential proxy: tls-admission-v1 decrypt binding_host ", "", "", false},
		{"embedded newline", "credential proxy: tls-admission-v1 decrypt\nbinding_host", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, reason, ok := tlsAdmissionEvent(tc.line)
			require.Equal(t, tc.valid, ok)
			if ok {
				require.Equal(t, tc.decision, decision)
				require.Equal(t, tc.reason, reason)
			}
		})
	}
}

// This exercises the real Python producer helper and child stdout pipe, the
// Go log forwarding/parser, and OTLP counter export to a local test receiver.
// Only the Python mitmproxy logging context is stubbed; no TLS handshake or
// runtime admission behavior is claimed by this bridge test.
func TestTLSAdmissionPythonOutputReachesOTLP(t *testing.T) {
	python := os.Getenv("OPENSANDBOX_TEST_PYTHON")
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		require.NoError(t, err, "Python is required for the TLS admission producer/consumer contract")
	}
	received := make(chan []byte, 8)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		received <- body
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", endpoint.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "600000")
	previousMeter, previousTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetMeterProvider(previousMeter); otel.SetTracerProvider(previousTracer) })
	shutdown, err := telemetry.Init(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(context.Background())) }()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	fixtureBytes, err := os.ReadFile(filepath.Join(root, "testdata", "tls_admission_events.json"))
	require.NoError(t, err)
	var fixture struct {
		Valid []struct{ Decision, Reason string }
	}
	require.NoError(t, json.Unmarshal(fixtureBytes, &fixture))
	require.Len(t, fixture.Valid, 14)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", `
import json
import sys
from pathlib import Path
from types import SimpleNamespace
root = Path(sys.argv[1])
sys.path.insert(0, str(root / "tests"))
from test_mitmscripts_system import _load_system_module
system = _load_system_module()
# Match the timestamp prefix emitted by mitmdump's terminal logger.
system.ctx.log = SimpleNamespace(warn=lambda line: print("[12:34:56.000] " + line, flush=True))
fixture = json.loads((root / "testdata" / "tls_admission_events.json").read_text())
for pair in fixture["valid"] + fixture["invalid"]:
    system._record_tls_admission(pair["decision"], pair["reason"])
# The real helper, not a handwritten event, produces the repeated count.
system._record_tls_admission("decrypt", "binding_host")
# Untrusted flow lines and malformed event lines must not increment counters.
print("10.0.0.1: GET /credential proxy: tls-admission-v1 decrypt binding_host")
print("credential proxy: tls-admission-v1 decrypt binding_host extra")
print("credential proxy: tls-admission-v2 decrypt binding_host")
# The existing forwarder strips trailing whitespace before parsing.
print("credential proxy: tls-admission-v1 decrypt binding_host \t")
`, root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// Consume until EOF before Wait closes StdoutPipe, exactly preserving every
	// producer record instead of synthesizing a Go strings.Reader fixture.
	forwardMitmdumpOutput(stdout)
	require.NoError(t, cmd.Wait(), "Python producer failed: %s", stderr.String())
	require.NoError(t, telemetry.ForceFlush(ctx))
	select {
	case body := <-received:
		var req collectormetrics.ExportMetricsServiceRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		counts := map[string]int64{}
		for _, resource := range req.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name != "egress.mitm.tls.connections_total" {
						continue
					}
					require.True(t, metric.GetSum().IsMonotonic)
					for _, point := range metric.GetSum().DataPoints {
						attrs := map[string]string{}
						for _, attr := range point.Attributes {
							attrs[attr.Key] = attr.Value.GetStringValue()
						}
						require.Equal(t, "credential-bound", attrs["mode"])
						counts[attrs["decision"]+"/"+attrs["reason"]] += point.GetAsInt()
						for key, value := range attrs {
							require.False(t, strings.Contains(value, "private.example.com"), "unexpected dynamic attribute %s", key)
						}
					}
				}
			}
		}
		expected := map[string]int64{}
		for _, pair := range fixture.Valid {
			expected[pair.Decision+"/"+pair.Reason]++
		}
		expected["decrypt/binding_host"] += 2 // Repeated helper event and normalized trailing whitespace.
		require.Equal(t, expected, counts)
	case <-ctx.Done():
		t.Fatal("Python TLS admission records never reached the OTLP receiver")
	}
}
