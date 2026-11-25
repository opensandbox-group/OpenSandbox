---
title: Egress Upstream Proxy Forward Mode
authors:
  - "@hittyt"
creation-date: 2026-11-13
last-updated: 2026-11-25
status: implementing
---

# OSEP-0030: Egress Upstream Proxy Forward Mode

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Server configuration](#server-configuration)
  - [Create and policy API behavior](#create-and-policy-api-behavior)
  - [Egress attachment for every sandbox](#egress-attachment-for-every-sandbox)
  - [Egress data plane in forward mode](#egress-data-plane-in-forward-mode)
  - [Fast Sandbox profile](#fast-sandbox-profile)
  - [Mode consistency check for Fast Sandbox](#mode-consistency-check-for-fast-sandbox)
  - [Tunnel authentication and sandbox identity](#tunnel-authentication-and-sandbox-identity)
  - [Gateway contract](#gateway-contract)
  - [Observability](#observability)
  - [Relationship to existing proposals](#relationship-to-existing-proposals)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
- [Questions for Review](#questions-for-review)
<!-- /toc -->

## Summary

Add `"forward"` to `[egress] mode` (`dns | dns+nft | forward`). In forward mode, egress transparently captures every outbound TCP connection of a sandbox and relays the original bytes, without decryption, through a standard HTTP `CONNECT` tunnel to a centralized egress gateway configured under `[egress.upstream_proxy]`, such as [agentgateway](https://github.com/agentgateway/agentgateway). The gateway is the only policy authority: it decides per connection whether to pass TLS through, terminate and inspect it, inject credentials, or deny. OpenSandbox is reduced to a trusted, transparent, non-bypassable on-ramp: it applies to every sandbox (with or without `networkPolicy`, sidecar and Fast Sandbox profiles), drops all traffic that is not forwarded, and never falls back to direct egress. Existing `dns` and `dns+nft` modes — including MITM-chained `[egress.upstream_proxy]` under `dns+nft` — remain the defaults and are unchanged.

## Motivation

The current upstream proxy integration ([guide][upstream-guide], [addon][upstream-addon]) chains the transparent mitmproxy path through a `CONNECT` proxy. That design is the right fit for the Credential Vault, but it is a poor fit for a centralized gateway that owns egress policy:

- **Local decryption is mandatory.** Chaining only exists inside the mitmproxy addon, so the server rejects a sandbox with `networkPolicy` unless transparent MITM is enabled ([`validate_upstream_proxy_request`][validate-upstream]). Pass-through flows cannot be chained at all (`ignore_hosts`/`tcp_hosts`/`udp_hosts` are refused, see [addon load][upstream-addon-load]). Every HTTPS connection is therefore decrypted and re-encrypted by mitmproxy even when nothing local needs the plaintext.
- **The gateway does not get plaintext without a second MITM.** mitmproxy re-originates TLS inside the tunnel, so a gateway that only passes TLS through cannot see L7 content. To inspect, the gateway must intercept again (double MITM), and the egress sidecar must trust the gateway CA via `ca_cert_path`/`ca_secret_name` ([config][upstream-config]).
- **Coverage is opt-in per request.** Egress is attached only when the create request contains `networkPolicy`; a policy-less sandbox egresses directly and bypasses the gateway. Only intercepted ports (80/443 plus at most 13 experimental extras) reach the gateway.
- **Fast Sandbox and pooled Kubernetes are unsupported** ([runtime matrix][upstream-matrix]); the shared-Fastlet server path rejects `networkPolicy` while `upstream_proxy` is configured ([`_reject_network_policy_with_upstream_proxy`][fsb-reject]).

Platforms that adopt a centralized agent egress gateway want the opposite division of labor: policy, credential injection, LLM/MCP governance and audit live in the gateway; OpenSandbox only guarantees that sandbox traffic reaches the gateway and cannot bypass it. The same split is used by [Agent Substrate][substrate-egress], where `ateworker` installs a TCP REDIRECT and `atunnel` reads `SO_ORIGINAL_DST` and opens a `CONNECT IP:port` tunnel to the egress policy enforcement point. OpenSandbox egress already owns the equivalent capture position in both profiles.

### Goals

1. Provide an administrator-selected forward mode in which every sandbox's outbound TCP is relayed byte-for-byte to the configured gateway via `CONNECT`, with no local TLS interception.
2. Make the mode platform-enforced: a create or policy request cannot opt out, and a sandbox without `networkPolicy` is covered.
3. Fail closed: traffic that is not forwarded (UDP, QUIC, ICMP, non-captured families) is dropped; gateway unavailability never results in direct egress.
4. Support the sidecar profile (Docker bridge, Kubernetes non-pooled) and the Fast Sandbox profile.
5. Detect, per sandbox and at enforcement time, a mismatch between the server's mode and the Fast Sandbox pool's egress configuration, and fail closed on mismatch.
6. Keep `dns` (the default) and `dns+nft` — including today's MITM-chained `[egress.upstream_proxy]` behavior — unchanged.

### Non-Goals

- Forwarding UDP, QUIC/HTTP3, or any non-TCP protocol. They are dropped, consistent with the current egress which does not relay them either.
- Local DNS policy in OpenSandbox, or carrying per-sandbox identity inside forwarded DNS packets (e.g. custom EDNS0/DoH extensions). OpenSandbox supports forwarding queries to an operator-configured policy DNS resolver fail-closed (see R7); destination policy belongs to the gateway and, optionally, that external resolver.
- Forwarding caller-supplied `CreateSandboxRequest.metadata` labels or an open-ended key-value map onto `CONNECT` headers. Tunnel identity headers are a closed set of platform-truth fields (`OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, and under multi-tenancy `OpenSandbox-Tenant-Name`) so caller input can never enter or spoof gateway policy selectors.
- Combining forward mode with OpenSandbox `networkPolicy` or the Credential Vault (`credentialProxy`). They are mutually exclusive; L7 effects belong to the gateway.
- Injecting the gateway's interception CA into sandboxes. When the gateway terminates TLS, the sandbox must trust the gateway CA; delivering trust bundles to sandboxes is a generic capability orthogonal to this proposal and is left to a separate proposal.
- Translating OpenSandbox `networkPolicy` semantics into gateway configuration, or defining a gateway policy language.
- Kubernetes pool mode, gVisor, and Docker host networking in the first phase (rejected, see below).
- Durable management, credential rotation, or reconciliation of gateway authorization (see the proposed managed egress integration in [#2308][pr-2308]).

## Requirements

| ID | Requirement |
|---|---|
| R1 | With `[egress] mode = "forward"`, every sandbox created by the server has egress enforcement attached in-band, regardless of `networkPolicy`, and enforcement (or a default-deny barrier) is active **before the workload entrypoint executes**; any failure to attach or initialize egress leaves networking blocked and fails sandbox creation or readiness rather than running the workload unprotected. |
| R2 | Requests carrying `networkPolicy` (create, replace, patch) or `credentialProxy.enabled=true`, or attempting to enable transparent MITM via env, are rejected with HTTP 400. |
| R3 | All outbound TCP from the sandbox (except loopback and DNS to the egress DNS proxy) is relayed via `CONNECT <original-ip>:<original-port>` to the configured gateway, carrying the configured `Proxy-Authorization` and a closed set of platform-truth identity headers: `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace` (on Kubernetes and Fast Sandbox, both single-tenant and multi-tenant), and `OpenSandbox-Tenant-Name` (multi-tenant only). |
| R4 | The relay does not parse or modify the relayed bytes. |
| R5 | No direct-dial fallback: if the tunnel cannot be established, the sandbox connection is closed. |
| R6 | Everything that is not relayed TCP or DNS to the egress DNS proxy is dropped, including UDP, ICMP and link-local destinations. |
| R7 | In forward mode OpenSandbox applies no local domain policy in the DNS proxy and forwards every query to the configured upstream resolver(s). Operators may configure a policy-enforcing upstream DNS resolver consistently across both profiles; when configured, policy denials (including `REFUSED`) and resolver failures fail closed with no fallback to an unrestricted resolver. |
| R8 | The sandbox cannot reach the gateway endpoint directly, cannot impersonate the forwarder, and cannot spoof another sandbox's dispatch identity, so it cannot forge tunnel metadata. In the sidecar profile the forwarder is identified by a capability-gated socket mark rather than a UID; in the Fast Sandbox profile source-IP unforgeability is enforced at the per-sandbox slot boundary. |
| R9 | On Fast Sandbox, a mismatch between the server mode and the egress mode makes the sandbox fail closed (no network) and surface as `Failed`. This holds for every source of a subject's policy: the binding input, binding replays after egress restarts or rollbacks, the persisted policy record, and snapshot restore. |
| R10 | Existing `[egress] mode` values (`dns`, `dns+nft`, or unset) and `[egress.upstream_proxy]` under `dns+nft` behave exactly as today, including the binding input format sent to existing Fast Sandbox pools. |
| R11 | Every tunnel is authenticated: `[egress] mode = "forward"` requires `upstream_proxy.url` and `upstream_proxy.authorization`, and the gateway treats tunnel metadata as trusted only on authenticated tunnels. |

## Proposal

```text
Sandbox process: connect(203.0.113.10:443), unaware of any proxy
        |
        v  netns redirect (sidecar) / per-subject redirect (Fast Sandbox)
OpenSandbox egress forwarder
        |  SO_ORIGINAL_DST -> 203.0.113.10:443
        |  CONNECT 203.0.113.10:443 HTTP/1.1
        |  Proxy-Authorization: <configured>
        |  OpenSandbox-Sandbox-Id: <server-assigned id>
        |  OpenSandbox-Sandbox-Namespace: <k8s namespace>   # K8s & Fast Sandbox (single & multi-tenant)
        |  OpenSandbox-Tenant-Name: <tenant.name>           # multi-tenant only
        v  (http:// or https:// to the gateway; only egress can reach it)
Centralized gateway (e.g. agentgateway)
        |  authenticate tunnel, read inner SNI / Host,
        |  pass through, terminate+inspect, inject credentials, or deny
        v
Origin
```

Configuration adds `"forward"` as an operating posture on `[egress] mode`, reusing `[egress.upstream_proxy]` for the remote gateway hop:

```toml
[egress]
mode = "forward"                           # dns | dns+nft | forward; default "dns"
# dns_upstreams = ["10.96.0.53:53"]        # optional policy-enforcing DNS upstream(s), fail-closed (R7)

[egress.upstream_proxy]
url = "https://gateway.example.com:3000"   # required when mode = "forward"
authorization = "Bearer <token>"           # required when mode = "forward" (R11)
# ca_cert_path / ca_secret_name: trust for the gateway listener (unchanged)
```

Responsibilities in forward mode:

| Concern | Owner |
|---|---|
| Capture, original destination, tunnel to the gateway, drop of everything else, no fallback | OpenSandbox egress |
| Sandbox identity on the tunnel, containment of the gateway endpoint | OpenSandbox egress / server |
| Destination allow/deny, TLS passthrough vs termination, credential injection, LLM/MCP policy, audit | Gateway |
| Trust in the gateway's interception CA inside the sandbox (only if the gateway terminates TLS) | Out of scope (separate trust-bundle proposal, or image) |

### Notes/Constraints/Caveats

- **CONNECT target is the original IP and port.** The forwarder does not peek at the ClientHello or Host header. The gateway reads SNI or Host from the inner stream itself. This keeps the forwarder protocol-agnostic and matches the Substrate transport contract ([network-egress.md][substrate-egress]).
- **Two independent TLS layers.** The optional outer TLS (`https://` gateway URL) is verified by egress using the existing upstream trust (`ca_cert_path`/`ca_secret_name`). The inner TLS is between the sandbox application and either the origin (passthrough) or the gateway (termination). Trust in the outer layer does not transfer to the inner layer.
- **Policy authority moves to the gateway.** Once relayed, the kernel only sees connections to the gateway, so OpenSandbox destination CIDR/port rules no longer apply to relayed traffic. Forward mode makes this explicit instead of layering two partial policies.
- **DNS is the only unrelayed channel.** Keeping local DNS policy out of OpenSandbox avoids syncing gateway policy back into the runtime, but an unrestricted upstream resolver leaves DNS as a possible exfiltration and reconnaissance channel. Deployments that need DNS-level filtering configure an operator-managed policy DNS via `[egress] dns_upstreams` / `OPENSANDBOX_EGRESS_DNS_UPSTREAM` (R7), which egress honors fail-closed with no fallback to an unrestricted resolver.
- **IPv6.** The sidecar transparent redirect is IPv4 `iptables` today. In the first phase IPv6 TCP is dropped rather than relayed.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Bypass through non-TCP or uncaptured traffic | R6: forward mode drops everything that is not relayed TCP or DNS to the egress DNS proxy; Fast Sandbox `forward` subjects never receive a per-subject accept mark. |
| Sandbox forges tunnel identity by dialing the gateway directly | R8: the gateway endpoint is reachable only by forwarder traffic (sidecar: forwarder socket mark, see below; Fast Sandbox: existing profile-wide endpoint drop for sandbox sources ([fsb upstream][fsb-nft-upstream])). |
| Sandbox process with `CAP_SETUID` assumes the forwarder's UID to skip the redirect and reach the gateway (sidecar shares the netns) | The sidecar exemption matches a forwarder socket mark instead of `meta skuid`. Setting `SO_MARK` requires `CAP_NET_ADMIN` or `CAP_NET_RAW`; forward mode requires and enforces that sandbox containers hold neither (Docker already drops both by default ([caps][docker-caps])). |
| Fast Sandbox subject spoofs another slot's source IP to misattribute `OpenSandbox-Sandbox-Id` | R8: container subjects hold neither `CAP_NET_ADMIN` nor `CAP_NET_RAW`, and microVM slot namespaces enforce source anti-spoofing so only the assigned slot IP can exit toward the bridge. |
| Sidecar attachment silently omitted (e.g. webhook timeout/ignore) or workload starts before egress rules are installed (Kubernetes peer-container race) | R1: the server constructs the container/Pod spec directly with the egress sidecar in-band (no mutating webhook), and enforcement (or a bootstrap default-deny barrier in the init container) is active before the workload entrypoint runs; initialization delay or failure leaves networking blocked. |
| Policy DNS denial (`REFUSED`) or resolver outage falls back to an unrestricted resolver | R7: when `dns_upstreams` / `OPENSANDBOX_EGRESS_DNS_UPSTREAM` is configured, egress never falls back to `/etc/resolv.conf` or `8.8.8.8:53` and treats `REFUSED` as a final authoritative denial rather than failing over. |
| Platform tunnel credential leaks, allowing impersonation of any sandbox identity | The credential is delivered only to egress (never to the sandbox container, no shared process namespace with egress); per-sandbox credentials or mTLS are a follow-up (see Questions). |
| SNI or Host spoofing (claim an allowed name, dial an arbitrary IP) | Gateway contract: when authorizing by hostname, the gateway must route to the authorized hostname rather than to the CONNECT IP (same rule as Substrate's trust boundary). |
| Hostname unavailable (ECH, TLS without SNI, opaque TCP) silently weakens hostname policy | Gateway contract: such connections are either admitted by an explicitly authorized `IP:port` rule or denied; an ECH outer/public name is never treated as the origin name. |
| Gateway outage silently opens egress | R5: no fallback; connections fail. Readiness reports tunnel health (see Observability). |
| Fast Sandbox pool not configured while server is in forward mode, so sandboxes egress directly | R9: the forward marker is part of the policy body, so every policy source (binding, persisted record, snapshot bundle) is checked; mismatch fails closed. |
| Egress rollback or snapshot restore onto an older Fastlet activates a stored allow policy | Forward subjects never store an allow policy; older egress rejects the stored `forward` value and degrades the subject to default-deny. |
| SSRF or metadata access through the gateway | Link-local is dropped before the gateway (R6); the gateway deployment must restrict private ranges on its own egress (documented gateway requirement). |
| Per-connection tunnel setup latency | One extra RTT to a nearby gateway per TCP connection; HTTP/2 multiplexed `CONNECT` is a possible follow-up. |

## Design Details

### Server configuration

`EgressConfig` ([config][egress-config]) expands `mode` from `Literal["dns", "dns+nft"]` and adds an optional policy-DNS upstream list:

```python
mode: Literal["dns", "dns+nft", "forward"] = "dns"
dns_upstreams: Optional[list[str]] = None  # literal IP or IP:port; injected as OPENSANDBOX_EGRESS_DNS_UPSTREAM
```

`EgressUpstreamProxyConfig` ([upstream config][upstream-config]) adds no new fields and retains a single responsibility: connection parameters for the remote gateway hop (`url`, `authorization`, `ca_cert_path`, `ca_secret_name`), shared by both upstream postures.

Three aspects of this configuration shape are deliberate:

- **Operating posture, built on the `dns+nft` capture plane.** Although `dns` and `dns+nft` name local enforcement capabilities while `forward` names policy placement, `[egress] mode` is the top-level operating posture of the egress subsystem because `forward` changes attachment, API surface, DNS resolution, and nftables behavior together. Under the hood `forward` still requires and builds on the `dns+nft` machinery (netns capture, DNS proxy, containment sets) rather than substituting a different engine.
- **Asymmetric posture selection for backward compatibility.** Forward mode is selected explicitly via `[egress] mode = "forward"`. The existing MITM-chained posture stays selected as today (`[egress] mode = "dns+nft"` with `[egress.upstream_proxy]` present and per-sandbox transparent MITM enabled), so existing deployments do not migrate.
- **Single posture channel to egress.** Both the sidecar and Fast Sandbox profiles consume `OPENSANDBOX_EGRESS_MODE=forward` rather than introducing a separate upstream-proxy mode variable.

Validation when `[egress] mode = "forward"`: `upstream_proxy` with non-empty `url` and `authorization` is required (R11); runtimes without an nftables capture plane (`[docker] network_mode = "host"`, `secure_runtime.type = "gvisor"`, Kubernetes pool mode) are rejected. When `dns_upstreams` is set, every entry must be a literal IP (optional `:port`, matching `OPENSANDBOX_EGRESS_DNS_UPSTREAM` validation to avoid self-recursive hostname resolution). When `[egress] mode` is not `"forward"`, `[egress.upstream_proxy]` continues to require `mode = "dns+nft"` as today.

Sandbox container privileges (sidecar profile, R8): the sandbox container must hold neither `CAP_NET_ADMIN` nor `CAP_NET_RAW` and must not share a process namespace with the egress container. On Docker the server rejects a forward-mode configuration whose `drop_capabilities` keeps either capability (both are dropped by default ([caps][docker-caps])). On Kubernetes the server drops both in the sandbox container `securityContext` and rejects pod templates that add them back or enable `shareProcessNamespace`.

Unsupported combinations fail explicitly at configuration validation rather than silently dropping guarantees, including combining forward mode with a managed-MITM profile such as the one proposed in [#2308][pr-2308] (see [Relationship to existing proposals](#relationship-to-existing-proposals)).

### Create and policy API behavior

When `[egress] mode = "forward"`, the server returns HTTP 400 for:

- create requests with `networkPolicy` or `credentialProxy.enabled=true`;
- create requests whose sandbox env sets `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` or `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE`;
- `PUT`/`PATCH /sandboxes/{id}/networkpolicy`.

`DELETE` of policy rules stays allowed, matching the existing Fast Sandbox behavior. No public API schema changes. `validate_upstream_proxy_request` ([helpers][validate-upstream]) is extended for the forward posture; the Fast Sandbox rejection helper ([service][fsb-reject]) is reused.

### Egress attachment for every sandbox

Sidecar profile: today Docker and Kubernetes attach the egress sidecar only when `networkPolicy` is present. In forward mode the server constructs the container or Pod spec with the egress sidecar in-band on every create (for Kubernetes via [`apply_egress_to_spec`][k8s-egress-spec], never via an external mutating webhook whose `failurePolicy=Ignore` or timeout could admit an unattached Pod). The sidecar receives `OPENSANDBOX_EGRESS_MODE=forward`, `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`, `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH`, `OPENSANDBOX_EGRESS_SANDBOX_ID`, `OPENSANDBOX_EGRESS_DNS_UPSTREAM` (when `[egress] dns_upstreams` is configured), `OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE` (on Kubernetes, from [`_resolve_namespace()`][k8s-resolve-ns] in both single-tenant and multi-tenant modes; omitted on Docker), and `OPENSANDBOX_EGRESS_TENANT_NAME` (only when multi-tenancy is active, from `TenantEntry.name`). All three identity env vars are reserved server-injected variables rejected from caller `CreateSandboxRequest.env`. The sandbox container receives no egress-related env and no CA.

Enforcement before workload execution (R1): attaching egress as an ordinary peer container with a `readinessProbe` does not stop a Kubernetes workload container from starting and dialing out directly before capture rules exist, or while egress initialization is delayed or failing. Forward mode requires an enforced pre-workload barrier across every supported runtime:

- **Docker:** starts the egress container and waits for `/healthz` ([`_wait_for_egress_sidecar_ready`][docker-egress-ready]) before starting the sandbox container; failure aborts creation and cleans up the sidecar.
- **Kubernetes non-pooled:** the privileged init step that already runs before all Pod containers ([`prep_execd_init_for_egress`][k8s-execd-init]) installs a bootstrap default-deny nftables fence in the Pod netns (allowing loopback only, dropping all other outbound traffic) before any container in `spec.containers` starts. Egress atomically replaces that fence with the forward capture and containment tables once initialized. If egress initialization is delayed or fails, outbound networking stays blocked rather than failing open. (Clusters running Kubernetes ≥ 1.29 may additionally run egress as a native sidecar `initContainer` with `restartPolicy: Always` and a `startupProbe` on `/healthz`, on top of the bootstrap default-deny fence.)
- **Fast Sandbox:** `SET_BINDING` installs deny-first and capture redirects before workload execution (`sandbox.runtime-ready`).

Fast Sandbox profile: no server-side attachment change is needed. Every fsb sandbox already registers an egress subject at create; a policy-less sandbox seeds `DEFAULT_ALLOW_POLICY` because "the registration is the point" ([create mapping][fsb-create-mapping]), and FastPath rejects the create if the pool does not declare the `egress` handler ([fast-sandbox validation][fsb-validate]). In forward mode the server seeds the internal forward policy instead (see [Mode consistency check](#mode-consistency-check-for-fast-sandbox)).

### Egress data plane in forward mode

Startup: `OPENSANDBOX_EGRESS_MODE=forward` is parsed in both profiles before profile dispatch. It requires `OPENSANDBOX_EGRESS_UPSTREAM_PROXY` and `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH`, is incompatible with `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` and with Credential Vault, and does not start mitmproxy. Any missing or conflicting env fails process startup immediately — `OPENSANDBOX_EGRESS_MODE=forward` never falls back to `dns`-only mode (unlike legacy `parseMode()` token fallback ([main][egress-parse-mode])). Conversely, an older egress binary given `OPENSANDBOX_EGRESS_MODE=forward` alongside `OPENSANDBOX_EGRESS_UPSTREAM_PROXY` (without `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true`) already aborts startup in [`upstreamProxySpecForProfile`][egress-upstream-startup] before `parseMode()` is reached. A Go forwarder listens on a local port and sets a dedicated `SO_MARK` (distinct from the DNS proxy's existing `0x1` bypass mark and the Fast Sandbox allow mark) on every socket it opens to the gateway.

Capture and filtering (sidecar `dns+nft`):

1. DNS to the egress DNS proxy is redirected and accepted as today.
2. All other outbound TCP, except loopback and sockets carrying the forwarder mark, is redirected to the forwarder.
3. Only sockets carrying the forwarder mark may reach the gateway endpoint (proxy endpoint sets matched on the forwarder mark).
4. Link-local destinations (`169.254.0.0/16`, `fe80::/10`) are dropped before redirect.
5. Everything else is dropped (UDP, ICMP, IPv6 TCP in phase 1). Sandbox `networkPolicy` evaluation does not exist in this mode.

The exemption deliberately uses a socket mark rather than UID matching (`meta skuid`). Because the sandbox shares the sidecar's network namespace, a workload retaining `CAP_SETUID` could assume an arbitrary UID; setting `SO_MARK` instead requires `CAP_NET_ADMIN` or `CAP_NET_RAW`, which forward mode requires the sandbox container not to hold (see [Server configuration](#server-configuration)).

Forwarder, per accepted connection:

1. Read the original destination with `SO_ORIGINAL_DST`.
2. Dial the gateway (TLS with SNI and hostname verification for `https://`).
3. Send `CONNECT <ip>:<port> HTTP/1.1` with `Host: <ip>:<port>`, the configured `Proxy-Authorization`, `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace` (when present, i.e. Kubernetes and Fast Sandbox), and `OpenSandbox-Tenant-Name` (when present, i.e. multi-tenant deployments).
4. On a 2xx response, copy bytes in both directions until either side closes; on any other result or error, close the sandbox connection (RST). No retry to another destination, no direct dial.

DNS (R7): the DNS proxy applies no local domain policy for forward-mode sandboxes and forwards every query to its configured upstream resolver(s), still serving the gateway hostname through the existing infrastructure-domain path. When `OPENSANDBOX_EGRESS_DNS_UPSTREAM` is configured (via `[egress] dns_upstreams` in sidecar mode or the pool `FastletTemplate` in Fast Sandbox):

- Egress validates the upstream list at startup and **never** falls back to `/etc/resolv.conf` or `fallbackUpstream` (`8.8.8.8:53`, [dnsproxy][dnsproxy-fallback]).
- Policy denials from the configured resolver — specifically **`REFUSED`**, alongside `NOERROR` and `NXDOMAIN` — are treated as final authoritative responses and returned directly to the sandbox without failing over to another upstream (changing [`shouldFailoverAfterResponse`][dnsproxy-failover], which currently retries on `REFUSED`).
- If all configured upstreams are unreachable or time out, egress returns `SERVFAIL` (fail closed).

### Fast Sandbox profile

The shared egress is configured per pool through the FastletTemplate env (`OPENSANDBOX_EGRESS_MODE=forward` alongside `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`, `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH`, and optionally `OPENSANDBOX_EGRESS_DNS_UPSTREAM`). In forward mode:

- `OnRegistered` ([server][fsb-onregistered]) installs a per-subject TCP redirect to the shared forwarder, modeled on the existing `setMitmRedirect`, in addition to deny-first and the gateway DNS redirect. Registration fails closed if any step fails, as today.
- A subject whose policy is `defaultAction = "forward"` never receives the per-subject accept mark that `allow` subjects get today ([fsb nft][fsb-nft]). The shared master chain gets a forward-mode rule set ahead of every per-subject jump: accept redirected TCP and DNS to the gateway resolver; everything else falls through to the existing master-chain tail drop, so UDP and ICMP cannot egress directly.
- The forwarder resolves the subject by source IP (existing subject registry) to fill `OpenSandbox-Sandbox-Id` (from `SET_BINDING` `SandboxRef.Name`), `OpenSandbox-Sandbox-Namespace` (from `SET_BINDING` `SandboxRef.Namespace`, already populated by the Fastlet on every binding ([fastlet action][fsb-action-ns]); forward mode requires both fields to be non-empty), and `OpenSandbox-Tenant-Name` (from the subject's persisted `"tenantName"` when multi-tenancy is active).
- **Source-IP anti-spoofing and isolation (R8).** Sandboxes do not share the egress network namespace in this profile, so a guest cannot assume the forwarder's identity, and gateway containment remains the existing profile-wide endpoint drop for sandbox sources ([fsb upstream][fsb-nft-upstream]). Because `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, and `OpenSandbox-Tenant-Name` are resolved from the connection's source IP at the shared bridge (where `iifname` cannot distinguish slots under `bridge-nf-call-iptables=1`, see [fsb nft][fsb-nft-dispatch]), forward mode requires source-IP unforgeability at each slot boundary: container-based subjects must hold neither `CAP_NET_ADMIN` nor `CAP_NET_RAW` in their slot network namespace, and microVM subjects behind a slot network namespace must have egress source anti-spoofing enforced inside the slot namespace so only the assigned slot IP can exit toward the bridge.
- **Policy DNS scope (R7).** The shared DNS proxy forwards every query for `forward` subjects to the pool's configured upstream resolver(s) with the same fail-closed `REFUSED`/outage semantics as the sidecar profile. Because ordinary forwarded DNS packets do not carry per-sandbox identity, an external policy resolver enforces **pool-wide** DNS policy in a shared pool. Under OSEP-0014 multi-tenancy, each tenant's `SandboxPool` lives in its `tenant.namespace`, so per-tenant pools naturally configure distinct tenant policy resolvers via `OPENSANDBOX_EGRESS_DNS_UPSTREAM`; per-sandbox identity inside DNS packets (such as custom EDNS0 options or DoH headers) is out of scope for phase 1.

### Mode consistency check for Fast Sandbox

The server cannot observe a pool's egress configuration: FastPath `PoolInfo` exposes resources, revisions and component ports only, and a pool-level check would miss Fastlets still on an older template during rollout. The check therefore runs in egress, per binding, at enforcement time.

The marker must survive every path by which a Fast Sandbox egress obtains a subject's policy, not only the binding input. Egress persists each subject's policy record, and on `SET_BINDING` replay a record whose fence matches supersedes the replayed input ([`rehydratePolicyReload`][fsb-policy-reload], [`applyBindingPolicyLocked`][fsb-policy-apply]); `POLICY_IMPORT` restores the same record from a snapshot bundle. A marker that lives outside the policy body (for example a versioned envelope around a default-allow policy) would be stripped on persistence, and an older egress replaying or importing the stored default-allow record would activate it without any forward-mode capture or drop rules.

The marker is therefore part of the policy itself, as an internal `defaultAction` value, with a flat optional `"tenantName"` field when multi-tenancy is enabled:

```json
{"defaultAction": "forward", "egress": [], "tenantName": "acme"}
```

(In single-tenant deployments, `"tenantName"` is omitted: `{"defaultAction": "forward", "egress": []}`. `OpenSandbox-Sandbox-Namespace` comes directly from `SET_BINDING` `SandboxRef.Namespace` and does not need to be duplicated in the policy JSON.)

- With `[egress] mode = "forward"`, the server sends this internal policy as the `egress` binding input. Persisting `"tenantName"` inside the policy record keeps the server-authenticated tenant name intact across egress restarts (`rehydratePolicyReload`) and pause/resume snapshots (`POLICY_EXPORT`/`POLICY_IMPORT`) without changing `fast-sandbox` CRD or `SET_BINDING` wire schemas. With `dns` or `dns+nft`, the server keeps sending today's policy JSON (R10).
- `forward` is internal to the server–egress binding and the egress-held policy record. It is not a public `networkPolicy` value: the lifecycle API schema stays `allow | deny`, and forward mode rejects `networkPolicy` anyway.
- A forward-mode egress requires `defaultAction = "forward"` with an empty rule list (and a valid `"tenantName"`, if present) on every policy source (binding input, persisted record, snapshot bundle, relay write) and rejects anything else. An egress not in forward mode rejects `forward` on every source.
- An older egress rejects `forward` everywhere, because policy parsing ignores unknown fields (`tenantName`) and accepts only `allow` and `deny` ([policy parsing][policy-parse]): the binding input fails validation before any side effect (HTTP 400); a persisted record with `forward` is quarantined and the subject degrades to default-deny; a snapshot bundle with `forward` is rejected on import. All three are fail-closed.
- The check runs before any side effect. A rejected binding leaves the subject unregistered or deny-first; unmarked traffic is dropped by the master-chain tail, and the binding reports `FAILED`, which the server maps to `Failed` once the data plane settles ([status mapping][fsb-status]). Because the Fastlet replays `SET_BINDING` after every egress restart, a pool reconfigured or rolled back after creation is detected on replay.

The sidecar profile needs no runtime handshake once attached: the server constructs the container/Pod spec directly with both the sidecar and its env (R1), so the modes agree by construction. The sidecar egress `/policy` API rejects `forward` from callers, and a forward-mode sidecar rejects policy updates.

### Tunnel authentication and sandbox identity

- **Channel trust boundaries.** Three hops participate in forward mode:
  1. *Server → egress configuration delivery* (sidecar container spec or Fastlet Pod template): authenticated by the container runtime or Kubernetes API server; sandbox workloads cannot read the egress container spec or share its process namespace.
  2. *Fastlet → shared egress management hop* (Fast Sandbox profile, Pod-loopback HTTP `:18080` carrying `SET_BINDING`, lifecycle hooks, and the OSEP-0022 relay): inherited from OSEP-0022, where the trust boundary is Pod-netns membership — sandbox slots live in separate network namespaces and cannot reach Pod loopback, while co-located containers in the Fastlet Pod share the netns without peer authentication on `:18080`. Hardening this management hop (for example via a mount-scoped Unix domain socket) is out of scope here and tracked in [fast-sandbox#208][fsb-issue-208].
  3. *Forwarder → gateway tunnel*: authenticated on every `CONNECT` via `Proxy-Authorization` (R11, or mTLS in a future profile), unreachable from sandboxes (R8).
- **Authentication (R11).** Forward mode requires `upstream_proxy.authorization`; egress sends it as `Proxy-Authorization` on every `CONNECT`. A gateway listener dedicated to forward mode must reject unauthenticated tunnels. mTLS client authentication of egress is a compatible future option.
- **Who may assert identity.** In the first phase the credential is platform-wide: any tunnel authenticated with it may assert any identity header. Identity integrity therefore rests on credential confinement and on R8. The credential is delivered only to egress (sidecar env or Fastlet template), never to the sandbox container, and the sandbox cannot share the egress process namespace. A leaked credential allows impersonation of any sandbox, namespace, or tenant identity until it is rotated; per-sandbox credentials or per-sandbox mTLS client certificates (as in Substrate) would bind identity cryptographically and are left to follow-up review.
- **Closed set of platform-truth identity headers (no open key-value map).** Forward mode deliberately uses three fixed, flat, server-derived fields rather than a generic `map[string]string` or caller-controlled metadata bag, so caller input can never enter or spoof a policy selector:
  - `OpenSandbox-Sandbox-Id` — always sent (sidecar: `OPENSANDBOX_EGRESS_SANDBOX_ID`; Fast Sandbox: `SET_BINDING` `SandboxRef.Name`).
  - `OpenSandbox-Sandbox-Namespace` — sent on Kubernetes and Fast Sandbox in **both** single-tenant and multi-tenant modes (sidecar: `OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE` from `_resolve_namespace()`; Fast Sandbox: `SET_BINDING` `SandboxRef.Namespace`), enabling external namespace-scoped gateway policies even in single-tenant clusters; omitted on Docker (no namespace concept).
  - `OpenSandbox-Tenant-Name` — sent **only** when OSEP-0014 multi-tenancy is enabled (sidecar: `OPENSANDBOX_EGRESS_TENANT_NAME` from `TenantEntry.name` [tenant model][tenant-model]; Fast Sandbox: flat persisted `"tenantName"` field). Keeping `OpenSandbox-Tenant-Name` and `OpenSandbox-Sandbox-Namespace` separate handles `FileTenantProvider` deployments where `tenant.name != tenant.namespace` or multiple tenants share a namespace, without promoting tenant to a separate nested entity in the egress wire model.
  - All three sidecar env vars are server-reserved and rejected if supplied in `CreateSandboxRequest.env`. Both server and egress validate every non-empty value as printable ASCII without control characters or whitespace (`0x21–0x7E`, no CR/LF/NUL); invalid values fail closed (HTTP 400 or egress startup/binding failure) rather than emitting malformed headers or silently dropping context.
- **Identity scope.** These three headers are added only by the forward-mode forwarder; the `dns+nft` MITM-chained path does not send them. They carry platform-assigned sandbox, namespace, and tenant identity and do not identify or bind a runtime incarnation (restart, pause/resume, snapshot restore, or replacement may keep the same ID). An integration that needs incarnation binding, such as the per-incarnation credential selection in [#2308][pr-2308], must define its own mapping rather than treat these headers as equivalent.

### Gateway contract

OpenSandbox stays gateway-neutral. A conforming gateway must:

1. Accept HTTP/1.1 `CONNECT` with an `IP:port` target and treat the established tunnel as an opaque byte stream.
2. Authenticate the tunnel (`Proxy-Authorization`, or mTLS in a future profile), reject unauthenticated tunnels on the forward-mode listener, and treat `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, and `OpenSandbox-Tenant-Name` as trusted only on authenticated tunnels.
3. When authorizing by hostname (SNI or Host), connect to the authorized hostname, never to the CONNECT IP on the basis of a client-supplied name.
4. When no origin hostname is available (TLS without SNI, ECH where the inner name is hidden, or opaque TCP), either admit the connection under an explicitly authorized `IP:port` rule or deny it. Hostname-based rules must not match such connections, and an ECH outer/public name must never be treated as the origin hostname.
5. Restrict its own egress from private and link-local ranges.

Forward mode always sends an IP-literal `CONNECT` target. A gateway that also serves clients whose admission rejects IP-literal authorities (for example the reference profile proposed in [#2308][pr-2308]) needs distinct admission behavior selected through trusted configuration or identity, for example separate listeners or separate credentials.

With agentgateway, this corresponds to a bind with `tunnelProtocol: connect` re-entering an internal bind that sniffs SNI, then TLS passthrough (`protocol: TLS` with a dynamic backend) or termination (`protocol: HTTPS`, `tls.mode: dynamicCa`) per hostname; see its [traffic egress proxy examples][ag-egress-example]. The proposal does not depend on agentgateway-specific features.

### Observability

- Egress `/healthz` (both profiles) and the Fast Sandbox actions status expose `egressMode` and gateway reachability, next to the existing `mitmCaFingerprint` precedent.
- Forward mode's redirect and containment nftables tables participate in per-sandbox enforcement-state readback and drift reconciliation once enforcement-state anchors ([#2718][issue-2718], [#2740][pr-2740]) land, providing positive per-sandbox proof that capture is installed and current beyond listener readiness.
- Forwarder logs and OTel metrics: tunnel attempts, CONNECT status codes, setup latency, failures by reason. Credentials are never logged.
- Mode-mismatch rejections log the expected and actual mode and the subject.

### Relationship to existing proposals

- **OSEP-0001 (FQDN egress) and `networkPolicy`:** not applied in forward mode; the gateway owns destination policy.
- **OSEP-0012 and OSEP-0023 (Credential Vault, credential-bound interception):** mutually exclusive with forward mode; `[egress] mode = "dns+nft"` (with or without `[egress.upstream_proxy]`) remains the path for local credential injection.
- **OSEP-0014 (multi-tenancy):** on Kubernetes and Fast Sandbox, forward mode always sends `OpenSandbox-Sandbox-Namespace` (covering both single-tenant and multi-tenant namespace-scoped gateway policies), and when multi-tenancy is enabled additionally propagates the server-authenticated `TenantEntry.name` as `OpenSandbox-Tenant-Name` so gateways can compose tenant-level, namespace-level, and sandbox-level policies.
- **OSEP-0022 (multi-sandbox egress control plane):** forward mode reuses subject registration, deny-first, binding replay and the fail-closed lifecycle unchanged; it adds one redirect kind and one internal policy value (`forward`) with an optional flat persisted `"tenantName"`.
- **Proposed enforcement-state anchors ([#2740][pr-2740]):** composable. Forward mode introduces redirect and containment tables of its own and can enroll them in the anchored table set so kernel-rule drift or missing capture is surfaced by readback rather than silently tolerated.
- **Proposed Envoy sidecar option ([#1221][pr-1221]):** orthogonal. #1221 places a per-sandbox Envoy sidecar inside the pod for local L7 proxy handling, whereas forward mode keeps the sandbox-side forwarder a minimal L4 byte relay and places L7 policy, governance, and credential handling in a centralized external gateway (an external Envoy deployment speaking HTTP `CONNECT` can serve as a conforming forward-mode gateway).
- **Proposed managed egress integration ([#2308][pr-2308]):** complementary, and the two can progress independently. That proposal manages durable intent and rotating gateway credentials on top of the MITM chain and keeps local denies in front of every CONNECT; forward mode hands destination policy to the gateway.
  - *Compatibility boundary.* #2308's initial managed-MITM profile is not supported in forward mode. Reusing a rotating credential source would not carry over that profile's local-deny guarantees, so any combination requires a separately reviewed compatibility profile; until then the combination is rejected at configuration validation.
  - *Shared enrollment.* Both proposals need sidecar egress attachment when `networkPolicy` is absent. The enrollment hook may be shared while validation stays profile-specific (forward mode rejects a supplied `networkPolicy`; #2308 requires it to match the administrator-selected policy). This is an implementation coordination point, not a dependency.
  - *Credentials and identity.* A rotating credential source defined there could later feed the forwarder's `Proxy-Authorization` under such a profile. `OpenSandbox-Sandbox-Id` does not bind an incarnation (see [Tunnel authentication and sandbox identity](#tunnel-authentication-and-sandbox-identity)).

## Test Plan

Unit tests:

- Server config: `[egress] mode` defaults to `dns`; forward-mode validation (`upstream_proxy.url` and `authorization` required, `dns_upstreams` literal IP validation, Docker host network, gVisor, pool mode, sandbox `NET_ADMIN`/`NET_RAW` and `shareProcessNamespace` rejected, managed-MITM combination rejected); non-forward `[egress.upstream_proxy]` still requires `mode = "dns+nft"`.
- Egress startup: `OPENSANDBOX_EGRESS_MODE=forward` without `UPSTREAM_PROXY` or `UPSTREAM_PROXY_AUTH`, or combined with `MITMPROXY_TRANSPARENT`, fails process startup immediately and never falls back to `dns`-only mode.
- Server request handling: 400 for `networkPolicy`, `credentialProxy`, MITM env, `PUT`/`PATCH` network policy in forward mode; unchanged behavior in `dns` and `dns+nft` modes.
- Sidecar attachment & startup barrier: Docker and Kubernetes attach egress in-band without `networkPolicy` in forward mode, with the expected env (including `OPENSANDBOX_EGRESS_DNS_UPSTREAM` when configured, `OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE` on Kubernetes in both single-tenant and multi-tenant modes, and `OPENSANDBOX_EGRESS_TENANT_NAME` under multi-tenancy) and no sandbox-container env or CA; Kubernetes init spec installs the bootstrap default-deny fence before workload containers start; simulated attachment or sidecar startup failure fails sandbox creation/readiness rather than admitting an unprotected workload.
- Identity headers & validation: server emits `OpenSandbox-Sandbox-Namespace` on Kubernetes and Fast Sandbox in both single-tenant and multi-tenant modes (omitted on Docker) and emits `OpenSandbox-Tenant-Name` only in multi-tenant mode (including when `tenant.name != tenant.namespace`); caller attempts to pass `OPENSANDBOX_EGRESS_SANDBOX_ID`, `OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE`, or `OPENSANDBOX_EGRESS_TENANT_NAME` via `request.env`, or to influence headers via `request.metadata`, are rejected/ignored; both server and egress reject control characters or whitespace in identity values fail-closed.
- Fast Sandbox mapping: `{"defaultAction": "forward", "egress": []}` (plus flat `"tenantName"` when multi-tenant) only when `[egress] mode = "forward"`; today's policy otherwise.
- Egress mode check on every policy source (binding input, persisted record, snapshot bundle, relay write) for both directions: forward egress rejects `allow`/`deny`, non-forward egress rejects `forward`.
- Legacy fail-closed: the current release's policy parser rejects `forward` (with or without `"tenantName"`) in the binding input, and the current reload path quarantines a persisted `forward` record and degrades the subject to default-deny.
- Policy DNS fail-closed: when `OPENSANDBOX_EGRESS_DNS_UPSTREAM` is set, `REFUSED` is returned immediately without failing over to another upstream; unreachable upstreams return `SERVFAIL` and never fall back to `/etc/resolv.conf` or `8.8.8.8:53`.
- Forwarder builds the expected `CONNECT` request (including `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, and `OpenSandbox-Tenant-Name` when present) and closes the client on non-2xx, timeout or dial failure without any direct dial.

Integration tests (egress container with a test CONNECT proxy and origin):

- TCP on 443, 80 and a non-standard port is relayed with the original `IP:port`, `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace` (when configured), and `OpenSandbox-Tenant-Name` (when configured); TLS bytes arrive unmodified (origin certificate fingerprint preserved).
- UDP (including UDP/443), ICMP, link-local and IPv6 TCP are dropped.
- DNS: without `dns_upstreams`, arbitrary names resolve via the default upstream; with `dns_upstreams` pointed at a test policy DNS, allowed names resolve, `REFUSED` domains return `REFUSED` without escaping to a second resolver, and stopping the policy DNS yields `SERVFAIL` with no fallback. The gateway endpoint is unreachable from the sandbox.
- Pre-workload startup enforcement: workload entrypoint immediately attempts direct outbound TCP and UDP while egress initialization is artificially delayed or forced to fail; zero direct connections succeed, and failed initialization leaves networking permanently blocked.
- Forwarder impersonation (sidecar): a root sandbox process with `CAP_SETUID` that switches to the forwarder's UID is still redirected and cannot reach the gateway; without `CAP_NET_ADMIN`/`CAP_NET_RAW` it cannot set the forwarder mark.
- Source-IP spoofing (Fast Sandbox): packets crafted inside a sandbox or guest with another slot's or an arbitrary source IP are dropped or rejected before dispatch and never cause the forwarder to emit another subject's `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, or `OpenSandbox-Tenant-Name`.
- Only forward mode sends `OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, and `OpenSandbox-Tenant-Name`; the `dns+nft` MITM-chained path never does.
- Gateway down: connections fail, nothing reaches the origin directly.

E2E:

- Docker bridge and Kubernetes non-pooled sandboxes without `networkPolicy` against agentgateway in passthrough and `dynamicCa` termination modes (sandbox trusting the gateway CA through its image), including Kubernetes startup-race tests with delayed and failing egress sidecar initialization, and verifying `OpenSandbox-Sandbox-Namespace` arrives on single-tenant Kubernetes while `OpenSandbox-Tenant-Name` is absent.
- Multi-tenant Kubernetes and Fast Sandbox pools in forward mode: verify `OpenSandbox-Sandbox-Namespace` and `OpenSandbox-Tenant-Name` arrive on `CONNECT` (including `FileTenantProvider` with `name != namespace`) and persist across Fast Sandbox egress restart and pause/resume snapshot restore.
- Fast Sandbox pool in forward mode: sandbox egress relayed; server-forward with pool-not-forward and the reverse both yield `Failed` with no network; egress restart with a changed pool mode is detected on replay.
- Rollback and restore: with a persisted forward record, roll the pool's egress back to the previous release and restart it, and restore a forward-mode snapshot onto a previous-release Fastlet; in both cases the sandbox has no network and never activates an allow policy.
- Gateway conformance (reference agentgateway configuration): unauthenticated tunnels are rejected; TLS without SNI and opaque TCP are denied unless an `IP:port` rule authorizes them; hostname rules never match them.

## Drawbacks

- DNS remains an unrelayed channel unless the operator points `[egress] dns_upstreams` / `OPENSANDBOX_EGRESS_DNS_UPSTREAM` at a policy-enforcing resolver (exfiltration, reconnaissance); in a shared Fast Sandbox pool, external DNS policy is pool-wide rather than per-sandbox.
- Forward mode gives up OpenSandbox `networkPolicy` and the Credential Vault for the affected deployment; mixed use requires separate deployments or pools.
- The gateway becomes a hard dependency on the data path; its availability bounds sandbox egress availability.
- One tunnel per TCP connection adds setup latency and gateway connection load.
- Fast Sandbox mode is configured in two places (server and pool); the handshake makes mismatches visible and safe, not impossible.

## Alternatives

- **Keep the MITM chain and double-intercept at the gateway.** Functional with today's components, but decrypts every connection twice, keeps mitmproxy on the data path, and still requires `networkPolicy` per request.
- **Put `mode = "mitm" | "forward"` under `[egress.upstream_proxy]`.** Keeps the two proxy postures symmetric inside the sub-block, but `forward` alters subsystem-wide behavior (unconditional attachment, rejecting `networkPolicy`, unrestricted DNS, capture-only nftables), forces redundant configuration (`[egress] mode = "dns+nft"` alongside `upstream_proxy.mode = "forward"`), and adds a second posture env var alongside `OPENSANDBOX_EGRESS_MODE`. Elevating `forward` to `[egress] mode` matches the blast radius of the setting and keeps `[egress.upstream_proxy]` purely about the remote hop.
- **Add `defaultAction: forward` to the public `networkPolicy`.** Unifies semantics in the policy model, but the gateway endpoint and credentials are platform secrets that cannot be per-request, and a caller could opt out by submitting another policy; platform enforcement would still need server configuration, leaving a public value without independent value. The same token is used only internally, as the mode marker between server and egress.
- **Versioned binding envelope as the mode marker.** A wrapper such as `{"version": 2, "upstreamMode": "forward", "policy": {...}}` parses as default-deny on older egress, but the wrapped policy is what egress persists. On replay or snapshot restore the persisted record supersedes the binding input, so an older egress would activate a stored default-allow policy without forward-mode rules.
- **Use mitmproxy pass-through (`ignore_hosts`) with chaining.** Not possible: pass-through connections cannot be chained through a CONNECT proxy and the addon refuses this combination.
- **PROXY protocol or HBONE instead of CONNECT.** PROXY protocol cannot multiplex and the gateway-side TLV handling is limited; HBONE is tied to Istio ambient service discovery. HTTP `CONNECT` is standard, carries headers for identity, and is the transport Substrate uses.
- **Filter DNS by the gateway's policy (Substrate-style).** Requires syncing gateway policy into OpenSandbox, recreating the coupling this proposal removes; operator-configured fail-closed policy DNS (R7) lets deployments enforce DNS policy externally without coupling OpenSandbox to any gateway's policy schema.
- **Generic `attributes` key-value map or caller-supplied `metadata` label injection.** Any open-ended map creates a risk that caller-controlled input later enters the map and spoofs tenant or namespace headers, while also adding JSON env parsing, key-validation, and header-sizing complexity. Using a closed set of three flat platform-truth fields (`OpenSandbox-Sandbox-Id`, `OpenSandbox-Sandbox-Namespace`, `OpenSandbox-Tenant-Name`) keeps the wire contract explicit and prevents untrusted inputs by construction.
- **Server-side pre-check of the pool configuration.** FastPath does not expose egress configuration, and a pool-level check misses Fastlets on older templates during rollout and later reconfiguration.

## Infrastructure Needed

No new services. E2E coverage needs a CONNECT gateway container (agentgateway or a minimal CONNECT proxy) in the existing test environments.

## Upgrade & Migration Strategy

- Opt-in: existing `[egress]` (`dns`, `dns+nft`, or unset) and `[egress.upstream_proxy]` configurations keep today's behavior unchanged, and the binding input sent to Fast Sandbox pools is unchanged unless `[egress] mode = "forward"`.
- Enabling forward mode on Fast Sandbox: roll the pool's egress configuration (`OPENSANDBOX_EGRESS_MODE=forward` + proxy settings) and the server configuration together; during the window, new sandboxes on mismatched Fastlets fail closed and surface as `Failed`. This is a new feature with no existing users to migrate.
- Switching a deployment between modes affects new sandboxes; sidecar sandboxes keep the mode they were created with, Fast Sandbox subjects are re-checked on binding replay.
- Downgrade: switching `[egress] mode` away from `"forward"` returns new sandboxes to today's behavior. Existing forward-mode Fast Sandbox subjects keep their `forward` record; an egress not in forward mode, or an older egress, rejects it and the subject fails closed rather than falling back to direct egress. An older egress binary started with `OPENSANDBOX_EGRESS_MODE=forward` and `OPENSANDBOX_EGRESS_UPSTREAM_PROXY` (without `MITMPROXY_TRANSPARENT`) also aborts at startup.

## Questions for Review

1. Identity binding: per-sandbox credentials or per-sandbox mTLS client certificates in a later phase, replacing the platform-wide credential as the basis for asserting `OpenSandbox-Sandbox-Id`.
2. Capture scope: all TCP (proposed) versus a configurable port set.
3. HTTP/2 multiplexed `CONNECT` to reduce per-connection setup cost.
4. IPv6 relay in the sidecar profile.
5. Kubernetes pool mode support once per-allocation egress attachment exists.
6. A separate proposal for generic sandbox trust-bundle injection, needed when the gateway terminates TLS.

[upstream-guide]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/docs/guides/egress-upstream-proxy.md
[upstream-matrix]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/docs/guides/egress-upstream-proxy.md#runtime-compatibility-matrix
[upstream-addon]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/mitmscripts/upstream_proxy.py
[upstream-addon-load]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/mitmscripts/upstream_proxy.py#L131-L140
[upstream-config]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/config.py#L950
[egress-config]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/config.py#L1118
[validate-upstream]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/helpers.py#L320
[k8s-egress-spec]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/k8s/egress_helper.py#L85
[k8s-execd-init]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/k8s/egress_helper.py#L46
[k8s-resolve-ns]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/k8s/kubernetes_service.py#L207
[docker-egress-ready]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/docker/networking.py#L634
[dnsproxy-failover]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/dnsproxy/proxy.go#L504
[dnsproxy-fallback]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/dnsproxy/proxy.go#L633-L636
[tenant-model]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/tenants/models.py#L22
[egress-parse-mode]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/main.go#L291-L298
[egress-upstream-startup]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/upstream_proxy_config.go#L32-L50
[fsb-reject]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/fast_sandbox/service.py#L842
[fsb-create-mapping]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/fast_sandbox/create_mapping.py#L39-L42
[fsb-status]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/fast_sandbox/status_mapping.py
[fsb-onregistered]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/fastsandbox_server.go#L737
[fsb-nft]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/fastsandboxnft/fastsandboxnft.go#L27
[fsb-nft-dispatch]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/fastsandboxnft/fastsandboxnft.go#L36-L41
[fsb-nft-upstream]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/fastsandboxnft/upstream.go
[docker-caps]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/config.py#L1343-L1354
[policy-parse]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/policy/policy.go#L154-L161
[fsb-policy-reload]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/fastsandbox_policy_persist.go#L371-L468
[fsb-policy-apply]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/fastsandbox_policy_persist.go#L470-L498
[fsb-validate]: https://github.com/opensandbox-group/fast-sandbox/blob/3b8f5dd5e59c36673ea84ec019a1973f194f2463/api/v1alpha2/validation.go#L74-L89
[fsb-action-ns]: https://github.com/opensandbox-group/fast-sandbox/blob/3b8f5dd5e59c36673ea84ec019a1973f194f2463/internal/fastlet/action/manager.go#L1400
[fsb-issue-208]: https://github.com/opensandbox-group/fast-sandbox/issues/208
[substrate-egress]: https://github.com/agent-substrate/substrate/blob/94e0a26ac90151886a76410568aaac2664a54d90/docs/guides/network-egress.md
[ag-egress-example]: https://github.com/agentgateway/agentgateway/tree/v1.7.0/examples/traffic-egress-proxy
[pr-1221]: https://github.com/opensandbox-group/OpenSandbox/pull/1221
[pr-2308]: https://github.com/opensandbox-group/OpenSandbox/pull/2308
[issue-2718]: https://github.com/opensandbox-group/OpenSandbox/issues/2718
[pr-2740]: https://github.com/opensandbox-group/OpenSandbox/pull/2740
