---
title: Auto-Pause & Wake-on-Access
description: Let idle Fast Sandbox sandboxes pause themselves after X seconds of inactivity and wake on the next request — opt-in via lifecycle.idlePolicy, with bounded request parking and SDK auto-retry.
---

# Auto-Pause & Wake-on-Access Guide

This guide shows how to enable and operate **auto-pause** and **wake-on-access** for Fast Sandbox (`fsb-`) sandboxes: a sandbox that opts in is paused automatically after **X** seconds without ingress traffic (checkpointed, memory and CPU released), and the next request to it **wakes** it — the gateway parks the request for up to **Y** seconds while the sandbox restores, then forwards normally instead of failing with a permanent `503`.

For the design rationale and internal mechanics, see [Auto-Pause and Wake-on-Access architecture](/architecture/network/auto-pause-wake-on-access).

## How it works in one minute

```text
active    every request through the ingress refreshes activity:{id} in Redis (async, coalesced)
idle      server sweeper (every 15s) checks gates → PauseSandbox → checkpoint → Paused
wake      next request → gateway resumes + parks the request ≤ Y → forwards when Ready
          (budget exhausted → 503 + Retry-After + OpenSandbox-Wake: expired; restore continues)
```

Three parties must agree before anything happens — the server, the ingress, and the sandbox itself. All three default to **off**, and a sandbox that never declares an `idlePolicy` keeps today's behavior exactly: never auto-paused, never auto-woken.

## Prerequisites

- **OpenSandbox full stack v1.2.0 or later.** The lifecycle server, ingress gateway, and SDKs must all be at v1.2.0+; earlier stacks do not accept `lifecycle.idlePolicy` or the wake-timeout contract.
- **Fast Sandbox backend only.** Docker and Kubernetes pod sandboxes are not covered (their manual pause/resume is separate — see [Pause & Resume](/guides/pause-resume)).
- **Traffic through the ingress gateway.** The ingress is the only activity source. Sandboxes reached through the server proxy path (`/sandboxes/{id}/proxy/{port}`, the SDK `useServerProxy` mode) neither record activity nor wake — do not opt those workloads in.
- **Redis reachable from both the ingress and the server** (the same deployment as renew-intents). Wake itself does not use Redis — a Redis outage can only delay pauses, never break wake.

## Step 1 — Enable the sweeper on the server

In the server's `config.toml`:

```toml
[automation.idle_pause]
enabled = true                    # master switch: enables the idle sweeper
sweep_interval_seconds = 15       # detection cadence
activity_ttl_seconds = 1800       # must stay ≥ the largest accepted X (see constraints)

[automation.idle_pause.redis]
dsn = "redis://127.0.0.1:6379/0"  # required when enabled = true
lock_ttl_seconds = 30
```

See [`[automation.idle_pause]` in the server configuration reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md) for all keys.

## Step 2 — Enable the ingress

```text
--activity-enabled=true           # record activity while proxying
--wake-enabled=true               # trigger resume + park requests
```

Relevant flags and defaults:

| Flag | Default | Purpose |
|---|---|---|
| `--activity-enabled` | `false` | Record per-sandbox activity in Redis |
| `--activity-redis-dsn` | `redis://127.0.0.1:6379/0` | Redis for activity writes |
| `--activity-min-interval` | `5s` | Per-sandbox write coalescing |
| `--activity-ttl-seconds` | `1800` | Key TTL — keep equal to the server's `activity_ttl_seconds` |
| `--wake-enabled` | `false` | Resume + request parking |
| `--wake-park-budget` | `5s` | Park budget **Y** (operator-owned latency policy) |
| `--wake-park-max` | `1024` | Parking-lot capacity (held requests) |
| `--wake-restore-max` | `256` | Concurrent restore permits (must be ≤ `--wake-park-max`) |
| `--wake-retry-interval` | `50ms` | First poll interval |
| `--wake-retry-factor` | `1.3` | Poll backoff factor |
| `--wake-retry-jitter` | `0.1` | Poll backoff jitter |
| `--wake-max-flight-lifetime` | `5m` | Cap on one flight's establish+poll phase |

## Step 3 — Opt in sandboxes via `lifecycle.idlePolicy`

Set the policy at create time:

```json
{
  "templateId": "tpl_2f0c...",
  "timeout": 86400,
  "lifecycle": {
    "idlePolicy": {
      "idleTimeoutSeconds": 300,
      "onIdle": "pause",
      "wakeOnAccess": true
    }
  }
}
```

Official SDKs expose typed parameters. Python (`create_from_template` is the fast-sandbox path — `idle_policy` rides alongside the template):

```python
from datetime import timedelta

from opensandbox.models import IdlePolicy
from opensandbox.sandbox import Sandbox

sandbox = await Sandbox.create_from_template(
    "tpl_2f0c...",
    timeout=timedelta(hours=24),
    idle_policy=IdlePolicy(
        idle_timeout_seconds=300,
        on_idle="pause",
        wake_on_access=True,
    ),
)
```

The policy is desired state: validated updates take effect on the next read, without re-creating the sandbox. It is stored as the `sandbox.opensandbox.io/idle-policy` Sandbox CR annotation and hidden from public metadata.

### `idlePolicy` reference

| Field | Type | Default | Meaning |
|---|---|---|---|
| `idleTimeoutSeconds` | int 30–86400 | — | Idle threshold **X**. Required together with `onIdle`. Create is rejected with `400` if `X > activity_ttl_seconds`. |
| `onIdle` | enum | — | Only `pause` today. The field exists so a future action (e.g. `terminate`) stays additive. |
| `wakeOnAccess` | bool | `true` | Within a declared policy, `false` opts out of wake: traffic never resumes the sandbox (permanent `503`), only `POST /sandboxes/{id}/resume` does. |
| `activeProbe` | object | — | App-defined activity probe — see below. **Auto-pause only:** requires `idleTimeoutSeconds` (a wake-only policy is never probed, so the create is rejected with `400`); all three sub-fields required when present. |

**Wake-only policy**: omit `idleTimeoutSeconds` and `onIdle`, keep `wakeOnAccess: true`. The sandbox is never auto-paused, but a (manually) paused sandbox wakes on traffic.

**Omission invariant**: no `lifecycle` or no `idlePolicy` ⇒ never auto-paused and never auto-woken, regardless of server/ingress flags.

### The activity probe

Ingress observation cannot see work that generates no inbound traffic — a request that started a 20-minute in-sandbox computation, a background poller. `activeProbe` lets the application veto such a pause (it is auto-pause only: it must be declared together with `idleTimeoutSeconds`):

```json
"activeProbe": {
  "port": 8080,
  "path": "/active",
  "timeoutSeconds": 2
}
```

When the sweeper is otherwise ready to pause, it issues `GET {path}` on `{port}` through the ingress gateway (sentinel-carried; it never resets the idle clock). Your endpoint answers:

```text
200 {"active": true}    → busy: counts as activity, pause dropped for this sweep (re-probed ~X later)
200 {"active": false}   → idle: pause proceeds; optional "message" lands in the pause audit log
anything else / timeout → no record, candidate dropped this sweep (fail-safe: only an explicit
                          "not active" lets a pause through)
```

::: tip
Use the probe when your workload has meaningful silent work. Without it, such a sandbox **will** be paused; the next request recovers state after restore, but the restore cost is real.
:::

## Wake behavior and SDK retry

A request arriving at a paused, opted-in, `wakeOnAccess: true` sandbox is parked while the gateway resumes it (same-node restore is typically tens of milliseconds; the wake P95 target is < 500 ms):

- **Ready within Y** → forwarded like any other request. For the client, the park is just added latency — no status or body bytes are written during the wait, so keep LB/Envoy idle timeouts and client response-header timeouts above Y.
- **Y exhausted** → the gateway answers:

```text
HTTP/1.1 503 Service Unavailable
Retry-After: 1
OpenSandbox-Wake: expired

{"error": {"code": "sandbox_wake_timeout",
           "message": "restore still in progress; request was never forwarded and is safe to retry",
           "sandboxId": "sb_..."}}
```

The `OpenSandbox-Wake: expired` header is gateway-owned (stripped from every upstream response), so its presence proves the request never reached the sandbox — that is what makes automatic retry safe for any method, including command/code-execution starts, provided the body is replayable. Official SDKs apply this contract to the marked response only:

- Wait per `Retry-After`, resend the original request. On by default, disable-able per client.
- Total wait bound: a wake-retry deadline (default 60 s) and a max-attempts cap (default 5) spanning all wake cycles; retries never reset the deadline.
- Success continues the original call; exhausted budget raises a typed wake-timeout error carrying the attempt count and waited time. A bare `503` (no marker) is never auto-retried for non-idempotent methods.

Python example:

```python
from datetime import timedelta

from opensandbox.config import ConnectionConfig
from opensandbox.transport import RetryPolicy

config = ConnectionConfig(
    retry_policy=RetryPolicy(
        wake_retry_enabled=False,             # disable auto wake-retry
        wake_overall_deadline=timedelta(seconds=120),
        wake_max_attempts=10,
    )
)
```

Exhaustion raises `SandboxWakeTimeoutException`.

## Composition with other features

- **Renew-on-access ([OSEP-0009](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0009-auto-renew-sandbox-on-ingress-access.md))** is independent — one request can feed both (see [Renew-on-Access](/guides/renew-on-access)). Recommended: enable both, so active sandboxes keep extending their expiry while idle ones get reclaimed.
- **Expiry is absolute**: pausing neither stops nor extends `expires_at`. A paused sandbox whose expiry passes becomes `Terminated`; wake then fails fast with `404`.
- **Manual pause/resume** keeps working. Wake treats manually and automatically paused sandboxes identically (unless `wakeOnAccess: false`).

## Constraints checklist

- `X ≤ activity_ttl_seconds` (server + ingress values must match); X floor is 30 s — lower values are rejected to keep the checkpoint cycle worth its cost.
- Everything in front of the ingress (LB idle timeouts, client response-header timeouts) must exceed **Y**, otherwise middle layers cut parked connections before the gateway can answer `503 + Retry-After`.
- Pause timing = X + at most one sweep interval of detection lag.
- Only new HTTP requests count: open WebSocket/SSE streams are not activity — frames (including WS ping/pong) are relayed without being recorded, so application-level WS pings cannot keep a sandbox alive. After X seconds without a fresh request the sandbox pauses and the connection breaks. Keep a background HTTP request cadence shorter than X, declare an `activeProbe`, or do not opt in.
- Scheduled workloads: a paused sandbox cannot wake itself. Arrange an external scheduler to call `POST /sandboxes/{id}/resume` before each run.
- Checkpoint cost is proportional to guest memory; every paused sandbox holds a full checkpoint in the artifact store until resumed or deleted. Size artifact-store capacity for peak concurrent paused sandboxes.

## Operating the feature

- **Rollout order**: deploy with all flags off → enable activity + sweeper for a canary tenant → enable wake on a canary ingress → opt in real workloads via `idlePolicy`.
- **Rollback**: disable the flags. Sandboxes left `Paused` stay resumable through the manual API — resume or delete stranded ones after disabling wake, since traffic will no longer wake them.
- **Key metrics** (see [Telemetry](/guides/telemetry)):
  - `opensandbox.server.idlep.pause.requests` (`outcome` = accepted/gated/failed) — pause pipeline health.
  - `opensandbox.server.idlep.probe.outcome` — sustained `error` ⇒ broken probe endpoint on an opted-in workload.
  - `ingress.activity.writes_dropped` — Redis pressure on the producer side.
  - `ingress.park.shed` — parking-lot overflows; sustained growth ⇒ raise `--wake-park-max` or investigate restore latency.
  - `ingress.park.wait.duration` and `ingress.wake.restore.active` — wake health; same-node wake P95 target < 500 ms.
  - Flap ratio (pauses followed by a quick wake — the primary tuning signal for X) is not emitted as an instrument yet: derive it per sandbox by joining `pause.requests{outcome="accepted"}` with subsequent wake flights. Sustained flapping means traffic period ≈ X — raise X above the workload period.

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| Sandbox never pauses | No `idlePolicy` annotation, traffic through server-proxy mode, or Redis unreachable | Check the sandbox was created with `idlePolicy`; route through the ingress; check server logs for skipped sweeps |
| Sandbox pauses while working | Silent background work invisible to the ingress | Declare `activeProbe` and answer `{"active": true}` while busy |
| Paused-and-expired sandbox returns 404 | Absolute expiry passed while paused | Expected — pausing does not extend expiry; compose with renew-on-access |
| Clients see plain 503 (no marker) on a paused sandbox | `wakeOnAccess: false`, no declared policy, or `--wake-enabled=false` | Check the three activation parties |
| Wake latency above P95 target | Cross-node restore (artifact fetch + full materialization) | Size Y from the cluster's actual resume distribution; checkpoint-affinity scheduling is future work |
| Pauses/wakes flapping | Traffic period ≈ X | Raise X; track pauses followed by a quick wake per sandbox (the flap ratio is derived, not an emitted instrument — see [Monitoring](#operating-the-feature)) |

## Reference

- [Architecture: Auto-Pause and Wake-on-Access](/architecture/network/auto-pause-wake-on-access)
- [OSEP-0024](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0024-auto-pause-and-wake-on-access-for-fast-sandbox.md) — full proposal, alternatives, test plan
- [Fast Sandbox: Pause, Resume, and Snapshots](/architecture/fast-sandbox/checkpoints)
