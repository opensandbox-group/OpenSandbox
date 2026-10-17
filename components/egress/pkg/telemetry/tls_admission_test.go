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
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type tlsAdmissionPair struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func admissionFixture(t *testing.T) (valid, invalid []tlsAdmissionPair) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/tls_admission_events.json")
	require.NoError(t, err)
	var fixture struct{ Valid, Invalid []tlsAdmissionPair }
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Len(t, fixture.Valid, 14, "closed admission vocabulary changed; review its privacy/cardinality contract")
	require.NotEmpty(t, fixture.Invalid)
	return fixture.Valid, fixture.Invalid
}

func TestTLSAdmissionHasClosedLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	require.NoError(t, registerEgressMetrics())
	valid, invalid := admissionFixture(t)
	expected := map[tlsAdmissionPair]int64{}
	for _, pair := range valid {
		RecordTLSAdmission(pair.Decision, pair.Reason)
		expected[pair]++
	}
	RecordTLSAdmission("decrypt", "binding_host")
	expected[tlsAdmissionPair{"decrypt", "binding_host"}]++
	for _, pair := range invalid {
		RecordTLSAdmission(pair.Decision, pair.Reason)
	}
	// Reject even recognized labels paired with the wrong decision.
	for _, pair := range valid {
		for _, decision := range []string{"decrypt", "passthrough", "deny", "unavailable"} {
			if decision != pair.Decision {
				RecordTLSAdmission(decision, pair.Reason)
			}
		}
	}
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	actual := map[tlsAdmissionPair]int64{}
	found := false
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "egress.mitm.tls.connections_total" {
				continue
			}
			found = true
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.True(t, sum.IsMonotonic)
			for _, dp := range sum.DataPoints {
				mode, ok := dp.Attributes.Value("mode")
				require.True(t, ok)
				require.Equal(t, "credential-bound", mode.AsString())
				decision, ok := dp.Attributes.Value("decision")
				require.True(t, ok)
				reason, ok := dp.Attributes.Value("reason")
				require.True(t, ok)
				require.Equal(t, len(egressSharedAttrs())+3, dp.Attributes.Len())
				for _, shared := range egressSharedAttrs() {
					value, exists := dp.Attributes.Value(shared.Key)
					require.True(t, exists)
					require.Equal(t, shared.Value, value)
				}
				actual[tlsAdmissionPair{decision.AsString(), reason.AsString()}] += dp.Value
			}
		}
	}
	require.True(t, found, "TLS admission counter not registered")
	require.Equal(t, expected, actual)
}

func TestTLSAdmissionBeforeInitialization(t *testing.T) {
	previous := tlsAdmissionConnections
	tlsAdmissionConnections = nil
	t.Cleanup(func() { tlsAdmissionConnections = previous })
	valid, invalid := admissionFixture(t)
	require.NotPanics(t, func() {
		for _, pair := range append(valid, invalid...) {
			RecordTLSAdmission(pair.Decision, pair.Reason)
		}
	})
}
