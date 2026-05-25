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
	"os"
	"strconv"
	"strings"
)

const (
	EnvBlockDoH443             = "OPENSANDBOX_EGRESS_BLOCK_DOH_443"
	EnvDoHBlocklist            = "OPENSANDBOX_EGRESS_DOH_BLOCKLIST"
	EnvEgressMode              = "OPENSANDBOX_EGRESS_MODE"
	EnvEgressHTTPAddr          = "OPENSANDBOX_EGRESS_HTTP_ADDR"
	EnvEgressToken             = "OPENSANDBOX_EGRESS_TOKEN"
	EnvEgressRules             = "OPENSANDBOX_EGRESS_RULES"
	EnvEgressPolicyFile        = "OPENSANDBOX_EGRESS_POLICY_FILE"
	EnvEgressLogLevel          = "OPENSANDBOX_EGRESS_LOG_LEVEL"
	EnvMaxEgressRules          = "OPENSANDBOX_EGRESS_MAX_RULES"
	EnvBlockedWebhook          = "OPENSANDBOX_EGRESS_DENY_WEBHOOK"
	EnvSandboxID               = "OPENSANDBOX_EGRESS_SANDBOX_ID"
	EnvEgressMetricsExtraAttrs = "OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS"
	EnvNameserverExempt        = "OPENSANDBOX_EGRESS_NAMESERVER_EXEMPT"

	// MITM: mitmdump transparent; Linux + CAP_NET_ADMIN, runs as a dedicated user.
	EnvMitmproxyTransparent      = "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"
	EnvMitmproxyPort             = "OPENSANDBOX_EGRESS_MITMPROXY_PORT"
	EnvMitmproxyConfDir          = "OPENSANDBOX_EGRESS_MITMPROXY_CONFDIR"
	EnvMitmproxyScript           = "OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT"
	EnvMitmproxyUpstreamTrustDir = "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR"
	EnvMitmproxyIgnoreHosts      = "OPENSANDBOX_EGRESS_MITMPROXY_IGNORE_HOSTS"
	EnvMitmproxySslInsecure      = "OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE"

	// Comma-separated upstream resolvers: literal IP only (optional :port) — no hostnames (see dnsproxy REDIRECT note).
	EnvDNSUpstream                 = "OPENSANDBOX_EGRESS_DNS_UPSTREAM"
	EnvDNSUpstreamTimeout          = "OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT"
	EnvDNSUpstreamProbe            = "OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE"
	EnvDNSUpstreamProbeIntervalSec = "OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE_INTERVAL_SEC"
)

const (
	PolicyDnsOnly = "dns"
	PolicyDnsNft  = "dns+nft"
)

const (
	DefaultEgressServerAddr      = ":18080"
	DefaultMitmproxyPort         = 18081
	ResolvNameserverCap          = 10
	DefaultMaxEgressRules        = 4096
	DefaultDNSUpstreamTimeoutSec = 5

	OpenSandboxRootDir = "/opt/opensandbox"
)

func EnvIntOrDefault(key string, defaultVal int) int {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func IsTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
