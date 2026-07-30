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
	"strconv"
	"strings"
)

// MitmproxyBasePorts: always-intercepted dports; extras cannot remove these.
const MitmproxyBasePorts = "80,443"

// iptables multiport hard cap.
const iptablesMultiportMax = 15

// BuildMitmproxyPortList returns the `--dports` string for iptables: base
// (80,443) plus validated extras from raw. Fail-closed on any invalid input.
func BuildMitmproxyPortList(raw string) (string, error) {
	extras, err := parseExtraPorts(raw)
	if err != nil {
		return "", err
	}
	if len(extras) == 0 {
		return MitmproxyBasePorts, nil
	}
	total := 2 + len(extras)
	if total > iptablesMultiportMax {
		return "", fmt.Errorf("mitmproxy extra ports: total ports %d exceeds iptables multiport limit %d", total, iptablesMultiportMax)
	}
	parts := make([]string, 0, total)
	parts = append(parts, "80", "443")
	for _, p := range extras {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ","), nil
}

func parseExtraPorts(raw string) ([]int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	tokens := strings.Split(s, ",")
	out := make([]int, 0, len(tokens))
	seen := map[int]struct{}{80: {}, 443: {}}
	for _, tok := range tokens {
		t := strings.TrimSpace(tok)
		if t == "" {
			return nil, fmt.Errorf("mitmproxy extra ports: empty entry in %q", raw)
		}
		port, err := strconv.Atoi(t)
		if err != nil {
			return nil, fmt.Errorf("mitmproxy extra ports: %q is not an integer", t)
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("mitmproxy extra ports: %d is out of range [1, 65535]", port)
		}
		if _, dup := seen[port]; dup {
			return nil, fmt.Errorf("mitmproxy extra ports: duplicate or reserved port %d (80/443 are always intercepted)", port)
		}
		seen[port] = struct{}{}
		out = append(out, port)
	}
	return out, nil
}
