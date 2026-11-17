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

package main

import (
	"encoding/json"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// The probe's matchDomain mirrors the sidecar evaluator. Every case asserts
// the expected verdict twice: against the real pkg/policy Evaluate, and
// against the probe, so the diagnostic tool can never drift from enforcement.
func TestMatchDomainAgreesWithEvaluator(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		query  string
		want   string
	}{
		{"default deny, dotted exact allow", `{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org."}]}`, "pypi.org.", "allow"},
		{"same rule, query without dot", `{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org."}]}`, "pypi.org", "allow"},
		{"default deny, dotted wildcard allow", `{"defaultAction":"deny","egress":[{"action":"allow","target":"*.github.com."}]}`, "api.github.com", "allow"},
		{"dotted wildcard still denies apex", `{"defaultAction":"deny","egress":[{"action":"allow","target":"*.github.com."}]}`, "github.com.", "deny"},
		{"default allow, dotted exact deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"pypi.org."}]}`, "pypi.org", "deny"},
		{"default allow, dotted wildcard deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"*.github.com."}]}`, "api.github.com.", "deny"},
		{"first match, dotted rule first", `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com."},{"action":"deny","target":"api.example.com"}]}`, "api.example.com", "allow"},
		{"first match, plain rule first", `{"defaultAction":"deny","egress":[{"action":"deny","target":"api.example.com"},{"action":"allow","target":"api.example.com."}]}`, "api.example.com.", "deny"},
		{"IP and CIDR targets skipped", `{"defaultAction":"deny","egress":[{"action":"allow","target":"10.0.0.0/8"},{"action":"allow","target":"1.2.3.4"},{"action":"allow","target":"pypi.org"}]}`, "pypi.org.", "allow"},
		{"default allow, dotted IP literal deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"1.2.3.4."}]}`, "1.2.3.4.", "deny"},
		{"default deny, dotted IP literal allow", `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4."}]}`, "1.2.3.4.", "allow"},
		{"plain IP literal is still skipped", `{"defaultAction":"deny","egress":[{"action":"allow","target":"1.2.3.4"}]}`, "1.2.3.4.", "deny"},
		{"dotted CIDR literal stays a domain rule", `{"defaultAction":"deny","egress":[{"action":"allow","target":"10.0.0.0/8."}]}`, "example.com", "deny"},
		{"unmatched query falls to defaultAction", `{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org."}]}`, "example.com", "deny"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evaluated, err := policy.ParsePolicy(tc.policy)
			require.NoError(t, err)
			want := evaluated.Evaluate(tc.query)
			require.Equalf(t, tc.want, want, "evaluator verdict for query=%s", tc.query)

			var probe NetworkPolicy
			require.NoError(t, json.Unmarshal([]byte(tc.policy), &probe))
			action, _ := probe.matchDomain(tc.query)
			require.Equalf(t, tc.want, orDeny(action),
				"probe verdict must agree with the evaluator for query=%s", tc.query)
		})
	}
}

func TestMatchDomainReportsNormalizedTarget(t *testing.T) {
	var probe NetworkPolicy
	require.NoError(t, json.Unmarshal([]byte(`{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org."}]}`), &probe))
	action, matched := probe.matchDomain("pypi.org")
	require.Equal(t, "allow", action)
	require.Equal(t, "pypi.org", matched, "the matched pattern is reported normalized, without the trailing dot")
}

func TestMatchDomainNilPolicyDenies(t *testing.T) {
	action, matched := (*NetworkPolicy)(nil).matchDomain("pypi.org")
	require.Equal(t, "deny", action)
	require.Equal(t, "policy API unavailable", matched)
}
