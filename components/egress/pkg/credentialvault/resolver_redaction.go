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

// Resolver response → interception decision (OSEP-0025 R9): resolved headers
// drive the derivation; resolver-volunteered redactions merge incrementally
// and can never shrink the derived set.

// deriveResolverDecision builds the decision from a validated envelope: each
// header's lowercased name joins the suppress set and its value joins the
// redactions via the 7-variant substitution expansion
// (substitutionRedactionVariants); resolver redactions are additive and
// deduped.
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
	// R9: resolver redactions only add, never remove derived entries.
	for _, values := range resolverRedactions {
		redactions = append(redactions, values...)
	}
	decision.Redactions = canonicalResolverRedactions(redactions)
	return decision
}
