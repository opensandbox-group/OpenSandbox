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

package constants

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func ParseFastSandboxActionsAPIVersion(raw string) (string, error) {
	version := strings.TrimSpace(raw)
	if version == "" {
		return ActionsAPIVersion, nil
	}
	switch version {
	case ActionsAPIVersion, ActionsAPIVersionV2:
		return version, nil
	default:
		return "", fmt.Errorf("unsupported Fast Sandbox Actions apiVersion %q", version)
	}
}

const (
	EnvCredentialVaultTrustedProxyCIDRs  = "OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_TRUSTED_PROXY_CIDRS"
	EnvCredentialVaultRequireScopedMatch = "OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_SCOPED_MATCH"
)

// Credential header resolver client (OSEP-0025): egress → resolver transport
// configuration. Ownership of this surface lives with the resolver client
// (pkg/credentialvault/resolver_client.go); interception consumers only
// consume the client interface.
//
// EnvCredentialResolverTimeout: per-resolve deadline in seconds (1..120).
// EnvCredentialResolverMaxConcurrency: in-flight resolve cap (1..1024).
// EnvCredentialResolverAuth: origin-scoped bearer tokens
// "scheme://host:port=token[,...]" (D5). An endpoint whose normalized origin
// has no entry resolves WITHOUT an Authorization header — there is no global
// token. Never logged.
const (
	EnvCredentialResolverTimeout        = "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_TIMEOUT"
	EnvCredentialResolverMaxConcurrency = "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_MAX_CONCURRENCY"
	EnvCredentialResolverAuth           = "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_AUTH"
)

// Fast Sandbox profile: the egress control plane serves N sandboxes
// sharing one host/network domain; sidecar remains the default profile.
const (
	EnvEgressProfile = "OPENSANDBOX_EGRESS_PROFILE"
)

const (
	ProfileSidecar = "sidecar"
	// ProfileFastSandbox: one egress control plane serving N sandboxes sharing one
	// host/network domain (fast-sandbox Fastlet Pod).
	ProfileFastSandbox = "fast-sandbox"
)

// Sandbox Actions Handler protocol (fast-sandbox, docs/concepts/
// sandbox-actions.md): the Fastlet delivers Binding synchronization and
// Lifecycle Hooks to the egress Handler over two Pod-loopback HTTP endpoints
// on the action target port.
const (
	ActionsAPIVersion               = "sandbox.fast.io/actions/v1"
	ActionsAPIVersionV2             = "sandbox.fast.io/actions/v2"
	ActionsStatusPath               = "/_fastlet/v1/actions/status"
	ActionsDispatchPath             = "/_fastlet/v1/actions"
	ActionsV2StatusPath             = "/_fastlet/v2/actions/status"
	ActionsV2DispatchPath           = "/_fastlet/v2/actions"
	ActionsV2OwnerPathPrefix        = "/_fastlet/v2/owners/"
	ActionsV2RelayPathPrefix        = "/_fastlet/v2/relay/"
	ActionsV2VaultRelayPathPrefix   = ActionsV2RelayPathPrefix + "vault/"
	ActionsV2PolicyRelayPathPrefix  = ActionsV2RelayPathPrefix + "policy/"
	EnvFastSandboxActionsAPIVersion = "OPENSANDBOX_EGRESS_ACTIONS_API_VERSION"
	EgressActionsOwnerHeader        = "X-Fast-Sandbox-Owner"
	EgressActionsSandboxUIDHeader   = "X-Sandbox-Uid"

	HookRuntimeReady   = "sandbox.runtime-ready"
	HookDataPlaneReady = "sandbox.data-plane-ready"
)

// Fast Sandbox-profile HTTP listener and trust model: the listener binds the Pod
// netns loopback only; the Fastlet's action dispatcher is the only peer. The
// action envelope carries the subject identity — there is no proxy-injected
// routing header.

// Relay vault surface (OSEP-0022): /vault/{sandboxUid} — the internal
// server↔egress contract terminated by the FastPath handler-relay. Every op
// may carry the expectedIncarnation precondition (the owning runtime
// instance recorded at SET_BINDING); a mismatch is a 412.
//
// Relay policy surface mirrors the vault relay: /policy/{sandboxUid}
// terminates the same handler-relay channel for the subject's recorded
// egress policy (full replace with an optional expectedRevision, deny-first
// on DELETE).
const (
	EgressVaultIncarnationHeader = "X-Fast-Sandbox-Incarnation"

	VaultRelayPathPrefix = "/vault/"

	PolicyRelayPathPrefix = "/policy/"
)

// EnvEgressVaultDir (fast-sandbox profile): directory for the per-subject
// vault files on the Pod-scoped emptyDir volume. Empty/unset keeps the vault
// memory-only; the sidecar profile never persists.
//
// EnvEgressPolicyDir (fast-sandbox profile): REQUIRED — directory for the
// per-subject policy files. The recorded policy is egress-held state; the
// profile fails startup without it.
const (
	EnvEgressVaultDir  = "OPENSANDBOX_EGRESS_VAULT_DIR"
	EnvEgressPolicyDir = "OPENSANDBOX_EGRESS_POLICY_DIR"
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
	// EnvMitmproxyUpstreamExtraCA: path to a PEM file with one or more extra CA
	// certificates, passed to mitmproxy ssl_verify_upstream_trusted_ca. Additive
	// with the system/confdir trust, and applies to every mitmproxy upstream TLS
	// connection (HTTPS proxy and intercepted origins), not only the proxy hop.
	EnvMitmproxyUpstreamExtraCA = "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA"
	EnvMitmproxySslInsecure     = "OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE"
	// EnvExperimentalRevisionRuntime enables the internal OSEP-0023
	// per-mitmdump revision bootstrap path. It is not a public interception mode.
	EnvExperimentalRevisionRuntime = "OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME"
	// OSEP-0023 live admission budgets, handed to the mitmdump child with the
	// revision session bundle. Decrypted-connection and admitted-request caps,
	// plus the credential transition drain timeout in seconds (1..300).
	// Operator-owned tuning only; not a public sandbox-user API.
	EnvRevisionTLSCapacity         = "OPENSANDBOX_EGRESS_REVISION_TLS_CAPACITY"
	EnvRevisionRequestCapacity     = "OPENSANDBOX_EGRESS_REVISION_REQUEST_CAPACITY"
	EnvRevisionDrainTimeoutSeconds = "OPENSANDBOX_EGRESS_REVISION_DRAIN_TIMEOUT_SECONDS"
	// EnvMitmproxyExtraPorts (EXPERIMENTAL): extra TCP dports to intercept,
	// appended to the always-on 80,443. Comma-separated. May change or be
	// removed without notice.
	EnvMitmproxyExtraPorts = "OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS"
	// EnvMitmCaSeedDir (fast-sandbox profile): read-only seed mount holding the
	// pool-pinned MITM CA keypair as `tls.crt` (certificate) + `tls.key`
	// (private key). When transparent MITM is enabled the seed is REQUIRED —
	// egress copies the pinned pair into the writable confdir before mitmdump
	// starts and NEVER generates a CA in this profile (OSEP-0022 MITM Trust
	// Anchor). A missing, empty, or unloadable seed fails the MITM addon
	// closed (healthz + actions status) while the rest of egress keeps
	// serving; there is NO fallback to generate-on-start. Only pools that
	// never enable transparent MITM need no CA and are unaffected by this
	// variable.
	EnvMitmCaSeedDir = "OPENSANDBOX_EGRESS_MITM_CA_SEED_DIR"

	// EnvUpstreamProxy: chained upstream proxy endpoint ("http://host:port" or
	// "https://host:port"). When set, the bundled upstream_proxy.py addon is
	// loaded and all mitm-handled egress is chained through it (fail-closed:
	// anything that cannot be chained is refused). Empty = disabled.
	EnvUpstreamProxy = "OPENSANDBOX_EGRESS_UPSTREAM_PROXY"
	// EnvUpstreamProxyAuth: complete Proxy-Authorization header value sent on
	// the upstream CONNECT (e.g. "Basic base64(user:pass)"). Requires
	// EnvUpstreamProxy. Never logged.
	EnvUpstreamProxyAuth = "OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH"

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
	DefaultFastSandboxServerAddr = "127.0.0.1:18080"
	DefaultMitmproxyPort         = 18081
	DefaultCredentialProxySocket = "/run/opensandbox/credential-proxy/active.sock"
	ResolvNameserverCap          = 10
	DefaultMaxEgressRules        = 4096
	DefaultDNSUpstreamTimeoutSec = 2
	OpenSandboxRootDir           = "/opt/opensandbox"
	// Credential header resolver client defaults (OSEP-0025).
	DefaultCredentialResolverTimeoutSeconds = 2
	DefaultCredentialResolverMaxConcurrency = 32
	// OSEP-0023 live admission defaults (experimental sidecar only).
	DefaultRevisionTLSCapacity         = 1024
	DefaultRevisionRequestCapacity     = 4096
	DefaultRevisionDrainTimeoutSeconds = 30
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

// EnvIntRange returns the env int only when it parses into [min, max];
// otherwise it returns defaultVal. Operator-owned budgets use this to reject
// silently-clamped nonsense rather than guessing a replacement value.
func EnvIntRange(key string, defaultVal, minValue, maxValue int) int {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < minValue || v > maxValue {
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

// EnvMitmproxyPassthroughHosts configures live-only static TLS/443 selectors.
const EnvMitmproxyPassthroughHosts = "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"
