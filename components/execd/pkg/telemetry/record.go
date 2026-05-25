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

func RecordHTTPRequest(ctx context.Context, method, route string, statusCode int, durationMillis float64) {
	if httpRequestDuration == nil {
		return
	}

	attrs := append([]attribute.KeyValue{}, execdSharedAttrs()...)
	attrs = append(attrs,
		attribute.String("http_method", method),
		attribute.String("http_route", normalizeRoute(route)),
		attribute.Int("http_status_code", statusCode),
	)
	opt := metric.WithAttributes(attrs...)

	httpRequestDuration.Record(ctx, durationMillis, opt)
}

func RecordExecutionDuration(ctx context.Context, operation, result string, durationMillis float64) {
	if executionDuration == nil {
		return
	}
	attrs := append([]attribute.KeyValue{}, execdSharedAttrs()...)
	attrs = append(attrs,
		attribute.String("operation", operation),
		attribute.String("result", result),
	)
	executionDuration.Record(ctx, durationMillis, metric.WithAttributes(attrs...))
}

func RecordFilesystemOperation(ctx context.Context, operation, result string, durationMillis float64) {
	if filesystemOperationDurMs == nil {
		return
	}
	attrs := append([]attribute.KeyValue{}, execdSharedAttrs()...)
	attrs = append(attrs,
		attribute.String("operation", operation),
		attribute.String("result", result),
	)
	filesystemOperationDurMs.Record(ctx, durationMillis, metric.WithAttributes(attrs...))
}

func normalizeRoute(route string) string {
	if route == "" {
		return "unknown"
	}
	return route
}
