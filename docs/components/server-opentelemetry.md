---
title: Server OpenTelemetry Metrics
description: Metric names, units, and attribute contract for the OpenSandbox server's business lifecycle telemetry.
---

# Server OpenTelemetry Metrics

Meter: `opensandbox.server` · Prefix: `server.` · Durations: `ms`

Counters end in `.count` (no `_total`); duration histograms end in `.duration`; attribute keys are `snake_case` with the closed value sets below. Sandbox IDs, tenant IDs, API keys, and raw paths never become attributes.

## General

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `server.http.request.duration` | Histogram | `ms` | `http_method`, `http_route`, `http_status_code` | Inbound HTTP request latency by route template. |
| `opensandbox.sandbox.create.duration` | Histogram | `ms` | `sdk.language`, `sdk.version`, `success` | SDK-reported create latency (client-observed, includes polling), ingested via `POST /metrics/events`. |

## Sandbox lifecycle

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `server.sandbox.create.count` | Counter | — | `runtime`, `result`, `error_type`, `source` | Create operations; duration measures the whole server-side operation (async 202 included), not just HTTP latency. |
| `server.sandbox.create.duration` | Histogram | `ms` | `runtime`, `result` | |
| `server.sandbox.delete.count` | Counter | — | `runtime`, `result`, `trigger` | `trigger`: DELETE API (`user`), TTL timer (`expiry`), startup recovery (`recovery`). |
| `server.sandbox.delete.duration` | Histogram | `ms` | `runtime`, `result` | |
| `server.sandbox.pause.count` | Counter | — | `runtime`, `result` | |
| `server.sandbox.resume.count` | Counter | — | `runtime`, `result` | |
| `server.sandbox.renew.count` | Counter | — | `runtime`, `result` | Renewals via the API and the access-renew pipeline. |
| `server.sandbox.active` | Gauge | — | `state`, `runtime` | Current sandbox count by lifecycle state. Namespace sweep covers the default namespace plus enumerable tenant providers (file-backed/config); HTTP per-key providers are not swept — their namespaces are not a complete inventory. |
| `server.sandbox.orphan_cleaned.count` | Counter | — | `runtime` | Orphaned sidecars/volumes cleaned at startup. |

## Snapshots

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `server.snapshot.create.count` | Counter | — | `runtime`, `result` | Counted at terminal state (READY/FAILED), not at 202. |
| `server.snapshot.create.duration` | Histogram | `ms` | `runtime`, `result` | Runtime snapshot creation in the async worker. |
| `server.snapshot.delete.count` | Counter | — | `runtime`, `result` | Counted at terminal state. |

## Proxy

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `server.proxy.request.count` | Counter | — | `proxy_type`, `http_method`, `http_status_code` | Server-proxy traffic to sandbox endpoints. |
| `server.proxy.request.duration` | Histogram | `ms` | `proxy_type`, `http_method`, `http_status_code` | HTTP: up to response headers; cancelled clients record 499. WebSocket: whole session, status is the handshake outcome (101/401/404/500/502). |

## Access renew

| Metric | Type | Unit | Attributes | Description |
|---|---|---|---|---|
| `server.access_renew.outcome.count` | Counter | — | `outcome` | Pipeline outcomes. |

## Attribute values

- `runtime`: `docker` | `kubernetes` | `fleets`
- `result`: `success` | `error`
- `error_type` (create): `quota` | `pool_timeout` | `image` | `timeout` | `invalid_request` | `runtime` | `unknown`
- `source` (create): `image` | `snapshot` | `pool`
- `trigger` (delete): `user` | `expiry` | `recovery`
- `state`: `pending` | `running` | `pausing` | `paused` | `resuming` | `stopping` | `terminated` | `failed` — the public lifecycle states
- `proxy_type`: `http` | `websocket`
- `outcome`: `extended` | `dropped` | `stale` | `throttled`

## Histogram boundaries

- HTTP/proxy: `1 … 60000` ms (standard ladder).
- Lifecycle (create/delete/snapshot): `100 … 300000` ms (longer tail for K8s cold starts).

## Configuration

`[otel]` settings in the [Server configuration reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#otel): `enabled`, `endpoint`, export interval. No endpoint configured means no export.
