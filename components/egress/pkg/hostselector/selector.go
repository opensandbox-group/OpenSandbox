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

// Package hostselector provides the exact/wildcard host algebra for OSEP-0023.
// It is not wired into the legacy Credential Vault or TLS path yet.
package hostselector

import (
	"errors"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Selector is a parsed exact FQDN or a wildcard matching all proper subdomains.
// Its zero value matches nothing.
type Selector struct {
	base     string
	wildcard bool
}

var lookup = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.BidiRule())
var errInvalid = errors.New("invalid host selector")

// Parse normalizes user input to lowercase ASCII with non-transitional UTS #46.
// One trailing root dot and surrounding whitespace are accepted. IP literals,
// single-label names, empty labels, and non-leftmost wildcards are rejected.
func Parse(raw string) (Selector, error) {
	if !utf8.ValidString(raw) {
		return Selector{}, errInvalid
	}
	raw = strings.TrimSpace(raw)
	wildcard := strings.HasPrefix(raw, "*.")
	if wildcard {
		raw = strings.TrimPrefix(raw, "*.")
	}
	ascii, err := lookup.ToASCII(raw)
	if err != nil {
		return Selector{}, errInvalid
	}
	ascii = strings.TrimSuffix(ascii, ".")
	if wildcard {
		ascii = "*." + ascii
	}
	return ParseCanonical(ascii)
}

// ParseCanonical reads the ASCII snapshot representation produced by Parse.
// It checks structural validity only; IDNA validation belongs to Parse on the
// trusted control-plane input boundary. Python consumes this same representation.
func ParseCanonical(text string) (Selector, error) {
	wildcard := strings.HasPrefix(text, "*.")
	base := text
	if wildcard {
		base = strings.TrimPrefix(text, "*.")
	}
	if !validHost(base) || (wildcard && len(base) > 251) {
		return Selector{}, errInvalid
	}
	return Selector{base: base, wildcard: wildcard}, nil
}

func validHost(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// String returns the canonical snapshot representation.
func (s Selector) String() string {
	if s.wildcard {
		return "*." + s.base
	}
	return s.base
}

// Matches accepts an ASCII wire hostname (case and one root dot are normalized).
// Unicode input is rejected: TLS SNI uses ASCII A-labels, not Unicode U-labels.
func (s Selector) Matches(host string) bool {
	if s.base == "" {
		return false
	}
	for i := 0; i < len(host); i++ {
		if host[i] >= 128 {
			return false
		}
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !validHost(host) {
		return false
	}
	if s.wildcard {
		return strings.HasSuffix(host, "."+s.base)
	}
	return host == s.base
}

// Overlaps reports whether any valid hostname belongs to both selectors.
// Wildcards exclude their apex and include nested subdomains.
func (s Selector) Overlaps(other Selector) bool {
	if s.base == "" || other.base == "" {
		return false
	}
	if !s.wildcard {
		return other.Matches(s.base)
	}
	if !other.wildcard {
		return s.Matches(other.base)
	}
	return s.base == other.base || strings.HasSuffix(s.base, "."+other.base) || strings.HasSuffix(other.base, "."+s.base)
}
