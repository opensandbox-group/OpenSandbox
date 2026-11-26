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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	require.NoError(t, RegisterDropMetrics())
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
				if strings.HasPrefix(m.Name, "egress.blocked.") || m.Name == "egress.drop_telemetry.sampler_up" {
					require.NotEmpty(t, m.Description)
				}
				if m.Name != "egress.drop_telemetry.sampler_up" {
					continue
				}
				g := m.Data.(metricdata.Gauge[int64])
				require.Len(t, g.DataPoints, 1)
				dp := g.DataPoints[0]
				require.EqualValues(t, 0, dp.Value)
				cause, ok := dp.Attributes.Value("cause")
				require.True(t, ok)
				require.Equal(t, "ruleset", cause.AsString())
				require.Equal(t, 1+len(egressSharedAttrs()), dp.Attributes.Len())
				for _, shared := range egressSharedAttrs() {
					value, present := dp.Attributes.Value(shared.Key)
					require.True(t, present)
					require.Equal(t, shared.Value, value)
				}
				found = true
			}
		}
		require.True(t, found)
	}
}

func TestDropMetricsSharedAttributes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDropMetrics$")
	cmd.Env = append(os.Environ(), "OPENSANDBOX_EGRESS_SANDBOX_ID=sbx-3094", "OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS=pool=alpha")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func TestDropMetricLabels(t *testing.T) {
	for _, reason := range []string{"deny_set", "port_deny", "dot", "doh", "default_deny", "established_revoke", "subject_input_deny", "unmarked_failclosed", "gateway_mitm_direct", "upstream_proxy_deny"} {
		require.Equal(t, reason, normalizeDropReason(reason))
	}
	for _, cause := range []string{"bind", "ruleset", "receive", "disabled", ""} {
		SetDropSampler(false, cause)
		require.Equal(t, cause, dropSamplerCause)
	}
	for _, value := range []string{"", "attacker-controlled", "DENY_SET"} {
		require.Equal(t, "unknown", normalizeDropReason(value))
	}
	SetDropSampler(false, "attacker-controlled")
	require.Equal(t, "unknown", dropSamplerCause)
	t.Cleanup(func() { SetDropSampler(false, "disabled") })
}

func TestDropMetricsRegistration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prior := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(prior); _ = provider.Shutdown(context.Background()) })
	require.NoError(t, registerEgressMetrics())
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	found := false
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			found = found || m.Name == "egress.drop_telemetry.sampler_up"
		}
	}
	require.Equal(t, false, found)
}

func TestDropReasonsReturnsCopy(t *testing.T) {
	reasons := DropReasons()
	original := reasons[0]
	reasons[0] = "caller-mutation"
	require.Equal(t, original, DropReasons()[0])
	require.Equal(t, original, normalizeDropReason(original))
	require.Equal(t, "unknown", normalizeDropReason("caller-mutation"))
}
