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

import "strings"

// Resolver response → interception decision (OSEP-0025 R9): the resolved
// headers themselves drive the suppress and redaction derivation; anything
// the resolver volunteers as redactions is merged incrementally and is never
// authoritative. The derived set cannot be shrunk by the response.

// deriveResolverDecision builds the per-request decision from a validated
// envelope. For every resolved header: the lowercased name joins the suppress
// set and the value joins the redaction set through the same 7-variant
// expansion used for substitutions (raw, URL-escaped ×2 case forms,
// form-escaped ×2 case forms, JSON-escaped ×2 encodings —
// substitutionRedactionVariants). Resolver-provided redactions are additive
// only, deduped, and never remove derived entries.
func deriveResolverDecision(headers map[string]string, resolverRedactions map[string][]string) *ResolverDecision {
	decision := &ResolverDecision{
		Headers:  headers,
		Suppress: make(map[string]struct{}, len(headers)),
	}
	redactions := make([]string, 0, len(headers)*7)
	for name, value := range headers {
		decision.Suppress[strings.ToLower(name)] = struct{}{}
		redactions = append(redactions, substitutionRedactionVariants(value)...)
	}
	// R9: resolver-supplied redactions merge incrementally only — they can
	// add entries but never remove or replace the derived ones.
	for _, values := range resolverRedactions {
		redactions = append(redactions, values...)
	}
	decision.Redactions = canonicalResolverRedactions(redactions)
	return decision
}
