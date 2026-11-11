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

package iptables

import (
	"os"
	"strings"
)

// RedirectBackendEnv selects how the OUTPUT redirects (DNS → the proxy, HTTP/HTTPS → mitmproxy)
// are installed:
//
//	auto     (default) iptables first; on a failure that the nft backend of iptables reports
//	         (a missing chain, a missing xt extension such as `owner` or `REDIRECT` in a kernel
//	         built without the xtables compat modules) fall back to native nft rules.
//	iptables iptables only; never fall back.
//	nft      native nft rules from the start (no xtables extensions required at all).
//
// Firecracker-style guest kernels (Fly.io, some microVM hosts) ship nf_tables without
// CONFIG_NETFILTER_XT_MATCH_OWNER / the IPv6 REDIRECT target: `nft` is the working choice there.
const RedirectBackendEnv = "OPENSANDBOX_EGRESS_REDIRECT_BACKEND"

type backend string

const (
	backendAuto     backend = "auto"
	backendIptables backend = "iptables"
	backendNft      backend = "nft"
)

func redirectBackend() backend {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RedirectBackendEnv))) {
	case "nft":
		return backendNft
	case "iptables":
		return backendIptables
	default:
		return backendAuto
	}
}

// isIptablesXtExtensionError: iptables (nf_tables backend) could not load an xtables extension
// the rule needs — the kernel has nf_tables but not the compat module. The rule is unusable and
// a native nft rule is the way to express it.
func isIptablesXtExtensionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "extension") && strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "xt target") && strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no chain/target/match by that name") ||
		strings.Contains(msg, "rule_append failed") && strings.Contains(msg, "no such file or directory")
}
