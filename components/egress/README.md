# OpenSandbox Egress Sidecar

The **Egress** is a core component of OpenSandbox that provides **FQDN-based egress control**. 

It runs alongside the sandbox application container (sharing the same network namespace) and enforces declared network policies.

## Features

- **FQDN-based Allowlist**: Control outbound traffic by domain name (e.g., `api.github.com`).
- **Wildcard Support**: Allow subdomains using wildcards (e.g., `*.pypi.org`).
- **Transparent Interception**: Uses transparent DNS proxying; no application configuration required.
- **Experimental: Transparent HTTPS MITM (mitmproxy)**: Optional transparent TLS interception for outbound `80/443` traffic in the sidecar network namespace. See [mitmproxy transparent mode](docs/mitmproxy-transparent.md).
- **Dynamic DNS (dns+nft mode)**: When a domain is allowed and the proxy resolves it, the resolved A/AAAA IPs are added to nftables with TTL so that default-deny + domain-allow is enforced at the network layer.
- **Privilege Isolation**: Requires `CAP_NET_ADMIN` only for the sidecar; the application container runs unprivileged.
- **Graceful Degradation**: If `CAP_NET_ADMIN` is missing, it warns and disables enforcement instead of crashing.

## Architecture

The egress control is implemented as a **Sidecar** that shares the network namespace with the sandbox application.

1.  **DNS Proxy (Layer 1)**:
    - Runs on `127.0.0.1:15353`.
    - `iptables` rules redirect all port 53 (DNS) traffic to this proxy.
    - Filters queries based on the allowlist.
    - Returns `NXDOMAIN` for denied domains.

2.  **Network Filter (Layer 2)** (when `OPENSANDBOX_EGRESS_MODE=dns+nft`):
    - Uses `nftables` to enforce IP-level allow/deny. Resolved IPs for allowed domains are added to dynamic allow sets with TTL (dynamic DNS).
    - At startup, the sidecar whitelists **127.0.0.1** (redirect target for the proxy) and **nameserver IPs** from `/etc/resolv.conf` so DNS resolution and proxy upstream work (including private DNS). Nameserver count is capped and invalid IPs are filtered; see [Configuration](#configuration).

## Requirements

- **Runtime**: Docker or Kubernetes.
- **Capabilities**: `CAP_NET_ADMIN` (for the sidecar container only).
- **Kernel**: Linux kernel with `iptables` support.

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

### Runtime HTTP API

- `GET /policy`: get current policy
- `POST /policy`: replace policy (`{}`, `null`, empty body => reset to deny-all)
- `PATCH /policy`: merge/append rules (body is JSON array of egress rules)

Quick example:

```bash
curl -XPOST http://127.0.0.1:18080/policy \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}'
```

### Experimental: Transparent MITM (mitmproxy)

> Status: **Experimental**. APIs, environment variables, and behavior may change.

Optional transparent HTTPS interception for outbound `80/443` traffic in the sidecar network namespace.
See [docs/mitmproxy-transparent.md](docs/mitmproxy-transparent.md) for configuration and limitations.

### Observability (OpenTelemetry)

Egress can export **OTLP metrics**; application logs use the **native zap** logger (JSON to stdout by default, configurable via `OPENSANDBOX_LOG_OUTPUT` / `OPENSANDBOX_EGRESS_LOG_LEVEL`). **OTLP log export is not used.**

See **[Egress OpenTelemetry reference](docs/opentelemetry.md)** for metrics, structured log fields, and how to enable OTLP metrics (`OTEL_EXPORTER_OTLP_*`, `OPENSANDBOX_EGRESS_SANDBOX_ID`, etc.).

## Build & Run

### Build Docker Image

```bash
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

- **Language**: Go 1.24+
- **Key Packages**:
    - `pkg/dnsproxy`: DNS server and policy matching logic.
    - `pkg/iptables`: `iptables` rule management.
    - `pkg/nftables`: nftables static/dynamic rules and DNS-resolved IP sets.
    - `pkg/policy`: Policy parsing and definition.
- **Main (egress)**:
    - `nameserver.go`: Builds the list of IPs to whitelist for DNS in nft mode (127.0.0.1 + validated/capped nameservers from resolv.conf).

```bash
# Run tests
go test ./...
```

### E2E benchmark: dns vs dns+nft (sync dynamic IP write)

An end-to-end benchmark compares **dns** (pass-through, no nft write) and **dns+nft** (sync `AddResolvedIPs` before each DNS reply) under real conditions: sidecar in Docker, iptables redirect, real DNS + HTTPS from a client container.

```bash
./tests/bench-dns-nft.sh
```

More details in [docs/benchmark.md](docs/benchmark.md).

## Troubleshooting

- **"iptables setup failed"**: ensure sidecar has `--cap-add=NET_ADMIN`.
- **DNS fails for all domains**: check sidecar upstream DNS reachability and logs.
- **Traffic not blocked as expected**: in `dns+nft`, verify nft applied (`nft list table inet opensandbox`) and check sidecar logs for fallback.
