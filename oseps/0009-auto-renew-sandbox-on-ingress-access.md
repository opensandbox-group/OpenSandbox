---
title: Auto-Renew Sandbox on Ingress Access
authors:
  - "@Pangjiping"
creation-date: 2026-03-15
last-updated: 2026-03-23
status: implemented
---

# OSEP-0009: Auto-Renew Sandbox on Ingress Access

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
  - [Scope: Supported Reverse Proxy Paths](#scope-supported-reverse-proxy-paths)
  - [Activation Model and Extensions Contract](#activation-model-and-extensions-contract)
  - [Control Strategy to Prevent Renewal Storms](#control-strategy-to-prevent-renewal-storms)
  - [Mode A: Server Proxy Path](#mode-a-server-proxy-path)
  - [Mode B: Ingress Gateway Path (Redis Queue)](#mode-b-ingress-gateway-path-redis-queue)
  - [Why Redis Between Ingress and Server](#why-redis-between-ingress-and-server)
  - [Redis Data Model](#redis-data-model)
  - [Configuration](#configuration)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Introduce an access-driven sandbox auto-renew mechanism for ingress traffic. When users access sandbox services through reverse proxy paths, OpenSandbox can automatically extend sandbox expiration for sandboxes that explicitly opt in to this capability.

This proposal only supports two proxy paths that can observe access traffic: server proxy and ingress gateway. Docker direct access is explicitly out of scope because no reverse proxy request can be reliably captured there.

## Motivation

Today users must renew expiration explicitly through `POST /sandboxes/{id}/renew-expiration`. For interactive workloads (IDE, notebook, web app), request traffic already implies sandbox activity, but expiration still depends on explicit lifecycle API calls from clients.

This creates two practical issues:

- User sessions can be interrupted even while ingress traffic is still active.
- Naively triggering renewal on every ingress request would create renewal storms under high QPS.

An access-driven renewal mechanism is needed, but it must be strongly rate-controlled and deduplicated.

### Goals

- Automatically renew sandbox expiration on observed ingress access for explicitly opted-in sandboxes.
- Support exactly two existing reverse proxy implementations:
  - server proxy path
  - ingress gateway path
- Use direct self-call renewal in server proxy mode.
- Use Redis-backed queue forwarding in ingress gateway mode.
- Require explicit capability enablement at three levels: server, ingress, and sandbox creation request.
- Strictly control actual renewal API calls to avoid excessive renew traffic.
- Preserve existing lifecycle API semantics and backward compatibility.

### Non-Goals

- Supporting Docker direct exposure mode for auto-renew triggers.
- Replacing manual renewal API (`renew-expiration`) behavior.
- Introducing per-request guaranteed renewal (best-effort under policy control is sufficient).
- Building a generic event bus for all lifecycle actions.

## Requirements

- The implementation must work with existing lifecycle API and runtime providers.
- Reverse proxy traffic must be the only trigger source for this proposal.
- Auto-renew must be disabled unless all three conditions are met:
  - server supports and enables `renew_intent`,
  - ingress supports and enables renew-intent signaling (for ingress mode),
  - sandbox creation request explicitly opts in via `extensions`.
- Renewal requests must be bounded by deduplication and throttling controls.
- Ingress gateway mode must use Redis as the forwarding queue.
- Renewal must be idempotent from the caller perspective (repeated access events do not imply repeated renew calls).
- The design must remain safe under burst traffic and multi-replica deployments.

## Proposal

Add an "access renew controller" that converts proxy access signals into controlled renewal attempts.

- In server proxy mode, the server path handling proxied traffic submits local renew intents and performs internal renewal calls.
- In ingress gateway mode, ingress publishes renew intents into Redis; OpenSandbox server consumes and executes controlled renewals.
- Both modes share the same renewal gate logic: opt-in check, sandbox state, server-side validity for each renew attempt, cooldown, and per-sandbox in-flight deduplication.

At a high level, access traffic indicates activity, but only eligible events produce actual `renew-expiration` operations.

### Notes/Constraints/Caveats

- This OSEP applies to reverse proxy captured traffic only.
- If a deployment bypasses proxy (direct pod/container access), no automatic renewal signal is available.
- Ingress-mode auto-renew is best-effort and depends on Redis availability.
- Renewal policy is intentionally conservative to prioritize control-plane stability.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Renewal storms under high ingress QPS | Multi-stage gating: validity checks + cooldown + in-flight dedupe |
| Duplicate renewals across server replicas | Redis lock keys for distributed dedupe in ingress mode; local dedupe in server proxy path |
| Redis backlog growth in traffic spikes | Queue TTL, bounded consumer concurrency, and drop-on-overload policy |
| False negatives (active sandbox not renewed) | Server-side eligibility rules and cooldown; metrics/alerts for missed renew opportunities |
| Added operational complexity | Feature flag rollout, default-off mode, and explicit docs/runbooks |

## Design Details

### Scope: Supported Reverse Proxy Paths

Only these two paths are supported:

1. **Server proxy path**
   - Access route: `/sandboxes/{sandbox_id}/proxy/{port}/...`
   - Traffic is observed inside OpenSandbox server directly.
2. **Ingress gateway path**
   - Access is observed by ingress/gateway implementation (wildcard/header/uri routing modes).
   - Signals are forwarded through Redis queue to server workers.

Explicitly unsupported:

- **Docker direct mode** (client accesses container endpoint directly):
  - No mandatory reverse proxy hop exists.
  - OpenSandbox cannot reliably observe all access requests.

### Activation Model and Extensions Contract

This feature uses explicit "three-party handshake" activation.

1. **Server-side capability switch**
   - `renew_intent.enabled = true` must be set (top-level TOML section `[renew_intent]`, model field on root `AppConfig`).
2. **Ingress-side capability switch** (ingress mode only)
   - ingress must be configured to publish renew-intents (`renew_intent.redis.enabled = true` and ingress integration enabled).
3. **Sandbox-level opt-in and duration**
   - sandbox must declare in `CreateSandboxRequest.extensions` how long each automatic renewal extends expiration (see below). Presence of a valid value opts the sandbox in.

If any condition is missing, access events are ignored for renewal.

Given current API schema (`extensions: Dict[str, str]`), this OSEP proposes:

- `extensions["access.renew.extend.seconds"]` = decimal integer **string** in the inclusive range **300–86400** seconds (**5 minutes** to **24 hours**), e.g. `"1800"`.

**Meaning:** When auto-renew on access is triggered for this sandbox, each renewal extends expiration by this many seconds. The key thus both opts the sandbox in and defines the per-renewal extension duration.

**Behavior rules:**

- Missing key means no renew-on-access for that sandbox.
- If the key is present, the value must parse as an integer in **300–86400**; otherwise the create request fails with **400** (validated in the HTTP API layer via `validate_extensions` in `src/extensions/validation.py` before the runtime service runs).
- Valid value enables auto-renew subject to policy gating; each successful renewal uses `new_expires_at = now + (value of access.renew.extend.seconds)`.

### Control Strategy to Prevent Renewal Storms

Both modes share the same strict control policy. An access event triggers renewal only when all checks pass:

1. **Opt-in check**: sandbox has `access.renew.extend.seconds` in extensions within **300–86400** (validated at creation).
2. **Sandbox state check**: sandbox must be `Running`.
3. **Validity check**: server decides whether the renewal attempt should proceed (e.g. `new_expires_at` meaningfully extends current expiration, lifecycle rules). There is **no** separate configurable “remaining TTL must be below N seconds” knob in server config.
4. **Cooldown check**: no successful renewal for this sandbox within `min_interval_seconds`.
5. **In-flight dedupe**: at most one renewal task per sandbox at a time.

If any check fails, the event is acknowledged and dropped without a renewal call.

Renew target time:

- `new_expires_at = now + (value of extensions["access.renew.extend.seconds"])`; the extension duration is taken only from the sandbox `extensions` (no server-side override or default for this value).
- must also satisfy `new_expires_at > current_expires_at` before calling renew API

This guarantees bounded renewal frequency even for very hot sandboxes.

### Mode A: Server Proxy Path

For requests handled by server proxy:

```
Client --> OpenSandbox Server Proxy --> Sandbox Service
              |
              +--> AccessRenewController (local signal)
                        |
                        +--> eligibility + cooldown + in-flight checks
                                |
                                +--> internal renew call (server -> own renew handler)
```

Implementation notes:

- Trigger point: after sandbox resolution and before/after proxy forward (implementation-defined), with non-blocking behavior.
- Renewal execution must not increase proxy path latency materially; use async/background task dispatch.
- Internal renewal uses existing service-level renewal logic to avoid API divergence.

### Mode B: Ingress Gateway Path (Redis Queue)

For requests first seen by ingress:

```
Client --> Ingress/Gateway
             |
             +--> publish renew-intent to Redis (sandbox_id, ts, route info)
                           |
                           v
                  OpenSandbox Renew Worker
                           |
                           +--> eligibility + cooldown + distributed dedupe
                                   |
                                   +--> renew call
```

Redis usage:

- Queue: **Redis List only** (required). Ingress pushes with LPUSH; server workers pop with BRPOP. No ack—best-effort delivery. Keeps the model simple and avoids Stream/consumer-group complexity.
- Intent payload (one JSON string per list element):
  - `sandbox_id` (string, required)
  - `observed_at` (string, required; RFC3339 or RFC3339Nano)
  - `port` (int, optional) — sandbox port accessed
  - `request_uri` (string, optional) — path forwarded to the sandbox
- Ingress may apply a **client-side throttle** (e.g. min interval per sandbox) so not every request produces an intent; queue key and optional list cap are configurable.
- Distributed dedupe lock key (server side):
  - `opensandbox:renew:lock:{sandbox_id}` with short TTL

Worker behavior:

- One or more workers block on BRPOP; on pop, parse payload, drop if stale, then run gate checks and maybe renew (with lock). No requeue on failure—best-effort.
- On publish/consume failures, log and drop.

### Why Redis Between Ingress and Server

Redis is selected for ingress -> server renew-intent delivery to decouple data-plane bursts from control-plane renew execution.

Compared with ingress directly calling server renew APIs:

- **Backpressure isolation**: ingress can LPUSH quickly; server workers process at their own pace.
- **Latency protection**: ingress request path does not wait on renew execution.
- **Multi-replica friendliness**: multiple server instances can BRPOP from the same list (competing consumers); each message is taken by one worker.
- **Failure containment**: when server is transiently unhealthy, intents can sit in the list briefly instead of ingress retrying synchronously.

Compared with other MQs (Kafka/NATS/Pulsar):

- **Scope fit**: best-effort, short-lived; Redis List is the minimal option and avoids Stream/consumer-group complexity.
- **Operational cost**: Redis is commonly available; List is the simplest structure.
- **Implementation speed**: LPUSH + BRPOP + lock is enough; no XREADGROUP/XACK or group management.

### Redis Data Model

This OSEP uses a Redis List for renew-intent events plus a lock key for per-sandbox dedupe (server side).

**Keys:**

- **Intent list key**: configurable, default `opensandbox:renew:intent` (Redis List)
- **Per-sandbox lock key**: `opensandbox:renew:lock:{sandbox_id}` (server consumer only)

**Intent payload** (single JSON string per list element):

| Field          | Type   | Required | Description                          |
|----------------|--------|----------|--------------------------------------|
| `sandbox_id`   | string | yes      | Sandbox identifier                   |
| `observed_at`  | string | yes      | Time of access (RFC3339 or RFC3339Nano) |
| `port`         | int    | no       | Sandbox port accessed                |
| `request_uri`  | string | no       | Path forwarded to the sandbox        |

Producer (ingress):

- Push with `LPUSH <queue_key> <serialized-json>`.
- Optional: cap list length (`LTRIM <queue_key> 0 max_len-1` after LPUSH); overflow is best-effort drop.
- Ingress may throttle: e.g. at most one intent per sandbox per N seconds (client-side) to limit queue growth.

Consumer (server):

- One or more workers block with `BRPOP opensandbox:renew:intent <timeout>`.
- On pop: parse payload; if the intent is older than a short implementation-defined max age (vs `observed_at`), drop and continue.
- Acquire lock: `SET opensandbox:renew:lock:{sandbox_id} <value> NX EX <ttl>` using a short implementation-defined lock TTL.
- If lock acquired: run gate checks (opt-in, state, validity, cooldown) and maybe renew; then lock expires by TTL.
- If lock not acquired: treat as in-flight dedupe, drop.
- No ack or requeue: if the worker crashes after pop, that intent is lost (best-effort).

Notes:

- Lock TTL and intent staleness thresholds are fixed in code (not Redis config); lock TTL must be short and greater than the renew critical section.
- Implementations must use Redis List; this LPUSH/BRPOP + lock flow is the only specified processing model.

### Configuration

Use the root config file: lifecycle API settings stay under `[server]`; renew-on-access is a **separate top-level section** `[renew_intent]` (not nested under `[server]`), alongside `[runtime]`, `[docker]`, etc.

```toml
[server]
# ... host, port, etc.

# Auto-detected by request path:
# - server-proxy path uses local trigger (no Redis required)
# - ingress path uses Redis consumer when renew_intent.redis is enabled

[renew_intent]
enabled = false
min_interval_seconds = 60
redis.enabled = false
redis.dsn = "redis://127.0.0.1:6379/0"
redis.queue_key = "opensandbox:renew:intent"
redis.consumer_concurrency = 8
```

Configuration rules:

- `renew_intent.enabled=false` means feature fully disabled.
- Ingress path renewal requires Redis block enabled and reachable on the server; the **ingress component** uses its own config (e.g. CLI flags: `--renew-intent-enabled`, `--renew-intent-redis-dsn`, `--renew-intent-queue-key`, `--renew-intent-queue-max-len`, `--renew-intent-min-interval`) to connect to Redis and publish intents. Queue key and default list name should match what the server consumer expects (e.g. `opensandbox:renew:intent`).
- Server proxy path can run without Redis.
- Per-renewal extension duration is **not** a server setting: it comes only from sandbox `extensions["access.renew.extend.seconds"]` (set at creation to **300–86400** seconds or creation fails with **400**). Omit the key to disable renew-on-access for that sandbox.
- Docker runtime direct mode remains unsupported regardless of this config.

Create request example:

```json
{
  "image": { "uri": "python:3.11-slim" },
  "entrypoint": ["python", "-m", "http.server", "8000"],
  "timeout": 3600,
  "extensions": {
    "access.renew.extend.seconds": "1800"
  }
}
```

## Test Plan

- **Unit Tests**
  - Extension validation for auto-renew opt-in keys and values
  - Renew eligibility function (validity/cooldown/state checks)
  - In-flight dedupe behavior under concurrent signals
  - Renew target time calculation and monotonicity checks
- **Integration Tests (Server Proxy)**
  - Non-opt-in sandbox never triggers renew under access traffic
  - Opt-in sandbox triggers bounded renew calls under same traffic
  - High-frequency proxy requests only trigger bounded renew calls
  - Proxy request path remains successful when renew path fails transiently
- **Integration Tests (Ingress + Redis)**
  - Non-opt-in sandbox intents are ignored at consumer side
  - Ingress event publish -> worker consume -> renew success
  - Duplicate events for same sandbox are coalesced
  - Redis unavailable path follows best-effort drop semantics
- **Stress Tests**
  - N sandboxes x high QPS access confirms renew call count stays within policy bound

Success criteria:

- Renewal request rate remains proportional to policy limits, not ingress QPS.
- Active sandboxes in supported proxy paths are renewed before expiration under normal operating conditions.

## Drawbacks

- Adds background components and policy tuning complexity.
- Ingress mode introduces hard dependency on Redis availability.
- Conservative gating may skip some renew opportunities under extreme failure conditions.

## Infrastructure Needed

- Redis service for ingress gateway mode.
- Ingress (or gateway) that publishes renew intents (e.g. OpenSandbox Ingress with `--renew-intent-enabled`, Redis DSN, optional queue key / list cap / client-side per-sandbox min-interval throttle).

## Upgrade & Migration Strategy

- Backward compatible and disabled by default.
- Rollout order:
  1. Deploy server with feature flag off.
  2. Enable in server proxy path for canary validation.
  3. Enable ingress + Redis path progressively.
- Rollback:
  - Disable `renew_intent.enabled` (and `renew_intent.redis.enabled` for ingress mode).
  - Existing manual renewal flow remains unchanged.
