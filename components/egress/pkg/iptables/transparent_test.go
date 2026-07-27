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

package iptables

import (
	"strings"
	"testing"
)

func TestTransparentHTTPRules_DportsAndOp(t *testing.T) {
	cases := []struct {
		name   string
		dports string
	}{
		{"default", "80,443"},
		{"extra_single", "80,443,8080"},
		{"extra_multi", "80,443,8080,8443,9000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := transparentHTTPRules(18081, 1234, tc.dports, "-A")
			if len(rules) != 2 {
				t.Fatalf("want 2 rules, got %d", len(rules))
			}
			redir := strings.Join(rules[1], " ")
			if !strings.Contains(redir, "--dports "+tc.dports) {
				t.Errorf("REDIRECT rule missing --dports %q: %s", tc.dports, redir)
			}
			if !strings.Contains(redir, "--to-ports 18081") {
				t.Errorf("REDIRECT rule missing --to-ports 18081: %s", redir)
			}
			if !strings.Contains(redir, "! --uid-owner 1234") {
				t.Errorf("REDIRECT rule missing uid-owner exclusion: %s", redir)
			}
			if !strings.HasPrefix(strings.Join(rules[0], " "), "iptables -t nat -A OUTPUT -p tcp -d 127.0.0.0/8 -j RETURN") {
				t.Errorf("loopback RETURN rule malformed: %s", strings.Join(rules[0], " "))
			}
		})
	}
}

func TestTransparentHTTPRules_OpFlagPropagates(t *testing.T) {
	rules := transparentHTTPRules(18081, 1234, "80,443", "-D")
	for _, r := range rules {
		if r[3] != "-D" {
			t.Errorf("expected -D op, got %v", r)
		}
	}
}
