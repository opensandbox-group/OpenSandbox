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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RecordTLSAdmission counts closed admission outcomes received from the system
// addon. Validation bounds labels; the log channel does not authenticate sources.
func RecordTLSAdmission(decision, reason string) {
	valid := false
	switch decision {
	case "decrypt":
		valid = reason == "binding_host"
	case "passthrough":
		switch reason {
		case "ech", "no_sni", "static_ignore", "no_binding_host":
			valid = true
		}
	case "deny":
		switch reason {
		case "unknown_identity", "invalid_sni", "snapshot_missing", "generation_mismatch",
			"invalid_input", "registry_exhausted", "admission_disabled", "ssl_insecure", "malformed_extensions":
			valid = true
		}
	}
	if !valid || tlsAdmissionConnections == nil {
		return
	}
	attrs := append([]attribute.KeyValue(nil), egressSharedAttrs()...)
	attrs = append(attrs, attribute.String("mode", "credential-bound"),
		attribute.String("decision", decision), attribute.String("reason", reason))
	tlsAdmissionConnections.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}
