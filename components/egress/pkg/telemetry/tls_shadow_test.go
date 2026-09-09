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

package telemetry

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"testing"
)

func TestTLSShadowHasBoundedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	old := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(old) })
	require.NoError(t, registerEgressMetrics())
	for _, outcome := range []string{"binding_host", "no_binding_host", "no_vault", "lookup_failed", "missing_sni", "invalid_sni", "invalid_snapshot", "observer_error", "unknown_subject_or_vault"} {
		RecordTLSShadow(outcome)
	}
	RecordTLSShadow("binding_host")
	RecordTLSShadow("secret.example.com")
	RecordTLSShadow("binding_host host=private.example.com")
	RecordTLSShadow("")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	found := false
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "egress.mitm.shadow.requests_total" {
				continue
			}
			found = true
			sum := m.Data.(metricdata.Sum[int64])
			require.Len(t, sum.DataPoints, 9)
			for _, dp := range sum.DataPoints {
				reason, ok := dp.Attributes.Value("reason")
				require.True(t, ok)
				decision, ok := dp.Attributes.Value("decision")
				require.True(t, ok)
				switch reason.AsString() {
				case "binding_host":
					require.Equal(t, "decrypt", decision.AsString())
					require.EqualValues(t, 2, dp.Value)
				case "no_binding_host", "no_vault":
					require.Equal(t, "passthrough", decision.AsString())
					require.EqualValues(t, 1, dp.Value)
				default:
					require.Equal(t, "unavailable", decision.AsString())
					require.EqualValues(t, 1, dp.Value)
				}
				require.Equal(t, len(egressSharedAttrs())+2, dp.Attributes.Len())
			}
		}
	}
	require.True(t, found, "shadow instrument not registered")
}
