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
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestTelemetryAllowRulesUnconfigured(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	require.Nil(t, telemetryAllowRules())

	existing := []policy.EgressRule{{Action: policy.ActionAllow, Target: "a.example.com"}}
	require.Equal(t, existing, withTelemetryAllow(existing), "no telemetry rules must not mutate the input")
}

func TestTelemetryAllowRulesFromMetricsEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example:4318/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	rules := telemetryAllowRules()
	require.Len(t, rules, 1)
	require.Equal(t, policy.ActionAllow, rules[0].Action)
	require.Equal(t, "collector.example", rules[0].Target)

	merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, rules)
	require.Equal(t, policy.ActionAllow, merged.Evaluate("collector.example."), "domain rule must allow DNS resolution")
	allowV4, allowV6, _, _ := merged.StaticIPSets()
	require.Empty(t, allowV4)
	require.Empty(t, allowV6)
}

func TestTelemetryAllowRulesFallbackEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel-collector:4318")
	rules := telemetryAllowRules()
	require.Len(t, rules, 1)
	require.Equal(t, "otel-collector", rules[0].Target)
}

func TestTelemetryAllowRulesFallbackNodeIP(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("HOST_IP", "10.0.0.9")
	rules := telemetryAllowRules()
	require.Len(t, rules, 1)
	require.Equal(t, "10.0.0.9", rules[0].Target)

	merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, rules)
	allowV4, allowV6, _, _ := merged.StaticIPSets()
	require.Equal(t, []string{"10.0.0.9"}, allowV4, "fallback node IP must land in the static allow v4 set")
	require.Empty(t, allowV6)
}

func TestTelemetryAllowRulesFQDNTrailingDot(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://otel-collector.ns.svc.cluster.local.:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	rules := telemetryAllowRules()
	require.Len(t, rules, 1)
	require.Equal(t, "otel-collector.ns.svc.cluster.local", rules[0].Target)

	merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, rules)
	require.Equal(t, policy.ActionAllow, merged.Evaluate("otel-collector.ns.svc.cluster.local."), "trailing-dot host must match DNS policy normalization")
	require.Equal(t, policy.ActionDeny, merged.Evaluate("other.ns.svc.cluster.local."))
}

func TestTelemetryAllowRulesIPEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://10.0.0.5:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	rules := telemetryAllowRules()
	require.Len(t, rules, 1)
	require.Equal(t, "10.0.0.5", rules[0].Target)

	merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, rules)
	allowV4, allowV6, _, _ := merged.StaticIPSets()
	require.Equal(t, []string{"10.0.0.5"}, allowV4, "IP target must land in the static allow v4 set")
	require.Empty(t, allowV6)
}

func TestTelemetryAllowRulesInvalidEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	require.Nil(t, telemetryAllowRules())
}

func TestTelemetryAllowRulesInvalidEndpointSkipsNodeIPFallback(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("HOST_IP", "10.0.0.9")
	require.Nil(t, telemetryAllowRules(), "configured-but-invalid endpoint must not open node-IP egress")

	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	rules := telemetryAllowRules()
	require.Len(t, rules, 1, "unset endpoint should fall back to the node IP")
	require.Equal(t, "10.0.0.9", rules[0].Target)
}

func TestWithTelemetryAllowAppends(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	existingRule, err := policy.ParseValidatedEgressRule(policy.ActionAllow, "a.example.com")
	require.NoError(t, err)
	existing := []policy.EgressRule{existingRule}
	rules := withTelemetryAllow(existing)
	require.Len(t, rules, 2)
	require.Equal(t, "a.example.com", rules[0].Target)
	require.Equal(t, "collector.example", rules[1].Target)

	merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, rules)
	require.Equal(t, policy.ActionDeny, merged.Evaluate("other.example.com."))
	require.Equal(t, policy.ActionAllow, merged.Evaluate("a.example.com."))
	require.Equal(t, policy.ActionAllow, merged.Evaluate("collector.example."))
}

func TestTelemetryDisabledDoesNotAutoAllowCollector(t *testing.T) {
	for _, key := range []string{"OTEL_SDK_DISABLED", "OTEL_METRICS_EXPORTER"} {
		for _, source := range []string{"metrics endpoint", "general endpoint", "node IP"} {
			t.Run(key+"/"+source, func(t *testing.T) {
				t.Setenv("OTEL_SDK_DISABLED", "")
				t.Setenv("OTEL_METRICS_EXPORTER", "")
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv("HOST_IP", "192.0.2.10")
				target := "192.0.2.10"
				if source == "metrics endpoint" {
					t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example:4318/v1/metrics")
					target = "collector.example"
				} else if source == "general endpoint" {
					t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example:4318")
					target = "collector.example"
				}
				require.Len(t, telemetryAllowRules(), 1, "enabled exporter needs the auto-allow rule")
				if key == "OTEL_SDK_DISABLED" {
					t.Setenv(key, "TrUe")
				} else {
					t.Setenv(key, "NoNe")
				}
				require.Nil(t, telemetryAllowRules())
				appRule, err := policy.ParseValidatedEgressRule(policy.ActionAllow, "app.example")
				require.NoError(t, err)
				existing := []policy.EgressRule{appRule}
				// Both initial policy construction and subsequent rebuilds use this
				// overlay path. Neither may reinstate the disabled collector rule.
				for i := 0; i < 2; i++ {
					allow := withTelemetryAllow(existing)
					require.Equal(t, existing, allow)
					merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, allow)
					require.Equal(t, policy.ActionDeny, merged.Evaluate(target))
					allowV4, allowV6, _, _ := merged.StaticIPSets()
					require.Empty(t, allowV4)
					require.Empty(t, allowV6)
					require.Equal(t, policy.ActionAllow, merged.Evaluate("app.example"))
				}
				// Disabling telemetry must not override an operator's explicit rule.
				collectorRule, err := policy.ParseValidatedEgressRule(policy.ActionAllow, target)
				require.NoError(t, err)
				explicit := []policy.EgressRule{collectorRule}
				merged := policy.MergeAlwaysOverlay(policy.DefaultDenyPolicy(), nil, withTelemetryAllow(explicit))
				if source == "node IP" {
					allowV4, _, _, _ := merged.StaticIPSets()
					require.Equal(t, []string{target}, allowV4)
				} else {
					require.Equal(t, policy.ActionAllow, merged.Evaluate(target))
				}
			})
		}
	}
}
