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

package constants

import (
	"fmt"
	"strings"
)

// ParseEgressMode: tokens "dns" (required) and "nft", joined with +, order-free. Empty env → dns-only.
func ParseEgressMode(raw string) (string, error) {
	seen, err := parseModeTokens(raw)
	if err != nil {
		return "", err
	}
	return normalizeMode(seen), nil
}

func parseModeTokens(raw string) (map[string]bool, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return map[string]bool{"dns": true}, nil
	}
	parts := strings.Split(raw, "+")
	seen := make(map[string]bool)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case "dns", "nft":
			seen[p] = true
		default:
			return nil, fmt.Errorf("unknown mode token %q", p)
		}
	}
	if !seen["dns"] {
		return nil, fmt.Errorf("egress mode must include dns")
	}
	return seen, nil
}

func normalizeMode(seen map[string]bool) string {
	if !seen["nft"] {
		return PolicyDnsOnly
	}
	return PolicyDnsNft
}

func ModeUsesNft(mode string) bool {
	seen, err := parseModeTokens(mode)
	if err != nil {
		return false
	}
	return seen["nft"]
}
