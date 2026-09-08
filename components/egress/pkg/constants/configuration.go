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

const EnvCredentialVaultTrustedProxyCIDRs = "OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_TRUSTED_PROXY_CIDRS"

// Fleet profile: the egress control plane serves N sandboxes
// sharing one host/network domain; sidecar remains the default profile.
const (
	EnvEgressProfile  = "OPENSANDBOX_EGRESS_PROFILE"
	EnvPendingPushTTL = "OPENSANDBOX_EGRESS_PENDING_PUSH_TTL"
)

const (
	ProfileSidecar = "sidecar"
	// ProfileFleet: one egress control plane serving N sandboxes sharing one
	// host/network domain (fast-sandbox Fastlet Pod).
	ProfileFleet = "fleet"
)

// Sandbox Actions Handler protocol (fast-sandbox, docs/concepts/
// sandbox-actions.md): the Fastlet delivers Binding synchronization and
// Lifecycle Hooks to the egress Handler over two Pod-loopback HTTP endpoints
// on the action target port.
const (
	ActionsAPIVersion   = "sandbox.fast.io/actions/v1"
	ActionsStatusPath   = "/_fastlet/v1/actions/status"
	ActionsDispatchPath = "/_fastlet/v1/actions"

	HookRuntimeReady   = "sandbox.runtime-ready"
	HookDataPlaneReady = "sandbox.data-plane-ready"
)

// Fleet-profile HTTP listener and trust model: the listener binds the Pod
// netns loopback only; the fastlet proxy and the Fastlet's action dispatcher
// are the only peers. The proxy injects the UID header that routes a push to
// its subject; the action dispatcher carries the identity in the envelope.
const (
	EgressSubjectUIDHeader        = "X-Fast-Sandbox-Uid"
	EgressSubjectGenerationHeader = "X-Fast-Sandbox-Generation"
	DefaultPendingPushTTL         = 30
)

const (
	EnvBlockDoH443               = "OPENSANDBOX_EGRESS_BLOCK_DOH_443"
	EnvDoHBlocklist              = "OPENSANDBOX_EGRESS_DOH_BLOCKLIST"
	EnvEgressMode                = "OPENSANDBOX_EGRESS_MODE"
	EnvEgressHTTPAddr            = "OPENSANDBOX_EGRESS_HTTP_ADDR"
	EnvEgressToken               = "OPENSANDBOX_EGRESS_TOKEN"
	EnvCredentialProxySocket     = "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET"
	EnvEgressRules               = "OPENSANDBOX_EGRESS_RULES"
	EnvEgressPolicyFile          = "OPENSANDBOX_EGRESS_POLICY_FILE"
	EnvEgressLogLevel            = "OPENSANDBOX_EGRESS_LOG_LEVEL"
	EnvMaxEgressRules            = "OPENSANDBOX_EGRESS_MAX_RULES"
	EnvBlockedWebhook            = "OPENSANDBOX_EGRESS_DENY_WEBHOOK"
	EnvSandboxID                 = "OPENSANDBOX_EGRESS_SANDBOX_ID"
	EnvEgressMetricsExtraAttrs   = "OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS"
	EnvNameserverExempt          = "OPENSANDBOX_EGRESS_NAMESERVER_EXEMPT"
	EnvCredentialVaultRequireTLS = "OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_TLS"

	// MITM: mitmdump transparent; Linux + CAP_NET_ADMIN, runs as a dedicated user.
	// Static mitm options (mode, connection_strategy, listen_host, stream_large_bodies,
	// ignore_hosts, ssl_verify_upstream_trusted_confdir default) live in
	// /var/lib/mitmproxy/.mitmproxy/config.yaml; only per-deployment overrides are env-driven.
	EnvMitmproxyTransparent      = "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"
	EnvMitmproxyPort             = "OPENSANDBOX_EGRESS_MITMPROXY_PORT"
	EnvMitmproxyScript           = "OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT"
	EnvMitmproxyUpstreamTrustDir = "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_TRUST_DIR"
	EnvMitmproxySslInsecure      = "OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE"
	// EnvMitmproxyExtraPorts (EXPERIMENTAL): extra TCP dports to intercept,
	// appended to the always-on 80,443. Comma-separated. May change or be
	// removed without notice.
	EnvMitmproxyExtraPorts = "OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS"

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
	DefaultFleetServerAddr       = "127.0.0.1:18080"
	DefaultMitmproxyPort         = 18081
	DefaultCredentialProxySocket = "/run/opensandbox/credential-proxy/active.sock"
	ResolvNameserverCap          = 10
	DefaultMaxEgressRules        = 4096
	DefaultDNSUpstreamTimeoutSec = 5
	OpenSandboxRootDir           = "/opt/opensandbox"
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
