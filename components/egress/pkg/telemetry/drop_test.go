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

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestDropMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prior := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prior)
		SetDropSampler(false, "disabled")
		_ = provider.Shutdown(context.Background())
	})
	require.NoError(t, registerEgressMetrics())
	RecordBlockedPackets("deny_set", 7)
	RecordBlockedPackets("deny_set", 2)
	RecordBlockedSample("deny_set")
	SetDropSampler(false, "ruleset")
	for i := 0; i < 2; i++ {
		var data metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &data))
		require.Equal(t, map[string]int64{"deny_set": 9}, counterByAttr(t, &data, "egress.blocked.packets", "reason"))
		require.Equal(t, map[string]int64{"deny_set": 1}, counterByAttr(t, &data, "egress.blocked.samples", "reason"))
		found := false
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "egress.drop_telemetry.sampler_up" {
					continue
				}
				g := m.Data.(metricdata.Gauge[int64])
				require.Len(t, g.DataPoints, 1)
				dp := g.DataPoints[0]
				require.EqualValues(t, 0, dp.Value)
				require.Equal(t, "ruleset", dp.Attributes.ToSlice()[0].Value.AsString())
				require.Len(t, dp.Attributes.ToSlice(), 1)
				found = true
			}
		}
		require.True(t, found)
	}
}
