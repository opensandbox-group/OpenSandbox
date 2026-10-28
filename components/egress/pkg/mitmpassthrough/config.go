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

// Package mitmpassthrough freezes operator TLS passthrough selectors at startup.
package mitmpassthrough

import (
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
)

const MaxBytes = 64 << 10
const MaxSelectors = 256

var ErrInvalid = errors.New("invalid OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS configuration")

// Config is immutable. Its zero value is an empty selector set.
type Config struct {
	selectors []hostselector.Selector
	canonical string
}

// Parse accepts the external operator JSON boundary, not child handoff data.
func Parse(raw string, present bool) (Config, error) {
	if !present {
		return Config{}, nil
	}
	if len(raw) > MaxBytes || !utf8.ValidString(raw) {
		return Config{}, ErrInvalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || entries == nil || len(entries) > MaxSelectors {
		return Config{}, ErrInvalid
	}
	values := make(map[string]hostselector.Selector, len(entries))
	for _, entry := range entries {
		var value *string
		if err := json.Unmarshal(entry, &value); err != nil || value == nil {
			return Config{}, ErrInvalid
		}
		selector, err := hostselector.Parse(*value)
		if err != nil {
			return Config{}, ErrInvalid
		}
		values[selector.String()] = selector
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	encoded, err := json.Marshal(names)
	if err != nil || len(encoded) > MaxBytes {
		return Config{}, ErrInvalid
	}
	selectors := make([]hostselector.Selector, 0, len(names))
	for _, name := range names {
		selectors = append(selectors, values[name])
	}
	return Config{selectors: selectors, canonical: string(encoded)}, nil
}
func (c Config) JSON() string {
	if c.canonical == "" {
		return "[]"
	}
	return c.canonical
}
func (c Config) Empty() bool { return len(c.selectors) == 0 }
func (c Config) Selectors() []hostselector.Selector {
	return append([]hostselector.Selector(nil), c.selectors...)
}
func (c Config) Overlaps(selector hostselector.Selector) bool {
	for _, s := range c.selectors {
		if s.Overlaps(selector) {
			return true
		}
	}
	return false
}
