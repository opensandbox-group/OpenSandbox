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
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestIsolationSessionGauge(t *testing.T) {
	reader := setupMetricsReader(t)
	original := isolationStatsProvider
	t.Cleanup(func() { isolationStatsProvider = original })

	SetIsolationStatsProvider(nil)
	metrics := collectMetrics(t, reader)
	require.NotContains(t, metrics, "execd.isolation.session.count")

	for _, count := range []int64{2, 0, 1} {
		SetIsolationStatsProvider(func() IsolationStats { return IsolationStats{ActiveSessions: count} })
		metrics = collectMetrics(t, reader)
		gauge := metrics["execd.isolation.session.count"].Data.(metricdata.Gauge[int64])
		require.Len(t, gauge.DataPoints, 1)
		require.Equal(t, count, gauge.DataPoints[0].Value)
		require.NotContains(t, metrics, "execd.isolation.upper.usage_bytes")
	}
}
