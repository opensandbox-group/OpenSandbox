---
title: Egress
description: FQDN-based egress control sidecar for OpenSandbox providing DNS filtering, nftables enforcement, and credential injection.
---

# OpenSandbox Egress Sidecar

The **Egress** is a core component of OpenSandbox that provides **FQDN-based egress control**.

It runs alongside the sandbox application container (sharing the same network namespace) and enforces declared network policies.

## Features

- **FQDN-based Allowlist**: Control outbound traffic by domain name (e.g., `api.github.com`).
- **IP / CIDR Targets**: Egress rules can also target literal IP addresses or CIDR ranges (e.g., `10.0.0.0/8`).
- **Wildcard Support**: Allow subdomains using wildcards (e.g., `*.pypi.org`).
- **Transparent Interception**: Uses transparent DNS proxying; no application configuration required.
- **Experimental: Transparent HTTPS MITM (mitmproxy)**: Optional transparent TLS interception for outbound `80/443` traffic in the sidecar network namespace.
- **Dynamic DNS (dns+nft mode)**: When a domain is allowed and the proxy resolves it, the resolved A/AAAA IPs are added to nftables with TTL so that default-deny + domain-allow is enforced at the network layer.
- **Credential Vault**: Automatic credential injection (bearer, basic, API-key, custom headers) for allowed hosts via transparent mitmproxy. See [Credential Vault](/guides/credential-vault).
- **Privilege Isolation**: Requires `CAP_NET_ADMIN` only for the sidecar; the application container runs unprivileged.
- **Fail-Closed Enforcement**: DNS redirect setup is required through `iptables` or the native nft fallback; the sidecar exits if no enforced redirect can be installed. Optional subsystems (OpenTelemetry, startup hooks) degrade gracefully.

## Architecture

The egress control is implemented as a **Sidecar** that shares the network namespace with the sandbox application.

1.  **DNS Proxy (Layer 1)**:
    - Runs on `127.0.0.1:15353`.
    - `iptables` rules redirect all port 53 (DNS) traffic to this proxy.
    - Filters queries based on the allowlist.
    - Returns `NXDOMAIN` for denied domains.

2.  **Network Filter (Layer 2)** (when `OPENSANDBOX_EGRESS_MODE=dns+nft`):
    - Uses `nftables` to enforce IP-level allow/deny. Resolved IPs for allowed domains are added to dynamic allow sets with TTL (dynamic DNS).
    - At startup, the sidecar whitelists **127.0.0.1** (redirect target for the proxy) and **nameserver IPs** from `/etc/resolv.conf` so DNS resolution and proxy upstream work (including private DNS). Nameserver count is capped and invalid IPs are filtered.

### Kubernetes Service Access Under `defaultAction: deny`

In Kubernetes deployments that use `defaultAction: deny`, reaching an in-cluster Service usually needs two separate allowances:

- allow the Service DNS name so the DNS proxy resolves it
- allow the Service CIDR (or a narrower ClusterIP range) so `dns+nft` does not drop the TCP connection after resolution

Allowing only `postgres.opensandbox.svc.cluster.local` is not sufficient if the resolved ClusterIP still belongs to a denied range such as `10.96.0.0/12`. Likewise, allowing only the CIDR is not sufficient if the DNS proxy still denies the hostname.

See [Network Isolation](/architecture/network-isolation#allowing-legitimate-in-cluster-services) for operator guidance and examples.

## Requirements

- **Runtime**: Docker or Kubernetes.
- **Capabilities**: `CAP_NET_ADMIN` (for the sidecar container only).
- **Kernel**: Linux kernel with `iptables` support.
- **Service mesh**: OpenSandbox egress is not currently supported inside pods that already have a transparent service-mesh sidecar (for example Istio/Envoy injection). Both layers rewrite outbound traffic in the same network namespace and can conflict.

## Configuration

Most deployments only need these settings:

- **Mode**: `OPENSANDBOX_EGRESS_MODE`
  - `dns` (default): DNS filtering only
  - `dns+nft`: DNS + nftables IP/CIDR enforcement (recommended for strict default-deny)
- **Initial policy**:
  - `OPENSANDBOX_EGRESS_RULES` (JSON, same shape as `POST /policy`)
  - or `OPENSANDBOX_EGRESS_POLICY_FILE` (if valid file exists, it takes precedence at startup)
- **HTTP API**:
  - `OPENSANDBOX_EGRESS_HTTP_ADDR` (default `:18080`)
  - `OPENSANDBOX_EGRESS_TOKEN` (optional auth via `OPENSANDBOX-EGRESS-AUTH`)
- **Rule limit**:
  - `OPENSANDBOX_EGRESS_MAX_RULES` for `POST/PATCH /policy` (default `4096`, `0` disables cap)

Optional advanced features:

- Nameserver bypass: `OPENSANDBOX_EGRESS_NAMESERVER_EXEMPT`
- Denied hostname webhook: `OPENSANDBOX_EGRESS_DENY_WEBHOOK`, `OPENSANDBOX_EGRESS_SANDBOX_ID`
- DoH/DoT controls: `OPENSANDBOX_EGRESS_BLOCK_DOH_443`, `OPENSANDBOX_EGRESS_DOH_BLOCKLIST`
- Custom DNS upstream: `OPENSANDBOX_EGRESS_DNS_UPSTREAM` (comma-separated IPs, optional `:port`), `OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT` (default `5` seconds)
- DNS upstream health probe: `OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE` (enable), `OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE_INTERVAL_SEC`
- Credential vault: `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_TLS`, `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_TRUSTED_PROXY_CIDRS`, `OPENSANDBOX_CREDENTIAL_PROXY_SOCKET` (default `/run/opensandbox/credential-proxy/active.sock`)
- Metrics: `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` (extra key=value attributes for OTLP metrics and structured log fields)

### Always-Rules Files

Static rule files under `/var/egress/rules/` are loaded at startup and take priority over dynamic API rules:

| File | Purpose |
|------|---------|
| `/var/egress/rules/deny.always` | Domains always denied, overrides user and allow rules |
| `/var/egress/rules/allow.always` | Domains always allowed, overrides user rules |
| `/var/egress/rules/log_skip.always` | Domain patterns whose DNS blocks are not logged (noise reduction) |

Format: one domain per line (supports wildcards like `*.example.com`). Lines starting with `#` are comments. Missing files are silently ignored.

Rule precedence: `deny.always` > `allow.always` > user policy (API/env).

Always-rules are hot-reloaded: the sidecar polls the files once per minute and applies changes without restart.

### Service Mesh Compatibility

::: warning Not Supported with Transparent Mesh Sidecars
OpenSandbox egress is designed to be the only transparent outbound interception layer inside the sandbox pod. Deployments that automatically inject a service-mesh sidecar such as Istio/Envoy into the same pod are not currently supported for egress-sidecar features.
:::

Why this conflicts today:

- OpenSandbox egress installs `iptables`/`nft` redirect rules in the shared pod network namespace so DNS and optional HTTPS MITM traffic flow through the egress sidecar.
- Service meshes such as Istio also redirect outbound traffic in that same namespace, usually to Envoy.
- When both are present, the redirect order becomes deployment-dependent and can produce double interception, broken TLS, or traffic that bypasses the expected Credential Vault / egress-policy path.

This matters for:

- per-sandbox `networkPolicy` / `network_policy` enforcement
- transparent mitmproxy mode
- Credential Vault / Credential Proxy

Recommended operator choices today:

1. Exclude OpenSandbox sandbox pods from automatic mesh sidecar injection when they need the egress sidecar.
2. If mesh injection is mandatory, do not rely on the OpenSandbox egress sidecar for outbound control in those pods; instead use a platform-level mechanism such as a CNI/network-policy solution.
3. Treat mesh-injected sandboxes as a separate runtime profile and document that Credential Vault and transparent egress interception are unavailable there until first-class coexistence support is implemented.

See also [Credential Vault](/guides/credential-vault) and [Network Isolation](/architecture/network-isolation).

### Runtime HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/policy` | Get current policy and enforcement mode |
| `POST` | `/policy` | Replace policy (`{}`, `null`, empty body => reset to deny-all) |
| `PUT` | `/policy` | Alias for `POST` |
| `PATCH` | `/policy` | Merge/append rules (body is JSON array of egress rules) |
| `DELETE` | `/policy` | Remove specific targets (body is JSON string array, e.g. `["*.example.com"]`) |
| `GET/POST/PATCH/DELETE` | `/credential-vault` | Manage the credential vault (create, update, delete) |
| `GET` | `/credential-vault/credentials` | List credential metadata |
| `GET` | `/credential-vault/credentials/{name}` | Get single credential metadata |
| `GET` | `/credential-vault/bindings` | List binding metadata |
| `GET` | `/credential-vault/bindings/{name}` | Get single binding metadata |
| `GET` | `/healthz` | Health check; returns `200 ok` or `503 mitmproxy not ready` (when transparent MITM is enabled but not yet initialized) |

Quick example:

```bash
# Replace policy
curl -XPOST http://127.0.0.1:18080/policy \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}'

# Remove specific targets
curl -XDELETE http://127.0.0.1:18080/policy \
  -d '["*.example.com"]'
```

### Experimental: Transparent MITM (mitmproxy)

::: warning Experimental
APIs, environment variables, and behavior may change.
:::

Optional transparent HTTPS interception for outbound `80/443` traffic in the sidecar network namespace.

### Credential Vault

The credential vault provides automatic credential injection for outbound requests to allowed hosts. Credentials are stored in-memory and injected into matching requests by the transparent mitmproxy layer.

Prerequisites: transparent mitmproxy enabled (`OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true`), egress API auth token set (`OPENSANDBOX_EGRESS_TOKEN`).

Supported auth types: `bearer`, `basic`, `apiKey`, `customHeaders`.

See [Credential Vault](/guides/credential-vault) for full API usage, binding rules, and security model.

### Observability (OpenTelemetry)

Egress can export **OTLP metrics**; application logs use the **native zap** logger (JSON to stdout by default, configurable via `OPENSANDBOX_LOG_OUTPUT` / `OPENSANDBOX_EGRESS_LOG_LEVEL`). OTLP log export is not used.

## Build & Run

### Build Docker Image

```bash
cd components/egress

# Build locally
docker build -t opensandbox/egress:local .

# Or use the build script (multi-arch)
./build.sh
```

### Run Locally

1. Start sidecar:

```bash
docker run -d --name sandbox-egress \
  --cap-add=NET_ADMIN \
  opensandbox/egress:local
```

2. Apply policy:

```bash
curl -XPOST http://127.0.0.1:18080/policy \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.google.com"}]}'
```

3. Run app container in the same network namespace:

```bash
docker run --rm -it \
  --network container:sandbox-egress \
  curlimages/curl sh
```

4. Verify from app container:

```bash
curl -I https://google.com
curl -I https://github.com
```

## Development

- **Language**: Go 1.25+
- **Key Packages**:
    - `pkg/dnsproxy`: DNS server and policy matching logic.
    - `pkg/iptables`: `iptables` rule management.
    - `pkg/nftables`: nftables static/dynamic rules and DNS-resolved IP sets.
    - `pkg/policy`: Policy parsing and definition.
    - `pkg/credentialvault`: Credential vault store and binding validation.
    - `pkg/startup`: Post-startup hook registry (`Register`/`RunPost`).
    - `hooks/`: Side-effect import target; `init()` functions register startup hooks that run after iptables/MITM setup.

```bash
cd components/egress
go test ./...
```

## Process Supervisor

The egress container runs under `opensandbox-supervisor`, a lightweight process wrapper that restarts the egress worker on crash with exponential backoff, a crashloop circuit breaker, and structured JSONL event logging.

```
ENTRYPOINT: supervisor --pre-start=cleanup.sh --name=egress --grace-period=20s -- /opt/opensandbox-egress/egress
```

Egress-specific configuration:

- **`--grace-period=20s`**: Egress needs extra time to drain DNS connections and tear down iptables/nft rules on shutdown (default is 10 s).
- **Pre-start hook** (`cleanup.sh`): Reaps orphaned `mitmdump` processes from a previous crash and removes stale DNS redirect iptables/native nft state that would otherwise point port 53 at a dead proxy. It does not manage the `inet opensandbox` policy table; the nftables manager deletes and recreates that table when policy enforcement starts.

## Troubleshooting

- **"iptables setup failed"**: ensure sidecar has `--cap-add=NET_ADMIN`.
- **DNS fails for all domains**: check sidecar upstream DNS reachability and logs.
- **Traffic not blocked as expected**: in `dns+nft`, verify nft applied (`nft list table inet opensandbox`) and check sidecar logs for fallback.
