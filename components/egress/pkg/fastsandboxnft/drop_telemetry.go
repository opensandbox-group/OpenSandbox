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

package fastsandboxnft

import (
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
)

func dropReason(line string) droptelemetry.Reason {
	f := strings.Fields(line)
	if len(f) < 5 {
		return ""
	}
	chain := f[4]
	switch {
	case strings.Contains(line, "@upstream_proxy_"):
		return droptelemetry.UpstreamProxyDeny
	case strings.HasPrefix(chain, "subj_") && strings.Contains(line, "ct original "):
		return droptelemetry.SubjectInputDeny
	case strings.Contains(line, "meta mark &"):
		return droptelemetry.UnmarkedFailclosed
	case chain == inputChain && (strings.Contains(line, "ip saddr ") || strings.Contains(line, "ip6 saddr ")):
		return droptelemetry.GatewayMITMDirect
	case strings.Contains(line, "dport 853"):
		return droptelemetry.DoT
	case strings.Contains(line, "@doh_block_") || strings.Contains(line, "dport 443") || strings.Contains(line, "proto-dst 443"):
		return droptelemetry.DoH
	case strings.Contains(line, "tcp dport {") || strings.Contains(line, "proto-dst {"):
		return droptelemetry.PortDeny
	case strings.Contains(line, "_deny_v"):
		return droptelemetry.DenySet
	case len(f) == 6 && strings.HasPrefix(chain, "subj_"):
		return droptelemetry.DefaultDeny
	}
	return ""
}

// Infer INPUT ownership from original-destination expressions, not a suffix:
// a sandbox UID can itself end in "-in" after nft identifier sanitization.
func dropReasons(script string) func(string) droptelemetry.Reason {
	inputs := make(map[string]bool)
	for _, line := range strings.Split(script, "\n") {
		f := strings.Fields(line)
		if len(f) > 5 && f[0] == "add" && f[1] == "rule" && strings.HasPrefix(f[4], "subj_") && strings.Contains(line, "ct original ") {
			inputs[f[4]] = true
		}
	}
	return func(line string) droptelemetry.Reason {
		f := strings.Fields(line)
		if len(f) > 5 && inputs[f[4]] {
			return droptelemetry.SubjectInputDeny
		}
		return dropReason(line)
	}
}
