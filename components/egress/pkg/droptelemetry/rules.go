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

package droptelemetry

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

type Reason string

const (
	DenySet            Reason = "deny_set"
	PortDeny           Reason = "port_deny"
	DoT                Reason = "dot"
	DoH                Reason = "doh"
	DefaultDeny        Reason = "default_deny"
	EstablishedRevoke  Reason = "established_revoke"
	SubjectInputDeny   Reason = "subject_input_deny"
	UnmarkedFailclosed Reason = "unmarked_failclosed"
	GatewayMITMDirect  Reason = "gateway_mitm_direct"
	UpstreamProxyDeny  Reason = "upstream_proxy_deny"
)

var reasons = []Reason{DenySet, PortDeny, DoT, DoH, DefaultDeny, EstablishedRevoke, SubjectInputDeny, UnmarkedFailclosed, GatewayMITMDirect, UpstreamProxyDeny}

func ValidReason(r Reason) bool {
	for _, v := range reasons {
		if r == v {
			return true
		}
	}
	return false
}
func counterName(chain string, r Reason) string {
	if strings.HasPrefix(chain, "subj_") {
		h := sha256.Sum256([]byte(chain))
		return fmt.Sprintf("osb_%x_%s", h[:8], r)
	}
	return string(r)
}

// Instrument preserves all original matching expressions and verdict order.
// Objects are declared only alongside their owning table or subject chain.
// New drop forms must update the reason mapping, TSV, and fixture tests; unmapped drops reject the apply.
func Instrument(script string, c Config, reasonFor func(string) Reason) (string, error) {
	if c.Mode == Off || c.Mode == "" {
		return script, nil
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		raw := strings.TrimSuffix(line, "\n")
		f := strings.Fields(raw)
		if len(f) >= 5 && f[0] == "add" && f[1] == "rule" && strings.HasSuffix(raw, " drop") {
			r := reasonFor(raw)
			if !ValidReason(r) {
				return "", fmt.Errorf("unmapped drop rule: %s", raw)
			}
			match := strings.TrimSuffix(raw, " drop")
			if c.Mode == Sample && r != EstablishedRevoke {
				tcpOnly := strings.Contains(match, "tcp ")
				udpOnly := strings.Contains(match, "udp ")
				if !udpOnly {
					fmt.Fprintf(&b, "%s meta l4proto tcp tcp flags & (syn | ack) == syn limit rate %s burst %d packets log group %d snaplen 128 prefix \"osb:%s\"\n", match, c.Rate, c.Burst, c.Group, r)
				}
				if !tcpOnly {
					fmt.Fprintf(&b, "%s meta l4proto udp limit rate %s burst %d packets log group %d snaplen 128 prefix \"osb:%s\"\n", match, c.Rate, c.Burst, c.Group, r)
				}
			}
			fmt.Fprintf(&b, "%s counter name \"%s\" drop\n", match, counterName(f[4], r))
			continue
		}
		b.WriteString(line)
		if len(f) >= 4 && f[0] == "add" && f[1] == "table" || len(f) >= 5 && f[0] == "add" && f[1] == "chain" && strings.HasPrefix(f[4], "subj_") {
			chain := ""
			if f[1] == "chain" {
				chain = f[4]
			}
			// Declare every reason intentionally so policy changes can reuse a fixed object set.
			for _, r := range reasons {
				fmt.Fprintf(&b, "add counter inet %s %s\n", f[3], counterName(chain, r))
			}
		}
	}
	return b.String(), nil
}
func reasonForCounter(name string) (Reason, bool) {
	for _, r := range reasons {
		if name == string(r) || strings.HasPrefix(name, "osb_") && strings.HasSuffix(name, "_"+string(r)) {
			return r, true
		}
	}
	return "", false
}
