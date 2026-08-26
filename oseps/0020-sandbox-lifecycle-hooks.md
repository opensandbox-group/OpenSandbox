---
title: Sandbox Lifecycle Hooks
authors:
  - "@Pangjiping"
  - "@peijianping"
creation-date: 2026-08-17
last-updated: 2026-08-26
status: implementing
---

# OSEP-0020: Sandbox Lifecycle Hooks

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Hook Set](#hook-set)
  - [Execution Channels](#execution-channels)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Public API](#1-public-api)
  - [2. Config Persistence](#2-config-persistence)
  - [3. In-Sandbox Config File](#3-in-sandbox-config-file)
  - [4. execd Lifecycle API](#4-execd-lifecycle-api)
  - [5. Orchestrated Transitions](#5-orchestrated-transitions)
  - [6. Signal-Driven preTerminate](#6-signal-driven-preterminate)
  - [7. Failure, Timeout, and Degradation Semantics](#7-failure-timeout-and-degradation-semantics)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Adds sandbox-level lifecycle hooks: `preStart`, `prePause`, `postResume`,
`preTerminate`, and cron-driven `periodic`. Declared declaratively in
`CreateSandboxRequest.lifecycle`; all hooks except `preStart` are updatable at
runtime via `PATCH /sandboxes/{sandboxId}/lifecycle`. Execution is split into
four channels chosen by event type (see [Execution Channels](#execution-channels));
the contract is identical on Docker and Kubernetes backends when the provider
topology satisfies the hook's capability requirements (R13).

## Motivation

Agent sandboxes must persist application/filesystem state before pause or
termination and restore it after resume. Today only task-level
`preStart`/`postStop` hooks exist (Kubernetes task-executor scope), which do not
cover the sandbox lifecycle.

Key events the hooks must cover (TTL expiry, idle-pause, error termination)
happen **without the user in the loop** — only declarative configuration can
guarantee a hook runs.

Related: [#1458](https://github.com/opensandbox-group/OpenSandbox/issues/1458) (request),
[#1366](https://github.com/opensandbox-group/OpenSandbox/issues/1366) (credential re-injection after resume),
[#1355](https://github.com/opensandbox-group/OpenSandbox/issues/1355) (fs sync before pause/snapshot),
[#1448](https://github.com/opensandbox-group/OpenSandbox/issues/1448) (idle-pause),
[openkruise/agents#743](https://github.com/openkruise/agents/issues/743),
[kubernetes-sigs/agent-sandbox#1237](https://github.com/kubernetes-sigs/agent-sandbox/issues/1237).

### Goals

1. Sandbox-level hooks for start, pause, resume, terminate, and periodic (cron) execution.
2. Runtime-agnostic: identical contract and behavior on capable Docker and Kubernetes providers.
3. Declarative at creation + runtime PATCH for all hooks except `preStart`.
4. Cover platform-driven transitions (TTL, idle-pause, eviction), subject to the hook's capability and grace constraints, not only user-initiated ones.
5. Per-hook timeout and failure policy; observable results; fully backward compatible (all hooks optional).

### Non-Goals

1. `postStart`/post-ready hooks (`preStart` + `postResume` cover boot and resume).
2. `preSnapshot`/`postSnapshot` — K8s pause embeds a rootfs snapshot already covered by `prePause`; standalone snapshot hooks deferred.
3. Server-side webhooks/notifications (`postTerminate` to external URLs).
4. Task-level hook unification — task-executor `preStart`/`postStop` remain unchanged.
5. K8s CRD schema changes — config rides a controller-ignored annotation; Windows in v1; 6-field cron/TZ; hook chaining.

## Requirements

| ID | Requirement |
|----|-------------|
| R1 | Hooks declared in `CreateSandboxRequest.lifecycle`; all optional |
| R2 | `preStart` executes in-sandbox before the entrypoint, with no server round trip; not PATCHable |
| R3 | `prePause`/`postResume` are triggered by an event-only server POST (`/v1/lifecycle/run`); `preTerminate` runs in-sandbox on SIGTERM; `periodic` runs on an execd ticker |
| R4 | `timeoutSeconds` defaults to 60. `preStart` accepts 1–10800 seconds; all other hooks accept 1–300 seconds. `failurePolicy`: `Abort` default for `prePause`/`postResume`; `preTerminate`/`periodic` fixed `Continue` (`Abort` rejected) |
| R5 | Abort: hook failure/timeout, lifecycle startup failure, or required execd lifecycle status being unreachable aborts the transition, leaving the pre-transition state with a machine-readable reason |
| R6 | Hooks never block **termination**: `preTerminate` runs only while `Running`; `Paused`/`Failed` sandboxes skip hooks (no SIGTERM handler / no pod) |
| R7 | `periodic` runs on cron inside execd without server liveness dependency; in-flight run of the same `name` skips the tick (no queueing) |
| R8 | Provider-held config survives server restarts (Docker: file-backed store; K8s: BatchSandbox/AgentSandbox annotation). Execd atomically persists TOML at the selected in-sandbox path across execd restarts and pause/resume; inability to resolve or write that path fails lifecycle startup |
| R9 | PATCH affects future transitions only (start-time snapshot); rejected with 409 when not `Running`; rejected for sandboxes whose execd lacks the lifecycle API (capability gate) |
| R10 | Termination grace covers `preTerminate` (K8s `terminationGracePeriodSeconds` / Docker `stop` timeout ≥ hook timeout + buffer); PATCH cannot raise the timeout beyond the provisioned grace |
| R11 | Pool-mode: per-sandbox injection via the existing task/alloc path (`spec.taskTemplate`), never the shared Pool template |
| R12 | `preTerminate` fires only on genuine termination, never on pause: K8s pause-induced pod deletions (`completePause` normal-mode deletion and pool-GC release deletion) use **grace-0 (immediate) deletion**, so no SIGTERM is delivered; Docker pause sends no signals — backend parity |
| R13 | Pool-mode `preTerminate` requires a process topology that delivers the task shim's TERM to execd. It is supported with `execd_run_as_init`; create/PATCH must reject `preTerminate` for the legacy background-bootstrap topology |
| R14 | `/ping` is liveness-only and remains available while `preStart` runs. Resume waits for a separate lifecycle startup-complete status before triggering `postResume` or publishing `Running` |

## Proposal

### Hook Set

| Hook | Channel | Declarative | Runtime PATCH | Failure default | Fires |
|---|---|---|---|---|---|
| `preStart` | execd startup | yes | no | Abort | before entrypoint, every boot (incl. K8s resume) |
| `prePause` | orchestrated (server triggers execd) | yes | yes | Abort | before pause (manual, idle, TTL-adjacent) |
| `postResume` | orchestrated (server triggers execd) | yes | yes | Abort | after resume, before public `Running` |
| `preTerminate` | signal-driven (execd catches SIGTERM) | yes | yes | Continue (fixed) | any platform termination of a `Running` sandbox (delete, TTL, eviction, `docker stop`) |
| `periodic[]` | execd cron ticker | yes | yes | Continue (fixed) | on schedule while the sandbox runs |

### Execution Channels

```
IN-SANDBOX channels (self-contained)          ORCHESTRATED channel (server-driven)
────────────────────────────────────          ────────────────────────────────────────
startup: bootstrap starts execd               transition: server POSTs an event-only
  → execd loads and persists config           trigger to execd → execd runs the hook
  → execd starts HTTP → runs preStart         resolved from effective config → result
  → normal: bootstrap starts entrypoint       decides advance/abort (prePause, postResume)
  → init: execd starts entrypoint
signal-driven: execd catches SIGTERM
  → run preTerminate → shutdown
timer-driven: execd cron ticker → periodic
```

Why: `preStart` is owned by the long-running execd process and runs after its
HTTP server starts, so hook duration does not consume the execd health-check
startup window. `/ping` therefore indicates liveness, not lifecycle readiness.
In normal mode, bootstrap waits for execd's private startup status before
launching the user process. In init mode, execd runs `preStart` before launching
the supervised entrypoint. It runs again on every boot, including K8s resume.
`preTerminate` is signal-driven —
every platform termination path (K8s pod delete for TTL/eviction/user delete,
Docker `stop`) ends in a SIGTERM to the container. Dedicated containers reach
execd through PID 1 bootstrap signal forwarding; Pool mode requires
`execd_run_as_init` so the task shim reaches execd directly (R13).
`prePause`/`postResume` are state-machine semantics: the server is the only
component that sees both the config and the transition, and it must wait for
the hook result before advancing. execd runs commands from its effective
lifecycle config on startup, trigger, ticker, or signal.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Config lost on server restart | Per-provider persistence re-read on rediscovery (R8) |
| PATCH races an in-flight transition | Start-time snapshot (R9) |
| Hook hangs forever | `timeoutSeconds` enforced by the execd runner |
| SIGKILL cuts `preTerminate` off | Grace ≥ hook timeout + buffer; grace remaining is the hard deadline (R10) |
| Old execd without lifecycle API | Capability gate on create/PATCH (R9) |
| Post-resume hook before lifecycle startup completes | Resume waits for lifecycle startup success after execd `/ping`, then runs `postResume` before public `Running` (R14); app readiness is the hook's own job |

## Design Details

> Code snippets are illustrative.

### 1. Public API

**CreateSandboxRequest.lifecycle** (`specs/sandbox-lifecycle.yml`, additive):

```yaml
lifecycle:
  preStart:                       # declarative only; PATCH rejects it
    timeoutSeconds: 60            # default 60
    command: ["/opt/hooks/pre-start.sh"]
  prePause:
    timeoutSeconds: 60
    failurePolicy: Abort          # Abort | Continue
    command: ["/opt/hooks/pre-pause.sh"]
  postResume: { timeoutSeconds: 60, failurePolicy: Abort, command: ["/opt/hooks/post-resume.sh"] }
  preTerminate:
    timeoutSeconds: 30            # ≤ provisioned grace − buffer
    command: ["/opt/hooks/pre-terminate.sh"]   # failurePolicy not allowed; fixed Continue
  periodic:
    - name: checkpoint            # unique within the sandbox
      schedule: "*/5 * * * *"     # 5-field cron; robfig descriptors (@hourly, @every 30s)
      timeoutSeconds: 60
      command: ["/opt/hooks/checkpoint.sh"]
```

`command` is an argv array (no shell expansion), matching the task-executor
`LifecycleHandler`. Event enum stays open (unknown events ignored).

**PATCH /sandboxes/{sandboxId}/lifecycle** — JSON Merge Patch (RFC 7396);
`null` removes a hook. Semantics:

- `400 PreStartNotPatchable`; `400` for `failurePolicy: Abort` on `preTerminate`/`periodic`; `400 GraceTooSmall` when raising `preTerminate.timeoutSeconds` beyond provisioned grace − buffer (TTL/eviction use the pod's original grace and bypass the server).
- `409 SandboxNotRunning` — non-`Running` sandboxes cannot receive the live execd update; deferred application would leave persisted config and live schedule inconsistent.
- Capability gate: rejected when the sandbox's execd lacks the lifecycle API (so `Abort`-default hooks never 404 mid-transition).
- Applies to future transitions only; no optimistic locking (single-writer assumption, as with metadata PATCH).
- Persistence: Docker file-backed store / K8s annotation; live push to execd (`POST /v1/lifecycle/config`, §4).

### 2. Config Persistence

No general server-side sandbox record exists today (only snapshot records), so
config rides the existing per-provider state:

| Provider | Storage | Rationale |
|---|---|---|
| Docker | file-backed store (same mechanism as `services/docker/metadata.py`) | labels on running containers are **immutable** |
| K8s | `sandbox.opensandbox.io/lifecycle` annotation on BatchSandbox **or AgentSandbox** — whichever workload CR the configured provider manages (`provider_factory.py` registers both) | schemaless, no CRD change; controller ignores the key (server-only contract; add to `kubernetes/AGENTS.md` annotation list) |
| Both | env `OPENSANDBOX_LIFECYCLE` (JSON content) at create | **transport only** — execd validates it and atomically persists the effective config; failure to persist aborts lifecycle startup, while provider-held config remains the recovery source |

**Pod-creation source stays current.** On PATCH, the server also updates the
pod-creation source — BatchSandbox `spec.template` / `taskTemplate` env, or
AgentSandbox `spec.podTemplate` env, whichever provider manages the sandbox —
so a **replacement** pod created by eviction, node drain, or pod failure
materializes the current config; creation-time env is a bootstrap default,
never the source of truth. The env change must not recycle the running pod:
the running pod receives the update via the live execd push only, and
implementation must verify the controller does not recreate the pod for this
env change (if it does, the update must be applied so its pod effect is
deferred to replacement).

### 3. In-Sandbox Config File

`$HOME/.execd/lifecycle.toml` is the default in-sandbox lifecycle config
(atomic write, `version` key). Operators may set `EXECD_LIFECYCLE_CONFIG` to
an explicit path; execd and bootstrap use that value as-is and do not fall
back to the default path:

```toml
version = 1

[preStart]
command = ["/opt/hooks/pre-start.sh"]
timeout_seconds = 60

[prePause]
command = ["/opt/hooks/pre-pause.sh"]
timeout_seconds = 60

[postResume]
command = ["/opt/hooks/post-resume.sh"]
timeout_seconds = 60

[preTerminate]
command = ["/opt/hooks/pre-terminate.sh"]
timeout_seconds = 30

[[periodic]]
name = "checkpoint"
schedule = "*/5 * * * *"
command = ["/opt/hooks/checkpoint.sh"]
timeout_seconds = 60
```

- **Location**: resolving from `HOME` makes the default writable for root and
  normal non-root images. Platform deployments must ensure the default HOME
  participates in the sandbox persistence/snapshot contract. An operator that
  selects an explicit path owns the same durability requirement. Do not use the
  K8s `opensandbox-bin` emptyDir (`/opt/opensandbox`).
- **Current startup load order** (`preStart` + `periodic`, no PATCH): a nonblank
  injected env is authoritative, is validated, and atomically refreshes the
  persisted file; when the env is absent, execd loads the file. Failure to
  resolve or persist either the default or an explicit path is fatal when a
  transport config is present. Runtime PATCH persistence/reconciliation is
  deferred with the PATCH phase.
- **Trust boundary**: hooks run as execd's OS user in the sandbox's existing
  container namespaces and do not use isolated-session confinement. The same
  identity (and any root workload) can modify or remove the persisted file.
  Hooks are trusted setup/maintenance code, not a tamper-resistant audit or
  mandatory-policy mechanism.
- **Persistence principles** (apply to any future daemon state): config files
  are TOML (matching `--isolation-config`); config vs. rebuildable runtime
  state are separated (hook run results/counters stay in memory —
  `GET /v1/lifecycle/status` reflects the current process only); one concern =
  one file/dir, events use append-only JSONL. Invalid transport or persisted
  config and transport persistence failures fail closed.

### 4. execd Lifecycle API

New routes (`pkg/web/router.go`), documented in `specs/execd-api.yaml`:

```yaml
POST /v1/lifecycle/run    # trigger by event; execd resolves the command from its config file
  body: { "event": "prePause" }
  200 → { executed: true,  exitCode, stdout, stderr, durationMs }
  200 → { executed: false, reason: "not_configured" | "config_unavailable" }
  504 → { executed: false, reason: "hook timeout" }        # 504 is the only timeout status

POST /v1/lifecycle/config # replace the whole in-sandbox config (hot update on PATCH; atomic persist)
  body: { prePause: {...}, preTerminate: {...}, periodic: [...] }
  # The server always sends the complete merged five-hook config from its
  # stored state — omitting a section removes it (incl. immutable preStart
  # and unchanged postResume must be re-sent).

GET  /v1/lifecycle/status
  # startup.state: pending | running | succeeded | failed
  # plus per-hook state: lastRunAt, lastExitCode, consecutiveFailures, nextRunAt
```

Design notes:

- `run` reuses the command runtime (`pkg/runtime/command.go`) synchronously
  (not `/command`'s background+poll shape — hooks need a hard deadline) and
  serves only `prePause`/`postResume`. **The request carries only the event,
  never the command** — execd's effective lifecycle config is the in-sandbox
  source of truth for every hook (the persisted file and the current process's
  matching validated startup config); the server stores config only for
  validation/display/re-provisioning.
- `executed: false` is explicit and differentiated: a hook the server believes
  is configured reporting `not_configured`/`config_unavailable` is treated as
  hook failure (R5) — the configured state flush must not silently vanish.
- `status.startup.state` is scoped to the current execd process. It becomes
  `succeeded` after `preStart` completes successfully, or immediately when no
  `preStart` is configured. `/ping` may already succeed while this state is
  `pending` or `running`; resume must not use `/ping` as its readiness gate.
- `preTerminate` has **no endpoint** — execd runs it in a SIGTERM handler (§6).
- Periodic scheduling is a `robfig/cron/v3` ticker inside execd (5-field cron +
  descriptors, container-local timezone), started from the injected env and
  replaced live by `POST /v1/lifecycle/config`. One in-flight run per `name`;
  Docker pause freezes the ticker naturally; K8s resume reconstructs it from
  the provider-injected or persisted effective config.
- `prePause` does not wait for in-flight periodic runs — the final flush is
  `prePause`'s own responsibility.

### 5. Orchestrated Transitions

| Transition | Sequence |
|---|---|
| Pause (manual, idle, TTL-adjacent) | trigger `prePause` → policy result → Docker `pause` / K8s patch `spec.pause=true` |
| Resume | Docker `unpause` / K8s patch `pause=false` → wait pod re-creation + execd `/ping` (liveness) → wait lifecycle startup `succeeded` → trigger `postResume` → success → public `Running`; startup failure, lifecycle-status unreachability, or hook abort → roll back to `Paused` |
| Terminate (user delete, Docker `stop`) | server deletes; runtime SIGTERMs the container; execd runs `preTerminate` (§6); server only ensures grace ≥ hook timeout (R10) |
| Docker TTL expiry | server timer routes expiration through a **grace-bounded `container.stop(timeout)`** (SIGTERM → hook → SIGKILL), then `remove` — never `kill` + `remove(force=True)` (the current `_expire_sandbox` path, which would skip the hook) |
| K8s TTL/eviction/node drain | controller/kubelet deletes the pod; SIGTERM path — no server involvement |

**K8s resume state gating**: the controller sets the CR phase to `Succeed`
as soon as the pod is ready, independently of execd's lifecycle startup state
and the server-side `postResume` call, so the CR phase cannot express
"resuming, hooks in progress". The server owns the public `SandboxState`
mapping: after `/ping` confirms liveness, it waits for
`GET /v1/lifecycle/status` to report startup `succeeded`, then triggers
`postResume`, and keeps reporting `Resuming` until both complete. Startup
failure, lifecycle-status unreachability, or an `Abort` result causes the server
to re-patch `spec.pause=true` so the controller re-pauses → public `Paused`. No
controller or CRD change needed.

Client visibility is unchanged: transitions surface as
`Pausing`/`Resuming`/`Stopping`, hook results in `reason`/`message`.

**preStart**: execd loads and persists the lifecycle config,
starts its HTTP server, then runs `[preStart]` with the configured timeout. In
normal mode, bootstrap starts execd first and waits on a private,
watchdog-bounded startup status channel; only a successful result releases the
user entrypoint. In init mode, execd is PID 1 and launches the supervised
entrypoint only after `preStart` succeeds. The public lifecycle startup status
tracks the same result for orchestrated resume; `/ping` remains responsive while
the hook runs. A failure or timeout aborts startup in both modes.

### 6. Signal-Driven preTerminate

Every platform termination converges on SIGTERM to the container (K8s:
kubelet during pod deletion; Docker: `docker stop`). In dedicated normal-mode
containers PID 1 bootstrap forwards TERM to execd (`_shutdown_children`); in
init mode execd is PID 1 and receives it directly. The legacy Pool task shape
backgrounds bootstrap behind a task shim and does not provide this guarantee,
so `preTerminate` is unavailable there unless `execd_run_as_init` is enabled
(R13). execd:

```go
// main.go: intercept SIGTERM, run preTerminate to completion, then shut down.
// The hook runs BEFORE the server context is canceled and before main returns —
// a goroutine started after cancellation would race process exit.
sig := make(chan os.Signal, 1)
signal.Notify(sig, syscall.SIGTERM)          // external container stop; v1 topology
<-sig                                        // (bootstrap forwards only trusted TERM to execd)
if hook := lifecycle.Load(resolvedConfigPath()).PreTerminate; hook != nil {
    runHook(hook, timeUntilGraceDeadline())  // bounded; reads the file fresh, never cached
}
server.Shutdown(shutdownCtx)                 // then stop HTTP and exit with the hook's outcome logged
```

Properties:

1. On capable process topologies (R13), covers platform-driven terminations with no server in the path (TTL, eviction, node drain, delete, `docker stop`) — the same delivery path as user-initiated ones.
2. `Running`-only by construction: a paused Docker sandbox is frozen, a paused K8s sandbox has no pod — no handler runs, termination proceeds (R6).
3. Inherently best-effort: SIGKILL follows the grace period regardless → fixed `Continue` (R4).
4. Grace-bounded: the deadline is `min(timeoutSeconds, grace remaining)` (R10).
5. Runs concurrently with the app's own shutdown in today's topology (TERM forwarded to both) — hook contract is state flush, not app coordination.
6. Resolves the effective lifecycle config at signal time, so the latest PATCHed definition applies.
7. OSEP-0018 interaction: as PID 1, execd must distinguish the *external* container-stop SIGTERM from an in-namespace `kill 1` (open item already tracked in OSEP-0018 §3); the v1 topology is unaffected.
8. **Never on pause** (R12): the K8s pause flow deletes the running pod (`completePause`), and kubelet would SIGTERM it — so pause-induced deletions (normal-mode `completePause` and pool-GC release) use grace-0 immediate deletion (SIGKILL, no SIGTERM). Docker pause sends no signals. `preTerminate` therefore runs only on genuine termination, with backend parity.

### 7. Failure, Timeout, and Degradation Semantics

- **Timeout**: enforced at the execd runner (kill, `504`/`hook timeout`); never counted as success.
- **Abort** (`prePause`/`postResume` and the resume startup gate): hook failure/timeout, startup `failed`, or the required lifecycle API remaining unreachable until the transition deadline → transition aborts, sandbox returns to its pre-transition state, reason recorded; user may retry.
- **Continue** (`preTerminate`, `periodic`): failure recorded, transition proceeds.
- **Not `Running`** (`Paused`/`Failed`/pod gone): hooks skipped; termination never blocks on a hook.
- **Idempotency**: one execution per transition — the existing phase state machine (`Pausing`/`Resuming`/`Stopping`) and K8s `PauseObservedGeneration` prevent re-entry; hooks should be idempotent (failed `Abort` transitions may be retried). `preTerminate` runs at most once per container lifetime.

## Test Plan

- **Unit**: schema validation (shapes, `periodic.name` uniqueness, cron parse, PATCH merge, `preStart` rejection); execd `run` (timeout kill, output, exit code, `executed:false` differentiation); lifecycle startup status (`pending`/`running`/`succeeded`/`failed`, including no-hook success); SIGTERM handler (effective-config-resolved `[preTerminate]`, grace-bounded deadline, exit-after, absent-hook no-op); periodic (scheduling, in-flight skip, hot replacement, failure counter); config file (TOML parse, unsupported `version`, atomic write, load order, invalid-config or persistence failure); failure-policy resolution.
- **Integration**: bootstrap starts execd before the user entrypoint; execd serves `/ping` while `preStart` is still non-ready, then reports lifecycle startup success before the entrypoint starts. Cover hook failure, execd failing before status, and watchdog termination when execd never reports status. Server orchestration uses a fake execd to verify resume waits for lifecycle startup success before `postResume`, plus hook ordering, abort paths, and lifecycle-status unreachability per R5.
- **E2E (Kind / Docker)**: Docker — `prePause` marker before `docker pause`, `postResume` after `unpause`, periodic checkpoints (ticker frozen while paused), `preTerminate` marker on `docker stop` **and on TTL expiry** (grace-bounded stop path). K8s — during resume, `/ping` succeeds while a long `preStart` still keeps lifecycle startup non-ready; `postResume` and public `Running` occur only after startup succeeds, while startup failure or lifecycle-status unreachability rolls back to `Paused`. Also cover TTL/delete/eviction producing the `preTerminate` marker and paused sandbox deletion producing none. Pool TTL/delete tests must cover `execd_run_as_init`; non-init Pool create/PATCH requests containing `preTerminate` must be rejected. PATCH mid-flight snapshot; server restart restore (file-backed store/annotation); config persistence across execd restart and both pause/resume models; replacement pod after eviction materializes the PATCHed config from the updated pod-creation source.

## Drawbacks

1. Contract surface grows (lifecycle field + PATCH endpoint + execd API) — every field optional and additive.
2. execd gains a scheduler + signal handler. Hook code deliberately shares execd's OS identity and container namespaces; the documented trust boundary must be acceptable for each use case.
3. Four execution channels — each event is mapped to exactly one channel in the contract to avoid ambiguity.
4. `preTerminate` is bounded by the termination grace; hooks must stay small (R10).
5. Hooks are in-sandbox code — a buggy hook slows transitions (bounded by timeout) or breaks boot (`preStart`).

## Alternatives

1. **Runtime-only hooks** (server POSTs commands at transition, no spec config) — rejected: TTL, idle-pause, and error paths have no caller present; hooks would silently not run.
2. **All hooks in-sandbox** (execd interprets lifecycle events) — rejected: execd cannot observe pause requests (Docker freeze is external; K8s pause is a CR patch); only the server sees config + event together.
3. **Hooks in the Kubernetes task-executor** — rejected: not runtime-agnostic (no task-executor in Docker sandboxes); task-scoped, not lifecycle-scoped.
4. **Kubernetes `preStop` container hooks** — rejected: bounded by termination grace, cannot express pause/post-resume, absent in Docker.
5. **Server-side timers for periodic** — rejected: server downtime stops checkpoints; per-sandbox timers don't scale; in-sandbox ticker is simpler and robust.

## Infrastructure Needed

- `robfig/cron/v3` added to `components/execd` (vendored, per repo convention).
- No new services, storage, or third-party infrastructure.

## Upgrade & Migration Strategy

**Backward compatible**: all fields optional; sandboxes without `lifecycle`
behave exactly as today; no existing endpoint/schema/CRD change (K8s config
rides a new, controller-ignored annotation).

**Phased rollout**:

1. execd: config persistence plus execd-owned `preStart` and `periodic`; tests and execd image release.
2. `specs/sandbox-lifecycle.yml`: additive `CreateSandboxRequest.lifecycle` with `preStart` and `periodic`; regenerate SDKs and add server create-time transport.
3. Future phase: `run`/`config`/`status` endpoints (including lifecycle startup readiness), PATCH, and the remaining transition hooks after their deferred persistence/failure semantics are resolved.
4. Docker and K8s provider orchestration, grace-period wiring, and transition E2E coverage.
5. Pool-mode parity via per-sandbox `spec.taskTemplate` injection (R11), with `preTerminate` gated on `execd_run_as_init` until another explicit TERM route exists (R13).

**Docs**: `docs/` lifecycle hook guide (hooks, failure policies, idempotency,
cron reference), `kubernetes/AGENTS.md` annotation contract note, SDK
regeneration for the `lifecycle` field and PATCH endpoint.
