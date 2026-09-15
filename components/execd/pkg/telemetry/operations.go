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
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OperationStats contains no caller identities, handles or request content.
// Records is indexed by command/pty, then creating/created/failed.
type OperationStats struct {
	Records           [2][3]int64
	Capacity          int64
	OldestCreatingAge float64
}

var operationMetrics struct {
	sync.RWMutex
	requests metric.Int64Counter
	provider func() OperationStats
}

func SetOperationStatsProvider(provider func() OperationStats) {
	operationMetrics.Lock()
	operationMetrics.provider = provider
	operationMetrics.Unlock()
}

func readOperationStats() OperationStats {
	operationMetrics.RLock()
	provider := operationMetrics.provider
	operationMetrics.RUnlock()
	if provider == nil {
		return OperationStats{}
	}
	return provider()
}

func RecordOperationResult(kind, action, result string) {
	operationMetrics.RLock()
	counter := operationMetrics.requests
	operationMetrics.RUnlock()
	if counter == nil {
		return
	}
	if kind != "command" && kind != "pty" {
		kind = "unknown"
	}
	attrs := append([]attribute.KeyValue{}, execdSharedAttrs()...)
	attrs = append(attrs, attribute.String("kind", kind), attribute.String("action", action), attribute.String("result", result))
	counter.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

func registerOperationMetrics(meter metric.Meter) error {
	counter, err := meter.Int64Counter("execd.operation.requests", metric.WithDescription("Runtime creation and recovery requests by bounded outcome"))
	if err != nil {
		return err
	}
	operationMetrics.Lock()
	operationMetrics.requests = counter
	operationMetrics.Unlock()
	_, err = meter.Int64ObservableGauge("execd.operation.records",
		metric.WithDescription("Retained operation records by resource kind and creation state"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			stats := readOperationStats()
			for kind, name := range []string{"command", "pty"} {
				for state, label := range []string{"creating", "created", "failed"} {
					attrs := append([]attribute.KeyValue{}, execdSharedAttrs()...)
					attrs = append(attrs, attribute.String("kind", name), attribute.String("state", label))
					observer.Observe(stats.Records[kind][state], metric.WithAttributes(attrs...))
				}
			}
			return nil
		}))
	if err != nil {
		return err
	}
	_, err = meter.Int64ObservableGauge("execd.operation.capacity",
		metric.WithDescription("Configured operation registry capacity"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			observer.Observe(readOperationStats().Capacity, metric.WithAttributes(execdSharedAttrs()...))
			return nil
		}))
	if err != nil {
		return err
	}
	_, err = meter.Float64ObservableGauge("execd.operation.creating.oldest_age",
		metric.WithDescription("Age of the oldest unresolved creation; not permission to retry"), metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, observer metric.Float64Observer) error {
			observer.Observe(readOperationStats().OldestCreatingAge, metric.WithAttributes(execdSharedAttrs()...))
			return nil
		}))
	return err
}
