---
title: Ingress
description: HTTP/WebSocket reverse proxy that routes traffic to OpenSandbox instances via header or URI-based routing modes.
---

# OpenSandbox Ingress

## Overview
- HTTP/WebSocket reverse proxy that routes to sandbox instances.
- Resolves legacy sandbox routes using the Kubernetes provider selected by `--provider-type`:
  - BatchSandbox: reads endpoints from `sandbox.opensandbox.io/endpoints` annotation.
  - AgentSandbox: reads `status.serviceFQDN`.
- Can serve Fast Sandbox routes from the same ingress when `--fastpath-endpoint` is set.
- Fast Sandbox routes lazily call FastPath v2 `ResolveEndpoint` when traffic arrives.
- Exposes `/status.ok` health check and a shadow-only network readiness assessment at `/status.ok/network-readiness`; prints build metadata (version, commit, time, Go/platform) at startup.

## Quick Start
```bash
cd components/ingress

go run main.go \
  --namespace <any-value-kept-for-compatibility> \
  --provider-type <batchsandbox|agent-sandbox|fast-sandbox> \
  --mode <header|uri> \
  --port 28888 \
  --log-level info
```
Endpoints: `/` (proxy), `/status.ok` (health), `/status.ok/network-readiness` (shadow network assessment).

## L7 Frontend Configuration for WebSocket

The ingress terminates HTTP/1.1 on the wire and does not terminate TLS —
TLS is expected to end at the L7 proxy in front (HAProxy, nginx, Envoy,
cloud load balancer). Because browsers and modern L7 proxies default to
advertising HTTP/2 in ALPN, and RFC 7540 §8.1.2.2 forbids the classic
`Upgrade: websocket` / `Connection: Upgrade` handshake on HTTP/2, the L7
must either translate WebSocket upgrades to HTTP/1.1 before forwarding, or
downgrade the whole connection to HTTP/1.1. Otherwise the L7 strips the
upgrade header, the ingress receives a bare `GET`, and the browser reports
`WebSocket close 1006` or `workbench failed to connect` — the failure mode
tracked in [issue #1105](https://github.com/opensandbox-group/OpenSandbox/issues/1105).

The ingress does not natively accept RFC 8441 HTTP/2 Extended CONNECT
WebSocket upgrades. `isWebSocketRequest` filters out any non-`GET` method
before the WebSocket path, so `CONNECT` + `:protocol=websocket` requests
are routed through the plain HTTP reverse proxy and typically fail at the
backend with an implementation-defined error. This limitation is inherent
to the underlying [coder/websocket](https://github.com/coder/websocket)
library — h2 support is tracked upstream in
[coder/websocket#4](https://github.com/coder/websocket/issues/4).

| Option | What the L7 does | Recommended for |
| ------ | ---------------- | --------------- |
| **Translate** | Convert h2 WebSocket upgrades into HTTP/1.1 `Upgrade: websocket` before forwarding to the ingress | Production, most deployments |
| **Downgrade** | Only advertise HTTP/1.1 to browsers (no h2 ALPN) | Simple setups, older L7s |

### Option 1: Translate (recommended)

The L7 proxy accepts h2 from the browser and converts WebSocket upgrades
into HTTP/1.1 `Upgrade` requests toward the ingress. The ingress sees a
standard h1 handshake and handles it normally.

- **nginx-ingress ≥ 1.13.10** performs this translation by default.
- **HAProxy 3.x** supports it via the `proto h1` backend directive combined
  with frontend options — consult the HAProxy 3.x release notes for the
  currently recommended syntax; earlier configurations rely on
  `option h2-workaround-bogus-websocket-clients`.

### Option 2: Downgrade to HTTP/1.1

Turn off h2 in the L7 proxy's browser-facing ALPN list. Browsers connect
over HTTP/1.1 and follow the classic RFC 6455 handshake end-to-end.

HAProxy example:

```haproxy
frontend main
    bind :443 ssl crt /etc/haproxy/certs/ alpn http/1.1
    # ↑ Remove "h2," from the alpn list to force HTTP/1.1
```

For `haproxytech/kubernetes-ingress`, set the following in the controller
ConfigMap:

```yaml
data:
  tls-alpn: "http/1.1"
```

Trade-off: browsers lose HTTP/2 multiplexing and header compression on the
browser leg, but the configuration is minimal and there is no version
dependency on the L7 proxy.

### h2c passthrough is not supported

Some HAProxy configurations forward RFC 8441 h2 Extended CONNECT to the
backend unchanged (`server ingress-svc <addr> proto h2`). The ingress does
not accept this shape: the CONNECT request never enters the WebSocket
handler, and the plain HTTP proxy path cannot complete a WebSocket
handshake either. Use Option 1 or Option 2, or open an issue describing
your topology so this ingress can be considered for h2 support.

### Diagnostics

If a browser or SDK reports `WebSocket close 1006`, first confirm that
HTTP/1.1 WebSocket to the ingress works. This is both the actual
acceptance test and the fastest way to isolate whether the failure is at
the L7 or at the ingress.

```bash
curl --http1.1 -sv \
    -H 'Connection: Upgrade' \
    -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    "https://${sandbox_id}-${port}.example.com/some/ws/path" 2>&1 | \
    grep -E '^< HTTP|^< Sec-WebSocket-Accept'

# Expected output:
# < HTTP/1.1 101 Switching Protocols
# < Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=
```

If this returns `200` with an HTML body, either the ingress or an L7 hop
lost the `Upgrade` header. Do **not** try to craft an HTTP/2 WebSocket
handshake with curl — RFC 7540 §8.1.2.2 forbids `Connection: Upgrade` and
`Upgrade: websocket` on h2 streams, and RFC 8441 uses `:method=CONNECT` +
`:protocol=websocket` returning 2xx (not 101). A curl invocation like
`curl --http2 -H 'Upgrade: websocket'` sends headers that the L7 strips
before forwarding and does not measure what you think it measures.

To confirm which protocol the L7 negotiated with the client (independent of
the WebSocket outcome), inspect only the ALPN line:

```bash
curl -k --http2 -sv --max-time 2 \
    "https://${sandbox_id}-${port}.example.com/" 2>&1 | \
    grep -E '^\* ALPN'
```

- `ALPN, server accepted: http/1.1` — Option 2 is active.
- `ALPN, server accepted: h2` — Option 1 must be in effect for WebSocket
  traffic to work; verify end-to-end with a real browser or WebSocket
  client rather than trying to reconstruct the handshake with curl.

### Related to WebSocket forwarding behavior

- The ingress recognizes case-insensitive `Connection` header tokens and
  accepts `Connection: keep-alive, Upgrade` in addition to
  `Connection: Upgrade`.
- Backend redirect (3xx) and error (4xx/5xx) responses during the
  handshake are surfaced to the caller verbatim; the ingress does not
  follow redirects, so a stale `Location: /login` never leaks
  Authorization headers to an unrelated endpoint.
- Client-selected WebSocket subprotocols and backend `Set-Cookie` headers
  are forwarded across the handshake.
- Application close codes (for example, `1008 policy violation`,
  `4001+ application codes`) are propagated to the peer without being
  rewritten to `1000`.
- There is no message size cap in the proxy; the underlying
  `coder/websocket` limit is explicitly disabled to match the historical
  gorilla behavior.

## Network Readiness Observation

Ingress observes the TCP connections that its HTTP transport and WebSocket
dialer open to upstream targets. Each connection is classified as `success`,
`timeout`, `unreachable`, `refused`, `dns_error`, `canceled`, or `other`.
Only timeouts and unreachable errors are treated as possible source-side
network degradation signals. Canceled connections remain visible in the
per-result connection metric but are excluded from the assessment denominator
because they do not establish whether the network path was healthy.

The assessment uses the most recent complete fixed window. It requires enough
connection attempts, distinct upstream targets, and distinct failing targets
before reporting `DEGRADED`. The endpoint remains shadow-only:
`/status.ok/network-readiness` always returns HTTP 200 with a body of `OK` or
`DEGRADED`, and responses are never cacheable. This path is reserved by the
Ingress itself. The normal `/status.ok` liveness and readiness endpoint is
unchanged.

| Flag | Default | Description |
|------|---------|-------------|
| `--network-readiness-shadow-window` | `1m` | Fixed aggregation window |
| `--network-readiness-shadow-max-targets` | `1024` | Maximum distinct targets retained per window |
| `--network-readiness-shadow-min-attempts` | `20` | Minimum connection attempts required to qualify a window |
| `--network-readiness-shadow-min-targets` | `5` | Minimum distinct targets required to qualify a window |
| `--network-readiness-shadow-min-signal-targets` | `2` | Minimum distinct targets with timeout or unreachable results |
| `--network-readiness-shadow-failure-ratio` | `0.2` | Failure ratio required to report `DEGRADED` |

Invalid shadow settings disable connection observation and make the shadow
endpoint return HTTP 404; they do not stop the Ingress data plane.

The following OpenTelemetry metrics are emitted when OTLP metrics are enabled:

See [component telemetry configuration](/guides/component-telemetry) for endpoint
selection, standard disable switches, and cumulative/delta export settings.

- `ingress.upstream.connect.count` and `ingress.upstream.connect.duration`,
  labeled only by connection result and proxy type.
- `ingress.network.shadow.*` gauges for attempts, signal failures, distinct
  targets, qualification, and the shadow decision.

`attempts` counts physical TCP connections, not HTTP requests. Distinct targets
are network hosts; multiple ports on the same host intentionally count once.
HTTP keep-alive
can therefore make the sample count much lower than the request count. Target
addresses and Sandbox IDs are intentionally excluded from metric attributes.
Deployments that route through a fixed central proxy, or otherwise connect to
fewer than the configured minimum number of targets, may never qualify with
the default thresholds. If `HTTP_PROXY` or `HTTPS_PROXY` is configured, HTTP
observations describe the connection to that proxy rather than the final
Sandbox endpoint.

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

When enabled, the ingress publishes **renew-intent** events to a Redis list on each proxied request (after resolving the sandbox). The OpenSandbox server consumes these events and may extend sandbox expiration for sandboxes that opted in at creation time.

::: info Requirements
The server must have `renew_intent` (and Redis consumer for ingress mode) enabled; the sandbox must opt in via `extensions["access.renew.extend.seconds"]` (decimal integer string between **300** and **86400** seconds). This feature is best-effort and disabled by default.
:::

| Flag | Default | Description |
|------|---------|-------------|
| `--renew-intent-enabled` | `false` | Enable publishing renew-intent events to Redis |
| `--renew-intent-redis-dsn` | `redis://127.0.0.1:6379/0` | Redis DSN (may include `user:password@`) |
| `--renew-intent-queue-key` | `opensandbox:renew:intent` | Redis List key for intent payloads |
| `--renew-intent-queue-max-len` | `0` | Max list length (0 = no cap); LTRIM applied when > 0 |
| `--renew-intent-min-interval` | `60` | Min seconds between intents per sandbox (client-side throttle) |

Fast Sandbox intents additionally carry the authenticated namespace. Their publisher
throttle key is `(namespace, sandbox_id)` so equal IDs in different tenant
namespaces remain independent.

## Fast Sandbox Provider {#fast-sandbox-provider}

The implementation is named `FastSandboxProvider`; its CLI provider value is
`fast-sandbox`. Server-created Fast Sandbox IDs use the `fsb-` prefix. Ingress verifies
route scopes with the same `opensandbox-fsb-route-v1` signing tag as the Server.
The `f1.` token prefix identifies the route-scope format version.

The Phase 1a Fast Sandbox provider accepts only an authenticated internal Fast Sandbox
route scope. It resolves execd port `44772` and other user ports as raw
ports. Execd must already be installed and started by the workload image/template;
it is not delivered as a runtime Infra Component. Pools using the old named
`execd` declaration must migrate to image/template-provided execd and remove
that declaration: FastPath rejects raw-port access to declared component ports.
Endpoint handles can be issued while a sandbox is pending; actual
traffic receives `503` with `Retry-After` until FastPath publishes the route.
Port `18080` handles are reserved for SDK compatibility and traffic returns
`501`; use the authenticated Server `GET/PUT /sandboxes/{id}/networkpolicy`
route instead. See [Server policy operations](/components/server#fast-sandbox-workload-and-network-policy).

| Flag | Default | Description |
|------|---------|-------------|
| `--provider-type` | `batchsandbox` | Select the legacy Kubernetes provider, or set to `fast-sandbox` for Fast Sandbox-only routing |
| `--fastpath-endpoint` | empty | FastPath v2 gRPC endpoint; a non-empty value enables Fast Sandbox routing |
| `--fastpath-access-mode` | `direct-fastlet-proxy` | Use `central-proxy` when ingress cannot reach Fastlet Pod IPs |
| `--fastpath-wait-timeout-millis` | `2000` | Deadline for one FastPath ResolveEndpoint RPC |
| `--secure-access-keys` | empty | Shared signing key ring; required for Fast Sandbox route-scope verification |

With `--provider-type=batchsandbox` and a non-empty `--fastpath-endpoint`, one ingress serves both
legacy BatchSandbox routes and authenticated Fast Sandbox routes. The same applies to
`agent-sandbox`. The verified route format selects the backend explicitly:
legacy host/URI routes use the Kubernetes provider, while `f1.*` route scopes
use FastPath. Invalid `f1.*` scopes are rejected and never fall back to the
legacy provider. `--provider-type=fast-sandbox` remains available for deployments
that do not need Kubernetes-backed routes. BatchSandbox and AgentSandbox remain
alternative Kubernetes providers; enabling FastPath does not enable both.

For a shared BatchSandbox and Fast Sandbox ingress:

```bash
go run main.go \
  --provider-type batchsandbox \
  --fastpath-endpoint fast-sandbox-fastpath.fast-sandbox-system.svc:9090 \
  --secure-access-keys 'a=<base64-secret>'
```

`--provider-type=fast-sandbox` also requires an explicit `--fastpath-endpoint`; the
ingress fails startup when the endpoint cannot establish a gRPC connection
within five seconds. FastPath gRPC uses plaintext transport in Phase 1a and
must be isolated with NetworkPolicy. TLS or mTLS requires matching support in
both FastPath and ingress.

Direct Fastlet mode bypasses fast-sandbox's central Sandbox Proxy. Restrict
Fastlet port `5780` so only trusted ingress Pods can reach it. A matching
NetworkPolicy can select an ingress Pod labeled
`fast-sandbox.io/control-plane-client=true` and
`fast-sandbox.io/direct-data-plane-client=true`, and label its namespace
`sandbox.fast.io/scope=system`. The FastPath policy must admit that trusted
namespace when the two systems are deployed in different namespaces.

Fast Sandbox supports Header and URI route scopes in Phase 1a. Wildcard-host scopes
are not supported because the authenticated namespace, sandbox ID, and MAC do
not fit safely in one DNS label.

### Server-issued Fast Sandbox endpoints {#server-issued-fast-sandbox-endpoints}

The Fast Sandbox Server adapter returns a stable route from
`GET /sandboxes/{sandboxId}/endpoints/{port}` without calling FastPath or waiting
for readiness. It signs the authenticated tenant namespace, sandbox ID, and port
using the existing ingress signing configuration. Without multi-tenancy, it uses
the configured Fast Sandbox namespace.

With `runtime.type = "kubernetes"`, the Server uses `CompositeSandboxService`
to serve Kubernetes and Fast Sandbox workloads together. Create requests with
`templateId` select `FastSandboxService`; other creates use the Kubernetes
workload provider. Subsequent operations on `fsb-` sandbox IDs use Fast Sandbox.
FastPath settings live under `[kubernetes]`; `fast-sandbox` is an Ingress provider
value, not a separate Server runtime type.

Fast Sandbox Get/List reads the existing `sandbox.fast.io/v1alpha2` Sandbox CRs through
the Kubernetes LIST/WATCH cache. Writes still go through FastPath and invalidate
the corresponding cache. Unsynced or invalidated reads use the live Kubernetes
API; Get does not interpret a cache miss as NotFound. Reads require access to the
same cluster as FastPath and `get`, `list`, `watch` permissions on `sandboxes` in
each tenant namespace. Kubernetes deployments reuse their configured cluster
client and `[kubernetes].namespace` when tenancy is disabled. No separate
sandbox database is created.

The shared sandbox list combines both backends for the current tenant, then
filters, orders by creation time descending (ID breaks ties), and paginates once.
Totals describe the whole filtered collection. An unregistered FastSandbox API
contributes no items. Permission failures or backend read failures fail the list
instead of returning a silently incomplete collection. These aggregation and CR
read behaviors are implementation decisions supplementing OSEP-0007, not claims
that the proposal already specifies cross-backend list aggregation.

Configure the Server with a gateway and a key also present in the Ingress
`--secure-access-keys` key ring:

```toml
[ingress]
mode = "gateway"

[ingress.gateway]
address = "ingress.example.com"

[ingress.gateway.route]
mode = "header" # or "uri"

[ingress.secure_access]
active_key = "k"

[[ingress.secure_access.keys]]
key_id = "k"
key = "<base64-encoded-secret>"
```

Header mode returns the gateway address and an `OpenSandbox-Ingress-To: f1.*`
header. URI mode returns `<gateway-address>/f1.*`. Clients must preserve the
returned route and headers. These scopes have no embedded expiration, so the
Fast Sandbox adapter rejects the optional `expires` parameter rather than silently
issuing a non-expiring route. Missing signing keys, direct mode, and wildcard
mode are also rejected. Endpoint discovery does not establish sandbox existence;
Ingress performs the tenant-scoped lookup when traffic arrives.

Ingress resolves the upstream address and short-lived credential only when a
request arrives. A FastPath `FailedPrecondition` (including a not-ready route)
returns `503` with `Retry-After`; it is not a missing Sandbox. Port `18080` handles
can be issued, but the policy proxy is not implemented in this adapter yet and
actual traffic still returns `501`.

The Ingress protobuf subset matches FastSandbox commit
`11b21bf6a6ce730d48ea6e3e3d0050db2607ae49`, also verified against the Server Python
protobuf. `ResolveEndpoint` no longer accepts `wait_until_ready` or
`wait_timeout_millis`; the existing timeout flag bounds the RPC itself. The
shared wire fixture in `components/ingress/pkg/fastpath/v2/testdata` is exercised
by both Python and Go tests.

The `f1.` prefix is reserved for Fast Sandbox route scopes. A legacy route whose first
host or URI segment starts with `f1.` is treated as a Fast Sandbox route and returns
`401` when verification fails; it never falls back to a legacy provider.

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
- If `--provider-type=agent-sandbox`: agent-sandbox >= v0.5.0 serving `agents.x-k8s.io/v1beta1`, with `Ready=True` and `status.serviceFQDN` populated. Set `spec.service: true` for service creation (the OpenSandbox server sets this automatically). See [compatibility and migration](/examples/agent-sandbox#compatibility-and-migration).
- If `--fastpath-endpoint` is set: network access to FastPath v2 and a matching
  `--secure-access-keys` key ring shared with the OpenSandbox server.

## Implementation Notes

### Header Mode Behavior
- Routing key priority: `OpenSandbox-Ingress-To` header first, otherwise Host parsing `<sandbox-name>-<port>.*`.
- Sandbox name extracted from request is used to query the sandbox CR (BatchSandbox or AgentSandbox) via informer cache:
  - BatchSandbox: endpoints annotation.
  - AgentSandbox: `status.serviceFQDN`.
- The original request path is preserved and forwarded to the target sandbox.

### URI Mode Behavior
- Routing information is extracted from the URI path: `/<sandbox-id>/<sandbox-port>/<path-to-request>`.
- The sandbox ID and port are extracted from the first two path segments.
- The remaining path (`/<path-to-request>`) is forwarded to the target sandbox as the request URI.
- If no remaining path is provided, the request URI defaults to `/`.

### Commons
- Error handling:
  - `ErrSandboxNotFound` (sandbox resource not exists) -> HTTP 404
  - `ErrSandboxNotReady` (not enough replicas, missing endpoints, invalid config) -> HTTP 503
  - Other errors (K8s API errors, etc.) -> HTTP 502
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
