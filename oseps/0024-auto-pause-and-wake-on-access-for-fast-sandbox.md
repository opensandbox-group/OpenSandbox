---
title: Auto-Pause and Wake-on-Access for Fast Sandbox
authors:
  - "@Pangjiping"
creation-date: 2026-10-09
last-updated: 2026-10-09
status: draft
---

# OSEP-0024: Auto-Pause and Wake-on-Access for Fast Sandbox

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
  - [Scope and Existing Building Blocks](#scope-and-existing-building-blocks)
  - [Activation Model](#activation-model)
  - [Public API Change: `SandboxLifecycle.idlePolicy`](#public-api-change-sandboxlifecycleidlepolicy)
  - [Activity Tracking (Ingress to Redis)](#activity-tracking-ingress-to-redis)
  - [Idle Sweeper and Pause Gates (Server)](#idle-sweeper-and-pause-gates-server)
  - [Pause Flow and State Mapping](#pause-flow-and-state-mapping)
  - [Wake-on-Access: Resume Trigger and Request Parking](#wake-on-access-resume-trigger-and-request-parking)
  - [Server Proxy Path (Mode A)](#server-proxy-path-mode-a)
  - [Wake Timeout Response and SDK Retry Contract](#wake-timeout-response-and-sdk-retry-contract)
  - [Interaction with Renewal, Expiry, and Manual Pause](#interaction-with-renewal-expiry-and-manual-pause)
  - [Configuration](#configuration)
  - [Observability](#observability)
  - [Security Considerations](#security-considerations)
- [Construction Phases](#construction-phases)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Introduce an ingress-driven lifecycle automation loop for Fast Sandbox (`fsb-`) sandboxes: **auto-pause** and **wake-on-access**. When a sandbox that explicitly opts in receives no ingress traffic for a continuous idle threshold **X**, the server pauses it through fast-sandbox's FastPath `PauseSandbox` (checkpoint to the artifact store, release the runtime, memory and CPU drop to zero). When a request later arrives for a paused sandbox, the ingress gateway (or the server proxy path) triggers `ResumeSandbox` and **parks** the request — holding it for up to a wait budget **Y** while the sandbox restores — then resolves the route and forwards normally instead of failing with 503.

The design reuses what already exists: fast-sandbox's desired-state pause/resume RPCs, the OpenSandbox lifecycle `Pausing/Paused/Resuming` states, and the OSEP-0009 three-party activation and Redis signaling pattern. Both features are off by default. The public contract changes are additive only: the optional `lifecycle.idlePolicy` create field (see [Public API Change](#public-api-change-sandboxlifecycleidlepolicy)) and the documented wake-timeout response marker (see [Wake Timeout Response and SDK Retry Contract](#wake-timeout-response-and-sdk-retry-contract)); absent both, behavior is unchanged.

Scope is the **fast-sandbox backend only**. Pod-backed sandboxes (Docker / Kubernetes) are not touched in this revision; extending the same idle-pause/wake model to them is a candidate for a future revision (see Non-Goals and Alternatives).

## Motivation

Fast Sandbox sandboxes are created in milliseconds but then occupy their full CPU and memory allocation for as long as they live, including the idle tail. Agent workloads are idle most of the time: an interactive coding session has seconds of activity followed by minutes of silence; a spawned subagent may receive one request every few minutes. At pool scale, idle sandboxes dominate the memory footprint and cap the density a Fastlet pool can host.

fast-sandbox already has the hard part — a full checkpoint (`vmstate` + rootfs) published to the artifact store, with local-node restore measured in tens of milliseconds. What is missing is the orchestration loop that turns those primitives into an automatic cycle:

- **No idle trigger.** A sandbox stays `Running` until its absolute expiry, no matter how long it has been quiet. FastPath `PauseSandbox` exists (`fast-sandbox/internal/controlplane/fastpath/sandbox_state.go`) but nothing calls it automatically.
- **Paused sandboxes are dead ends on the data path.** `ResolveEndpoint` returns `FailedPrecondition` ("Sandbox is paused; call ResumeSandbox to resume it") for a paused sandbox, which the ingress provider maps to `ErrSandboxNotReady` and serves as a permanent 503 (`components/ingress/pkg/sandbox/fastsandbox_provider.go`, `components/ingress/pkg/proxy/errors.go`). The only recovery is a manual `POST /sandboxes/{id}/resume`.

Idle-suspend with wake-on-request is the natural operating model for agent sandboxes: suspend when idle, resume **on incoming traffic** at the routing layer, and park the request for a bounded budget instead of failing it. With fast-sandbox's local restore latency, this model should yield sub-second wakeups on OpenSandbox.

### Goals

- Pause an opted-in `fsb-` sandbox automatically after **X** seconds of continuously observed ingress inactivity.
- Serve a request to a paused `fsb-` sandbox by triggering resume and parking the request for up to **Y** seconds, then forwarding once the sandbox is `Running` and the route resolves.
- Keep the wake path free of new synchronous dependencies: the ingress resumes directly over its existing FastPath gRPC connection; Redis is used only for activity tracking on the pause side.
- Collapse concurrent wakeups for the same sandbox into a single resume flight (singleflight); never cancel an in-flight resume when the park budget expires.
- Compose with OSEP-0009 renew-on-access and with manual pause/resume; preserve all existing lifecycle semantics and SDK contracts.
- Ship disabled by default with a three-party activation model (server config, ingress flags, sandbox `lifecycle.idlePolicy`), mirroring OSEP-0009.

### Non-Goals

- Supporting auto-pause/wake for the Docker or Kubernetes pod backends in this revision (the pod backend's manual pause/resume is OSEP-0008). A pod-backend idle-pause/wake equivalent is a plausible future revision, but pod pause economics (minutes-scale rootfs commit vs tens-of-ms VM restore) differ enough to warrant a separate design.
- Detecting idle from **egress** traffic or from in-guest signals the application does not declare (background jobs, timers). Ingress traffic is the only automatic observation source; `idlePolicy.activeProbe` is the single app-declared exception (see [Public API Change](#public-api-change-sandboxlifecycleidlepolicy)).
- Tracking open WebSocket/SSE connections as activity. Only new requests count (see [Caveats](#notesconstraintscaveats)).
- Changing pause/resume semantics of fast-sandbox itself (checkpoint format, CRDs, restore pipeline).
- Changing the expiry model: pausing does not stop or extend the absolute `expires_at` clock (see [Interaction](#interaction-with-renewal-expiry-and-manual-pause)).
- A generic lifecycle event bus or a new message queue.
- Per-sandbox override of the park budget **Y** (deferred; Y is an operator-level setting in this revision).

## Requirements

- Only sandboxes that explicitly opt in at create time via `lifecycle.idlePolicy` may be auto-paused. Absence of the field is a hard no-op.
- A sandbox may declare an application-defined activity probe (`idlePolicy.activeProbe`); a busy answer is recorded as ordinary activity. The pause decision remains a pure function of the activity key — the probe introduces no second decision path.
- The idle threshold **X** is per sandbox and carries its own opt-in; the park budget **Y** is deployment configuration.
- Activity recording must never add synchronous latency or failure modes to the proxy path: writes are fire-and-forget and dropped on error.
- The wake path must not depend on Redis. It uses the FastPath connection the ingress already holds.
- Multi-replica safety: activity state is shared through Redis; pause decisions are fenced by a per-sandbox distributed lock; resume decisions converge through FastPath's compare-and-set semantics without a coordinator.
- If Redis is unavailable, the system must fail toward **not pausing** (liveness of workloads is never at risk); wake continues to work because it does not touch Redis. A missing activity key — from a dropped write or from Redis restart/eviction/flush — must be treated as **unknown activity, never as proof of idleness**: it may delay a pause, never accelerate one.
- A request racing a pause on a route-cache hit must be recoverable without double execution: replay authority is split by whether the request was provably never forwarded — parked (pre-forward) requests replay with any reproducible body; post-forward stale-route re-entry retries only idempotent methods with reproducible bodies, and everything else is answered 503 rather than silently re-executed.
- A paused sandbox that has passed its expiry must never be resumed; wake fails fast with 404.
- Budget exhaustion must not cancel an already-started resume: the restore is desired state and converges regardless; the request gets 503 with `Retry-After`, and later requests find a `Running` sandbox.
- Concurrent requests to the same paused sandbox must produce at most one concurrent `ResumeSandbox` call per ingress replica (one flight), with all requests sharing its outcome.
- The number of concurrently parked requests per ingress replica is bounded by the parking lot (`--wake-park-max`); concurrent restore flights are bounded **separately** by restore permits (`--wake-restore-max`), held to flight terminal. A new flight may not start without a parking slot **and** a restore permit. Overflow is shed with 503 **before** any `ResumeSandbox` is issued.
- Two public contract surfaces, both additive: the optional `lifecycle.idlePolicy` create field (see [Public API Change](#public-api-change-sandboxlifecycleidlepolicy)) and the documented wake-timeout response (503 + marker) on a path that does not exist today (see [Wake Timeout Response and SDK Retry Contract](#wake-timeout-response-and-sdk-retry-contract)). No changes to existing routes, response models, or defaults. `GET /sandboxes/{id}` already exposes `Pausing`/`Paused`/`Resuming`; this OSEP only makes those states reachable automatically.

## Proposal

Two mechanisms, one observation point — client traffic and the activity probe alike transit the ingress gateway:

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant IG as Ingress Gateway
    participant R as Redis
    participant S as Server (idle sweeper)
    participant FP as Fast-Path (fast-sandbox)
    participant VM as Sandbox runtime

    rect rgb(235, 245, 255)
    Note over C,VM: active phase
    C->>IG: requests (exec / file / user ports)
    IG->>VM: proxy
    IG--)R: SET activity:{id} = now_ms EX ttl (async, coalesced)
    end

    rect rgb(245, 245, 245)
    Note over C,VM: idle phase (no ingress traffic for X)
    S->>R: read last_active
    S->>S: gates: opt-in / Running / idle >= X (missing key =<br/>unknown, tombstoned) / not expired / lock / re-verify
    opt activeProbe declared
        S->>IG: GET {path} on sandbox host (sentinel header:<br/>skip activity recording)
        IG->>VM: proxy to probe endpoint
        VM-->>IG: {"active": true | false}
        IG-->>S: probe response
        alt active = true
            S--)R: SET activity:{id} = now_ms (a busy answer IS activity)
            Note over S: candidate dropped for this sweep;<br/>clock reset → probed again at next idle window
        else active = false or probe failed
            Note over S: failed/timeout → no record, drop for this sweep
        end
        S->>R: re-verify (a real request may have landed<br/>while the probe ran)
    end
    S->>FP: PauseSandbox (desired state, idempotent CAS)
    FP->>VM: checkpoint, then release runtime
    Note over FP,VM: Pausing -> Paused (CRD retained, footprint zero)
    end

    rect rgb(235, 255, 235)
    Note over C,VM: wake phase (next request)
    C->>IG: request
    IG->>FP: ResolveEndpoint -> FailedPrecondition (paused)
    IG->>FP: GetSandbox -> Paused / Pausing / Resuming
    IG->>FP: ResumeSandbox (singleflight, async intent)
    loop park until Ready or budget Y
        IG->>FP: GetSandbox / ResolveEndpoint (bounded backoff)
    end
    alt Ready within budget Y
        IG->>VM: forward
        IG-->>C: response
    else budget Y exhausted
        IG-->>C: 503 + Retry-After: 1 (resume NOT canceled)
    end
    end
```

- **Auto-pause (X).** The ingress records per-sandbox last-activity timestamps in Redis as a side effect of proxying (async, coalesced). A server-side idle sweeper periodically scans opted-in `fsb-` sandboxes, verifies inactivity against the threshold, and issues FastPath `PauseSandbox`. fast-sandbox checkpoints the sandbox and releases its runtime; the CRD is retained and the lifecycle state becomes `Pausing → Paused`.
- **Wake-on-access (Y).** When route resolution hits a paused sandbox, the gateway triggers `ResumeSandbox` (an asynchronous desired-state patch in fast-sandbox) and parks the request: it retries resolution with bounded backoff until the sandbox is `Running` and the route resolves, or the park budget **Y** elapses. Concurrent requests for the same sandbox join one resume flight. On success the request is forwarded exactly like any other; on budget exhaustion the client receives a retryable 503 while the restore continues in the background.

### Notes/Constraints/Caveats

- **Ingress observation is near-complete for fast-sandbox.** Sandboxes have private IPs and are reachable only through the gateway proxy chain, so exec, file transfer, and every user port transit the ingress — one observation point covers all client-driven activity. What it cannot see is activity that originates inside the sandbox with no inbound traffic (a background process polling an external API, or a request that started a long in-sandbox computation). Such a sandbox **will be paused** — unless it declares `idlePolicy.activeProbe` and the application answers busy while its work runs (see Activity Tracking). Opt-in is still the control: workloads with meaningful background work should either use the probe or not enable the feature. A future revision may add execd/egress-handler activity signals as complementary sources.
- **Silent long-lived connections.** An open WebSocket or SSE stream generates no new requests; after **X** seconds of frame silence the sandbox is paused and the connection breaks. This is the sharpest edge of ingress-only observation. Workloads using long-lived connections should send application-level pings more frequently than X, or not opt in. Connection-aware activity (counting live upstream connections as activity) is a possible future extension, not part of this revision.
- **Wake applies to any paused `fsb-` sandbox whose policy allows it.** Wake gates on the sandbox's own `idlePolicy.wakeOnAccess` (default `true`), not on who paused: a sandbox paused by the idle sweeper or manually via `POST /sandboxes/{id}/resume` is treated uniformly as *hibernation reachable by traffic* — unless it declared `wakeOnAccess: false`, in which case traffic never wakes it and only the resume API does. The finer "who paused me" distinction remains deferred.
- **Pausing is not free.** Checkpoint dump and artifact upload take time proportional to guest memory and consume artifact-store capacity for every paused sandbox. The idle threshold floor (30 s) and the flap metrics exist to keep the cycle worth its cost. fast-sandbox's `Pausing` phase is cancelable: a wake arriving during the checkpoint cancels the pause intent instead of waiting for it to finish, which bounds the damage of a wrongly-timed pause.
- **Local-node restore is the fast path.** A resume that lands on a different node pays artifact fetch and full rootfs/materialization cost (currently seconds for multi-GiB guests). This OSEP does not change scheduling; checkpoint-affinity scheduling is fast-sandbox future work. The wake budget Y must be sized with the cluster's actual resume distribution in mind.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Sandbox paused while doing invisible background work | Opt-in only; documented limitation; `idlePolicy.activeProbe` lets the application veto while work runs (busy answer = activity); wake-on-next-request recovers state; future execd/egress activity signals |
| Pause/resume flapping under periodic "heartbeat" traffic with period ≈ X | Idle floor 30 s; flap metrics (pause followed by wake within a short window); operators tune X above workload period |
| Wake storm: burst of requests to many paused sandboxes | Per-sandbox singleflight collapses fan-out; parking admission precedes flight creation (lot bounds held requests) and a restore permit held to flight terminal bounds concurrent restores; resume RPC is a cheap CAS patch |
| Redis outage or data loss (restart, eviction, flush) | Sweeper skips runs when Redis is unreachable; a missing activity key is treated as unknown and starts a tombstone, so data loss can only **delay** pauses, never accelerate them; wake does not use Redis |
| Producer-side silent write failure (an ingress replica partitioned from Redis; server reads stay healthy) | Bounded, self-healing: at most one spurious pause per stale window, recovered by wake-on-access, no data loss (see "Trust boundary" under the sweeper design); per-replica `activity.writes_dropped` alerting; automatic gating (writer watermark) deferred — see Alternatives |
| Two server replicas race to pause the same sandbox | Distributed lock `SET NX EX` per sandbox; FastPath `PauseSandbox` is idempotent on replay, so a lost race is harmless |
| Resume racing a re-pause | `expected_checkpoint_id` fence returns `Aborted` on checkpoint change; wake retries within its budget |
| Requests parked too long on a slow restore (cross-node) | Budget Y bounds client wait; 503 + `Retry-After` on exhaustion; the in-flight restore is never canceled, so the retry succeeds quickly |
| Ingress gains write authority over sandbox state | Scoped to `ResumeSandbox` (plus `GetSandbox` reads) on the tenant namespace it already routes for; pause stays a server-side authority; no delete/create authority; FastPath network is cluster-internal as today |
| Artifact-store growth from many checkpoints | Capacity planning noted in Infrastructure; delete of a sandbox removes its checkpoint (existing fast-sandbox behavior) |

## Design Details

### Scope and Existing Building Blocks

Everything in the table below exists today; this OSEP composes them and specifies the missing glue.

| Building block | Where | Role here |
| --- | --- | --- |
| FastPath v2 `PauseSandbox` / `ResumeSandbox` | `fast-sandbox/api/proto/v2`, `internal/controlplane/fastpath/sandbox_state.go` | Desired-state CAS patches (`spec.state` Paused/Running); idempotent replays; `expected_checkpoint_id` fence (`Aborted` on change); resume before checkpoint release cancels the pause |
| Paused route resolution failure | `ResolveEndpoint` → `FailedPrecondition` ("Sandbox ... is paused; call ResumeSandbox") | The wake trigger signal |
| Ingress FastPath provider | `components/ingress/pkg/sandbox/fastsandbox_provider.go` | Already holds the gRPC connection and route cache; gains pause-awareness and the wake flight |
| Lifecycle states | OSEP-0007 status mapping; `Pausing/Paused/Resuming` in `RuntimeState` | `GET /sandboxes/{id}` already renders the new states; no API change |
| Server pause/resume for fsb | `FastSandboxService.pause_sandbox/resume_sandbox`, `FastPathClient` | Manual API already works; automation reuses the same client |
| Renew-on-access signaling | OSEP-0009; `components/ingress/pkg/renewintent`; server `[renew_intent]` | The activation and Redis patterns are copied; independent gates |

This OSEP also implicitly amends OSEP-0007's non-goal "no pause/resume on fast-sandbox": the FastPath RPCs, server client methods, and status mapping have since landed, and OSEP-0007's text is stale on this point (tracked separately). This proposal does not otherwise revise OSEP-0007.

### Activation Model

Three-party activation, mirroring OSEP-0009. Auto-pause and wake are independently switchable but designed to be enabled together.

1. **Server**: `[idle_pause] enabled = true` (sweeper + activity registry).
2. **Ingress**: `--activity-enabled=true` (record activity) and `--wake-enabled=true` (park + resume).
3. **Sandbox**: declares its idle behavior at create time via the structured `lifecycle.idlePolicy` field — the single opt-in channel (see [Public API Change](#public-api-change-sandboxlifecycleidlepolicy)).

**Meaning:** the sandbox may be auto-paused after `X` consecutive seconds without ingress-observed traffic. The park budget **Y** is not a sandbox field: it is `--wake-park-budget` on the ingress and `resume_budget_seconds` in the server config for the proxy path. It bounds client-perceived wait and is owned by the operator.

### Public API Change: `SandboxLifecycle.idlePolicy`

The API schema change in this revision is one new optional field on the existing create-time `SandboxLifecycle` object (the OSEP-0020 hooks container); the wake-timeout response marker ([Wake Timeout Response and SDK Retry Contract](#wake-timeout-response-and-sdk-retry-contract)) is the second contract surface, on a response that does not exist today:

```yaml
lifecycle:
  idlePolicy:
    idleTimeoutSeconds: 300   # X; integer 30–86400
    onIdle: pause             # enum; only "pause" in this revision
    wakeOnAccess: true        # default true
    activeProbe:              # optional; presence enables the app-defined activity probe
      port: 8080
      path: /active           # app-owned endpoint answering {"active": bool, "message"?: string}
      timeoutSeconds: 2       # 1–10, default 2
```

Field semantics:

- **`idleTimeoutSeconds`** (required when `idlePolicy` is present): the idle threshold **X**. Integer 30–86400, validated in the HTTP API layer. Constraint `X ≤ activity_ttl_seconds` (server config, default 1800); larger values are rejected with 400 so that "activity key missing" can always imply "idle for at least X" (see below).
- **`onIdle`**: enum, `pause` only today. The field exists so a future action (e.g. `terminate`) is additive.
- **`wakeOnAccess`**: default `true`. `false` opts the sandbox out of wake-on-access: once paused — manually or automatically — traffic never triggers `ResumeSandbox`; requests keep today's paused behavior (permanent 503 through the gateway), and only `POST /sandboxes/{id}/resume` restores it.
- **`activeProbe`** (optional): lets the application veto a pause that ingress observation alone cannot see — a request that started a 20-minute computation and generated no further traffic. When the sweeper is otherwise ready to pause, it issues `GET {path}` on `{port}`; `200 {"active": true}` is recorded **as an activity observation** (same key, same monotonic max) — a busy answer simply resets the idle clock. `{"active": false}`, any non-200, malformed body, timeout, or connection error records nothing. An optional `message` string on a not-active answer travels no further than the structured pause log: when the pause proceeds, it is emitted in the audit line (length-capped), giving operators the application's own account of why the sandbox was idle. The sweeper's decision logic is unchanged: it still only reads the activity key; the probe is just another producer of observations (pull-mode, see Activity Tracking). All three fields are required when `activeProbe` is present.
- **Omission**: no `lifecycle` or no `idlePolicy` preserves today's behavior exactly — never auto-paused. Wake-on-access still applies by default (wake is not gated on declaring a pause policy: a manually paused sandbox without any policy is still reachable by traffic — see Caveats).

Mechanics and compatibility:

- **Wire and storage**: the server validates the field at create and persists it under a fast-sandbox-reserved metadata key, hidden from public metadata/list — the same convention as the renew extension. The sweeper and the ingress read this reserved metadata through the existing `GetSandbox`, so the internal contract is unchanged; only the public create request gains a typed field.
- **Additive and backward compatible**: one new optional field on an existing object; no existing field, route, response model, or default changes. Official SDKs gain typed parameters (`IdlePolicy(idle_timeout_seconds=300, on_idle="pause", wake_on_access=True)` in Python, equivalents elsewhere), and docs are updated.
- **Naming**: `idlePolicy` is deliberately policy, not a hook — the container's two semantics stay apart: hooks (`preStart`, `periodic`) run commands inside the sandbox; policy (`idlePolicy`) tells the control plane what to do with the sandbox's state machine.

### Activity Tracking (Ingress to Redis)

The ingress updates one key per sandbox as a side effect of proxying:

- **Key**: `opensandbox:activity:{sandbox_id}`, value = Unix milliseconds of the observation. The prefix is a code-level convention fixed identically on the ingress and the server — not a deployment knob: a mismatch between the two sides can only delay pauses (via tombstones), never corrupt decisions.
- **Write**: monotonic max update (Lua: overwrite only when the new timestamp is greater than the stored one), `EX <activity_ttl_seconds>`, issued asynchronously (buffered channel + background pipeline) after a request is routed — never inline on the hot path. Failures are logged and dropped. Monotonic max keeps the key at the latest observation even when buffered writes from different replicas arrive out of order.
- **Coalescing**: at most one write per sandbox per `--activity-min-interval` (default 5 s), bounding Redis write rate to ≤ 0.2 writes/s per active sandbox per replica regardless of request rate. The coalescing lag — a pause may fire up to one interval early against the recorded activity — is noise against the X ≥ 30 s floor.
- **Coverage**: every proxied request counts — exec and file calls on raw port 44772, user ports, health pings. Any port wakes the idle clock. The `access renew skip` sentinel header (OSEP-0009) does **not** suppress activity tracking: skipping a renewal is not skipping activity.
- **TTL semantics**: the key expires `activity_ttl_seconds` after the last write; `X ≤ activity_ttl_seconds` is enforced at create time, so a key that is present but stale by ≥ X is already idle **by value**. A **missing key is not a signal**: asynchronous writes can be dropped, and Redis can restart, evict, or flush while remaining reachable, so absence cannot distinguish "quiet for X" from "data lost". Missing keys are resolved by the sweeper's unknown-state tombstone below.
- **Missing-key tombstone**: the first time the sweeper observes the activity key missing, it records `SET opensandbox:idlep:unknown:{sandbox_id} <now_ms> NX EX <activity_ttl_seconds + sweep_interval_seconds>`. The sandbox becomes idle-eligible only once the tombstone itself has existed for ≥ X — inactivity is measured from the first *observation* of unknown state. Redis data loss therefore can only **delay** a pause by up to one unknown window, never accelerate one. The tombstone is cleared when the activity key reappears and expires by TTL otherwise.
- **Producers**: the ingress gateway (all sandbox traffic) and the server proxy path (`/sandboxes/{id}/proxy/{port}`, which bypasses the ingress) write the same keys, so both access paths keep sandboxes awake.

On create and on every successful resume, the actor performing the operation (server for create/resume API, ingress for wake flights) writes an immediate activity observation, so the idle clock starts from the latest lifecycle transition rather than from nothing.

**Pull producer: the activity probe.** Ingress and server-proxy writes are push-mode — driven by traffic. A sandbox that declares `idlePolicy.activeProbe` adds a pull-mode producer: when the sweeper is otherwise ready to pause it, the sweeper asks the sandbox itself whether work is in progress, and a busy answer is recorded exactly like any other observation (`200 {"active": true}` → the standard monotonic-max write). This closes ingress observation's one blind spot — a request that started a long in-sandbox task and generated no further traffic — without giving the sweeper a second decision input: the pause pipeline still reads only the activity key; the probe only creates observations. Mechanics:

- **Path**: the probe is an ordinary `GET` issued **through the ingress gateway** — the same routing, addressing, and TLS as client traffic — carrying the internal sentinel header `X-OpenSandbox-Skip-Activity`. The recorder skips the write for sentinel-carried requests, so the probe itself never resets the idle clock (otherwise every probe would keep the sandbox alive forever). The sentinel is honored only on the ingress's internal listener; the external listener strips it, so a client cannot suppress recording of its own traffic.
- **Semantics**: `{"active": true}` → record `now_ms` (clock reset; the candidate is dropped for this sweep and probed again at its next idle window — a continuously busy sandbox sees one probe per ~X). `{"active": false}` → no record; the idle clock keeps aging and the pause proceeds. Anything else — non-200, malformed body, timeout, connection error → no record, candidate dropped for this sweep, `idlep.probe.outcome` counter incremented. The fail-safe direction matches the rest of the design: a timeout may be an app too busy to answer (exactly the case the probe protects), so only an explicit "not active" lets a pause through. When the pause proceeds, the structured pause log line embeds the probe verdict and the answer's `message` (length-capped) as the auto-pause audit trail — the application's own account of why it was idle.
- **Racing a real request**: the probe spans up to `timeoutSeconds`, widening the window between the lock-scoped re-verify and `PauseSandbox`. A second re-verify after the probe (one `MGET` under the same lock) closes it back to sub-flush-latency: a request that landed while the probe ran drops the candidate.

### Idle Sweeper and Pause Gates (Server)

A server background task runs every `sweep_interval_seconds` (default 15 s):

1. Enumerate candidates per tenant namespace with FastPath `ListSandboxes` (paginated with continue tokens, same handling as OSEP-0007; filtered to `Running` client-side). Then batch-read the candidates' activity keys from Redis (`MGET`) — the cheap shared read always happens before any per-sandbox FastPath `GetSandbox`. The sandbox list, not a Redis key scan, must be the enumeration source: a sandbox whose activity key is absent is in the unknown state that needs tombstone handling, and a key scan would make it invisible.
2. For each candidate, evaluate the gates **in order**; first failure drops the candidate silently for this sweep:
   - **Opt-in**: reserved metadata carries a valid idle policy (translated from the create-time `lifecycle.idlePolicy` field; read via FastPath `GetSandbox` — only fetched for candidates that already look idle, so the per-sweep RPC count stays low).
   - **State**: sandbox is `Running` (Runtime + DataPlane Ready). Anything else is skipped — `Pausing`/`Paused` need no action, `Resuming` is protected, terminal states are left to their own paths.
   - **Idle**: activity key present and `now − last_active ≥ X` → idle; stale tombstone (`opensandbox:idlep:unknown:{sandbox_id}` aged ≥ X) → idle. Key missing with no tombstone or a fresh tombstone → **unknown, skip this sweep** (creating or keeping the tombstone). Never pausing from a missing key alone is the fail-safe invariant.
   - **Expiry**: `expires_at > now`. An expired sandbox is not paused; the expiry path owns it.
   - **Lock**: `SET opensandbox:idlep:lock:{sandbox_id} <replica> NX EX <lock_ttl>` (default 30 s). Failure means another replica is handling it — drop.
   - **Re-verify under lock**: re-read the activity key; if a newer observation appeared between the idle check and the lock (a request landed meanwhile), release interest and drop for this sweep.
   - **Activity probe** (only when `idlePolicy.activeProbe` is declared): issue the probe per Activity Tracking. A busy answer records an observation and drops the candidate for this sweep; an explicit `{"active": false}` proceeds; any failure drops the candidate (outcome counted).
   - **Re-verify after probe**: for candidates that reached this point with a probe declared, re-read the activity key once more — the probe spans seconds, and a request landing during it may not have flushed at the first re-verify. A newer observation drops the candidate.
3. Issue `PauseSandbox` with an idempotent `request_id` (`idlepause-{sandbox_id}`) and the observed generation. Outcomes: accepted → record `paused_at`, increment metrics; the structured log line carries the auto-pause audit context — threshold X, last observation age, and, when a probe is declared, its verdict and `message` (length-capped). `FailedPrecondition` (state changed under us) → drop, next sweep re-evaluates; lock released by TTL.

**Idle-window semantics across replicas.** The window is defined over shared state, not per-replica local state. The **start point** is the shared `activity:{sandbox_id}` key: every ingress replica (and the server proxy path) applies a monotonic max update with its own observation time, so the start is the latest observation across all replicas (modulo each writer's clock skew against the others). The **end point** is the evaluating replica's local `now` — the replica whose clock crosses the threshold first wins the lock and pauses, so the effective threshold is X ± max(writer skew, evaluator skew). Both skews are bounded by cluster NTP synchronization (typically tens of milliseconds) against an X floor of 30 s, and are therefore noise. The race that matters is not clock skew but the **async activity write**: a request can land on one ingress replica just after another replica's sweeper has judged the sandbox idle and before the activity write flushes. The lock-scoped re-verify above narrows this to sub-flush-latency races; the residual case is a request racing the pause on a **route-cache hit**, which cannot see the pause at resolution time. That path is recovered by the stale-route re-entry specified in Wake-on-Access (wake + one retry under the replay rules); a request already streaming a response is covered by the long-lived-connection caveat instead.

If Redis is unreachable the sweep is skipped entirely (no pauses). Pause failures of any kind leave the sandbox `Running` — the worst case of this subsystem is a missed pause, never a lost sandbox.

**Trust boundary of that invariant.** It covers Redis-side failures — faults the sweeper can see: unreachable Redis (sweep skipped), and restart/eviction/flush (missing key ⇒ tombstone, pause delayed). It does **not** cover a producer-side silent failure: an ingress replica partitioned from Redis keeps serving requests whose activity writes are dropped, so its sandbox's key ages past X and the sweeper takes one spurious pause of an active sandbox. The damage is bounded and self-healing — the next request wakes and is served after restore, with no data loss (checkpointing is safe at any instant) — but a persistent partition with sparse traffic repeats the pause↔wake cycle at the request rate. Detection is operational (per-replica `activity.writes_dropped` alerting); automatic admission gating is deferred (see Alternatives).

Sweep cost per namespace per interval is one paginated list plus a small number of `GetSandbox` calls (only for idle candidates), rare `PauseSandbox` calls, and one probe GET per otherwise-eligible candidate that declares `activeProbe`. For pools with thousands of opted-in sandboxes the Redis-first ordering keeps FastPath load negligible.

**Multi-replica sweep (no leader election).** Every server replica runs its own sweeper on its own timer; there is no leader lease. Deduplication happens at the point of effect: the per-sandbox `SET NX EX` lock admits exactly one replica to issue `PauseSandbox` per idle episode — losers drop the candidate for that sweep. Correctness never depends on the lock: FastPath `PauseSandbox` is a compare-and-set desired-state patch whose replay on an already-paused sandbox is an idempotent success, so a lost race (lock expiry mid-call, replica crash) converges to a single pause anyway, and a pause racing a user-initiated resume is rejected through the `FailedPrecondition`/generation fence and re-evaluated on the next sweep. A replica crashing mid-sweep leaves no stuck state: the lock expires by TTL and any replica's next sweep re-evaluates the candidate. The accepted cost is N× duplicated candidate scanning (namespace lists + Redis reads across replicas); at current scale Redis-first filtering keeps this cheap, and if sweep load ever shows in metrics it can be replaced by namespace sharding or a leader lease without changing pause semantics.

### Pause Flow and State Mapping

```mermaid
stateDiagram-v2
    [*] --> Running
    Running --> Pausing : idle >= X, gates pass, PauseSandbox
    Pausing --> Paused : checkpoint durable, runtime released
    Pausing --> Running : wake arrives, resume cancels pause
    Paused --> Resuming : wake-on-access, ResumeSandbox
    Resuming --> Running : runtime + dataplane Ready
    Paused --> Terminated : expiry passes (CRD retained)
    Running --> Terminated : delete / expiry
```

Timeline of one episode: `t0` last ingress request observed (activity key written); `t0+X` sweeper gates pass, `PauseSandbox(spec.state=Paused)`; the fast-sandbox controller checkpoints and publishes artifacts (`Pausing`); `t0+X+Δ` checkpoint durable, runtime released (`Paused`, CRD retained) — memory/CPU footprint zero, artifact set (`vmstate` + rootfs manifest) in the store.

The mapping to public states is exactly OSEP-0007's existing table (`Pausing → Pausing`, `Paused → Paused`, retained expired CR → `Terminated`). `GET /sandboxes/{id}` on a paused sandbox returns 200 `Paused` with the retained CRD as source of truth; list includes paused sandboxes.

### Wake-on-Access: Resume Trigger and Request Parking

The wake path lives in the ingress FastPath provider, next to route resolution.

```mermaid
sequenceDiagram
    autonumber
    participant C1 as Client A
    participant C2 as Client B..N
    participant IG as Ingress replica
    participant SF as Flight registry (per replica)
    participant FP as Fast-Path
    participant VM as Sandbox runtime

    C1->>IG: request
    IG->>FP: ResolveEndpoint -> FailedPrecondition (paused)
    IG->>FP: GetSandbox -> Paused / Pausing / Resuming
    IG->>SF: join-or-start flight (namespace, sandbox_id)
    alt flight owner (first request)
        SF->>FP: ResumeSandbox (expected_checkpoint_id fence, async)
        FP-->>SF: accepted
        loop poll until Ready or budget Y (50ms x1.3 + jitter)
            SF->>FP: GetSandbox
        end
    else concurrent requests
        C2->>IG: request (joins flight, shares remaining budget)
    end
    alt Ready within budget
        IG->>VM: forward (also late joiners)
        IG-->>C1: response
        IG-->>C2: response
    else budget exhausted
        IG-->>C1: 503 + Retry-After: 1 (flight NOT canceled)
        IG-->>C2: 503 + Retry-After: 1
    end
```

**Detection.** On a route miss or a resolution error, before answering 503 the provider calls `GetSandbox` — the FastPath v2 RPC on the fast-sandbox Fast-Path Server, over the same gRPC connection and endpoint the provider already uses for `ResolveEndpoint` (not the Kubernetes API; Fast-Path resolves the Sandbox CR through its informer-backed cache). The `FastPathResolver` interface gains one method; the hot path (route-cache hit) issues no new RPC.

| Runtime state | Behavior |
| --- | --- |
| `Paused` | Start/join a **resume flight**, park |
| `Pausing` | Start/join a resume flight (fast-sandbox cancels the pause intent before the checkpoint releases — no wait for the checkpoint) |
| `Resuming` | Join the in-flight state (poll), park |
| `Ready` (transient race) | Re-resolve and forward |
| Terminal / expired (`expires_at ≤ now`, retained CR → `Terminated`) | Fail fast 404 — never resume |
| Deletion in progress | Fail fast per current mapping |
| `NotFound` | Fail fast 404 (unchanged) |

A sandbox whose policy declares `wakeOnAccess: false` is exempt from every row above: its paused state keeps today's behavior (permanent 503), and no `ResumeSandbox` is ever issued for it. The flag rides the same `GetSandbox` call (reserved metadata), so gating costs no extra RPC.

**Stale-route re-entry (route-cache hits).** A request forwarded on a cached route does not resolve again, so a pause landing between the cache hit and the forward surfaces later as a stale upstream response (`X-Fast-Sandbox-Proxy-Error` — the existing stale-route signal). On that error the provider already invalidates the cache; it now additionally calls `GetSandbox` and re-enters the wake path when the state is `Paused`/`Pausing`/`Resuming`, retrying the request once after the flight resolves.

**Replay authority: pre-forward vs post-forward.** A reproducible body proves a request can be re-sent; it does not prove the sandbox never executed it. The gateway therefore splits replay authority by the one fact it can prove — whether the request bytes were ever sent upstream:

- **Pre-forward (wake parking).** Parking happens before any upstream dial, so a parked request is provably never executed, and the forward after the flight resolves is the **first** forward. Any method parks here as long as the request is reproducible — no body, or a body fully buffered within the capture cap. A buffered exec-start `POST` is safe precisely because it has not been sent.
- **Post-forward (stale-route re-entry).** The stale-route signal surfaces after the request bytes reached the upstream chain, and "executed, then disconnected before responding" is indistinguishable from "forwarded but not executed" — the gateway treats the request as forwarded. Re-entry retries **once** only when the method is idempotent (`GET`/`HEAD`/`PUT`/`DELETE`) **and** the body is reproducible (none, or fully buffered). Everything else — `POST`/`PATCH`, exec and code-execution starts, SSE establishment, any non-reproducible body — keeps today's stale-route `503` + `Retry-After`; the client retries, and the gateway never silently re-executes.

A response already in progress is never truncated by the proxy; keeping streams alive across a pause is the long-lived-connection caveat, an opt-in concern, not a proxy guarantee.

**Resume flight (singleflight).** Keyed by `(namespace, sandbox_id)` in a per-replica in-flight registry:

- The first request for a paused sandbox starts the flight **after acquiring its parking slot and a restore permit** (see Parking): call `ResumeSandbox` (asynchronous desired-state patch; `request_id = wake-{sandbox_id}-{flight_epoch}` for tracing), then poll `GetSandbox` until Runtime and DataPlane are Ready. The flight performs no route warm-up: the first parked request re-resolves on forward through the normal serve path, populating the provider route cache anyway.
- Concurrent requests for the same sandbox join the flight and share its remaining budget and outcome — N parked requests, one resume RPC, one poll loop.
- A `FailedPrecondition` ("not paused") from `ResumeSandbox` means someone else already resumed — treat as joined. An `Aborted` (checkpoint changed, i.e. a re-pause raced in) restarts the flight within the remaining budget.

**Parking.** A parked request waits in a bounded **parking lot**:

- **Budget Y** (`--wake-park-budget`, default 5 s) starts when the flight starts; late joiners share the remaining budget (per-flight, not per-request). The budget bounds retries, not a committed resume.
- Retry loop: exponential backoff starting at `--wake-retry-interval` (50 ms), factor 1.3, jitter 0.1, no attempt cap — the budget alone bounds the wait.
- **Budget exhaustion** → respond the wake-timeout `503` defined in [Wake Timeout Response and SDK Retry Contract](#wake-timeout-response-and-sdk-retry-contract). The flight itself is **never canceled**: `ResumeSandbox` is desired state that fast-sandbox converges regardless, and discarding an in-progress restore would only waste the work. The next request after exhaustion typically finds `Ready` and is served on the fast path.
- **Parking lot capacity** (`--wake-park-max`, default 1024): admission happens **before flight creation** — a request that would start a new resume flight acquires a slot first, and a full lot sheds it with `503` before any `ResumeSandbox` is issued. The lot bounds held requests (memory, file descriptors, client connections); its slots belong to requests and are released when a request ends — so by itself it does **not** bound restore work. First-attempt hits — the overwhelming majority — never touch the lot, preserving fast-path headroom.
- **Restore permits** (`--wake-restore-max`, default 256, validated ≤ `--wake-park-max`): a second admission that bounds concurrent **restores**. A flight acquires its permit after the parking slot and before `ResumeSandbox` is issued, and holds it **until the flight reaches a terminal outcome** (Ready, not-found, or the fence-restart loop ends) — never on client disconnect, budget exhaustion, or parking-slot release, all of which the flight outlives. When no permit is free, flight start queues FIFO; each waiting request stays subject to its own budget, so exhaustion answers `503` + `Retry-After` with no `ResumeSandbox` ever issued. Joiners acquire a parking slot but no permit — the flight holds the single permit. A crashing replica drops its permits with its process; correctness is unaffected (`ResumeSandbox` is desired state the controller converges), so the permit is per-replica resource control, not coordination. Worst case is `restore-max × R` concurrent restores across R replicas; a cluster-wide cap belongs in the fast-sandbox controller as defense in depth (future revision).
- **Mechanics of holding a request.** The ingress is a Go `net/http` server with `httputil.ReverseProxy`; parking means the handler goroutine simply does not invoke the reverse proxy yet. After admission it blocks on a `select` over three signals — the flight's completion channel, the request context's `Done()` (client disconnected), and the remaining-budget timer — then either proceeds through the normal `serve` path (re-resolve, forward) or writes `503 + Retry-After`. Because parking happens before any upstream dial, a parked request holds no upstream connection: its cost is one goroutine plus the held client connection, which is why the lot can be large. The flight itself runs on a background context, never the request's — budget exhaustion or client disconnect must not cancel the resume. Clients experience the park as added latency: no status or body bytes are written during the wait, so Y must stay below typical client response-header timeouts.
- **Protocol coverage**: parking applies to ordinary HTTP requests and to requests that arrive as WebSocket upgrades — the upgrade completes after wake, against the restored upstream. A connection that is *already established* is never parked mid-stream; only its next request would wake (see the long-connection caveat).

**Latency expectation.** Same-node restore on fast-sandbox is tens of milliseconds, so the common wake should land well under the 500 ms P95 acceptance target (see Test Plan). Cross-node restores can exceed Y; the 503 + `Retry-After` path keeps those honest instead of hanging clients.

**Multi-replica wake (no cross-replica coordination).** The singleflight registry is per replica by design — the asymmetry with the pause side is deliberate. Pause needs a distributed lock because the checkpoint has real cost and must happen once per idle episode; resume needs nothing because duplicates are harmless by construction: `ResumeSandbox` is a compare-and-set desired-state patch, idempotent on replay ("already Running" is a success), so R ingress replicas produce at most R RPCs of which exactly **one** performs the state patch — and the expensive step, the checkpoint restore, is executed **once** by the fast-sandbox controller because it is driven by `spec.state`, not by the number of callers. Dedup comes from convergence, not coordination; there is no lock and no leader on this path. Two details follow: (1) wake flights fence on `expected_checkpoint_id` (the only race that matters — a re-pause slipping in) and omit the generation fence, which changes on every transition and would only add a conflict-retry into the idempotent path; (2) every flight that observes `Ready` writes the shared activity key, so the post-resume idle window starts cleanly regardless of which replica won. The park budget is per flight per replica, starting at that replica's flight start: budgets across replicas are not synchronized, so a client's wait bound depends on which replica received it — bounded by Y either way, and poll amplification stays at one `GetSandbox` loop per replica flight (R loops), never per request.

### Server Proxy Path (Mode A)

Requests served by the server proxy (`/sandboxes/{id}/proxy/{port}`) bypass the ingress gateway. The server applies the same semantics inside its proxy handler:

- Activity: writes the same Redis activity key (async, coalesced) — this is what keeps proxy-path traffic from looking idle.
- Wake: on endpoint resolution signaling a paused sandbox, run the same flight (asyncio task registry, one `resume_sandbox` call + readiness poll) and hold the request up to `resume_budget_seconds` before answering the wake-timeout `503` — the same marker as the ingress (see [Wake Timeout Response and SDK Retry Contract](#wake-timeout-response-and-sdk-retry-contract)). The server already holds `FastPathClient`; no new connection surface.

This keeps OSEP-0009's dual-path parity: both supported reverse proxy paths observe activity and wake.

### Wake Timeout Response and SDK Retry Contract

Budget exhaustion answers `503 + Retry-After: 1`. The status stays 503 deliberately: it is the only code with defined `Retry-After` semantics, the established signal for "backend temporarily unavailable" across load balancers, Envoy, and service meshes ("no healthy upstream" is exactly this class), and the parked request was never forwarded — any upstream-facing code such as 504 would be wrong. The distinction the SDK needs rides in markers, not the status code:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 1
X-OpenSandbox-Wake: expired

{"error": {"code": "sandbox_wake_timeout",
           "message": "restore still in progress; request was never forwarded and is safe to retry",
           "sandboxId": "sb_..."}}
```

The bare `503` — no marker — remains today's paused-sandbox answer (wake disabled or `wakeOnAccess: false`), byte-for-byte. The ingress and the server proxy path emit the identical marker.

Official SDKs apply the following contract to this response, and only this response:

- **Recognition**: status 503 plus `code: sandbox_wake_timeout` (the header is mirrored for non-JSON media types). Any other 503 follows the SDK's existing retry rules untouched — in particular, a bare 503 is never auto-retried for non-idempotent methods.
- **Automatic retry**: wait per `Retry-After` (delta-seconds or HTTP-date), then resend the original request. The marker is the gateway's proof the request never reached the sandbox, so retry is safe for any method — including command/code-execution starts — provided the body is replayable (same capture rules as pre-forward parking). On by default, disable-able per client.
- **Established streams**: the marked request never produced a stream, so there is nothing to double-execute; once a retry succeeds and a stream is established, further failures follow the SDK's existing transport policy (no re-execution).
- **Total wait bound**: a per-client retry policy — `overall_deadline` (default 60 s) and `max_attempts` (default 5) — spans all wake cycles: waits, retries, and request time. Retries never reset the deadline; if the next `Retry-After` exceeds the remaining budget, stop.
- **Terminal outcomes**: success continues the original call; 404/expiry surfaces as-is; exhausted budget returns a typed diagnostic error (`WakeTimeoutExceeded`, carrying attempt count and waited time). Caller cancellation or SDK timeout never cancels the server-side resume.

### Interaction with Renewal, Expiry, and Manual Pause

- **OSEP-0009 renew-on-access** is independent: its gates (opt-in, cooldown, in-flight dedupe) are unchanged. One observed request feeds both consumers — a renew intent (if opted in) and an activity write (if enabled). A sandbox may opt into either, both, or neither.
- **Expiry** is untouched. `expires_at` is absolute; pausing neither stops nor extends it. Composition guidance: enable renew-on-access alongside idle-pause so active sandboxes keep extending while idle ones get reclaimed. A paused sandbox whose expiry passes becomes `Terminated` (retained CR, reason Expired); wake checks expiry before resuming and fails fast with 404.
- **Manual pause/resume** (`POST /sandboxes/{id}/pause|resume`) continues to work for `fsb-` sandboxes. Auto-pause uses the same FastPath calls; wake treats manually and automatically paused sandboxes identically (see Caveats). There is no auto-pause for the Kubernetes pod backend in this revision, so OSEP-0008 semantics are unaffected.
- **OSEP-0021 pool warmup** composes naturally: warm pools provide fresh capacity; idle-pause returns capacity back. Neither depends on the other.

### Configuration

Server (`config.toml`, top-level section, mirroring `[renew_intent]`):

```toml
[idle_pause]
enabled = false                    # master switch: sweeper + server-side activity writes
sweep_interval_seconds = 15
activity_ttl_seconds = 1800        # must be ≥ max accepted X (30–86400 enforced at create)
resume_budget_seconds = 5          # park budget Y for the server proxy path
redis.enabled = false
redis.dsn = "redis://127.0.0.1:6379/0"
redis.lock_ttl_seconds = 30
```

Ingress (flags, mirroring the `--renew-intent-*` family):

```text
--activity-enabled                    (default false)
--activity-redis-dsn                  (default redis://127.0.0.1:6379/0)
--activity-min-interval               (default 5s; per-sandbox write coalescing)
--activity-ttl-seconds                (default 1800; must match the server's activity_ttl_seconds)
--wake-enabled                        (default false)
--wake-park-budget                    (default 5s; park budget Y)
--wake-park-max                       (default 1024; parking lot capacity)
--wake-restore-max                    (default 256; concurrent restore permits; must be ≤ --wake-park-max)
--wake-retry-interval                 (default 50ms)
--wake-retry-factor                   (default 1.3)
--wake-retry-jitter                   (default 0.1)
```

Rules:

- `idle_pause.enabled=false` and `--wake-enabled=false` reproduce today's behavior exactly (paused sandbox accessed through the gateway → 503).
- The activity key prefix `opensandbox:activity` is fixed in code on both sides (ingress producer and server reader); it is deliberately not exposed as a flag or a config key.
- Ingress-path pause automation requires Redis reachable from both ingress and server. Wake requires only FastPath reachability the ingress already has.
- Timeout budget: `--wake-park-budget` (Y) must exceed `--fastpath-wait-timeout-millis` plus one `GetSandbox` round trip so at least one full readiness check fits in the budget; the per-call RPC timeout stays the existing `--fastpath-wait-timeout-millis` (default 2 s, provider max 5 min). Pause timing = X + at most one `sweep_interval_seconds` of detection lag; `activity_ttl_seconds` must remain ≥ the largest accepted X, and the ingress `--activity-ttl-seconds` must be kept equal to the server value (both gate the same keys; a producer-side mismatch shortens key lifetime and at worst delays pauses via tombstones — it cannot corrupt decisions). Operators must also align everything in front of the ingress — LB/Envoy idle timeouts and client response-header timeouts — to exceed Y, otherwise middle layers will cut parked connections before this OSEP can answer `503 + Retry-After`.
- Docker direct access remains unsupported as an observation point (no reverse proxy hop), same as OSEP-0009.

Create request example (pause + renew composed):

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
  },
  "extensions": {
    "access.renew.extend.seconds": "1800"
  }
}
```

### Observability

Aligned with OSEP-0010 (OTLP). Proposed instruments:

Ingress (meter `ingress`):

| Instrument | Type | Labels | Notes |
| --- | --- | --- | --- |
| `opensandbox.ingress.wake.flights` | counter | `outcome` = `triggered`/`joined`/`none` | `none` = sandbox already running on first check; the triggered:joined ratio shows fan-out |
| `opensandbox.ingress.park.active` | up-down counter | — | currently parked requests |
| `opensandbox.ingress.park.wait.duration` | histogram (s) | `outcome` = `served`/`budget_exhausted`/`canceled`/`error` | recorded once per parked request at wait end; sub-budget `budget_exhausted` samples are expected for late joiners |
| `opensandbox.ingress.park.shed` | counter | — | lot full; sustained growth ⇒ capacity or restore-latency problem |
| `opensandbox.ingress.wake.restore.active` | up-down counter | — | flights currently holding a restore permit; the concurrent-restore gauge |
| `opensandbox.ingress.activity.writes_dropped` | counter | — | Redis pressure indicator |

Server (meter `server`):

| Instrument | Type | Labels |
| --- | --- | --- |
| `opensandbox.server.idlep.pause.requests` | counter | `outcome` = `accepted`/`gated`/`failed` |
| `opensandbox.server.idlep.pause.latency` | histogram (s) | `PauseSandbox` accepted → observed `Paused` |
| `opensandbox.server.idlep.probe.outcome` | counter | `outcome` = `active`/`not_active`/`error` | app-defined activity probe results; sustained `error` ⇒ broken probe endpoint on an opted-in workload |
| `opensandbox.server.idlep.flap.ratio` | gauge | fraction of pauses followed by a wake within a configurable window; the single most useful tuning signal for X |

Dashboards/alerts: wake P95 (target < 500 ms same-node), shed rate ≈ 0, flap ratio, paused-sandbox count and artifact-store usage per pool.

### Security Considerations

- **Ingress privilege scope.** The ingress can already resolve routes for every tenant namespace it serves; this OSEP adds exactly two FastPath calls on that same channel — `GetSandbox` (read) and `ResumeSandbox` (write). The ingress never calls `PauseSandbox`: pause remains a server-side authority, so the ingress holds least privilege (it can wake what it routes for, but never pause). No create/delete/exec authority is granted. FastPath remains cluster-internal (plaintext gRPC inside a NetworkPolicy-isolated cluster, unchanged from Phase 1a of the existing provider).
- **Wake is not an auth bypass.** Parking happens after the existing host/route-scope/signature verification; a request that would be rejected today is rejected before any resume is triggered. Malicious clients cannot resume arbitrary sandboxes — only sandboxes they can already address — and cannot pause anything (pause is server-side only).
- **Resource exhaustion.** The parking lot and per-sandbox singleflight bound the wake path's memory and fan-out; restore permits bound concurrent VM restores independently of request lifetimes; activity writes are rate-limited per sandbox; the sweeper's per-sweep RPC count is bounded by candidate count after Redis-first filtering.
- **Fail-safe direction.** Every dependency failure (Redis down, FastPath errors, lock contention) resolves toward *leave the sandbox running* or *answer 503* — never toward data loss or an unintended pause.

## Construction Phases

- **Phase 1 — Activity + idle pause.** `lifecycle.idlePolicy` API field (spec + server validation + SDK parameters, persisted as reserved metadata); server `[idle_pause]` config, Redis activity keys, idle sweeper with gates and lock; activity-probe execution (sentinel-carried GET through the gateway, verdict mapping, pause audit line); `PauseSandbox` wiring through the existing `FastPathClient`; metrics. Exit: an opted-in sandbox goes `Running → Pausing → Paused` X seconds after its last request in a Kind/MinIO environment; non-opted-in sandboxes are untouched; Redis outage blocks pauses without side effects.
- **Phase 2 — Wake-on-access.** Pause-aware provider; singleflight registry; parking lot and restore permits with budget/backoff/shedding; wake-timeout `503` marker + `Retry-After` semantics; wake metrics. Exit: request to paused sandbox → parked → forwarded; concurrent burst produces one resume; budget exhaustion → marked 503 while the restore completes and the next request is served fast-path; expired paused sandbox → 404.
- **Phase 3 — Server proxy parity + SDK retry contract + polish.** Mode A activity writes and server-side parking (identical wake marker); official-SDK wake retry (recognition, retry policy, typed diagnostic error) with cross-language acceptance tests; docs (feature guide + limitations); flap-ratio dashboard; OSEP-0007 README/status-table correction where it contradicts shipped behavior.

## Test Plan

- **Unit Tests**
   - `idlePolicy` validation: `idleTimeoutSeconds` range 30–86400, `X ≤ activity_ttl_seconds`, unknown fields rejected, omission is a no-op; `wakeOnAccess=false`: pause still happens, requests keep 503, manual resume works, no auto-wake.
   - Activity probe verdict mapping: `active: true` → observation recorded; `active: false` → none, with the `message` carried into the pause audit line; timeout/error/malformed → none + outcome counted; sentinel-carried requests are never recorded by the activity recorder.
   - Idle decision function: boundary at exactly X; stale activity key ⇒ idle by value; missing key ⇒ unknown (tombstone created, sweep skipped); tombstone aged ≥ X ⇒ idle; activity reappearance clears the tombstone; expiry and state gates.
   - Activity write: monotonic max — an older timestamp never overwrites a newer one.
   - Parking admission: slot acquired before flight creation; full lot sheds before `ResumeSandbox`; first-attempt hits take no slot.
   - Restore permits: flight start queues when none is free and issues nothing on budget exhaustion; permit released only at flight terminal — client disconnect and slot release never free it; joiners acquire no permit.
  - Singleflight registry: N concurrent arrivals → one flight, shared budget, late-joiner outcome; `Aborted` fence restart; "not paused" treated as joined.
  - Parking lot: slot taken only at park transition; shedding at capacity; budget exhaustion returns 503 + `Retry-After` without canceling the flight.
  - Backoff sequence with factor/jitter bounds.
- **Integration Tests (Kind + fast-sandbox + MinIO + Redis)**
  - Opted-in sandbox: traffic stop → `Paused` within X + sweep interval; memory released on the Fastlet (runtime gone, CRD retained).
  - Request to paused sandbox → wake → forwarded; response correctness identical to a never-paused sandbox; exec and file calls through raw port 44772 wake identically.
  - Request arriving during `Pausing` cancels the pause (no completed checkpoint) and serves from the running sandbox.
  - Manual pause → access → wake (uniform semantics).
  - Paused + expired → access → 404, no resume.
   - Redis down: no pauses occur; wake still works; proxy path unaffected.
   - Redis data loss (`FLUSHALL`) with an active opted-in sandbox: no immediate pause; after continued silence the tombstone window elapses and the pause proceeds — delayed by ≥ X from first observation, never accelerated.
   - Producer partition (an ingress replica's activity writes fail while server reads succeed and traffic keeps flowing): the sandbox may take one spurious pause; the in-flight request recovers through wake and completes correctly; no data loss; `activity.writes_dropped` increments on the affected replica.
   - Activity probe (Kind, probe endpoint served on a user port): busy answer keeps the sandbox alive with re-probe at ~X cadence; `active: false` → pause proceeds and the structured pause audit log carries the answer's `message`; timeout/error/malformed → no pause this sweep and `idlep.probe.outcome` increments; the probe request itself never refreshes the activity key (sentinel honored on the internal listener, stripped on the external one); a real request landing while the probe runs is caught by the second re-verify; no `activeProbe` declared → no probe traffic at all.
   - Cached-route request racing a pause: stale upstream response → `GetSandbox` → wake → idempotent request with reproducible body retried once and served; `POST` or non-reproducible body answered 503 without replay.
   - `POST` forwarded, executed upstream, upstream disconnects before responding, pause surfaces as stale-route: the gateway does **not** re-execute — client gets `503` + `Retry-After`, upstream observed exactly one execution.
   - SDK wake retry (cross-language): a `sandbox_wake_timeout` response is retried per `Retry-After` and succeeds once the sandbox is `Running`; repeated timeouts exhaust `overall_deadline` → typed error with attempt count; `Retry-After` parsed as delta-seconds and HTTP-date; caller cancellation stops retries without affecting the server-side resume; a bare 503 is never auto-retried for `POST`; SSE start retried exactly once and an established stream is never re-executed; marker identical on ingress and server proxy paths.
   - `POST` parked pre-forward (pause detected at resolution, no upstream dial): forwarded exactly once after wake.
   - Idempotent method with a chunked (non-reproducible) body on the stale-route path: answered 503, not retried.
   - Burst across more than `--wake-park-max` distinct paused sandboxes: concurrent restores bounded by restore permits per replica; overflow shed before any `ResumeSandbox`.
   - Two waves of distinct paused sandboxes with restores slower than Y: the first wave exhausts budgets and disconnects (parking slots released) while its restores continue; concurrent restores never exceed `--wake-restore-max` — assert the number of **still-running restores**, not parked handlers; second-wave starters queue on permits without issuing `ResumeSandbox`.
  - Two server replicas: exactly one pause per idle episode (lock + idempotency).
  - Multi-replica ingress: activity from any replica keeps the sandbox awake.
  - OSEP-0009 composition: opted into both — renew intents and activity writes both flow; renew gate cooldown unchanged.
- **Stress Tests**
  - Burst of 200 concurrent requests to one paused sandbox: exactly one `ResumeSandbox` per replica; all requests served after wake or shed deterministically at lot capacity.
   - Many paused sandboxes hit simultaneously: lot and restore-permit bounds respected, no unbounded queueing, FastPath RPC rate bounded by sandbox count not request count.
- **Performance Tests**
  - Wake P95 (same-node, warm artifact cache): target < 500 ms; record pause duration distribution per guest memory size.
  - Flap ratio under synthetic periodic traffic (period ≈ X, ≈ 2X) to validate the floor and tuning guidance.
- **Manual Validation**
  - `GET /sandboxes/{id}` renders `Pausing`/`Paused`/`Resuming` through the cycle; delete of a paused sandbox removes CRD + checkpoint artifacts.

## Drawbacks

- **New privileged behavior in the data plane.** The ingress can change sandbox desired state; the blast radius is small but real, and reviewed deployments must treat the ingress as trusted infrastructure.
- **Redis becomes load-bearing for pause automation.** Its failure mode is benign (no pauses) but it is one more moving part; wake avoids it by design.
- **Blind spots remain for workloads that cannot self-report.** `activeProbe` covers app-aware tasks (a busy answer counts as activity); silent long-lived connections and background jobs the application does not surface are still mis-paused. The full fix is additional activity sources (future work).
- **Flapping cost.** A workload with traffic period near X pays repeated checkpoint/restore cycles; the floor and flap metrics mitigate but cannot eliminate misuse.
- **Artifact-store usage scales with pause count.** Every paused sandbox holds a full checkpoint until resumed or deleted.
- **Extra configuration surface** on both server and ingress, and a two-feature flag matrix to document and support.

## Alternatives

1. **execd/egress-handler activity reporting** — more accurate idle detection, but a new reporting path from every sandbox and a new trust surface; deferred as a future complementary signal. Ingress observation alone is sufficient and immediate for fast-sandbox because all client traffic already transits it.
2. **Ingress pauses directly (no server sweeper)** — fewer hops, but pause policy (opt-in validation, expiry, hysteresis, future multi-source activity) belongs to the control plane, and the ingress should stay a stateless forwarder plus this narrowly-scoped wake authority. Also, only the server can coordinate future execd signals with ingress signals.
3. **Wake via Redis queue + server worker (OSEP-0009 Mode B style)** — uniform with renew, but inserts a queue round trip and worker latency into the wake path, which is the latency-critical direction; the ingress already holds a FastPath connection, so direct resume is strictly better here.
4. **Expiry-driven pause only (`on_timeout=pause`)** — reuses the expiry sweep with no activity tracking, but captures "old" sandboxes, not "idle" ones; orthogonal and worth a follow-up revision that shares this OSEP's pause pipeline.
5. **Extend to the Kubernetes pod backend** — a pod equivalent would build on OSEP-0008's snapshot machinery plus the same sweeper, but pod pause is minutes-scale (registry commit), which changes the wake economics entirely; out of scope.
6. **Per-sandbox park budget override via extensions** — plausible (interactive vs batch tuning) but adds a second sandbox-level knob whose value is operator-owned latency policy; deferred.
7. **Observation-health gating (writer watermark)** — fully closes the producer-partition blind spot: each producer heartbeats a `healthy_since` key over its activity-write connection (heartbeat success proves the same connection can write activity), and the sweeper floors the idle clock at the newest recovery point across live ingress replicas, with replica liveness taken from the pod list — a partitioned replica cannot report its own ill health through the Redis it can no longer reach, so "who should be alive" must come from outside. Deferred: a server↔ingress deployment coupling plus extra idle-gate math and rollout-time behavior, defending against a rare failure whose damage self-heals; revisit if `writes_dropped` telemetry ever shows producer partitions in practice.

## Infrastructure Needed

- Redis (already required for ingress-mode renew intents; the same deployment serves activity tracking).
- Artifact-store capacity sized for peak concurrent paused sandboxes × average checkpoint size (documented in the feature guide).
- Metrics dashboards for the wake/park/pause instruments above.

## Upgrade & Migration Strategy

- Fully backward compatible and disabled by default: `idle_pause.enabled=false`, `--activity-enabled=false`, `--wake-enabled=false` reproduce current behavior byte-for-byte.
- Rollout order:
  1. Deploy server + ingress with flags off (no behavior change).
  2. Enable activity tracking and the sweeper in a canary tenant; verify pauses and flap ratio.
  3. Enable wake on the canary ingress; verify parked-request latency distribution and shed rate.
  4. Enable per-sandbox via `lifecycle.idlePolicy` for real workloads.
- Rollback: disable the flags. Sandboxes left `Paused` remain resumable through the manual API; operators should resume or delete stranded paused sandboxes after disabling wake, since traffic will no longer wake them.
- Two additive public contract changes: the optional `lifecycle.idlePolicy` create field (spec + SDK parameters + docs; absent field ⇒ byte-identical behavior), and the wake-timeout response marker with the SDK auto-retry contract (Phase 3). The policy persists in existing reserved metadata — no CRD or storage migrations, and no other API or schema changes. Official SDKs retry only the marked response; all other 503 handling is unchanged.
