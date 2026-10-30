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

package credentialvault

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveResolverDecisionVariantCompleteness(t *testing.T) {
	// One value exercising every encoding family: spaces, plus signs,
	// reserved URL characters, JSON-escaped quotes/newlines, non-ASCII, and
	// an astral character (surrogate pair territory).
	value := "a b+c/d?e=\"f\"&g\nh\U0001F600"
	decision := deriveResolverDecision(map[string]string{"X-Handled": value}, nil)

	require.Equal(t, map[string]struct{}{"x-handled": {}}, decision.Suppress)

	// All 7 variants of the alignment helper are present.
	variants := substitutionRedactionVariants(value)
	require.Len(t, variants, 7)
	for _, variant := range variants {
		require.Contains(t, decision.Redactions, variant)
	}

	// URL encoding appears in both percent-escape cases; form encoding in
	// both plus/percent forms; JSON escaping in both raw-UTF-8 and
	// ASCII-escape encodings.
	expected := []string{
		value, // raw
		"a%20b%2Bc%2Fd%3Fe%3D%22f%22%26g%0Ah%F0%9F%98%80", // URL, uppercase escapes, space as %20
		"a%20b%2bc%2fd%3fe%3d%22f%22%26g%0ah%f0%9f%98%80", // URL, lowercase escapes
		"a+b%2Bc%2Fd%3Fe%3D%22f%22%26g%0Ah%F0%9F%98%80",   // form, uppercase escapes, space as +
		"a+b%2bc%2fd%3fe%3d%22f%22%26g%0ah%f0%9f%98%80",   // form, lowercase escapes
	}
	for _, want := range expected {
		require.Contains(t, decision.Redactions, want)
	}
	// JSON escape: quote, newline, and ampersand escaped, non-ASCII kept
	// as UTF-8 (encoding/json default HTML escaping).
	require.Contains(t, decision.Redactions, "a b+c/d?e=\\\"f\\\"\\u0026g\\nh\U0001F600")
	// JSON ASCII escape: emoji as the UTF-16 surrogate pair.
	require.Contains(t, decision.Redactions, "a b+c/d?e=\\\"f\\\"&g\\nh\\ud83d\\ude00")

	// Exactly the 7 variants, no extras, canonically ordered.
	require.Len(t, decision.Redactions, len(variants))
	require.True(t, slices.Equal(canonicalResolverRedactions(decision.Redactions), decision.Redactions))
}

func TestResolverRedactionsAreIncrementalR9(t *testing.T) {
	value := "secret"
	variants := substitutionRedactionVariants(value)
	// A plain-ASCII value collapses to one deduped variant.
	expected := canonicalResolverRedactions(variants)
	require.Equal(t, []string{"secret"}, expected)
	decision := deriveResolverDecision(
		map[string]string{"x-a": value},
		map[string][]string{
			"x-a":     {"extra-token", value}, // resolver may repeat derived values
			"x-other": {""},                   // empty entries are dropped
		},
	)
	for _, variant := range expected {
		require.Contains(t, decision.Redactions, variant, "derived variants must survive the merge")
	}
	require.Contains(t, decision.Redactions, "extra-token")
	require.NotContains(t, decision.Redactions, "")
	require.Len(t, decision.Redactions, len(expected)+1)

	// The resolver cannot shrink the set: with no redactions at all the
	// derived variants remain.
	decision = deriveResolverDecision(map[string]string{"x-a": value}, nil)
	require.Equal(t, expected, decision.Redactions)
}

func TestCanonicalResolverRedactionsOrdering(t *testing.T) {
	ordered := canonicalResolverRedactions([]string{"ccc", "bb", "a", "bb", ""})
	require.Equal(t, []string{"a", "bb", "ccc"}, ordered)

	ordered = canonicalResolverRedactions([]string{"b", "a"})
	require.Equal(t, []string{"a", "b"}, ordered)

	// The order matches the decision-snapshot canonical form.
	require.True(t, canonicalRedactions(ordered))
}

func TestSuppressSetLowercase(t *testing.T) {
	decision := deriveResolverDecision(map[string]string{"X-Signature": "sig", "x-token": "tok"}, nil)
	require.Equal(t, map[string]struct{}{"x-signature": {}, "x-token": {}}, decision.Suppress)
}
