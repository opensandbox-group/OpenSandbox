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

package policy

import (
	"encoding/json"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
	"github.com/stretchr/testify/require"
)

func TestAllowsEntireHostSelector(t *testing.T) {
	cases := []struct {
		name, host, fallback string
		rules                []EgressRule
		want                 bool
	}{
		{"exact denied", "api.example.com", "allow", []EgressRule{{Action: "deny", Target: "api.example.com"}}, false},
		{"exact first allow", "api.example.com", "deny", []EgressRule{{Action: "allow", Target: "api.example.com"}, {Action: "deny", Target: "*.example.com"}}, true},
		{"wildcard denied exact leaf", "*.example.com", "deny", []EgressRule{{Action: "deny", Target: "secret.example.com"}, {Action: "allow", Target: "*.example.com"}}, false},
		{"wildcard denied nested suffix", "*.example.com", "deny", []EgressRule{{Action: "deny", Target: "*.secret.example.com"}, {Action: "allow", Target: "*.example.com"}}, false},
		{"shadowed later deny", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: "*.example.com"}, {Action: "deny", Target: "secret.example.com"}}, true},
		{"shadowed leaf deny", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: "secret.example.com"}, {Action: "deny", Target: "secret.example.com"}, {Action: "allow", Target: "*.example.com"}}, true},
		{"partial allow does not shadow suffix deny", "*.example.com", "allow", []EgressRule{{Action: "allow", Target: "secret.example.com"}, {Action: "deny", Target: "*.example.com"}}, false},
		{"nested allow shadows nested deny", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: "*.secret.example.com"}, {Action: "deny", Target: "*.secret.example.com"}, {Action: "allow", Target: "*.example.com"}}, true},
		{"apex not included", "*.example.com", "deny", []EgressRule{{Action: "deny", Target: "example.com"}, {Action: "allow", Target: "*.example.com"}}, true},
		{"disjoint deny", "*.example.com", "deny", []EgressRule{{Action: "deny", Target: "other.com"}, {Action: "allow", Target: "*.example.com"}}, true},
		{"top level wildcard", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: "*.com"}}, true},
		{"trailing dot is inert", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: "*.example.com."}}, false},
		{"case and whitespace", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: " *.EXAMPLE.COM "}}, true},
		{"IP and CIDR cannot authorize hosts", "*.example.com", "deny", []EgressRule{{Action: "allow", Target: "1.2.3.4"}, {Action: "allow", Target: "0.0.0.0/0"}}, false},
		{"default allow", "*.example.com", "allow", nil, true},
		{"default deny", "*.example.com", "deny", nil, false},
		{"default allow retains deny", "*.example.com", "allow", []EgressRule{{Action: "deny", Target: "private.example.com"}}, false},
		{"invalid bound selector", "*.*.example.com", "allow", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(NetworkPolicy{DefaultAction: tc.fallback, Egress: tc.rules})
			require.NoError(t, err)
			pol, err := ParsePolicy(string(raw))
			require.NoError(t, err)
			require.Equal(t, tc.want, pol.AllowsEntireHostSelector(tc.host))
		})
	}
}

// Cross-check positive proofs against the actual evaluator over exact, nested,
// apex, mixed-case and unrelated hosts. This is bounded differential coverage,
// not an assertion that finite probes prove wildcard authorization.
func TestAllowsEntireHostSelectorPositiveProofMatchesEvaluator(t *testing.T) {
	patterns := []string{"example.com", "*.example.com", "secret.example.com", "*.secret.example.com", "api.secret.example.com", "*.com", "*.other.com", "*.example.com.", "0.0.0.0/0"}
	hosts := []string{"example.com", "api.example.com", "secret.example.com", "api.secret.example.com", "deep.api.secret.example.com", "other.com", "api.other.com"}
	for i := 0; i < 1000; i++ {
		rules := make([]EgressRule, 3)
		for j := range rules {
			action := ActionAllow
			if (i>>j)&1 == 1 {
				action = ActionDeny
			}
			rules[j] = EgressRule{Action: action, Target: patterns[(i/(j+1)+j*j)%len(patterns)]}
		}
		fallback := ActionAllow
		if i&1 == 1 {
			fallback = ActionDeny
		}
		raw, err := json.Marshal(NetworkPolicy{DefaultAction: fallback, Egress: rules})
		require.NoError(t, err)
		pol, err := ParsePolicy(string(raw))
		require.NoError(t, err)
		for _, binding := range []string{"*.example.com", "*.secret.example.com", "api.example.com"} {
			if !pol.AllowsEntireHostSelector(binding) {
				continue
			}
			selector, err := hostselector.ParseCanonical(binding)
			require.NoError(t, err)
			for _, host := range hosts {
				if selector.Matches(host) {
					require.Equal(t, ActionAllow, pol.Evaluate(host), "positive proof for %s permitted denied %s under %s", binding, host, raw)
				}
			}
		}
	}
}
