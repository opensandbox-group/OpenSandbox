---
title: Egress Upstream Proxy Forward Mode
authors:
  - "@hittyt"
creation-date: 2026-11-13
last-updated: 2026-11-14
status: draft
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

Add `mode = "forward"` to `[egress.upstream_proxy]`. In forward mode, egress transparently captures every outbound TCP connection of a sandbox and relays the original bytes, without decryption, through a standard HTTP `CONNECT` tunnel to a centralized egress gateway such as [agentgateway](https://github.com/agentgateway/agentgateway). The gateway is the only policy authority: it decides per connection whether to pass TLS through, terminate and inspect it, inject credentials, or deny. OpenSandbox is reduced to a trusted, transparent, non-bypassable on-ramp: it applies to every sandbox (with or without `networkPolicy`, sidecar and Fast Sandbox profiles), drops all traffic that is not forwarded, and never falls back to direct egress. The existing behavior is preserved as the default `mode = "mitm"`.

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
6. Keep `mode = "mitm"` (today's behavior) the default and unchanged.

### Non-Goals

- Forwarding UDP, QUIC/HTTP3, or any non-TCP protocol. They are dropped, consistent with the current egress which does not relay them either.
- DNS policy. In forward mode the DNS proxy resolves every name; destination policy is the gateway's job.
- Combining forward mode with OpenSandbox `networkPolicy` or the Credential Vault (`credentialProxy`). They are mutually exclusive; L7 effects belong to the gateway.
- Injecting the gateway's interception CA into sandboxes. When the gateway terminates TLS, the sandbox must trust the gateway CA; delivering trust bundles to sandboxes is a generic capability orthogonal to this proposal and is left to a separate proposal.
- Translating OpenSandbox `networkPolicy` semantics into gateway configuration, or defining a gateway policy language.
- Kubernetes pool mode, gVisor, and Docker host networking in the first phase (rejected, see below).
- Durable management, credential rotation, or reconciliation of gateway authorization (see the proposed managed egress integration in [#2308][pr-2308]).

## Requirements

| ID | Requirement |
|---|---|
| R1 | With `mode = "forward"`, every sandbox created by the server has egress enforcement attached, regardless of `networkPolicy`. |
| R2 | Requests carrying `networkPolicy` (create, replace, patch) or `credentialProxy.enabled=true`, or attempting to enable transparent MITM via env, are rejected with HTTP 400. |
| R3 | All outbound TCP from the sandbox (except loopback and DNS to the egress DNS proxy) is relayed via `CONNECT <original-ip>:<original-port>` to the configured gateway, carrying the configured `Proxy-Authorization` and a server-assigned sandbox identity. |
| R4 | The relay does not parse or modify the relayed bytes. |
| R5 | No direct-dial fallback: if the tunnel cannot be established, the sandbox connection is closed. |
| R6 | Everything that is not relayed TCP or DNS to the egress DNS proxy is dropped, including UDP, ICMP and link-local destinations. |
| R7 | The DNS proxy resolves every name for forward-mode sandboxes. |
| R8 | The sandbox cannot reach the gateway endpoint directly, so it cannot forge tunnel metadata. |
| R9 | On Fast Sandbox, a mismatch between the server mode and the egress mode makes the sandbox fail closed (no network) and surface as `Failed`. This holds for every source of a subject's policy: the binding input, binding replays after egress restarts or rollbacks, the persisted policy record, and snapshot restore. |
| R10 | `mode = "mitm"` and an unset `mode` behave exactly as today, including the binding input format sent to existing Fast Sandbox pools. |

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
        v  (http:// or https:// to the gateway; only egress can reach it)
Centralized gateway (e.g. agentgateway)
        |  authenticate tunnel, read inner SNI / Host,
        |  pass through, terminate+inspect, inject credentials, or deny
        v
Origin
```

Configuration is one new field on the existing table:

```toml
[egress]
mode = "dns+nft"

[egress.upstream_proxy]
url = "https://gateway.example.com:3000"
mode = "forward"            # default "mitm": today's behavior
# authorization = "Bearer <token>"
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
- **DNS is the only unrelayed channel.** Resolving every name keeps policy in one place, but leaves DNS as a possible exfiltration and reconnaissance channel. Substrate filters DNS by its own policy because the runtime owns that policy; with an external gateway, OpenSandbox does not know the gateway policy, and syncing it back would recreate the coupling this proposal removes.
- **IPv6.** The sidecar transparent redirect is IPv4 `iptables` today. In the first phase IPv6 TCP is dropped rather than relayed.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Bypass through non-TCP or uncaptured traffic | R6: forward mode drops everything that is not relayed TCP or DNS to the egress DNS proxy; Fast Sandbox `forward` subjects never receive a per-subject accept mark. |
| Sandbox forges tunnel identity by dialing the gateway directly | R8: the gateway endpoint is reachable only by the egress forwarder identity (sidecar: existing UID-scoped proxy accept ([nft upstream][nft-upstream]); Fast Sandbox: existing profile-wide endpoint drop ([fsb upstream][fsb-nft-upstream])). |
| SNI or Host spoofing (claim an allowed name, dial an arbitrary IP) | Gateway contract: when authorizing by hostname, the gateway must route to the authorized hostname rather than to the CONNECT IP (same rule as Substrate's trust boundary). |
| Gateway outage silently opens egress | R5: no fallback; connections fail. Readiness reports tunnel health (see Observability). |
| Fast Sandbox pool not configured while server is in forward mode, so sandboxes egress directly | R9: the forward marker is part of the policy body, so every policy source (binding, persisted record, snapshot bundle) is checked; mismatch fails closed. |
| Egress rollback or snapshot restore onto an older Fastlet activates a stored allow policy | Forward subjects never store an allow policy; older egress rejects the stored `forward` value and degrades the subject to default-deny. |
| SSRF or metadata access through the gateway | Link-local is dropped before the gateway (R6); the gateway deployment must restrict private ranges on its own egress (documented gateway requirement). |
| Per-connection tunnel setup latency | One extra RTT to a nearby gateway per TCP connection; HTTP/2 multiplexed `CONNECT` is a possible follow-up. |

## Design Details

### Server configuration

`EgressUpstreamProxyConfig` ([config][upstream-config]) gains:

```python
mode: Literal["mitm", "forward"] = "mitm"
```

Validation when `mode = "forward"`: `url` is required; `[egress] mode` must be `dns+nft`; `[docker] network_mode = "host"` is rejected (no netns to capture); `secure_runtime.type = "gvisor"` is rejected (no nftables, same as today); Kubernetes pool mode is rejected because per-allocation egress cannot be attached. `authorization`, `ca_cert_path` and `ca_secret_name` keep their meaning: tunnel credential and trust for the gateway listener.

### Create and policy API behavior

When `mode = "forward"`, the server returns HTTP 400 for:

- create requests with `networkPolicy` or `credentialProxy.enabled=true`;
- create requests whose sandbox env sets `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` or `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE`;
- `PUT`/`PATCH /sandboxes/{id}/networkpolicy`.

`DELETE` of policy rules stays allowed, matching the existing Fast Sandbox behavior. No public API schema changes. `validate_upstream_proxy_request` ([helpers][validate-upstream]) is extended for the forward branch; the Fast Sandbox rejection helper ([service][fsb-reject]) is reused.

### Egress attachment for every sandbox

Sidecar profile: today Docker and Kubernetes attach the egress sidecar only when `networkPolicy` is present. In forward mode the sidecar is attached unconditionally, configured with `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`, `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH`, the server-injected `OPENSANDBOX_EGRESS_SANDBOX_ID`, and a new `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_MODE=forward`. The sandbox container receives no egress-related env and no CA.

Fast Sandbox profile: no server-side attachment change is needed. Every fsb sandbox already registers an egress subject at create; a policy-less sandbox seeds `DEFAULT_ALLOW_POLICY` because "the registration is the point" ([create mapping][fsb-create-mapping]), and FastPath rejects the create if the pool does not declare the `egress` handler ([fast-sandbox validation][fsb-validate]). In forward mode the server seeds the internal forward policy instead (see [Mode consistency check](#mode-consistency-check-for-fast-sandbox)).

### Egress data plane in forward mode

Startup: `OPENSANDBOX_EGRESS_UPSTREAM_PROXY_MODE=forward` requires `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`, is incompatible with `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` and with Credential Vault, and does not start mitmproxy. A Go forwarder listens on a local port as a dedicated UID.

Capture and filtering (sidecar `dns+nft`):

1. DNS to the egress DNS proxy is redirected and accepted as today.
2. All other outbound TCP, except loopback and traffic from the forwarder UID, is redirected to the forwarder.
3. The forwarder UID may reach only the gateway endpoint (existing UID-scoped proxy sets).
4. Link-local destinations (`169.254.0.0/16`, `fe80::/10`) are dropped before redirect.
5. Everything else is dropped (UDP, ICMP, IPv6 TCP in phase 1). Sandbox `networkPolicy` evaluation does not exist in this mode.

Forwarder, per accepted connection:

1. Read the original destination with `SO_ORIGINAL_DST`.
2. Dial the gateway (TLS with SNI and hostname verification for `https://`).
3. Send `CONNECT <ip>:<port> HTTP/1.1` with `Host: <ip>:<port>`, the configured `Proxy-Authorization`, and `OpenSandbox-Sandbox-Id`.
4. On a 2xx response, copy bytes in both directions until either side closes; on any other result or error, close the sandbox connection (RST). No retry to another destination, no direct dial.

DNS: the DNS proxy answers every query for forward-mode sandboxes (policy evaluation is bypassed), still serving the gateway hostname through the existing infrastructure-domain path.

### Fast Sandbox profile

The shared egress is configured per pool through the FastletTemplate env, as today for `OPENSANDBOX_EGRESS_UPSTREAM_PROXY`. In forward mode:

- `OnRegistered` ([server][fsb-onregistered]) installs a per-subject TCP redirect to the shared forwarder, modeled on the existing `setMitmRedirect`, in addition to deny-first and the gateway DNS redirect. Registration fails closed if any step fails, as today.
- A subject whose policy is `defaultAction = "forward"` never receives the per-subject accept mark that `allow` subjects get today ([fsb nft][fsb-nft]). The shared master chain gets a forward-mode rule set ahead of every per-subject jump: accept redirected TCP and DNS to the gateway resolver; everything else falls through to the existing master-chain tail drop, so UDP and ICMP cannot egress directly.
- The forwarder resolves the subject by source IP (existing subject registry) to fill `OpenSandbox-Sandbox-Id`.
- The DNS proxy resolves every name for `forward` subjects, which satisfies R7.

### Mode consistency check for Fast Sandbox

The server cannot observe a pool's egress configuration: FastPath `PoolInfo` exposes resources, revisions and component ports only, and a pool-level check would miss Fastlets still on an older template during rollout. The check therefore runs in egress, per binding, at enforcement time.

The marker must survive every path by which a Fast Sandbox egress obtains a subject's policy, not only the binding input. Egress persists each subject's policy record, and on `SET_BINDING` replay a record whose fence matches supersedes the replayed input ([`rehydratePolicyReload`][fsb-policy-reload], [`applyBindingPolicyLocked`][fsb-policy-apply]); `POLICY_IMPORT` restores the same record from a snapshot bundle. A marker that lives outside the policy body (for example a versioned envelope around a default-allow policy) would be stripped on persistence, and an older egress replaying or importing the stored default-allow record would activate it without any forward-mode capture or drop rules.

The marker is therefore part of the policy itself, as an internal `defaultAction` value:

```json
{"defaultAction": "forward", "egress": []}
```

- With `mode = "forward"`, the server sends exactly this policy as the `egress` binding input. With `mode = "mitm"` or no upstream proxy, the server keeps sending today's policy JSON (R10).
- `forward` is internal to the server–egress binding and the egress-held policy record. It is not a public `networkPolicy` value: the lifecycle API schema stays `allow | deny`, and forward mode rejects `networkPolicy` anyway.
- A forward-mode egress requires `defaultAction = "forward"` with an empty rule list on every policy source (binding input, persisted record, snapshot bundle, relay write) and rejects anything else. An egress not in forward mode rejects `forward` on every source.
- An older egress rejects `forward` everywhere, because policy parsing accepts only `allow` and `deny` ([policy parsing][policy-parse]): the binding input fails validation before any side effect (HTTP 400); a persisted record with `forward` is quarantined and the subject degrades to default-deny; a snapshot bundle with `forward` is rejected on import. All three are fail-closed.
- The check runs before any side effect. A rejected binding leaves the subject unregistered or deny-first; unmarked traffic is dropped by the master-chain tail, and the binding reports `FAILED`, which the server maps to `Failed` once the data plane settles ([status mapping][fsb-status]). Because the Fastlet replays `SET_BINDING` after every egress restart, a pool reconfigured or rolled back after creation is detected on replay.

The sidecar profile needs no handshake: the server generates the sidecar env, so the modes agree by construction. The sidecar egress `/policy` API rejects `forward` from callers, and a forward-mode sidecar rejects policy updates.

### Gateway contract

OpenSandbox stays gateway-neutral. A conforming gateway must:

1. Accept HTTP/1.1 `CONNECT` with an `IP:port` target and treat the established tunnel as an opaque byte stream.
2. Authenticate the tunnel (`Proxy-Authorization` or mTLS) and treat `OpenSandbox-Sandbox-Id` as trusted only on authenticated tunnels.
3. When authorizing by hostname (SNI or Host), connect to the authorized hostname, never to the CONNECT IP on the basis of a client-supplied name.
4. Restrict its own egress from private and link-local ranges.

With agentgateway, this corresponds to a bind with `tunnelProtocol: connect` re-entering an internal bind that sniffs SNI, then TLS passthrough (`protocol: TLS` with a dynamic backend) or termination (`protocol: HTTPS`, `tls.mode: dynamicCa`) per hostname; see its [traffic egress proxy examples][ag-egress-example]. The proposal does not depend on agentgateway-specific features.

### Observability

- Egress `/healthz` (both profiles) and the Fast Sandbox actions status expose `upstreamMode` and gateway reachability, next to the existing `mitmCaFingerprint` precedent.
- Forwarder logs and OTel metrics: tunnel attempts, CONNECT status codes, setup latency, failures by reason. Credentials are never logged.
- Mode-mismatch rejections log the expected and actual mode and the subject.

### Relationship to existing proposals

- **OSEP-0001 (FQDN egress) and `networkPolicy`:** not applied in forward mode; the gateway owns destination policy.
- **OSEP-0012 and OSEP-0023 (Credential Vault, credential-bound interception):** mutually exclusive with forward mode; `mode = "mitm"` remains the path for local credential injection.
- **OSEP-0022 (multi-sandbox egress control plane):** forward mode reuses subject registration, deny-first, binding replay and the fail-closed lifecycle unchanged; it adds one redirect kind and one internal policy value (`forward`).
- **Proposed managed egress integration ([#2308][pr-2308]):** complementary. That proposal manages durable intent and rotating gateway credentials on top of the MITM chain and keeps local denies in front of every CONNECT. Forward mode instead hands destination policy to the gateway. A rotating credential source defined there could later feed the forwarder's `Proxy-Authorization`.

## Test Plan

Unit tests:

- Server config: `mode` defaults to `mitm`; forward-mode validation (`dns+nft`, Docker host network, gVisor, pool mode).
- Server request handling: 400 for `networkPolicy`, `credentialProxy`, MITM env, `PUT`/`PATCH` network policy in forward mode; unchanged behavior in `mitm` mode.
- Sidecar attachment: Docker and Kubernetes attach egress without `networkPolicy` in forward mode, with the expected env and no sandbox-container env or CA.
- Fast Sandbox mapping: `{"defaultAction": "forward", "egress": []}` only in forward mode; today's policy otherwise.
- Egress mode check on every policy source (binding input, persisted record, snapshot bundle, relay write) for both directions: forward egress rejects `allow`/`deny`, non-forward egress rejects `forward`.
- Legacy fail-closed: the current release's policy parser rejects `forward` in the binding input, and the current reload path quarantines a persisted `forward` record and degrades the subject to default-deny.
- Forwarder builds the expected `CONNECT` request and closes the client on non-2xx, timeout or dial failure without any direct dial.

Integration tests (egress container with a test CONNECT proxy and origin):

- TCP on 443, 80 and a non-standard port is relayed with the original `IP:port` and identity header; TLS bytes arrive unmodified (origin certificate fingerprint preserved).
- UDP (including UDP/443), ICMP, link-local and IPv6 TCP are dropped.
- DNS resolves arbitrary names; the gateway endpoint is unreachable from the sandbox UID.
- Gateway down: connections fail, nothing reaches the origin directly.

E2E:

- Docker bridge and Kubernetes non-pooled sandboxes without `networkPolicy` against agentgateway in passthrough and `dynamicCa` termination modes (sandbox trusting the gateway CA through its image).
- Fast Sandbox pool in forward mode: sandbox egress relayed; server-forward with pool-not-forward and the reverse both yield `Failed` with no network; egress restart with a changed pool mode is detected on replay.
- Rollback and restore: with a persisted forward record, roll the pool's egress back to the previous release and restart it, and restore a forward-mode snapshot onto a previous-release Fastlet; in both cases the sandbox has no network and never activates an allow policy.

## Drawbacks

- DNS remains an unrelayed channel (exfiltration, reconnaissance).
- Forward mode gives up OpenSandbox `networkPolicy` and the Credential Vault for the affected deployment; mixed use requires separate deployments or pools.
- The gateway becomes a hard dependency on the data path; its availability bounds sandbox egress availability.
- One tunnel per TCP connection adds setup latency and gateway connection load.
- Fast Sandbox mode is configured in two places (server and pool); the handshake makes mismatches visible and safe, not impossible.

## Alternatives

- **Keep the MITM chain and double-intercept at the gateway.** Functional with today's components, but decrypts every connection twice, keeps mitmproxy on the data path, and still requires `networkPolicy` per request.
- **Add `defaultAction: forward` to the public `networkPolicy`.** Unifies semantics in the policy model, but the gateway endpoint and credentials are platform secrets that cannot be per-request, and a caller could opt out by submitting another policy; platform enforcement would still need server configuration, leaving a public value without independent value. The same token is used only internally, as the mode marker between server and egress.
- **Versioned binding envelope as the mode marker.** A wrapper such as `{"version": 2, "upstreamMode": "forward", "policy": {...}}` parses as default-deny on older egress, but the wrapped policy is what egress persists. On replay or snapshot restore the persisted record supersedes the binding input, so an older egress would activate a stored default-allow policy without forward-mode rules.
- **Use mitmproxy pass-through (`ignore_hosts`) with chaining.** Not possible: pass-through connections cannot be chained through a CONNECT proxy and the addon refuses this combination.
- **PROXY protocol or HBONE instead of CONNECT.** PROXY protocol cannot multiplex and the gateway-side TLV handling is limited; HBONE is tied to Istio ambient service discovery. HTTP `CONNECT` is standard, carries headers for identity, and is the transport Substrate uses.
- **Filter DNS by the gateway's policy (Substrate-style).** Requires syncing gateway policy into OpenSandbox, recreating the coupling this proposal removes.
- **Server-side pre-check of the pool configuration.** FastPath does not expose egress configuration, and a pool-level check misses Fastlets on older templates during rollout and later reconfiguration.

## Infrastructure Needed

No new services. E2E coverage needs a CONNECT gateway container (agentgateway or a minimal CONNECT proxy) in the existing test environments.

## Upgrade & Migration Strategy

- Opt-in: existing `[egress.upstream_proxy]` configurations without `mode` keep today's behavior, and the binding input sent to Fast Sandbox pools is unchanged unless `mode = "forward"`.
- Enabling forward mode on Fast Sandbox: roll the pool's egress configuration and the server configuration together; during the window, new sandboxes on mismatched Fastlets fail closed and surface as `Failed`. This is a new feature with no existing users to migrate.
- Switching a deployment between modes affects new sandboxes; sidecar sandboxes keep the mode they were created with, Fast Sandbox subjects are re-checked on binding replay.
- Downgrade: removing `mode = "forward"` returns new sandboxes to today's behavior. Existing forward-mode Fast Sandbox subjects keep their `forward` record; an egress not in forward mode, or an older egress, rejects it and the subject fails closed rather than falling back to direct egress.

## Questions for Review

1. Identity header name and form: plain `OpenSandbox-Sandbox-Id` on an authenticated tunnel, or a signed token / per-sandbox mTLS client certificate in a later phase.
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
[validate-upstream]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/helpers.py#L320
[fsb-reject]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/fast_sandbox/service.py#L842
[fsb-create-mapping]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/fast_sandbox/create_mapping.py#L39-L42
[fsb-status]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/server/opensandbox_server/services/fast_sandbox/status_mapping.py
[fsb-onregistered]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/fastsandbox_server.go#L737
[fsb-nft]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/fastsandboxnft/fastsandboxnft.go#L27
[fsb-nft-upstream]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/fastsandboxnft/upstream.go
[nft-upstream]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/nftables/upstream.go#L67-L68
[policy-parse]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/pkg/policy/policy.go#L154-L161
[fsb-policy-reload]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/fastsandbox_policy_persist.go#L371-L468
[fsb-policy-apply]: https://github.com/opensandbox-group/OpenSandbox/blob/058ac8a599a963099b38ec9b144d8505fed4311e/components/egress/fastsandbox_policy_persist.go#L470-L498
[fsb-validate]: https://github.com/opensandbox-group/fast-sandbox/blob/3b8f5dd5e59c36673ea84ec019a1973f194f2463/api/v1alpha2/validation.go#L74-L89
[substrate-egress]: https://github.com/agent-substrate/substrate/blob/94e0a26ac90151886a76410568aaac2664a54d90/docs/guides/network-egress.md
[ag-egress-example]: https://github.com/agentgateway/agentgateway/tree/v1.7.0/examples/traffic-egress-proxy
[pr-2308]: https://github.com/opensandbox-group/OpenSandbox/pull/2308
