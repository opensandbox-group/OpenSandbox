# OpenSandbox Ingress

## Overview
- HTTP/WebSocket reverse proxy that routes to sandbox instances.
- Watches sandbox CRs (BatchSandbox or AgentSandbox, chosen by `--provider-type`) across all namespaces:
  - BatchSandbox: reads endpoints from `sandbox.opensandbox.io/endpoints` annotation.
  - AgentSandbox: reads `status.serviceFQDN`.
- Exposes `/status.ok` health check; prints build metadata (version, commit, time, Go/platform) at startup.

## Quick Start
```bash
go run main.go \
  --namespace <any-value-kept-for-compatibility> \
  --provider-type <batchsandbox|agent-sandbox> \
  --mode <header|uri> \
  --port 28888 \
  --log-level info
```
Endpoints: `/` (proxy), `/status.ok` (health).

## Routing Modes

The ingress supports two routing modes for discovering sandbox instances:

### Header Mode (default: `--mode header`)

Routes requests based on the `OpenSandbox-Ingress-To` header or the `Host` header.

**Format:**
- Header: `OpenSandbox-Ingress-To: <sandbox-id>-<port>`
- Host: `<sandbox-id>-<port>.<domain>`

**Example:**
```bash
# Using OpenSandbox-Ingress-To header
curl -H "OpenSandbox-Ingress-To: my-sandbox-8080" https://ingress.opensandbox.io/api/users

# Using Host header
curl -H "Host: my-sandbox-8080.example.com" https://ingress.opensandbox.io/api/users
```

**Parsing logic:**
- Extracts sandbox ID and port from the format `<sandbox-id>-<port>`
- The last segment after the last `-` is treated as the port
- Everything before the last `-` is treated as the sandbox ID

### URI Mode (`--mode uri`)

Routes requests based on the URI path structure.

**Format:**

`/<sandbox-id>/<sandbox-port>/<path-to-request>`

**Example:**
```bash
# Request to sandbox "my-sandbox" on port 8080, forwarding to /api/users
curl https://ingress.opensandbox.io/my-sandbox/8080/api/users

# WebSocket example
wss://ingress.opensandbox.io/my-sandbox/8080/ws
```

**Parsing logic:**
- First path segment: sandbox ID
- Second path segment: sandbox port
- Remaining path: forwarded to the target sandbox as the request URI
- If no remaining path is provided, defaults to `/`

**Use cases:**
- When you cannot modify HTTP headers
- When you need path-based routing
- For simpler client configuration without custom headers

## Auto-Renew on Ingress Access (OSEP-0009)

When enabled, the ingress publishes **renew-intent** events to a Redis list on each proxied request (after resolving the sandbox). The OpenSandbox server consumes these events and may extend sandbox expiration for sandboxes that opted in at creation time. See [OSEP-0009](https://github.com/alibaba/opensandbox/blob/main/oseps/0009-auto-renew-sandbox-on-ingress-access.md) for the full design.

**Requirements:** The server must have `renew_intent` (and Redis consumer for ingress mode) enabled; the sandbox must opt in via `extensions["access.renew.extend.seconds"]` (decimal integer string between **300** and **86400** seconds, see OSEP-0009). This feature is best-effort and disabled by default.

| Flag | Default | Description |
|------|---------|-------------|
| `--renew-intent-enabled` | `false` | Enable publishing renew-intent events to Redis |
| `--renew-intent-redis-dsn` | `redis://127.0.0.1:6379/0` | Redis DSN (may include `user:password@`) |
| `--renew-intent-queue-key` | `opensandbox:renew:intent` | Redis List key for intent payloads |
| `--renew-intent-queue-max-len` | `0` | Max list length (0 = no cap); LTRIM applied when &gt; 0 |
| `--renew-intent-min-interval` | `60` | Min seconds between intents per sandbox (client-side throttle) |

**Example (with Redis):**
```bash
go run main.go \
  --namespace opensandbox \
  --renew-intent-enabled \
  --renew-intent-redis-dsn "redis://user:pass@redis:6379/0" \
  --renew-intent-min-interval 120
```

## Build
```bash
cd components/ingress
make build
# override build metadata if needed
VERSION=1.2.3 GIT_COMMIT=$(git rev-parse HEAD) BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ") make build
```

## Docker Build
Dockerfile already wires ldflags via build args:
```bash
docker build \
  --build-arg VERSION=$(git describe --tags --always --dirty) \
  --build-arg GIT_COMMIT=$(git rev-parse HEAD) \
  --build-arg BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ") \
  -t opensandbox/ingress:local .
```

## Multi-arch Publish Script
`build.sh` uses buildx to build/push linux/amd64 and linux/arm64:
```bash
cd components/ingress
TAG=local VERSION=1.2.3 GIT_COMMIT=abc BUILD_TIME=2025-01-01T00:00:00Z bash build.sh
```

## Runtime Requirements
- Access to Kubernetes API (in-cluster or via KUBECONFIG).
- If `--provider-type=batchsandbox`: BatchSandbox CRs in any namespace with `sandbox.opensandbox.io/endpoints` annotation containing Pod IPs.
- If `--provider-type=agent-sandbox`: AgentSandbox CRs in any namespace with `status.serviceFQDN` populated.

## Implementation Notes

### Header Mode Behavior
- Routing key priority: `OpenSandbox-Ingress-To` header first, otherwise Host parsing `<sandbox-name>-<port>.*`.
- Sandbox name extracted from request is used to query the sandbox CR (BatchSandbox or AgentSandbox) via informer cache:
  - BatchSandbox → endpoints annotation.
  - AgentSandbox → `status.serviceFQDN`.
- The original request path is preserved and forwarded to the target sandbox.

### URI Mode Behavior
- Routing information is extracted from the URI path: `/<sandbox-id>/<sandbox-port>/<path-to-request>`.
- The sandbox ID and port are extracted from the first two path segments.
- The remaining path (`/<path-to-request>`) is forwarded to the target sandbox as the request URI.
- If no remaining path is provided, the request URI defaults to `/`.

### Commons
- Error handling:
  - `ErrSandboxNotFound` (sandbox resource not exists) → HTTP 404
  - `ErrSandboxNotReady` (not enough replicas, missing endpoints, invalid config) → HTTP 503
  - Other errors (K8s API errors, etc.) → HTTP 502
- WebSocket path forwards essential headers and X-Forwarded-*; HTTP path strips `OpenSandbox-Ingress-To` before proxying (header mode only).

## Development & Tests
```bash
cd components/ingress
go test ./...
```
Key code:
- `main.go`: entrypoint and handlers.
- `pkg/proxy/`: HTTP/WebSocket proxy logic, sandbox endpoint resolution.
- `pkg/sandbox/`: Sandbox provider abstraction and BatchSandbox implementation.
- `version/`: build metadata output (populated via ldflags).

