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
	require.Len(t, fixture.Valid, 15, "closed admission vocabulary changed; review its privacy/cardinality contract")
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

func tlsBudgetReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	require.NoError(t, registerEgressMetrics())
	return reader
}

func collectTLSBudgets(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	result := map[string]metricdata.Metrics{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "egress.mitm.tls.registry.occupancy" || m.Name == "egress.mitm.tls.registry.configured_budget" {
				result[m.Name] = m
			}
		}
	}
	return result
}

func TestTLSBudgetRejectsInvalidObservations(t *testing.T) {
	for _, tc := range []struct {
		name, scope      string
		occupied, budget int64
	}{
		{"scope", "subject-id", 0, 32},
		{"zero_budget", "subject", 0, 0},
		{"negative_budget", "process", 0, -1},
		{"oversized_budget", "process", 0, 1<<20 + 1},
		{"negative_occupancy", "subject", -1, 32},
		{"over_budget", "subject", 33, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := tlsBudgetReader(t)
			RecordTLSBudget(tc.scope, tc.occupied, tc.budget)
			require.Empty(t, collectTLSBudgets(t, reader), "invalid observations must not reach either histogram")
		})
	}
}

func TestTLSBudgetHasClosedLabels(t *testing.T) {
	reader := tlsBudgetReader(t)
	RecordTLSBudget("process", 131072, 1<<20)
	RecordTLSBudget("subject", 0, 1)
	metrics := collectTLSBudgets(t, reader)
	require.Len(t, metrics, 2)
	for name, m := range metrics {
		hist, ok := m.Data.(metricdata.Histogram[int64])
		require.True(t, ok, name)
		require.Len(t, hist.DataPoints, 2)
		seen := map[string]bool{}
		for _, dp := range hist.DataPoints {
			profile, exists := dp.Attributes.Value("profile")
			require.True(t, exists)
			require.Equal(t, "fast-sandbox", profile.AsString())
			scope, exists := dp.Attributes.Value("scope")
			require.True(t, exists)
			seen[scope.AsString()] = true
			require.Equal(t, len(egressSharedAttrs())+2, dp.Attributes.Len())
			for _, shared := range egressSharedAttrs() {
				value, exists := dp.Attributes.Value(shared.Key)
				require.True(t, exists)
				require.Equal(t, shared.Value, value)
			}
			require.EqualValues(t, 1, dp.Count)
			want := int64(0)
			if scope.AsString() == "process" {
				want = 131072
			}
			if name == "egress.mitm.tls.registry.configured_budget" {
				want = 1
				if scope.AsString() == "process" {
					want = 1 << 20
				}
			}
			require.Equal(t, want, dp.Sum)
		}
		require.Equal(t, map[string]bool{"process": true, "subject": true}, seen)
	}
}

func TestTLSRegistryHistogramBucketsAndUnits(t *testing.T) {
	reader := tlsBudgetReader(t)
	for _, observation := range [][2]int64{{0, 1}, {16, 32}, {24, 32}, {32, 32}, {48, 64}, {64, 64}, {131072, 131072}, {1 << 20, 1 << 20}} {
		RecordTLSBudget("subject", observation[0], observation[1])
	}
	metrics := collectTLSBudgets(t, reader)
	require.Len(t, metrics, 2)
	for name, m := range metrics {
		t.Run(name, func(t *testing.T) {
			hist, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			require.Len(t, hist.DataPoints, 1)
			dp := hist.DataPoints[0]
			require.NotEmpty(t, dp.Bounds)
			require.Equal(t, float64(0), dp.Bounds[0])
			for _, boundary := range []float64{16, 24, 32, 48, 64, 131072, 1 << 20} {
				require.Contains(t, dp.Bounds, boundary)
			}
			require.Zero(t, dp.BucketCounts[len(dp.BucketCounts)-1], "supported slots must not collapse into +Inf")
			require.Equal(t, "{slot}", m.Unit)
		})
	}
}

func TestTLSBudgetBeforeInitialization(t *testing.T) {
	occupancy, budget := tlsRegistryOccupancy, tlsRegistryBudget
	t.Cleanup(func() { tlsRegistryOccupancy, tlsRegistryBudget = occupancy, budget })
	for _, missing := range []string{"occupancy", "budget", "both"} {
		tlsRegistryOccupancy, tlsRegistryBudget = occupancy, budget
		if missing != "budget" {
			tlsRegistryOccupancy = nil
		}
		if missing != "occupancy" {
			tlsRegistryBudget = nil
		}
		require.NotPanics(t, func() { RecordTLSBudget("subject", 0, 32) })
	}
}
