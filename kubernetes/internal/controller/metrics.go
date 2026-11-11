// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// Allocator-path metrics; see docs/telemetry.md for the signal specification.
const (
	allocatorMeterName = "opensandbox/controller"

	allocatorScheduleDurationMetricName          = "controller.allocator.schedule.duration"
	allocatorPersistAllocStateDurationMetricName = "controller.allocator.persist_alloc_state.duration"
	allocatorSyncAllocResultDurationMetricName   = "controller.allocator.sync_alloc_result.duration"
	allocatorSyncSingleAllocResultDurationName   = "controller.allocator.sync_single_alloc_result.duration"

	allocatorSuccessAttribute = "success"
)

// allocatorDurationBuckets covers API-call-scale allocation latencies, in seconds.
var allocatorDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

var (
	allocatorScheduleDurationHistogram          metric.Float64Histogram
	allocatorPersistAllocStateDurationHistogram metric.Float64Histogram
	allocatorSyncAllocResultDurationHistogram   metric.Float64Histogram
	allocatorSyncSingleAllocResultHistogram     metric.Float64Histogram
)

func init() {
	meter := otel.GetMeterProvider().Meter(allocatorMeterName)
	allocatorScheduleDurationHistogram = newHistogram(meter, allocatorScheduleDurationMetricName,
		"Duration of allocator schedule decisions")
	allocatorPersistAllocStateDurationHistogram = newHistogram(meter, allocatorPersistAllocStateDurationMetricName,
		"Duration of persisting sandbox allocation state to the BatchSandbox annotation")
	allocatorSyncAllocResultDurationHistogram = newHistogram(meter, allocatorSyncAllocResultDurationMetricName,
		"Duration of syncing the batch allocation result to all sandboxes")
	allocatorSyncSingleAllocResultHistogram = newHistogram(meter, allocatorSyncSingleAllocResultDurationName,
		"Duration of syncing the allocation result for a single sandbox")
}

// newHistogram degrades to a no-op instrument on failure: telemetry must never
// take down controller startup.
func newHistogram(meter metric.Meter, name, description string) metric.Float64Histogram {
	histogram, err := meter.Float64Histogram(
		name,
		metric.WithDescription(description),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(allocatorDurationBuckets...),
	)
	if err != nil {
		logf.Log.Error(err, "telemetry: failed to create histogram, using no-op", "name", name)
		noopp, _ := noop.NewMeterProvider().Meter("noop").Float64Histogram(name)
		return noopp
	}
	return histogram
}

func recordAllocatorScheduleDuration(ctx context.Context, pool *sandboxv1alpha1.Pool, duration time.Duration, err error) {
	allocatorScheduleDurationHistogram.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("namespace", pool.Namespace),
		attribute.String("pool_name", pool.Name),
		attribute.Bool(allocatorSuccessAttribute, err == nil),
	))
}

func recordAllocatorPersistAllocStateDuration(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, duration time.Duration, err error) {
	allocatorPersistAllocStateDurationHistogram.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("namespace", sandbox.Namespace),
		attribute.String("pool_name", sandbox.Spec.PoolRef),
		attribute.Bool(allocatorSuccessAttribute, err == nil),
	))
}

func recordAllocatorSyncAllocResultDuration(ctx context.Context, pool *sandboxv1alpha1.Pool, duration time.Duration, err error) {
	allocatorSyncAllocResultDurationHistogram.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("namespace", pool.Namespace),
		attribute.String("pool_name", pool.Name),
		attribute.Bool(allocatorSuccessAttribute, err == nil),
	))
}

func recordAllocatorSyncSingleAllocResultDuration(ctx context.Context, sandbox *sandboxv1alpha1.BatchSandbox, duration time.Duration, err error) {
	allocatorSyncSingleAllocResultHistogram.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("namespace", sandbox.Namespace),
		attribute.String("pool_name", sandbox.Spec.PoolRef),
		attribute.Bool(allocatorSuccessAttribute, err == nil),
	))
}
