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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

func TestAppendMetricAttrsFromKeyValuePairs(t *testing.T) {
	var base []attribute.KeyValue
	out := inttelemetry.AppendAttrsFromKeyValuePairs(base, "a=b")
	assert.Len(t, out, 1)
	assert.Equal(t, "a", string(out[0].Key))
	assert.Equal(t, "b", out[0].Value.AsString())

	out = inttelemetry.AppendAttrsFromKeyValuePairs(nil, "  foo=bar  , baz=qux ")
	assert.Len(t, out, 2)
	assert.Equal(t, "foo", string(out[0].Key))
	assert.Equal(t, "bar", out[0].Value.AsString())
	assert.Equal(t, "baz", string(out[1].Key))
	assert.Equal(t, "qux", out[1].Value.AsString())

	out = inttelemetry.AppendAttrsFromKeyValuePairs(nil, "k=v=x")
	assert.Len(t, out, 1)
	assert.Equal(t, "k", string(out[0].Key))
	assert.Equal(t, "v=x", out[0].Value.AsString())

	out = inttelemetry.AppendAttrsFromKeyValuePairs(nil, "novalue=,=bad,nokv")
	assert.Len(t, out, 0)
}

// The instrument records seconds, so it needs boundaries on a seconds ladder. With the
// SDK default (the spec's millisecond ladder) every realistic DNS latency collapses into
// one bucket and the quantiles are meaningless.
func TestDNSQueryDurationBucketsSpanRealisticLatencies(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	require.NoError(t, registerEgressMetrics())

	// Cache hit, LAN upstream, slow upstream, one upstream timeout, a serial retry through
	// three resolvers at the default timeout, and a late success after two resolvers each
	// burning the configurable 120s maximum.
	for _, seconds := range []float64{0.0008, 0.012, 0.4, 5, 15, 240} {
		RecordDNSForward(seconds)
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	dp := dnsDurationDataPoint(t, &rm)
	require.NotEmpty(t, dp.Bounds)
	assert.Less(t, dp.Bounds[0], 0.01,
		"boundaries look like the millisecond default, not a seconds ladder")
	// forward() retries resolvers serially with the full timeout each and records the
	// whole chain, so the tail has to reach well past a single timeout.
	assert.Greater(t, dp.Bounds[len(dp.Bounds)-1], float64(constants.DefaultDNSUpstreamTimeoutSec),
		"the top boundary must leave room for a serial retry chain, not just one timeout")

	populated := 0
	for _, count := range dp.BucketCounts {
		if count > 0 {
			populated++
		}
	}
	assert.Equal(t, 6, populated,
		"the six latencies must land in six different buckets, got counts %v for bounds %v",
		dp.BucketCounts, dp.Bounds)
	assert.Zero(t, dp.BucketCounts[len(dp.BucketCounts)-1],
		"a retry-chain latency fell into +Inf, where it cannot be distinguished or interpolated")
}

func dnsDurationDataPoint(t *testing.T, rm *metricdata.ResourceMetrics) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "egress.dns.query.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "unexpected aggregation %T", m.Data)
			require.Len(t, hist.DataPoints, 1)
			return hist.DataPoints[0]
		}
	}
	t.Fatal("egress.dns.query.duration not collected")
	return metricdata.HistogramDataPoint[float64]{}
}

// The failure counters carry a bounded attribute on top of the shared set. This checks the
// attribute lands and, critically, that adding it does not corrupt the shared slice: it is
// returned by a sync.OnceValue and may have spare capacity, so appending in place would
// leak one call's reason into the next.
func TestFailureCountersCarryBoundedAttributeWithoutSharingState(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })
	require.NoError(t, registerEgressMetrics())

	RecordDNSQueryFailed(DNSFailureUpstreamError)
	RecordDNSQueryFailed(DNSFailureRcode)
	RecordDNSQueryFailed(DNSFailureUpstreamError)
	RecordNftablesUpdateFailed(NftOpDynamicAdd)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	dns := counterByAttr(t, &rm, "egress.dns.query.failed_total", "reason")
	assert.Equal(t, map[string]int64{
		DNSFailureUpstreamError: 2,
		DNSFailureRcode:         1,
	}, dns, "each reason must be its own stream")

	nft := counterByAttr(t, &rm, "egress.nftables.updates.failed_total", "operation")
	assert.Equal(t, map[string]int64{NftOpDynamicAdd: 1}, nft)
}

// counterByAttr sums an Int64 counter's data points keyed by one attribute, and asserts
// every point still carries the shared attributes it was created with.
//
// It compares against egressSharedAttrs() rather than a fixed sandbox_id: that slice comes
// from a sync.OnceValue resolved by whichever test records first, so hardcoding a value here
// would make this test depend on the order tests run in.
func counterByAttr(t *testing.T, rm *metricdata.ResourceMetrics, name, key string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "unexpected aggregation %T for %s", m.Data, name)
			for _, dp := range sum.DataPoints {
				value, found := dp.Attributes.Value(attribute.Key(key))
				require.True(t, found, "%s data point without a %q attribute: %v", name, key, dp.Attributes)
				for _, want := range egressSharedAttrs() {
					got, present := dp.Attributes.Value(want.Key)
					require.True(t, present, "shared attribute %s was lost: %v", want.Key, dp.Attributes)
					require.Equal(t, want.Value.AsString(), got.AsString())
				}
				out[value.AsString()] += dp.Value
			}
			return out
		}
	}
	t.Fatalf("%s not collected", name)
	return nil
}
