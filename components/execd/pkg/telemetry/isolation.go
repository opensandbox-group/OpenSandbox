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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// IsolationStats holds current isolation subsystem state for gauge callbacks.
type IsolationStats struct {
	ActiveSessions  int64
	UpperUsageBytes int64
}

// IsolationStatsProvider is called by observable gauge callbacks to get
// the latest isolation stats.
type IsolationStatsProvider func() IsolationStats

var isolationStatsProvider IsolationStatsProvider

// SetIsolationStatsProvider registers a callback the telemetry gauges will
// query on each collection interval.
func SetIsolationStatsProvider(p IsolationStatsProvider) {
	isolationStatsProvider = p
}

// RecordIsolatedRun records the duration of an isolated session run.
func RecordIsolatedRun(ctx context.Context, result string, durationMillis float64) {
	if isolationRunDurationMs == nil {
		return
	}
	attrs := append([]attribute.KeyValue{}, execdSharedAttrs()...)
	attrs = append(attrs, attribute.String("result", result))
	isolationRunDurationMs.Record(ctx, durationMillis, metric.WithAttributes(attrs...))
}
