---
title: Egress Chained Upstream Proxy Operations
description: Configure, operate, verify, and troubleshoot chained upstream HTTP CONNECT proxies in OpenSandbox.
---

# Egress Chained Upstream Proxy Operations

OpenSandbox provides an administrator-managed chained upstream proxy capability for sandboxes running with egress network policies. This guide explains how to configure, operate, verify, and roll back an upstream forward proxy next hop across Docker and Kubernetes environments.

## Overview and prerequisites

The chained upstream proxy routes egress traffic through an external HTTP/HTTPS forward proxy via standard HTTP CONNECT tunnels. The configuration is lifecycle-server-wide and administrator-only; request environments cannot override proxy destinations, credentials, or custom CA bundles.

Prerequisites:
- Egress sidecar enabled by provisioning sandboxes with a `networkPolicy`.
- Server `[egress]` configuration set to `mode = "dns+nft"`.
- Transparent interception enabled via `credentialProxy.enabled=true` or `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true`.
- For Docker: `[docker] network_mode = "bridge"`.
- For Kubernetes: non-pooled AgentSandbox or BatchSandbox workloads.

::: warning
Transparent service-mesh sidecars (such as Envoy or Istio) injected into the same pod network namespace are unsupported because both layers intercept and rewrite outbound packets. Use the external chained upstream proxy hop instead.
:::

## Docker and Kubernetes configuration examples

Configure the `[egress.upstream_proxy]` section in the lifecycle server configuration. For the complete configuration schema and field definitions, refer to the [Server Configuration Reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#egress).

### Docker configuration

For a private proxy CA in Docker, `ca_cert_path` must reference an absolute POSIX path on the Docker daemon host. The file is bind-mounted read-only into the egress sidecar container only.

```toml
[egress]
image = "opensandbox/egress:<version>"
mode = "dns+nft"

[egress.upstream_proxy]
url = "https://proxy.example.com:8443"
# Optional complete Proxy-Authorization value:
# authorization = "Basic <base64>"
ca_cert_path = "/etc/ssl/certs/proxy-ca.pem"
```

### Kubernetes configuration

For a private proxy CA in Kubernetes, `ca_secret_name` references a Secret in the sandbox namespace. The Secret must contain the fixed key `ca.crt`, which is projected read-only into the egress sidecar container.

```toml
[egress]
image = "opensandbox/egress:<version>"
mode = "dns+nft"

[egress.upstream_proxy]
url = "https://proxy.example.com:8443"
# Optional complete Proxy-Authorization value:
# authorization = "Basic <base64>"
ca_secret_name = "upstream-proxy-ca"
```

## Traffic and security model

For a detailed overview of egress packet flows, see [/architecture/network/egress](/architecture/network/egress).

1. **Resolution & Routing**: Under `dns+nft`, proxy hostnames are resolved by infrastructure DNS even under a deny-all policy. Egress reachability is enforced by dedicated nftables sets scoped strictly to the mitmproxy UID, proxy IP, and proxy port, isolating proxy traffic from sandbox dynamic allow sets.
2. **Protocol & Next Hop**: Both `http://` and `https://` proxy endpoints are supported. For HTTPS endpoints, SNI and server hostname validation are enforced. Configured CA bundles augment the system trust store and apply to all mitmproxy upstream TLS verifications.
3. **CONNECT Authority**: Intercepted TLS traffic uses the SNI or Host-derived FQDN for the CONNECT authority when available, falling back to `IP:port`. Direct dials are strictly refused when chaining is enabled to prevent silent bypass.
4. **Credential Isolation**: Business credentials from Credential Vault are injected into the inner target request, while `authorization` is attached exclusively as `Proxy-Authorization` on the outer CONNECT request. Neither value is written to logs. For secret management details, see [/guides/credential-vault](/guides/credential-vault).
5. **Secret Exposure Bounds**: Proxy authorization values appear as literal environment variables inside the egress container and are visible via `docker inspect` or Kubernetes Pod specs. Restrict host and cluster inspection permissions accordingly. The authorization environment and CA mount are added only to the egress container, not the sandbox container.

## Runtime compatibility matrix

Compatibility reflects current upstream `main` only:

| Runtime / Platform | Support Status | Operational Constraints |
|---|---|---|
| Docker (`runc`) | Supported | Requires `[docker] network_mode = "bridge"`; use `ca_cert_path` only when a private CA is required. |
| Kubernetes (`runc`) | Supported | Non-pooled AgentSandbox and BatchSandbox workloads; use `ca_secret_name` only when a private CA is required. |
| Kubernetes (`Kata`) | Supported | Accepted by egress validation; validate the selected RuntimeClass, CNI, and kernel in your deployment. |
| gVisor (`secure_runtime.type = "gvisor"`) | Rejected | Sandbox creation returns HTTP 400 because gVisor does not support the nftables interface the egress sidecar uses for its redirect and policy rules. |
| Kubernetes Pool Mode | Unsupported | Rejects per-request `networkPolicy`; cannot attach per-allocation egress sidecars. |
| Fast Sandbox | Unsupported | Current fast-sandbox profile rejects `networkPolicy` when `upstream_proxy` is set. |

## Protocol and streaming behavior

| Protocol / Traffic Type | Supported | Behavior & Constraints |
|---|---|---|
| HTTP/1.1 and HTTP/2 application traffic | Yes | Mitmproxy can negotiate HTTP/2 with application targets. The upstream web-proxy hop uses HTTP/1 CONNECT; HTTP/2 CONNECT is not supported by mitmproxy 11.0.2. |
| Ports 80 & 443 | Yes | Always intercepted; Credential Vault binding applies to canonical ports 80/443. |
| Extra TCP Ports | Bounded | Intercepted when configured, but Credential Vault binding does not apply. |
| Streaming and large request bodies | Preserved with limits | Credential header injection occurs in `requestheaders` before body streaming. Body placeholder substitution is skipped for streamed bodies. |
| Server-Sent Events (SSE) | Caveat | Forced into streaming mode. Truncation can occur if origin closes with TCP RST; see [/guides/egress-sse-truncation](/guides/egress-sse-truncation). |
| TLS pass-through | No | Non-empty `ignore_hosts`, `tcp_hosts`, or `udp_hosts` makes the upstream addon fail to load; no-SNI pass-through is refused at request time. |
| UDP and QUIC | No | These flows cannot use HTTP CONNECT and are refused rather than sent directly. |
| WebSocket (WSS) | Unverified | Dedicated chained-proxy WSS compatibility is not independently verified. |
| IPv6 Egress | Incomplete | Incomplete Kubernetes dual-stack support; `egress.disable_ipv6` defaults to `true`. |

The default-off experimental OSEP-0023 revision runtime has a narrower live HTTP/2 admission boundary: only the fixed-dependency direct/lazy path is wired, and configured chained upstream proxies remain rejected for that path. Existing HTTP/1 and legacy proxy behavior in the matrix above is unchanged. See the [HTTP/2 integration boundary](/architecture/network/osep-0023-h2-integration) for drain semantics and incomplete client-migration and Linux acceptance.

## Deployment, rotation, and rollback

- **Application Bounds**: Proxy configuration changes apply only to newly created sandboxes. Existing sidecars retain their creation-time proxy settings.
- **Proxy or Authorization Rotation**: Update the lifecycle server configuration, restart the lifecycle server, then recreate affected sandboxes.
- **CA Rotation**: Update the Docker CA file or Kubernetes Secret. If its configured path or Secret name is unchanged, the lifecycle server does not need a restart, but affected sandboxes still need recreation. Recreating avoids relying on bind-mount or Secret `subPath` update behavior.
- **Rollback Procedure**: Remove the `[egress.upstream_proxy]` section, restart the lifecycle server, and recreate affected sandboxes. Existing sandboxes remain unchanged until they are recreated, and proxy failure never triggers an automatic direct fallback.

## Verification checklist

1. Confirm the lifecycle server starts with the new configuration; invalid URL, mode, and runtime-specific CA fields fail validation before sandbox creation.
2. Create a new sandbox with `networkPolicy` and transparent interception enabled.
3. Confirm the egress health endpoint returns `200 OK`; `503 mitmproxy not ready` means the interception process did not initialize.
4. Check startup logs for the `credential proxy: upstream proxy chaining enabled` message, without printing environment values.
5. From the sandbox, access an allowed HTTPS destination with normal certificate verification. Do not use `curl -k` or set `ssl_insecure`.
6. Confirm a denied destination stays denied and inspect the external proxy's telemetry for the expected FQDN CONNECT authority.
7. If Credential Vault is enabled, confirm the target receives the business credential while the forward proxy receives only `Proxy-Authorization`.

## Troubleshooting table

| Failure Category | Observed Symptoms | Root Cause & Resolution |
|---|---|---|
| Config-load error | Server fails to start; configuration parse errors | Invalid proxy URL, non-absolute Docker CA path, or mixing Docker and Kubernetes CA settings. |
| Request 400 error | Sandbox creation rejected with HTTP 400 | Transparent interception is missing, `secure_runtime.type = "gvisor"` is selected, or Pool/Fast Sandbox is combined with per-request `networkPolicy`. |
| Pod or bind setup failure | Kubernetes Pod stays pending, or Docker container creation fails | The Kubernetes Secret/key is missing, or the Docker daemon cannot read the configured host path. Correct the administrator-managed source and recreate the sandbox. |
| Sidecar startup failure | Egress container restarts or fails readiness | Custom pass-through settings are incompatible, or the mounted CA bundle is malformed or unreadable. |
| Sidecar 503 error | Egress health checks return HTTP 503 | Mitmproxy has not initialized. Inspect sidecar startup logs; runtime proxy outages affect requests but do not by themselves make the local listener unready. |
| TLS / SAN failure | Handshake failure; certificate validation errors | Upstream proxy certificate SAN does not match proxy hostname, or missing custom CA bundle. |
| Proxy auth failure | Requests through the proxy fail; proxy telemetry reports 407 | Supply the complete, valid `authorization` value in server configuration and recreate the sandbox. |
| Direct-dial error | Logs contain `upstream proxy required: direct egress dial refused` | Expected fail-closed behavior for a flow that cannot be chained; do not bypass it by disabling TLS verification. |
| SSE / stream truncation | Streaming connection drops prematurely | Origin closed connection with TCP RST while unread bytes remained; review upstream gateway keep-alive settings. |

## Known limits and references

- **No Per-Request Proxy**: Proxy settings cannot be modified per request or through environment variables.
- **No Hot Reload**: Existing sandbox sidecars cannot reload proxy configurations dynamically.
- **No Dedicated OTLP Metrics**: There is no dedicated chained-proxy metric family; rely on egress health checks, DNS/nftables metrics, and proxy logs.
- **Runtime Scope**: No production certification is claimed for every Kata implementation, custom RuntimeClass, CNI, or kernel combination.
- **References**:
  - [Egress Architecture Specification](/architecture/network/egress)
  - [Credential Vault Operations Guide](/guides/credential-vault)
  - [Server-Sent Events Truncation Guide](/guides/egress-sse-truncation)
  - [Server Configuration Reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#egress)
