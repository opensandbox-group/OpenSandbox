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

package policy

import "strings"

// DomainSet is a read-only set of domain patterns used for membership checks
// (e.g. suppressing log output for selected hosts). Patterns use the same
// exact / wildcard-suffix semantics as policy egress rules: "foo.com" matches
// the bare host; "*.foo.com" matches any subdomain but not the bare host.
type DomainSet struct {
	idx *compiledDomainIndex
}

// NewDomainSet compiles the given patterns into a DomainSet. Empty/whitespace
// entries are ignored. Patterns are assumed to be already validated as domain
// targets (e.g. via LoadLogSkipFile); invalid entries are not rejected here.
func NewDomainSet(patterns []string) *DomainSet {
	if len(patterns) == 0 {
		return &DomainSet{}
	}
	rules := make([]EgressRule, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		rules = append(rules, EgressRule{Action: ActionAllow, Target: p, targetKind: targetDomain})
	}
	if len(rules) == 0 {
		return &DomainSet{}
	}
	return &DomainSet{idx: compileDomainIndex(rules)}
}

// Match reports whether host matches any pattern in the set. Hosts are
// normalised by lowercasing and stripping a trailing dot before lookup.
func (d *DomainSet) Match(host string) bool {
	if d == nil || d.idx == nil || host == "" {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	_, ok := d.idx.match(h)
	return ok
}

// Empty reports whether the set has no patterns.
func (d *DomainSet) Empty() bool {
	return d == nil || d.idx == nil
}
