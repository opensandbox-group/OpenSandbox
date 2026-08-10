---
title: Auto Idle-Pause and Resume-on-Revisit
authors:
  - "@marin"
creation-date: 2026-08-06
last-updated: 2026-08-06
status: draft
---

# OSEP-0018: Auto Idle-Pause and Resume-on-Revisit

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
  - [Confirmed Facts From Code Reading](#confirmed-facts-from-code-reading)
  - [Component: IdlePauseCoordinator](#component-idlepausecoordinator)
  - [Activity Signal Coverage](#activity-signal-coverage)
  - [Pause (no pre-pause TTL push)](#pause-no-pre-pause-ttl-push)
  - [Auto-Resume on Revisit](#auto-resume-on-revisit)
  - [Scanner Loop](#scanner-loop)
  - [End-to-End Flow](#end-to-end-flow)
  - [Configuration](#configuration)
  - [File-Level Changes](#file-level-changes)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

OSEP-0008 delivered manual pause/resume (rootfs snapshot → release compute, files preserved, `sandboxId` stable) and OSEP-0009 delivered auto-renew-on-access. This proposal closes the gap between them: a server-side coordinator that **automatically pauses sandboxes that have not been accessed for a configurable idle period**, and **automatically resumes them when traffic returns**. The goal is to convert a hard memory constraint into a cheap storage constraint for long-lived Agent workloads that are idle most of the time (e.g. waiting on an LLM).

## Motivation

Code-sandbox workloads — especially Agent tasks driving a code interpreter — are long-lived but bursty: they do real I/O for short windows and sit idle for the majority of their lifetime (LLM think time). On a memory-constrained Kubernetes namespace (e.g. ~20 Gi allocatable, per-sandbox memory limit 500 Mi that cannot be lowered because of large-file reads), the resident concurrency ceiling is fixed by physics — roughly `allocatable / (active_ratio × limit + (1 − active_ratio) × idle_usage)`, on the order of tens of sandboxes — regardless of how the ResourceQuota or runtime is tuned.

These workloads also **must not lose state while they are still in use**: an Agent writes many intermediate files into the sandbox, and reclaiming the sandbox on a short TTL destroys that state, causing the Agent to hallucinate missing artifacts. Short TTL is therefore not an acceptable lever *for a sandbox the Agent is still driving*.

The only lever left is to **release the memory of idle sandboxes while preserving their filesystem state** — exactly what pause/resume (OSEP-0008) was built for. But pause/resume is currently manual: nothing decides *when* to pause, and nothing wakes a paused sandbox when the Agent returns. This proposal adds that automation, mirroring the existing `renew_intent` feature (OSEP-0009) as its symmetric counterpart:

- `renew_intent`: **on access → extend TTL.**
- `idle_pause`: **no access beyond a threshold → pause; on next access → resume.**

With this, total concurrent tasks (resident + paused) can far exceed the resident memory ceiling, because paused sandboxes occupy object storage (a committed OCI snapshot), not node memory.

### Goals

- Automatically pause a Running sandbox after a configurable idle period of no proxy access.
- Automatically resume a Paused sandbox on the next proxy access, transparent to the caller (single slow request).
- Preserve sandbox filesystem state *within the TTL window* — pause releases memory without reclaiming the sandbox; only expiry (an idle sandbox never revisited) reclaims it. This is the anti-hallucination guarantee for any sandbox the Agent actually returns to.
- Ship entirely in the Python server, reusing the `renew_intent` module's patterns. No Go controller changes, no new dependencies, **no fork/client code changes** (global config switch).
- Default off; operators opt in via `[idle_pause]` TOML.

### Non-Goals

- Per-sandbox opt-in/opt-out via `extensions` (v1 is a global cluster policy; the opt-out hook point is left available for later).
- Patching the Go controller to freeze the TTL clock while paused (rejected — paused sandboxes are allowed to expire naturally; see [Alternatives](#alternatives)).
- Resume-progress UX surfaced to the client (a single slow request is acceptable for v1; a "waking up" transition state is a follow-up).
- Support under the `fleets` (fast-sandbox) runtime — it rejects pause/resume; this feature targets the standard Kubernetes backend.

## Requirements

- **Hard:** a paused sandbox is reclaimed when its TTL expires, exactly like a Running sandbox. Pause preserves files *within* the TTL window; an idle sandbox that is never revisited is allowed to expire and be cleaned up. Only *active* (accessed/resumed) sandboxes have their TTL extended — by `renew_intent` on access. This deliberately weakens "arbitrarily long pause" to "within TTL": a sandbox idle past its TTL has no state worth preserving.
- **Hard:** no client/fork code changes. Activation is operator config only.
- **Hard:** the deployment routes all Agent data-plane I/O through the server proxy. The Python SDK defaults to `use_server_proxy=false` (direct endpoint) and ingress-gateway mode bypasses the proxy — under either, idle_pause observes no activity and must not be enabled (see [Notes/Constraints/Caveats](#notesconstraintscaveats)).
- **Soft:** the proxy hot path (sandbox Running) pays nothing; `get_sandbox` is only called when endpoint lookup fails.
- **Soft:** a mis-paused sandbox (one doing work that emits no proxy I/O for the threshold) costs latency on resume, never data loss.

## Proposal

Introduce an `IdlePauseCoordinator` (in `server/opensandbox_server/integrations/idle_pause/`) that owns three concerns in one class: activity tracking, idle scanning, and auto-resume. It is wired into the app lifespan exactly like `RenewIntentConsumer`, and reads the same proxy activity signal that `renew_intent` already taps.

Two surgical hooks in `api/proxy.py`:

1. **`_touch_idle`** — sits right next to the existing `_schedule_proxy_renew` call (HTTP and WS paths); refreshes a per-sandbox `last_activity` timestamp.
2. **Resume-on-endpoint-miss** — when `get_endpoint` raises `HTTPException(404, K8S_POD_IP_NOT_AVAILABLE)` (the Paused state has cleared the endpoints annotation, so no Pod IP resolves), call `coord.ensure_resumed(sandbox_id)`, then retry the endpoint resolution once.

A background scanner lists Running sandboxes every `scan_interval_seconds`; any idle longer than `idle_threshold_seconds` are paused with a single `pause_sandbox` patch. The sandbox keeps its existing `expireTime`; if it is never revisited, the controller's expiration check reclaims it on schedule (acceptable — see [Requirements](#requirements)).

### Notes/Constraints/Caveats

- **The Go controller does not freeze TTL during pause — intended, not worked around.** `pause_sandbox` only patches `spec.pause=True`; `spec.expireTime` keeps ticking and the expiration check in `batchsandbox_controller.go` runs unconditionally. A paused sandbox is reclaimed when its TTL expires, like any other sandbox — the desired behavior for a sandbox never revisited.
- **Neither `resume_sandbox` nor the coordinator resets TTL.** After resume the sandbox keeps its existing `expireTime`; TTL extension for the now-active sandbox is handled by `renew_intent` on the proxy access that triggered the resume (`_schedule_proxy_renew` runs after the resumed request succeeds). `idle_pause` therefore assumes `renew_intent` is enabled.
- **`renew_expiration` does not check sandbox state** — it only rejects when `current_expiration is None` (409). This is what makes renew-while-Running valid.
- **There are no `/exec` or `/files` endpoints** in the server. All Agent data-plane I/O flows through `/sandboxes/{id}/proxy/{port}/...`, so the proxy hook is the single, complete activity signal. Control-plane calls (`get_sandbox`, list, metadata patch) are deliberately *not* counted as activity — an orchestrator polling a finished Agent must not keep it alive.
- **idle_pause only sees traffic that flows through the server proxy.** Two common paths bypass it and would read as permanently idle:
  - **SDK default `use_server_proxy=false`** (`sdks/sandbox/python/.../services/sandbox.py:143,163`): the Python SDK's `get_sandbox_endpoint` / `get_signed_sandbox_endpoint` return a *direct* endpoint by default, so Agent data-plane traffic never touches `api/proxy.py`. An Agent using the SDK default would be paused mid-use.
  - **ingress-gateway (router) mode**: traffic is captured by the `renew_intent` Redis queue, not `_schedule_proxy_renew`.

  `start()` checks `ingress_config.mode` and warns under gateway mode; but `use_server_proxy` is a per-request client choice the server cannot enforce statically, so a server-proxy deployment is a **hard prerequisite** (see [Requirements](#requirements)), not merely a runtime check. Consuming the `renew_intent`/gateway signal too — to cover direct/gateway deployments — is an explicit follow-up, not v1.
- **Activity is process-local — `idle_pause` v1 targets a single server replica.** `last_activity` lives in an in-process `OrderedDict`; with the Helm default of two replicas (`kubernetes/charts/opensandbox-server/values.yaml:30-31`), traffic load-balanced to replica A is invisible to replica B, which can list an active sandbox as idle and pause it mid-use. v1 guards this with a single-replica deployment prerequisite (a shared activity store or leader-elected scanner is a follow-up).

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Paused sandbox expires during pause | Intended. Pause preserves files only within the TTL window; a sandbox idle past its TTL is reclaimed by the controller. No special handling — this is the desired cleanup. |
| Multi-replica server: activity not shared | v1 requires a single server replica. `last_activity` is process-local; a 2nd replica cannot see another's traffic and would mis-pause. Shared store / leader scanner is a follow-up. |
| Mis-pause of a long-compute sandbox with no proxy I/O | Auto-resumes on the next proxy hit; cost is latency. `idle_threshold_seconds` is tunable. |
| Resume latency visible to the Agent | Poll every 1 s, timeout 60 s; typical K8s resume is a few seconds. |
| Concurrent resume stampede | Per-sandbox `resume_lock` + `resuming` flag → exactly one `resume_sandbox` call per pause cycle. |
| `list_sandboxes` cost at scale | One K8s list per `scan_interval` (default 60 s), label-filtered; same pattern as the existing list endpoint. |
| LRU unbounded growth | Capped at 8192 entries (mirrors `PROXY_RENEW_MAX_TRACKED_SANDBOXES`). |

## Design Details

### Confirmed Facts From Code Reading

Load-bearing facts verified against current source (these anchor the design):

- Activity hook: `_schedule_proxy_renew` (`api/proxy.py:166-169`) → `app.state.proxy_renew_coordinator.schedule(id)` → `integrations/renew_intent/proxy_renew.py:37` → `consumer.submit_from_proxy(id)` (`consumer.py:154-161`). The new `_touch_idle` mirrors this exactly.
- Activity call sites: `proxy.py:265` (HTTP `_proxy_http_request`) and `proxy.py:431` (WS `_proxy_websocket_request`).
- HTTP forward: `client.send` at `proxy.py:296`; `ConnectError → 502` at `proxy.py:317-321` (only reached when the endpoint resolves).
- Endpoint resolution: `get_endpoint(resolve_internal=True)` (`proxy.py:263` HTTP / `:406` WS → `kubernetes_service.py:1419`) reads pod IP from the `sandbox.opensandbox.io/endpoints` annotation (`kubernetes_service.py:1483-1494`); empty IP ⇒ `HTTPException(404, K8S_POD_IP_NOT_AVAILABLE)` (`kubernetes_service.py:1498-1505`).
- **Paused ⇒ the controller clears the endpoints annotation.** `batchsandbox_status.go:289-299` writes `[]` when the annotation previously existed (the explicit "pause scenario" at `:294-295`). A Paused sandbox therefore fails at `get_endpoint` with **404 before** the request ever reaches `client.send` — `ConnectError` is never hit on the resume path. **Resume hooks endpoint-resolution failure, not `client.send`.**
- `renew_expiration` (`services/k8s/kubernetes_service.py:1321-1379`): calls `ensure_future_expiration(request.expires_at)`, rejects only when `current_expiration is None` (409); does **not** check state.
- `pause_sandbox` / `resume_sandbox` (`kubernetes_service.py:1217` / `:1258` → `batchsandbox_provider.py:626` / `:672`): patch only `spec.pause`; do not touch `spec.expireTime`.
- Go expiration check: `kubernetes/internal/controller/batchsandbox_controller.go:113-129`, unconditional, runs before pause dispatch.
- Background-task pattern: `class + start() classmethod + asyncio.create_task + asyncio.Event stop + stop() cancel` (`integrations/renew_intent/consumer.py`). Scanner loop uses `asyncio.sleep(N)` instead of `wait_for(queue.get())`.
- Dedup pattern: `_MemSandboxState` LRU `OrderedDict` with per-sandbox `asyncio.Lock` (`consumer.py:65-68, 194-215`); capacity 8192.
- Config: Pydantic `BaseModel` + hand-read TOML; `RenewIntentConfig` at `config.py:170-195`, attached to `AppConfig` at `:972-973`, exported in `__all__` at `:1110`.
- Service instances: module-level singletons, `api/lifecycle.py:56 sandbox_service = create_sandbox_service()`. Sync service methods are wrapped with `asyncio.to_thread` inside async tasks (mirrors `controller.py:126`).
- `list_sandboxes(ListSandboxesRequest(filter=SandboxFilter(state=["Running"])))`; state values: Pending/Running/Pausing/Paused/Resuming/Stopping/Terminated/Failed (`api/schema.py:361`).
- App lifespan: `main.py:85-157`; `renew_intent` wired at `main.py:132-143` (start) and `:149-156` (stop).

### Component: IdlePauseCoordinator

Single class owning activity tracking, auto-resume, and the scanner loop. It is both the `ProxyRenewCoordinator`-style `app.state` adapter and the background-task owner, combined.

```python
@dataclass
class _IdleSandboxState:
    last_activity: float          # wall-clock epoch seconds (time.time()); survives server restart
    resume_lock: asyncio.Lock     # serializes + dedupes concurrent resumes
    resuming: bool

class IdlePauseCoordinator:
    def __init__(self, app_config, sandbox_service, extension_service): ...

    @classmethod
    async def start(cls, app_config, sandbox_service, extension_service) -> Optional["IdlePauseCoordinator"]:
        # returns None when idle_pause.enabled is False (mirrors RenewIntentConsumer.start)

    # ---- activity hook (called from proxy; sync, non-blocking) ----
    def touch(self, sandbox_id: str) -> None: ...

    # ---- auto-resume hook (called from proxy on endpoint-404 / Paused) ----
    async def ensure_resumed(self, sandbox_id: str) -> bool: ...

    # ---- scanner ----
    def _spawn_tasks(self) -> None: ...          # one _scanner_loop task
    async def _scanner_loop(self) -> None: ...   # while not _stop: sleep(scan_interval); _scan_once()
    async def _scan_once(self) -> None: ...      # list Running -> idle check -> renew + pause
    async def stop(self) -> None: ...            # mirrors RenewIntentConsumer.stop
```

The coordinator owns its **own** `OrderedDict[str, _IdleSandboxState]` with the same LRU-evict pattern and 8192 cap as `consumer.py`. It does **not** share `_MemSandboxState` with `renew_intent`: the concerns differ (cooldown vs. idleness), as do their lifetimes and eviction semantics.

### Activity Signal Coverage

Hook **only** the proxy path, via `_touch_idle`, called immediately after `_schedule_proxy_renew` at `proxy.py:265` (HTTP) and `:431` (WS). There is no shared middleware wrapping all `/sandboxes/{id}/*` routes; adding one would be a larger change than the two-line hook, and would also wrongly count control-plane polling as activity (keeping a finished Agent alive). The proxy hook gives identical coverage to `renew_intent` by construction.

### Pause (no pre-pause TTL push)

The scanner does **not** renew before pausing. It is a single `_scanner_loop` task (no concurrent pauser, so no lock — `touch()` is the only other writer and a float read is atomic under the GIL):

```
0. re-read last_activity; if fresh (proxy hit since last scan), skip pause
1. pause_sandbox(id)   # to_thread — patches spec.pause only
```

The sandbox keeps its existing `expireTime` and the controller's TTL clock keeps ticking through pause. A paused sandbox that is not revisited is reclaimed on expiry — the desired behavior (see [Requirements](#requirements)). There is no "renew→pause window" to race and no TTL to push; the only failure mode of step 1 is a transient `pause_sandbox` error, retried next cycle. This drops an entire invariant (`paused_retention`, the pre-pause renew, and the renew-fail-skip branch) versus an earlier draft.

### Auto-Resume on Revisit

`ensure_resumed(sandbox_id) -> bool`:

0. **State pre-check** — `get_sandbox(id)`: if state is not `Paused`/`Resuming` (e.g. `Pending` — a freshly created sandbox whose Pod has not yet registered an endpoint), return False fast. The 404 is the genuine "Pod not ready" case, not a pause, so resume is never triggered against a warming-up sandbox. This also keeps resume off the hot path.
1. Acquire `resume_lock`; if `resuming` is already True, poll `get_sandbox` until `state == "Running"` (shared wait), return True.
2. Else set `resuming = True`, call `resume_sandbox(id)` via `asyncio.to_thread`.
3. Poll `get_sandbox` every `IDLE_RESUME_POLL_INTERVAL_SECONDS` (1 s) until `state == "Running"` or `IDLE_RESUME_TIMEOUT_SECONDS` (60 s). Transition: Paused → Resuming → Running.
4. Clear `resuming`, release lock, return True.

TTL is **not** reset here. The proxy access that triggered this resume flows on to `_schedule_proxy_renew` (`proxy.py:265`) once `get_endpoint` succeeds, so `renew_intent` extends the now-active sandbox's TTL on access — the same path every other active sandbox uses. This removes the earlier `resume_ttl_seconds` renew and its "shortening the TTL" contract problem.

Called inline from `proxy.py` at endpoint resolution (the request never reaches `client.send` for a Paused sandbox — see [Confirmed Facts](#confirmed-facts-from-code-reading)):

```python
# pseudo, around proxy.py:263 (HTTP) / :406 (WS)
try:
    endpoint = lifecycle.sandbox_service.get_endpoint(sandbox_id, port, resolve_internal=True)
except HTTPException as e:
    if e.status_code != 404 or e.detail_code != SandboxErrorCodes.K8S_POD_IP_NOT_AVAILABLE:
        raise
    coord = getattr(request.app.state, "idle_pause_coordinator", None)
    if coord is None or not await coord.ensure_resumed(sandbox_id):
        raise  # state pre-check said "not paused" → keep the original 404
    endpoint = lifecycle.sandbox_service.get_endpoint(sandbox_id, port, resolve_internal=True)  # single retry
```

The WebSocket path (`_proxy_websocket_request`) hits the same `get_endpoint` 404 at `:406` before the `websockets.connect` at `:452`; the retry wraps that resolution identically.

**Concurrency dedup:** per-sandbox `resume_lock` + `resuming` flag. N concurrent proxy hits on one Paused sandbox ⇒ exactly one `resume_sandbox` call; the rest block on the lock and observe Running afterward (mirrors `consumer.py:292`).

**Failure:** state pre-check returns False (not Paused), resume 409 (not Paused), or 60 s poll timeout ⇒ return False ⇒ proxy re-raises the original 404 as today. The K8s-side resume continues asynchronously; the next proxy hit succeeds. No data loss.

### Scanner Loop

```
_scan_once():
    resp = list_sandboxes(ListSandboxesRequest(filter=SandboxFilter(state=["Running"])))
    for sb in resp.sandboxes:
        st = _ensure_mru(sb.id)                       # create/refresh LRU entry
        now = time.time()
        created = sb.created_at.timestamp()           # datetime → epoch; baseline when never touched
        if (now - created) < min_sandbox_age_seconds: continue
        idle = now - (st.last_activity or created)
        if idle < idle_threshold_seconds: continue
        await _pause_one(sb.id)                       # pause only (see Pause)
```

A sandbox never touched by the proxy uses its `created_at` (as epoch) as the activity baseline, so it becomes pausable after `idle_threshold` from creation (subject to `min_sandbox_age`). Wall-clock is used throughout so idle stays computable after a server restart — `time.monotonic()` resets to zero on restart and is not comparable with the persisted `created_at`.

The scanner only reads `_IdleSandboxState`; `touch()` (writing `time.time()`) is the only writer from the proxy path. The single-task scanner needs no lock on the pause side.

### End-to-End Flow

Happy path:

```
1. Agent stops hitting the proxy.
2. Scanner tick: list Running → idle > threshold →
   pause_sandbox(id)                   [to_thread] ✓
   Running → Pausing → Paused; Pod scaled to 0; memory freed; rootfs snapshot kept.
   (expireTime unchanged; if never revisited, the controller reclaims it on expiry.)
3. Agent revisits → proxy request → get_endpoint raises 404 (endpoints annotation cleared on pause; `K8S_POD_IP_NOT_AVAILABLE`). The request does not reach `client.send`.
4. coord.ensure_resumed(id):
   get_sandbox → Paused; resume_lock; resuming=True; resume_sandbox(id) → Paused → Resuming → Running
   poll get_sandbox every 1 s until Running (≤ 60 s)
   resuming=False; return True
5. proxy retries get_endpoint → Pod IP restored → client.send → success; `_touch_idle` + `_schedule_proxy_renew` fire → `renew_intent` extends TTL on access.
```

Exception summary:

| Event | Behavior |
|-------|----------|
| `pause_sandbox` fails (transient K8s error) | Log; scanner retries next cycle. Sandbox stays Running. |
| Paused sandbox reaches its `expireTime` | Controller reclaims it normally — intended cleanup, not an error. |
| `resume_sandbox` 409 (not Paused) / 60 s timeout | Return False → proxy re-raises the original 404; client retries; next hit re-enters `ensure_resumed`. |
| Concurrent proxy hits during resume | N−1 wait on `resume_lock`; single `resume_sandbox` call. |
| Activity arrives between scanner's list and pause | `last_activity` re-read before pause (step 0); fresh ⇒ skip. No mis-pause. |
| Sandbox Terminated between list and pause | `pause_sandbox` 404; log; drop from LRU. |
| Scanner loop raises | Logged per-iteration (mirrors `consumer.py:275-283`); loop continues. |

### Configuration

New `[idle_pause]` section in the TOML config:

```toml
[idle_pause]
enabled = false
scan_interval_seconds = 60          # scanner period
idle_threshold_seconds = 600        # idle duration before pause; MUST be >= scan_interval
min_sandbox_age_seconds = 120       # don't pause a sandbox younger than this (warmup)
# No paused_retention / resume_ttl: a paused sandbox keeps its existing expireTime and
# is reclaimed on expiry if never revisited; an active (resumed) sandbox is extended by renew_intent on access.
```

`IdlePauseConfig` (Pydantic `BaseModel`) with a `@model_validator(mode="after")` enforcing `idle_threshold_seconds >= scan_interval_seconds` to prevent flapping. Default `enabled = false`. Attached to `AppConfig` alongside `RenewIntentConfig`.

### File-Level Changes

**New** (mirror `integrations/renew_intent/`):

| File | Responsibility |
|------|----------------|
| `server/opensandbox_server/integrations/idle_pause/__init__.py` | Re-export `IdlePauseCoordinator`, `start_idle_pause_coordinator` |
| `server/opensandbox_server/integrations/idle_pause/coordinator.py` | The class (activity + resume + scanner), ~220 LOC |
| `server/opensandbox_server/integrations/idle_pause/logutil.py` | Event constants + `idle_bundle()` → `(line, extra)`, `idle_pause_*` prefix |
| `server/opensandbox_server/integrations/idle_pause/constants.py` | `IDLE_MAX_TRACKED_SANDBOXES=8192`, `IDLE_RESUME_POLL_INTERVAL_SECONDS=1`, `IDLE_RESUME_TIMEOUT_SECONDS=60` |

**Modified:**

| File | Change |
|------|--------|
| `server/opensandbox_server/config.py` | Add `IdlePauseConfig` (after `RenewIntentConfig`, ~line 195), attach to `AppConfig` (~line 973), add to `__all__` (~line 1110) |
| `server/opensandbox_server/api/proxy.py` | Add `_touch_idle` helper + 2 call sites (`:265`, `:431`); wrap `get_endpoint` retry-on-404-`POD_IP_NOT_AVAILABLE` (HTTP `:263` and WS `:406`) |
| `server/opensandbox_server/main.py` | Lifespan: `start_idle_pause_coordinator` at startup (after `:132`); `stop()` at shutdown (after `:151`) |
| `server/opensandbox_server/examples/example.config.toml` | Add `[idle_pause]` section (after `[renew_intent]`, ~line 77); mirror across the 4 example configs |

Net: 4 new source files + 4 modified files, ~600 LOC. No Go changes, no fork changes, no new dependencies.

## Test Plan

Mirror `tests/test_proxy_renew_coordinator.py` (unit, no app, mock services) and `tests/test_routes_pause_resume.py` (route, `TestClient` + `monkeypatch lifecycle.sandbox_service` with a `StubService`).

**Unit — `tests/test_idle_pause_coordinator.py`:**

- `start()` returns None when disabled.
- `touch` updates `last_activity`.
- `_scan_once`: skips non-Running; skips younger than `min_sandbox_age`; pauses an idle sandbox with a single `pause_sandbox` call (assert **no** `renew_expiration` precedes it); aborts pause when activity arrives between list and pause (step-0 re-read).
- `ensure_resumed`: no-op when Running; Paused→Resuming→Running issues exactly one `resume_sandbox` and **does not** call `renew_expiration` (TTL is left to `renew_intent`); returns False on timeout; concurrent calls dedupe to one resume.
- LRU eviction under capacity.

**Route — `tests/test_routes_proxy_auto_resume.py`:**

- Preserves the original 404 (`K8S_POD_IP_NOT_AVAILABLE`) when no coordinator is set.
- Mock `sandbox_service.get_endpoint` to raise `HTTPException(404, K8S_POD_IP_NOT_AVAILABLE)` once then return an endpoint; `StubService.get_sandbox` reports `Paused` → `Resuming` → `Running` (the capitalized API states); assert `resume_sandbox` called and response is the backend's.
- A 404 on a `Pending` / non-`Paused` sandbox returns the original 404 without calling `resume_sandbox` (state pre-check).
- WebSocket path (`get_endpoint` 404 → resume → retry).
- `touch` invoked on a normal forwarded request.

**Config — append to `tests/test_config.py`:** default disabled; validator rejects `idle < scan`.

**Manual / E2E:** with `[idle_pause] enabled=true idle_threshold_seconds=120` (and `renew_intent` enabled), create a sandbox, write a file, stop accessing it, observe state → Paused and Pod count drop within ~2–3 min. Issue a proxy request: observe second-scale resume latency, state → Running, and **the previously written file is still present** (within-TTL retention check). Then create a second short-TTL sandbox, pause it, and **do not** revisit: confirm it is **deleted when its TTL expires** — the intended cleanup.

## Drawbacks

- Adds a second background task and a second in-memory LRU alongside `renew_intent`. Justified by the differing concerns; they could be consolidated later.
- A long-running sandbox computation that emits no proxy I/O for `idle_threshold_seconds` will be paused and incur resume latency on its next access. Acceptable for the target Agent workload (periodic proxy I/O), and tunable.
- Resume is visible to the caller as a single slow request (seconds to tens of seconds). v1 does not surface a "waking up" state to the client.
- v1 requires a single server replica (process-local activity). Multi-replica needs a shared activity store or leader-elected scanner — a follow-up.

## Alternatives

- **Patch the Go controller to freeze TTL while paused (keep paused sandboxes indefinitely).** This is what you would need *if* paused sandboxes had to survive past their TTL. This proposal instead lets paused sandboxes expire naturally (see [Requirements](#requirements)) — a sandbox idle past its TTL has no state worth preserving — so no Go change is needed. Freeze-on-pause remains an option if a future use case demands unbounded pause retention.
- **Per-sandbox `extensions` opt-in** (e.g. `lifecycle.idle-pause.after-seconds`, mirroring `access.renew.extend.seconds`). More granular, but requires fork changes + new validation/codec paths for no benefit in the target deployment where idle-pause is a uniform cluster policy. Deferred — the scanner already reads extensions via `ExtensionService`, so the hook point is available when needed.
- **Short TTL + auto-renew only (no pause).** Rejected — it destroys intermediate files on expiry, the exact hallucination problem this proposal exists to solve.
- **fast-sandbox (`fleets`) for higher density.** Rejected — it does not support pause/resume or `resource_requests` overcommit, the two mechanisms this workload depends on; and it does not raise the physical memory ceiling.

## Infrastructure Needed

None. Reuses the existing Kubernetes runtime, OCI snapshot registry (from OSEP-0008), and in-process `asyncio` scheduling. No new services, stores, or third-party dependencies.

## Upgrade & Migration Strategy

- Default `enabled = false`; existing deployments are unaffected.
- Operators opt in by adding `[idle_pause] enabled = true` to the server TOML and restarting. No data migration, no schema change.
- Requires `renew_intent` enabled: resume does not extend TTL itself — the proxy access following a resume does, via `renew_intent`. Without it, resumed sandboxes are not re-extended and expire on their original schedule.
- Rollback: set `enabled = false` and restart. Already-paused sandboxes remain Paused until their next access (which resumes normally) or their TTL expires — no stranded state.
