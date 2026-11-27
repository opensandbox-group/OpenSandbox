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
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	meter metric.Meter

	httpRequestCount    metric.Int64Counter
	httpRequestDuration metric.Float64Histogram
	requestsActive      metric.Int64UpDownCounter

	routingResolutionDuration metric.Float64Histogram
	upstreamConnectDuration   metric.Float64Histogram

	wakeDuration          metric.Float64Histogram
	parkActive            metric.Int64UpDownCounter
	parkWaitDuration      metric.Float64Histogram
	parkShed              metric.Int64Counter
	restoreActive         metric.Int64UpDownCounter
	policyReads           metric.Int64Counter
	activityWritesDropped metric.Int64Counter

	connectivityProviderMu sync.RWMutex
	connectivityProvider   func() ConnectivitySnapshot
)

// ConnectivitySnapshot contains bounded, low-cardinality shadow aggregates.
type ConnectivitySnapshot struct {
	Qualified bool
	Degraded  bool
}

func registerIngressMetrics() error {
	meter = otel.Meter("opensandbox/ingress")

	var err error
	httpRequestCount, err = meter.Int64Counter(
		"ingress.http.request.count",
		metric.WithDescription("Ingress HTTP request count"),
	)
	if err != nil {
		return err
	}

	httpRequestDuration, err = meter.Float64Histogram(
		"ingress.http.request.duration",
		metric.WithDescription("Ingress HTTP request duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	requestsActive, err = meter.Int64UpDownCounter(
		"ingress.requests.active",
		metric.WithDescription("Currently active ingress requests by proxy type"),
	)
	if err != nil {
		return err
	}

	routingResolutionDuration, err = meter.Float64Histogram(
		"ingress.routing.resolution.duration",
		metric.WithDescription("Routing resolution duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	upstreamConnectDuration, err = meter.Float64Histogram(
		"ingress.upstream.connect.duration",
		metric.WithDescription("Ingress upstream TCP connection duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	wakeDuration, err = meter.Float64Histogram(
		"ingress.wake.duration",
		metric.WithDescription("Wake flight duration by terminal outcome"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		return err
	}

	parkActive, err = meter.Int64UpDownCounter(
		"ingress.park.active",
		metric.WithDescription("Currently parked requests"),
	)
	if err != nil {
		return err
	}

	parkWaitDuration, err = meter.Float64Histogram(
		"ingress.park.wait.duration",
		metric.WithDescription("Per-parked-request wait duration by outcome (served, budget_exhausted, canceled, error)"),
		metric.WithUnit("s"),
		// Sized around the default 5s park budget: the interesting signal is
		// a ~200ms same-node resume versus a near-exhaustion wait, which the
		// SDK default second-scale buckets would collapse into one bucket.
		metric.WithExplicitBucketBoundaries(0.01, 0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 1.5, 2, 3, 4, 5, 7.5, 10),
	)
	if err != nil {
		return err
	}

	parkShed, err = meter.Int64Counter(
		"ingress.park.shed",
		metric.WithDescription("Requests shed because the parking lot was full"),
	)
	if err != nil {
		return err
	}

	restoreActive, err = meter.Int64UpDownCounter(
		"ingress.wake.restore.active",
		metric.WithDescription("Wake flights currently holding a restore permit (the concurrent-restore gauge)"),
	)
	if err != nil {
		return err
	}

	policyReads, err = meter.Int64Counter(
		"ingress.idlep.policy.reads",
		metric.WithDescription("Idle-policy gate outcomes (allowed, denied, error); sustained error means an RBAC or apiserver problem"),
	)
	if err != nil {
		return err
	}

	activityWritesDropped, err = meter.Int64Counter(
		"ingress.activity.writes_dropped",
		metric.WithDescription("Activity observations dropped before reaching Redis or due to Redis write failures"),
	)
	if err != nil {
		return err
	}

	return registerConnectivityMetrics()
}

func registerConnectivityMetrics() error {
	qualified, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.qualified",
		metric.WithDescription("Whether the most recent complete shadow window has enough samples"),
	)
	if err != nil {
		return err
	}
	degraded, err := meter.Int64ObservableGauge(
		"ingress.network.shadow.degraded",
		metric.WithDescription("Whether the most recent complete qualified shadow window is degraded"),
	)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, observer metric.Observer) error {
			snapshot, ok := connectivitySnapshot()
			if !ok {
				return nil
			}
			observer.ObserveInt64(qualified, boolToInt64(snapshot.Qualified))
			observer.ObserveInt64(degraded, boolToInt64(snapshot.Degraded))
			return nil
		},
		qualified,
		degraded,
	)
	return err
}

// SetConnectivitySnapshotProvider installs the callback used by shadow gauges.
func SetConnectivitySnapshotProvider(provider func() ConnectivitySnapshot) {
	connectivityProviderMu.Lock()
	defer connectivityProviderMu.Unlock()
	connectivityProvider = provider
}

func connectivitySnapshot() (ConnectivitySnapshot, bool) {
	connectivityProviderMu.RLock()
	provider := connectivityProvider
	connectivityProviderMu.RUnlock()
	if provider == nil {
		return ConnectivitySnapshot{}, false
	}
	return provider(), true
}

func boolToInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func RecordHTTPRequest(method string, statusCode int, proxyType string, outcome string, durationMs float64) {
	if httpRequestCount == nil || httpRequestDuration == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("http_method", method),
		attribute.Int("http_status_code", statusCode),
		attribute.String("proxy_type", proxyType),
	}
	// Keep outcome off the histogram to avoid multiplying bucket cardinality.
	httpRequestDuration.Record(context.Background(), durationMs, metric.WithAttributes(attrs...))
	httpRequestCount.Add(context.Background(), 1, metric.WithAttributes(append(attrs, attribute.String("outcome", outcome))...))
}

// RecordRequestDelta adjusts the count of active requests by proxy type.
func RecordRequestDelta(proxyType string, delta int64) {
	if requestsActive == nil {
		return
	}
	requestsActive.Add(context.Background(), delta, metric.WithAttributes(attribute.String("proxy_type", proxyType)))
}

func RecordRouting(result string, durationMs float64) {
	if routingResolutionDuration == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("routing_result", result))
	routingResolutionDuration.Record(context.Background(), durationMs, attrs)
}

// RecordUpstreamConnect records only low-cardinality connection attributes.
// The target address is intentionally excluded because Sandbox endpoints are
// high-cardinality and short-lived.
func RecordUpstreamConnect(result, proxyType string, durationMs float64) {
	if upstreamConnectDuration == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("connect_result", result),
		attribute.String("proxy_type", proxyType),
	)
	upstreamConnectDuration.Record(context.Background(), durationMs, attrs)
}

// RecordWakeDuration records one wake flight's duration by terminal outcome.
func RecordWakeDuration(outcome string, duration time.Duration) {
	if wakeDuration == nil {
		return
	}
	wakeDuration.Record(context.Background(), duration.Seconds(), metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordParkDelta adjusts the count of currently parked requests.
func RecordParkDelta(delta int64) {
	if parkActive == nil {
		return
	}
	parkActive.Add(context.Background(), delta)
}

// RecordParkWait records one parked request's wait duration by outcome.
func RecordParkWait(outcome string, duration time.Duration) {
	if parkWaitDuration == nil {
		return
	}
	parkWaitDuration.Record(
		context.Background(),
		duration.Seconds(),
		metric.WithAttributes(attribute.String("outcome", outcome)),
	)
}

// RecordRestoreDelta adjusts the count of flights currently holding a
// restore permit — the concurrent-restore gauge.
func RecordRestoreDelta(delta int64) {
	if restoreActive == nil {
		return
	}
	restoreActive.Add(context.Background(), delta)
}

// RecordPolicyRead counts one idle-policy gate decision by outcome.
func RecordPolicyRead(outcome string) {
	if policyReads == nil {
		return
	}
	policyReads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordParkShed counts requests shed by parking-lot admission control.
func RecordParkShed() {
	if parkShed == nil {
		return
	}
	parkShed.Add(context.Background(), 1)
}

func RecordActivityWriteDropped(count int64) {
	if activityWritesDropped == nil {
		return
	}
	activityWritesDropped.Add(context.Background(), count)
}
