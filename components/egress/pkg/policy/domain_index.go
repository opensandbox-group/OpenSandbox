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
	"slices"
	"strings"
)

// compiledDomainRule records first-seen index for a pattern; match() picks lowest index on conflicts.
type compiledDomainRule struct {
	index  int
	action string
}

// compiledDomainIndex: exact map + wildcard suffix map; order in Evaluate follows merged egress order.
type compiledDomainIndex struct {
	exact          map[string]compiledDomainRule
	wildcard       map[string]compiledDomainRule
	scoped         []EgressRule // all declarations, including invalid scopes, for validation
	scopedExact    map[string][]int
	scopedWildcard map[string][]int
}

func compileDomainIndex(egress []EgressRule) *compiledDomainIndex {
	idx := &compiledDomainIndex{
		exact:    make(map[string]compiledDomainRule),
		wildcard: make(map[string]compiledDomainRule),
	}
	for i, r := range egress {
		if r.targetKind != targetDomain {
			continue
		}
		if len(r.Ports) != 0 {
			idx.scoped = append(idx.scoped, r)
			if r.Action == ActionAllow {
				idx.indexScoped(normalizeDomainPattern(r.Target))
			}
			continue
		}
		pattern := normalizeDomainPattern(r.Target)
		if pattern == "" {
			continue
		}
		cr := compiledDomainRule{
			index:  i,
			action: r.Action,
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if _, exists := idx.wildcard[suffix]; !exists {
				idx.wildcard[suffix] = cr
			}
			continue
		}
		if _, exists := idx.exact[pattern]; !exists {
			idx.exact[pattern] = cr
		}
	}
	return idx
}

func (idx *compiledDomainIndex) match(domain string) (string, bool) {
	if idx == nil || domain == "" {
		return "", false
	}

	var best compiledDomainRule
	matched := false

	if rule, ok := idx.exact[domain]; ok {
		if rule.index == 0 {
			return rule.action, true
		}
		best = rule
		matched = true
	}
	if len(idx.wildcard) == 0 {
		return best.action, matched
	}

	for cursor := domain; ; {
		dot := strings.IndexByte(cursor, '.')
		if dot < 0 {
			break
		}
		suffix := cursor[dot:]
		if rule, ok := idx.wildcard[suffix]; ok {
			if rule.index == 0 {
				return rule.action, true
			}
			if !matched || rule.index < best.index {
				best = rule
				matched = true
			}
		}
		cursor = cursor[dot+1:]
	}

	if !matched {
		return "", false
	}
	return best.action, true
}

func (idx *compiledDomainIndex) indexScoped(pattern string) {
	if pattern == "" {
		return
	}
	groups := &idx.scopedExact
	if strings.HasPrefix(pattern, "*.") {
		groups = &idx.scopedWildcard
		pattern = pattern[1:]
	}
	if *groups == nil {
		*groups = make(map[string][]int)
	}
	(*groups)[pattern] = append((*groups)[pattern], len(idx.scoped)-1)
}

// Merge only the heads of the matching, ordered buckets. The scratch space
// depends on the number of matching suffixes, not the number of matching
// declarations. Advancing a bucket only changes its local slice header.
func (idx *compiledDomainIndex) scopedPorts(domain string) []int {
	var local [4][]int
	buckets := local[:0]
	if exact := idx.scopedExact[domain]; len(exact) != 0 {
		buckets = append(buckets, exact)
	}
	if len(idx.scopedWildcard) != 0 {
		for cursor := domain; ; {
			dot := strings.IndexByte(cursor, '.')
			if dot < 0 {
				break
			}
			if group := idx.scopedWildcard[cursor[dot:]]; len(group) != 0 {
				buckets = append(buckets, group)
			}
			cursor = cursor[dot+1:]
		}
	}
	var ports domainPortUnion
	if len(buckets) == 2 {
		a, b := buckets[0], buckets[1]
		for len(a) != 0 && len(b) != 0 {
			if a[0] < b[0] {
				ports.add(idx.scoped[a[0]].Ports)
				a = a[1:]
			} else {
				ports.add(idx.scoped[b[0]].Ports)
				b = b[1:]
			}
		}
		if len(a) == 0 {
			a = b
		}
		buckets[0], buckets = a, buckets[:1]
	} else if len(buckets) > 2 {
		// Sorted heads form a min-heap. After consuming its root, only the
		// replacement head may need to move down.
		slices.SortFunc(buckets, func(a, b []int) int { return a[0] - b[0] })
		for len(buckets) > 1 {
			head := buckets[0]
			ports.add(idx.scoped[head[0]].Ports)
			if len(head) > 1 {
				buckets[0] = head[1:]
			} else {
				last := len(buckets) - 1
				buckets[0], buckets = buckets[last], buckets[:last]
			}
			for i := 0; 2*i+1 < len(buckets); {
				child := 2*i + 1
				if right := child + 1; right < len(buckets) && buckets[right][0] < buckets[child][0] {
					child = right
				}
				if buckets[i][0] <= buckets[child][0] {
					break
				}
				buckets[i], buckets[child] = buckets[child], buckets[i]
				i = child
			}
		}
	}
	if len(buckets) != 0 {
		for _, i := range buckets[0] {
			ports.add(idx.scoped[i].Ports)
		}
	}
	return ports.ports
}
