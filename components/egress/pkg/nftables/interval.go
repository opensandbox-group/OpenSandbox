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

package nftables

import (
	"fmt"
	"net/netip"
)

// normalizeNFTIntervalSet drops entries that are redundant for nftables sets with
// `flags interval`: a host or smaller CIDR that falls strictly inside another
// listed CIDR would make `nft add element` fail with "conflicting intervals specified".
// Used on every ApplyStatic (startup and /policy updates).
func normalizeNFTIntervalSet(elems []string) ([]string, error) {
	if len(elems) == 0 {
		return nil, nil
	}
	prefs := make([]netip.Prefix, 0, len(elems))
	for _, s := range elems {
		p, err := parseAsPrefix(s)
		if err != nil {
			return nil, err
		}
		prefs = append(prefs, p.Masked())
	}
	prefs = uniquePrefixes(prefs)
	prefs = removeStrictSubnets(prefs)
	out := make([]string, 0, len(prefs))
	for _, p := range prefs {
		out = append(out, formatPrefixForNFT(p))
	}
	return out, nil
}

func parseAsPrefix(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		if a.Is4() {
			return a.Prefix(32)
		}
		return a.Prefix(128)
	}
	return netip.Prefix{}, fmt.Errorf("nftables: invalid IP or CIDR %q", s)
}

func uniquePrefixes(prefs []netip.Prefix) []netip.Prefix {
	seen := make(map[string]struct{}, len(prefs))
	out := make([]netip.Prefix, 0, len(prefs))
	for _, p := range prefs {
		k := p.String()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, p)
	}
	return out
}

// strictSupernet reports whether super covers all addresses of sub, with super
// strictly larger (smaller prefix length) than sub.
func strictSupernet(super, sub netip.Prefix) bool {
	if super.Addr().Is4() != sub.Addr().Is4() {
		return false
	}
	if super.Bits() >= sub.Bits() {
		return false
	}
	return super.Contains(sub.Addr())
}

func removeStrictSubnets(prefs []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(prefs))
	for _, p := range prefs {
		redundant := false
		for _, q := range prefs {
			if strictSupernet(q, p) {
				redundant = true
				break
			}
		}
		if !redundant {
			out = append(out, p)
		}
	}
	return out
}

func formatPrefixForNFT(p netip.Prefix) string {
	p = p.Masked()
	if p.Addr().Is4() && p.Bits() == 32 {
		return p.Addr().String()
	}
	if p.Addr().Is6() && p.Bits() == 128 {
		return p.Addr().String()
	}
	return p.String()
}
