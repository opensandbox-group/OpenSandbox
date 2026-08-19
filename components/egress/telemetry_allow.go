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

package main

import (
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

// telemetryAllowRules returns an always-allow egress rule for the OTLP
// destination the exporter will dial, so metric export works under the default
// deny-all policy without operator-provided allowlist rules. The destination
// is the endpoint env var
// (OTEL_EXPORTER_OTLP_METRICS_ENDPOINT / OTEL_EXPORTER_OTLP_ENDPOINT, URL form
// as required by otlpmetrichttp) or, only when neither is set, the exporter
// fallback node IP (HOST_IP / /etc/hostinfo). A set-but-unparseable endpoint
// is not treated as unset: the exporter never falls back to the node IP in
// that case, so no rule is injected. The rule targets the host (any port),
// matching the egress rule model. Operators can still block the target via
// deny.always, which takes precedence. Returns nil when no OTLP destination
// is configured.
func telemetryAllowRules() []policy.EgressRule {
	host, port, ok := inttelemetry.OTLPEndpointHostPort()
	if !ok {
		if inttelemetry.OTLPEndpointEnvSet() {
			log.Warnf("telemetry: configured OTLP endpoint is not a valid URL; skipping auto egress allow")
			return nil
		}
		host, port, ok = inttelemetry.OTLPEndpointFallbackHostPort()
	}
	if !ok {
		return nil
	}
	rule, err := policy.ParseValidatedEgressRule(policy.ActionAllow, host)
	if err != nil {
		log.Warnf("telemetry: skipping auto egress allow for OTLP endpoint host %q: %v", host, err)
		return nil
	}
	log.Infof("telemetry: auto-allowing egress to OTLP endpoint %s:%s (deny.always can override)", host, port)
	return []policy.EgressRule{rule}
}

// withTelemetryAllow appends the auto-generated OTLP allow rule(s) to the
// always-allow list so every effective-policy merge (startup, policy updates,
// always-file reloads) keeps telemetry egress open.
func withTelemetryAllow(allow []policy.EgressRule) []policy.EgressRule {
	rules := telemetryAllowRules()
	if len(rules) == 0 {
		return allow
	}
	out := make([]policy.EgressRule, 0, len(allow)+len(rules))
	out = append(out, allow...)
	return append(out, rules...)
}
