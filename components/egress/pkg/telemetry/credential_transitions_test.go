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
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	transitionCounterName   = "egress.credential_vault.transitions_total"
	transitionHistogramName = "egress.credential_vault.transition.duration"
)

type transitionKey struct{ transition, result string }

type transitionSeries struct {
	count    int64
	observed uint64
	sum      float64
	buckets  []uint64
}

func transitionReader(t *testing.T) *sdkmetric.ManualReader {
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

// collectTransitions reads both instruments from the real SDK output, checks
// their exported shape and that every point carries only shared attributes
// plus the two closed labels, and pairs counter and histogram per label set.
func collectTransitions(t *testing.T, reader *sdkmetric.ManualReader) (map[transitionKey]*transitionSeries, []float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	series := map[transitionKey]*transitionSeries{}
	get := func(attrs attribute.Set) *transitionSeries {
		require.Equal(t, len(egressSharedAttrs())+2, attrs.Len())
		for _, shared := range egressSharedAttrs() {
			value, ok := attrs.Value(shared.Key)
			require.True(t, ok)
			require.Equal(t, shared.Value, value)
		}
		transition, ok := attrs.Value("transition")
		require.True(t, ok)
		result, ok := attrs.Value("result")
		require.True(t, ok)
		key := transitionKey{transition.AsString(), result.AsString()}
		if series[key] == nil {
			series[key] = &transitionSeries{}
		}
		return series[key]
	}
	var bounds []float64
	counterFound, histogramFound := false, false
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch m.Name {
			case transitionCounterName:
				counterFound = true
				require.Empty(t, m.Unit)
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter must be an Int64Counter")
				require.True(t, sum.IsMonotonic)
				for _, dp := range sum.DataPoints {
					get(dp.Attributes).count += dp.Value
				}
			case transitionHistogramName:
				histogramFound = true
				require.Equal(t, "s", m.Unit)
				histogram, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok, "duration must be a Float64Histogram")
				for _, dp := range histogram.DataPoints {
					bounds = dp.Bounds
					entry := get(dp.Attributes)
					entry.observed += dp.Count
					entry.sum += dp.Sum
					entry.buckets = dp.BucketCounts
				}
			}
		}
	}
	require.True(t, counterFound, "transition counter not registered")
	require.True(t, histogramFound, "transition histogram not registered")
	for key, entry := range series {
		require.Equal(t, uint64(entry.count), entry.observed, "counter and histogram diverged for %v", key)
	}
	return series, bounds
}

func TestCredentialVaultTransitionClosedLabels(t *testing.T) {
	reader := transitionReader(t)
	transitions := []string{"register", "install", "suspend", "unload", "replay"}
	results := []string{"ok", "rejected", "indeterminate", "failed"}
	expected := map[transitionKey]int64{}
	for i, transition := range transitions {
		for j, result := range results {
			repeat := i + j + 1
			for n := 0; n < repeat; n++ {
				RecordCredentialVaultTransition(transition, result, 3*time.Millisecond)
			}
			expected[transitionKey{transition, result}] = int64(repeat)
		}
	}
	// Caller strings outside the closed sets collapse to one fixed fallback.
	const leak = "s-private-subject api.secret.example token=abc123 dial unix /run/receiver.sock: refused"
	for _, bad := range []string{leak, "", "OK", "Register", " ok", "ok\n"} {
		RecordCredentialVaultTransition(bad, "ok", time.Millisecond)
		RecordCredentialVaultTransition("register", bad, time.Millisecond)
		RecordCredentialVaultTransition(bad, bad, time.Millisecond)
	}
	expected[transitionKey{"unknown", "ok"}] += 6
	expected[transitionKey{"register", "unknown"}] += 6
	expected[transitionKey{"unknown", "unknown"}] += 6
	series, bounds := collectTransitions(t, reader)
	actual := map[transitionKey]int64{}
	for key, entry := range series {
		actual[key] = entry.count
		require.NotContains(t, key.transition+key.result, "secret")
	}
	require.Equal(t, expected, actual)
	require.Equal(t, []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}, bounds,
		"explicit second-scale buckets, not the SDK millisecond default")
	entry := series[transitionKey{"register", "ok"}]
	require.InDelta(t, 0.003, entry.sum, 1e-9)
	require.Equal(t, uint64(1), entry.buckets[3], "3ms lands in the (0.0025, 0.005] bucket")
}

func TestCredentialVaultTransitionDurationBoundaries(t *testing.T) {
	reader := transitionReader(t)
	// Negative durations drop the whole observation: counter and histogram stay
	// on the same footing. Zero and very long calls are valid observations.
	RecordCredentialVaultTransition("install", "ok", -time.Nanosecond)
	RecordCredentialVaultTransition("install", "ok", time.Duration(math.MinInt64))
	RecordCredentialVaultTransition("install", "ok", 0)
	RecordCredentialVaultTransition("install", "ok", time.Minute)
	series, _ := collectTransitions(t, reader)
	require.Len(t, series, 1)
	entry := series[transitionKey{"install", "ok"}]
	require.Equal(t, int64(2), entry.count)
	require.InDelta(t, 60, entry.sum, 1e-9)
	require.Equal(t, uint64(1), entry.buckets[0], "zero is in the first bucket")
	require.Equal(t, uint64(1), entry.buckets[len(entry.buckets)-1], "a minute overflows into +Inf")
}

func TestCredentialVaultTransitionConcurrent(t *testing.T) {
	reader := transitionReader(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				RecordCredentialVaultTransition([]string{"register", "install"}[i%2], "ok", time.Duration(n)*time.Microsecond)
			}
		}(i)
	}
	wg.Wait()
	series, _ := collectTransitions(t, reader)
	require.Equal(t, int64(400), series[transitionKey{"register", "ok"}].count)
	require.Equal(t, int64(400), series[transitionKey{"install", "ok"}].count)
}

func TestCredentialVaultTransitionMissingInstruments(t *testing.T) {
	require.NotPanics(t, func() {
		counter, histogram := credentialVaultTransitions, credentialVaultTransitionDur
		defer func() { credentialVaultTransitions, credentialVaultTransitionDur = counter, histogram }()
		credentialVaultTransitions, credentialVaultTransitionDur = nil, nil
		RecordCredentialVaultTransition("register", "ok", time.Millisecond)
		RecordCredentialVaultTransition(strings.Repeat("x", 64), "bogus", -time.Second)
	})
	// With only one instrument present, nothing is written to the other one:
	// the exported counter and histogram never disagree.
	for _, missing := range []string{"counter", "histogram"} {
		t.Run(missing, func(t *testing.T) {
			reader := transitionReader(t)
			counter, histogram := credentialVaultTransitions, credentialVaultTransitionDur
			t.Cleanup(func() { credentialVaultTransitions, credentialVaultTransitionDur = counter, histogram })
			if missing == "counter" {
				credentialVaultTransitions = nil
			} else {
				credentialVaultTransitionDur = nil
			}
			require.NotPanics(t, func() { RecordCredentialVaultTransition("register", "ok", time.Millisecond) })
			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &rm))
			for _, scope := range rm.ScopeMetrics {
				for _, m := range scope.Metrics {
					require.NotEqual(t, transitionCounterName, m.Name)
					require.NotEqual(t, transitionHistogramName, m.Name)
				}
			}
		})
	}
}
