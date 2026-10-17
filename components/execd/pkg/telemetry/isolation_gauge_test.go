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
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestIsolationUsageGaugeOmitsInvalidMeasurements(t *testing.T) {
	originalProvider := isolationStatsProvider
	t.Cleanup(func() { isolationStatsProvider = originalProvider })

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	originalMeterProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(originalMeterProvider)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})

	if err := registerExecdMetrics(); err != nil {
		t.Fatal(err)
	}

	snapshot := IsolationStats{ActiveSessions: 2}
	SetIsolationStatsProvider(func() IsolationStats { return snapshot })

	collect := func(t *testing.T) (usage []int64, sessions []int64) {
		t.Helper()
		var metrics metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &metrics); err != nil {
			t.Fatal(err)
		}
		for _, scope := range metrics.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "execd.isolation.upper.usage_bytes" && m.Name != "execd.isolation.session.count" {
					continue
				}
				gauge, ok := m.Data.(metricdata.Gauge[int64])
				if !ok {
					t.Fatalf("metric %s has unexpected data type %T", m.Name, m.Data)
				}
				for _, point := range gauge.DataPoints {
					if m.Name == "execd.isolation.upper.usage_bytes" {
						usage = append(usage, point.Value)
					} else {
						sessions = append(sessions, point.Value)
					}
				}
			}
		}
		return usage, sessions
	}

	for _, step := range []struct {
		name      string
		value     int64
		valid     bool
		wantUsage []int64
	}{
		{"empty", 0, true, []int64{0}},
		{"occupied", 512, true, []int64{512}},
		{"scan-error", 0, false, nil},
		{"recovered", 256, true, []int64{256}},
	} {
		t.Run(step.name, func(t *testing.T) {
			snapshot.UpperUsageBytes = step.value
			snapshot.UpperUsageValid = step.valid
			usage, sessions := collect(t)
			if !equalInt64s(usage, step.wantUsage) {
				t.Fatalf("usage points = %v, want %v", usage, step.wantUsage)
			}
			if !equalInt64s(sessions, []int64{2}) {
				t.Fatalf("session points = %v, want [2]", sessions)
			}
		})
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
