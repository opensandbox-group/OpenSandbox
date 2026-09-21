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
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
)

var ErrInvalidDecisionSnapshot = errors.New("invalid credential decision snapshot")

type decisionSnapshot struct {
	Version                 int             `json:"version"`
	VaultRevision           int64           `json:"vaultRevision"`
	EffectivePolicyEpoch    int64           `json:"effectivePolicyEpoch"`
	InterceptionMode        string          `json:"interceptionMode"`
	State                   string          `json:"state"`
	TLSBindingHostSelectors []string        `json:"tlsBindingHostSelectors"`
	FullRenderedBindings    []ActiveBinding `json:"fullRenderedBindings"`
	Redactions              []string        `json:"redactions"`
}

// MarshalDecisionSnapshot constructs the exact payload bytes installed through
// the revision transaction. The outer revision envelope supplies generations,
// decision epoch, and digest; this payload contains the state known before the
// coordinator allocates that identity. Input errors never expose rendered data.
func MarshalDecisionSnapshot(snapshot ActiveSnapshot, effectivePolicyEpoch int64) ([]byte, error) {
	if effectivePolicyEpoch < 0 || snapshot.Revision < 0 || snapshot.Revision == 0 && len(snapshot.Bindings) > 0 {
		return nil, ErrInvalidDecisionSnapshot
	}
	redactions := append([]string{}, snapshot.Redactions...)
	if !canonicalRedactions(redactions) || len(snapshot.Bindings) == 0 && len(redactions) > 0 {
		return nil, ErrInvalidDecisionSnapshot
	}
	redactionSet := make(map[string]struct{}, len(redactions))
	for _, value := range redactions {
		redactionSet[value] = struct{}{}
	}

	bindings := append([]ActiveBinding{}, snapshot.Bindings...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	selectors := make(map[string]struct{})
	for i := range bindings {
		binding := &bindings[i]
		if i > 0 && binding.Name == bindings[i-1].Name || !validRenderedBinding(binding, redactionSet, selectors) {
			return nil, ErrInvalidDecisionSnapshot
		}
	}
	tlsSelectors := make([]string, 0, len(selectors))
	for selector := range selectors {
		tlsSelectors = append(tlsSelectors, selector)
	}
	sort.Strings(tlsSelectors)
	state := "active-empty"
	if len(bindings) > 0 {
		state = "active"
	}
	return json.Marshal(decisionSnapshot{
		Version:                 1,
		VaultRevision:           snapshot.Revision,
		EffectivePolicyEpoch:    effectivePolicyEpoch,
		InterceptionMode:        "credential-bound",
		State:                   state,
		TLSBindingHostSelectors: tlsSelectors,
		FullRenderedBindings:    bindings,
		Redactions:              redactions,
	})
}

func canonicalRedactions(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if value == "" {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
		if i > 0 && (len(values[i-1]) < len(value) || len(values[i-1]) == len(value) && values[i-1] > value) {
			return false
		}
	}
	return true
}

func validRenderedBinding(binding *ActiveBinding, redactions map[string]struct{}, selectors map[string]struct{}) bool {
	if binding.Name == "" || binding.Name != strings.TrimSpace(binding.Name) || len(binding.Match.Ports) != 0 {
		return false
	}
	https, ok := validCanonicalStrings(binding.Match.Schemes, func(value string) bool {
		return value == "http" || value == "https"
	}, "https")
	if !ok || !validCanonicalStringsOnly(binding.Match.Methods, func(value string) bool {
		return value != "" && value == strings.ToUpper(strings.TrimSpace(value))
	}) || !validCanonicalStringsOnly(binding.Match.Paths, func(value string) bool {
		return value == strings.TrimSpace(value) && strings.HasPrefix(value, "/")
	}) {
		return false
	}
	seenHosts := make(map[string]struct{}, len(binding.Match.Hosts))
	for _, host := range binding.Match.Hosts {
		normalized, err := normalizeCredentialHost(host)
		if err != nil || normalized != host {
			return false
		}
		if _, duplicate := seenHosts[host]; duplicate {
			return false
		}
		seenHosts[host] = struct{}{}
		if https && !(strings.HasPrefix(host, "*.") && len(host[2:]) > 251) {
			selector, err := hostselector.ParseCanonical(host)
			if err != nil || selector.String() != host {
				return false
			}
			selectors[host] = struct{}{}
		}
	}
	if len(seenHosts) == 0 || !validRenderedCredentials(binding, redactions) {
		return false
	}
	binding.Headers = append([]InjectionHeader{}, binding.Headers...)
	binding.Substitutions = append([]InjectionSubstitution{}, binding.Substitutions...)
	for i := range binding.Substitutions {
		binding.Substitutions[i].In = append([]string{}, binding.Substitutions[i].In...)
	}
	return true
}

func validCanonicalStrings(values []string, valid func(string) bool, target string) (bool, bool) {
	return slices.Contains(values, target), validCanonicalStringsOnly(values, valid)
}

func validCanonicalStringsOnly(values []string, valid func(string) bool) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !valid(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return len(values) > 0
}

func validRenderedCredentials(binding *ActiveBinding, redactions map[string]struct{}) bool {
	seenHeaders := make(map[string]struct{}, len(binding.Headers))
	for _, header := range binding.Headers {
		key := strings.ToLower(header.Name)
		if validateCredentialHeaderName(header.Name) != nil {
			return false
		}
		if _, duplicate := seenHeaders[key]; duplicate {
			return false
		}
		seenHeaders[key] = struct{}{}
		if header.Value != "" {
			if _, ok := redactions[header.Value]; !ok {
				return false
			}
		}
	}
	for _, substitution := range binding.Substitutions {
		if substitution.Placeholder == "" || !validCanonicalStringsOnly(substitution.In, func(value string) bool {
			return value == "path" || value == "query" || value == "header" || value == "body"
		}) {
			return false
		}
		required := append(substitutionRedactionVariants(substitution.Value), substitution.Placeholder)
		for _, value := range required {
			if value == "" {
				continue
			}
			if _, ok := redactions[value]; !ok {
				return false
			}
		}
	}
	return true
}
