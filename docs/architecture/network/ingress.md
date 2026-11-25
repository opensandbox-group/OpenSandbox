---
title: Ingress
description: The HTTP/WebSocket gateway that routes client traffic to sandbox ports — header- and path-based routing, WebSocket-native proxying, and precise error semantics.
---

# Ingress

The ingress gateway is the front door for sandbox traffic on Kubernetes: one reverse proxy standing in front of every sandbox, with no per-sandbox configuration. It watches the state workloads already publish — endpoint annotations, service FQDNs, or the FastPath control plane — and resolves each incoming request against a live cache.

![Ingress routing](../../public/images/ingress-routing.svg)

## How a request reaches a sandbox

The route to a sandbox is a pair of sandbox ID and port, discovered in one of two modes:

- **Header mode** (default): the `OpenSandbox-Ingress-To` header carries `<sandbox-id>-<port>`, or the host label encodes the same pair (`my-sandbox-8080.example.com`). The pre-rename legacy header is still accepted.
- **URI mode**: the first two path segments carry the pair — `/<sandbox-id>/<port>/<path>` — so clients that cannot set headers still work.

With [secure access](/guides/secure-access), the route can instead be a signed route token: sandbox ID, port, expiry, and MAC in one label, verifiable without a lookup.

Resolution reads the endpoint from the workload that owns the sandbox — BatchSandbox endpoint annotations, agent-sandbox service FQDNs, or FastPath for Fast Sandbox routes. Before forwarding, the ingress strips the routing headers, adds `X-Forwarded-*`, filters hop-by-hop headers, and preserves the request path and query exactly.

## Error semantics

When routing cannot proceed, the failure is precise — clients can branch on status codes:

| Status | Meaning |
|---|---|
| `404` | No such sandbox (or no longer exists) |
| `503` | Sandbox exists but is not ready — no reachable endpoint yet |
| `502` | Infrastructure-level failure (cluster API errors and the like) |

Native gRPC callers get these failures as gRPC status codes instead (`404` → `NOT_FOUND`, `503`/`502` → `UNAVAILABLE`, `401` → `UNAUTHENTICATED`), sent as a trailers-only response because gRPC clients ignore `grpc-status` on a non-200 HTTP status. Ingress metrics still record the HTTP status.

Once a sandbox is reached, its responses pass through **verbatim**, including redirects and errors. The ingress never follows a redirect itself, so a stale `Location` can never leak credentials to an unrelated endpoint.

## Upstream HTTP connection reuse

Each ingress replica reuses one HTTP transport across requests to different sandbox upstreams. Its idle connection pool can be configured independently of connection observation; WebSocket traffic keeps a separate transport, and native gRPC uses its own h2c transport with the same pool settings. See [Ingress HTTP connection pool](/guides/ingress-connection-pool) for configuration and zero-value semantics, and the [defaults decision](/architecture/network/ingress-http-pool-defaults) for compatibility and resource trade-offs.

## WebSocket support

WebSocket is a first-class path, not an upgrade hack bolted onto HTTP proxying:

- Upgrades follow RFC 6455 over HTTP/1.1, with a 45-second backend handshake budget.
- Client-selected subprotocols and backend `Set-Cookie` headers cross the handshake.
- Application close codes are propagated as-is — a policy violation (`1008`) or app code (`4001+`) is never rewritten to `1000`.
- Single messages up to 64 MiB work in either direction, sized for terminal and Jupyter traffic.
- `Connection` header tokens are matched case-insensitively, and both directions stream without buffering the peer.

## gRPC support

Native gRPC reaches sandbox ports over cleartext HTTP/2 (h2c prior knowledge) on the same listener port as HTTP/1.1 and WebSocket. It is off by default: start the ingress with `--h2c-enabled` (Helm: `gateway.h2cEnabled: true`). Without it, h2c connections are rejected and HTTP/1.1 and WebSocket behave exactly as before.

- Unary, server-streaming, client-streaming, and bidirectional streaming calls are supported; streams flow message by message, and `grpc-status` trailers reach the client unchanged.
- A request is treated as gRPC when it arrives over HTTP/2 with `Content-Type: application/grpc` or `application/grpc+<codec>`; it is forwarded to the sandbox over h2c. gRPC-Web and all other HTTP keep the HTTP/1.1 upstream path.
- Route with the `:authority` `<sandbox-id>-<port>.<domain>` or with the `opensandbox-ingress-to` metadata key. Platform metadata (`opensandbox-ingress-to`, `opensandbox-secure-access`, `opensandbox-access-renew`) is stripped before forwarding, as for HTTP headers.
- Wake-on-access parks the call before it is forwarded. gRPC calls are `POST`, so a call that already reached a stale upstream is never replayed.
- Fast Sandbox routes also need h2c on the Fastlet proxy hop, which is provided by the fast-sandbox platform.

## Fronting ingress with TLS

The ingress speaks cleartext HTTP/1.1 (plus h2c with `--h2c-enabled`) and never terminates TLS. Put an L7 proxy in front of it — but mind the WebSocket constraint:

![Ingress behind TLS](../../public/images/ingress-tls-l7.svg)

WebSocket-over-HTTP/2 (RFC 8441) is **not** accepted, and RFC 7540 forbids the `Upgrade` headers on plain h2 requests. The front proxy must therefore deliver WebSocket upgrades as classic HTTP/1.1. In order of preference:

1. Remove `h2` from the browser-facing ALPN list — portable, verified fallback.
2. Per-WebSocket HTTP/1.1 fallback, if the proxy supports it while keeping h2 for other traffic.
3. RFC 8441 → RFC 6455 translation — proxy-specific; verify with a real browser before trusting it.

Never forward a WebSocket upgrade to the ingress over HTTP/2: the upgrade never reaches the WebSocket handler and the plain proxy path cannot complete the handshake either. With `--h2c-enabled`, gRPC (`application/grpc*`) must be forwarded as h2c and other HTTP may use either HTTP/1.1 or h2c; without it, forward everything as HTTP/1.1.

## Health and network readiness

- `/status.ok` — liveness: the process is up and serving.
- `/status.ok/network-readiness` — a **shadow** signal, for operators watching node-to-sandbox network health. It classifies the TCP connections the ingress opens (success, timeout, unreachable, refused, …) over a recent window and reports `DEGRADED` when timeouts and unreachables dominate across enough distinct targets.

The shadow endpoint always answers `200` with `OK` or `DEGRADED` and never gates traffic — it is a decision input for operators, not a load-balancer hook. Window length and thresholds are tunable; targets and sandbox IDs are deliberately absent from its metrics.

## Extending sandbox expiration on access

When enabled, the ingress publishes a **renew-intent** event for each proxied request (after the sandbox resolves), and the lifecycle server extends the expiration of sandboxes that opted in at creation. Publishing is throttled per sandbox and delivery is best-effort — an idle sandbox still expires; a busy one stays alive without client changes.

### Per-request opt-out

A single request can opt out of renew-intent publishing with a control header:

```text
OpenSandbox-Access-Renew: skip
```

- The exact sentinel value `skip` suppresses the intent for that one request (it bypasses the per-sandbox throttle entirely); unknown values are ignored for forward compatibility.
- The header is stripped before forwarding upstream, like `OpenSandbox-Ingress-To`, so backend applications never observe it.
- Typical use: health probes and other background traffic hitting an opted-in sandbox without extending its lifetime.

WebSocket upgrades carry the header on the handshake request; long-lived connections do not produce per-request intents anyway.

## Fast Sandbox routes

The `FastSandboxProvider` resolves endpoints through FastPath v2 and forwards traffic to Fastlet pods. It can run standalone or share one ingress process with the legacy Kubernetes providers. Header and URI `f1.` route scopes use the same signing key ring as the server. The single-value `--fast-sandbox-namespace` option additionally enables unsigned `<fsb-sandbox-id>-<port>.<gateway-domain>` routes in header mode, carried in the Host or the routing header, and restricts all Fast Sandbox routes, including signed scopes, to that namespace. Unsigned routes require external access control; they do not authenticate clients. See [Fast Sandbox: Networking](/architecture/fast-sandbox/networking).
