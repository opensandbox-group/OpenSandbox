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

// RecordTLSShadow records a request-weighted projection received from the
// trusted system addon. Unknown outcomes are discarded, never made labels.
// These are observations after the existing vault lookup, not TLS decisions.
func RecordTLSShadow(outcome string) {
	var decision string
	switch outcome {
	case "binding_host":
		decision = "decrypt"
	case "no_binding_host", "no_vault":
		decision = "passthrough"
	case "lookup_failed", "missing_sni", "invalid_sni", "invalid_snapshot", "observer_error", "unknown_subject_or_vault":
		decision = "unavailable"
	default:
		return
	}
	if tlsShadowRequests == nil {
		return
	}
	attrs := append([]attribute.KeyValue(nil), egressSharedAttrs()...)
	attrs = append(attrs, attribute.String("decision", decision), attribute.String("reason", outcome))
	tlsShadowRequests.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}
