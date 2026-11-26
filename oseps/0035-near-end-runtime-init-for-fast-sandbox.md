---
title: Near-End Runtime Initialization for Fast Sandbox
authors:
  - "@Pangjiping"
creation-date: 2026-11-25
last-updated: 2026-11-26
status: implementing
---

# OSEP-0035: Near-End Runtime Initialization for Fast Sandbox

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Why Fast Sandbox needs runtime initialization](#why-fast-sandbox-needs-runtime-initialization)
  - [Why `opensandbox-server` must not call `execd` remotely](#why-opensandbox-server-must-not-call-execd-remotely)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Flow diagrams](#flow-diagrams)
  - [Contract summary](#contract-summary)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Responsibility boundaries and prior art](#1-responsibility-boundaries-and-prior-art)
  - [2. Unified `SandboxPool` watch (`PoolCRReader`)](#2-unified-sandboxpool-watch-poolcrreader)
  - [3. Template build modes and `ActionBinding` mapping](#3-template-build-modes-and-actionbinding-mapping)
  - [4. Stateless `execd` token derivation on Fast Sandbox](#4-stateless-execd-token-derivation-on-fast-sandbox)
  - [5. Impact on `fast-sandbox` (`ActionHandler.exec`)](#5-impact-on-fast-sandbox-actionhandlerexec)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

`execd` already implements deferred runtime initialization (`--runtime-init` / `EXECD_RUNTIME_INIT`, `POST /internal/init`, `GET /ready`, and atomic `RuntimeBinding` swap), hardened by [PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103) (`--init-auth`, 1 MiB body limit, `500` startup rollback, token rotation rules) and [PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111) (Ingress `403` block on port `44772` `/internal/init`). However, on the Fast Sandbox (`fsb`) backend, Golden Image VM snapshots and `Full` sandbox snapshots start from pre-booted guest memory where container-level environment variables are already frozen, leaving per-sandbox identity (`sandboxId`, `generation`), runtime `env`, telemetry tags, and stateless `execd` token verifiers ([PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080)) unbound.

This OSEP proposes per-sandbox runtime initialization on Fast Sandbox **entirely at the near end (`Fastlet`) with zero `opensandbox-server` → `execd` network calls**, reusing `components/execd`, `components/ingress`, `FastletProxy`, and existing `Sandbox` binding fields unchanged. Its default path uses existing `ActionBinding.input`, but the upstream implementation also adds an optional Protobuf sensitive-input channel (see Section 5).

**Implementation status**: Pool discovery/RBAC and template-env capability validation are implemented. The schema allows `templateId` with `env`; `FastSandboxService` rejects non-empty env on pools without `runtime-init`. This is validation preparation only: runtime-init binding delivery, Fast Sandbox `execd` authentication, and deferred-template classification remain unimplemented, so passing validation does not apply env to the guest and must not be advertised as working runtime-env support. The high-level template-create APIs in Python, JavaScript, Kotlin, Go, and C# do not yet expose per-create env; their API additions and end-to-end delivery tests belong to the later implementation. The upstream implementation baseline is [fast-sandbox PR #269](https://github.com/opensandbox-group/fast-sandbox/pull/269) at [commit `fb176be93fa1b8a2f75b66847bc418b6a07af6e6`](https://github.com/opensandbox-group/fast-sandbox/tree/fb176be93fa1b8a2f75b66847bc418b6a07af6e6).

1. **Per-sandbox payload via `ActionBinding`**: `opensandbox-server` packs the standard `RuntimeInitRequest` JSON (`sandboxId`, `generation`, 2-part `accessTokenHash`, `entrypointPolicy`, `preserveRuntimeState`, `envs`, `telemetry`) into `CreateSandboxRequest.action_bindings` (`handler: "runtime-init"`, ordered after `"egress"`).
2. **Pool-owned command-line template (`ActionHandler.exec`)**: The upstream implementation lets a `SandboxPool` declare a host-local `execve` `argv` template with binding input always delivered on stdin and execution always skipped when `Restored || Recovered`. It runs plain `curl` against `http://{{.Attachment.Network.IP}}:44772/internal/init` today; direct in-VM execution over `virtio-vsock` is only a future transport option requiring runtime-driver and template-context extensions. `FastPath.ResolveEndpoint` gates route-credential issuance on aggregate `Ready` (all `ActionBindings` `ActionReady`), keeping ordinary creates and clones fail-closed until initialization succeeds, including async cold boots. This guarantee does not cover the accepted recovery crash window described below.
3. **Unified `SandboxPool` Informer watch (`PoolCRReader`)**: `opensandbox-server` watches `sandboxpools.sandbox.fast.io/v1alpha2` via the shared `K8sClient` `WorkloadInformer`, unifying real-time `actionHandlers` capability discovery with [OSEP-0026](0026-resource-oversubscription-accounting-for-fast-sandbox.md) `ResourceAware` mode and default pool resource lookups — eliminating both the 15-minute `_pool_mode_cache` and per-create `FastPath.GetPool` gRPC round-trips.

## Motivation

### Why Fast Sandbox needs runtime initialization

On `fsb`, `SandboxTemplate` Golden Images capture a running Firecracker VM after `execd` has already booted as PID 1 ([OSEP-0018](0018-execd-as-sandbox-init.md)), and `Full` snapshots capture live workload memory. At create or clone time:

- **Template creates cannot inject per-sandbox `env` or `OPENSANDBOX_ID` via OCI config**: the guest process tree is restored from `vmstate`. The schema now admits template env and the service checks the selected pool's capability, but the template mapping does not yet deliver a runtime-init binding. Consequently env is not applied and `execd` still reports the build-time placeholder identity in OTel metrics and eBPF audit records.
- **Stateless `execd` authentication ([PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080)) does not yet cover `fsb`**: Kubernetes `BatchSandbox` / `AgentSandbox` workloads receive `EXECD_ACCESS_TOKEN="sha256:<kid>:<nonce>:<verifier>"` through Pod env, which does not apply to restored `fsb` VMs or cloned `Full` snapshots (where a clone would otherwise inherit the source sandbox's in-memory token).

### Why `opensandbox-server` must not call `execd` remotely

Having `opensandbox-server` call `POST /internal/init` over the data plane after `CreateSandbox` returns breaks down across three structural boundaries:

1. **Pre-init exposure window (breaks fail-closed)**: Reaching `execd` from the server through the data plane requires the sandbox route to become externally routable *before* `accessTokenHash` and runtime `env` are installed, directly conflicting with [PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111) (which blocks `/internal/init` at Ingress).
2. **Async cold boot (`202 Pending`)**: When a cold Fastlet parks a sandbox for background image/snapshot pull (`parkForImageDelivery`), `CreateSandbox` returns `Pending` immediately; `opensandbox-server` is no longer on the call stack when the VM eventually boots.
3. **Cross-hop latency and half-initialized orphans**: An extra control-plane → proxy → node → guest RTT adds tail latency to sub-100ms Firecracker restores and leaves `Running` but uninitialized sandboxes behind on transient network blips.

### Goals

- **Zero control-plane callback**: Execute `runtime-init` locally from `Fastlet` before a new sandbox becomes `Ready` or routable via `FastPath.ResolveEndpoint`, subject to the explicitly accepted recovery crash window.
- **Reuse `execd` and `ingress` unchanged**: Consume existing `POST /internal/init` ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)) and Ingress `:44772/internal/init` blocking ([PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111)) with zero `execd` or `ingress` code changes.
- **Protocol- and transport-decoupled `fast-sandbox` surface**: Carry per-sandbox init data in existing `ActionBinding.input`; keep `fast-sandbox` agnostic to `execd` HTTP paths and JSON schemas via a minimal command-line `ActionHandler.exec` template on `SandboxPool` (supporting HTTP via `curl` today and `vsock` command execution later).
- **Watch-driven pool discovery + `OSEP-0026` unification**: Replace `FastPath.GetPool` polling/TTL caching in `opensandbox-server` with a shared `WorkloadInformer` watch on `SandboxPool` CRs covering `actionHandlers`, `ResourceAware` mode, and pool default resources.
- **Extend stateless HMAC `execd` authentication ([PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080)) to `fsb`**: Bind one-way `sha256:<hex>` digests at create/clone time via `ExecdTokenManager` and return derived `X-EXECD-ACCESS-TOKEN` headers on `GetEndpoint(44772)` with zero plaintext tokens in etcd, CRDs, or node disk.

### Non-Goals

- **Public `entrypointPolicy` override field on `CreateSandboxRequest`**: Entrypoint policy (`"restart"` vs `"keep"`) is derived deterministically by the server from the template/snapshot source kind in Phase 1.
- **Sandbox lifecycle hooks ([OSEP-0020](0020-sandbox-lifecycle-hooks.md)) on `fsb`**: `preStart`, `periodic`, `prePause`, `postResume`, and `preTerminate` evolve independently and remain rejected on `fsb` in Phase 1.
- **Credential Vault transport changes**: Secret material continues to use the [OSEP-0022](0022-multi-sandbox-egress-control-plane.md) handler-relay channel and never enters `ActionBinding`.
- **`execd`-specific changes to `execd`, `ingress`, `FastletProxy`, or `Sandbox` binding fields**: Reuse the existing runtime-init API and binding payload. This is not a claim of zero upstream Protobuf changes: PR #269 adds optional `ActionBinding.sensitive_input = 3`, unused by this OSEP's default path. OpenSandbox work also includes lifecycle spec and SDK consumer alignment, not only server and Helm RBAC changes.

## Requirements

- **Fail-closed readiness, subject to the accepted recovery window**: `FastPath.ResolveEndpoint` must reject route resolution (`FailedPrecondition`) while `runtime-init` is pending. All command failures, including HTTP `4xx/5xx` surfaced as curl exit `22`, receive bounded retries; exhaustion fails the sandbox, deletes the newly created CR, and returns synchronous `FailedPrecondition` without trying another node. Recovery's unconditional skip can mark an uninitialized binding ready in the accepted crash window; strict fail-closed behavior does not cover that window.
- **No command injection**: User-supplied `request.env` keys/values may contain arbitrary shell metacharacters, newlines, or quotes; `Fastlet` must pass `ActionBinding.input` strictly over `stdin` to direct `execve` `argv` arrays, never interpolating `binding.input` into command arguments or invoking a shell.
- **No plaintext token persistence**: Only irreversible `sha256` verifiers may appear in `Sandbox.spec.actionBindings`.
- **Preserve checkpoint state on `Pause -> Resume`**: Resuming a paused sandbox restores its already-initialized `execd` memory (`RuntimeBinding`, `accessTokenHash`, and running user processes); the exec handler unconditionally skips execution when `Restored || Recovered`, rather than exposing a configurable restore policy.
- **Zero-downtime pool-scoped rollout**: Pools without a `"runtime-init"` `ActionHandler` continue serving existing workloads unchanged; pools opting into `"runtime-init"` require Golden Templates built with an `/internal/init`-capable `execd` image, and deferred-entrypoint templates (`EXECD_RUNTIME_INIT=1`) fail fast if targeted at a pool lacking `"runtime-init"`.

## Proposal

### Flow diagrams

Ordering `actionBindings` as `[egress, runtime-init]` on `sandbox.data-plane-ready` ensures that **egress policy and subject are ready before `runtime-init` launches the user `entrypoint`**. Vault credentials are installed through a separate API call after sandbox creation; egress action readiness does not guarantee that credentials are pre-provisioned. On the normal create path below, **`FastPath.ResolveEndpoint` stays gated (`Ready=false`) until `runtime-init` exits `0`**; the accepted recovery crash window remains an exception.

```mermaid
sequenceDiagram
    participant C as SDK / Caller
    participant S as OpenSandbox Server
    participant W as PoolCRReader (K8s Watch)
    participant FP as FastPath / Controller
    participant FL as Fastlet (ActionManager)
    participant EG as Egress Handler (Loopback)
    participant CMD as Exec Runner (curl)
    participant EX as Guest execd (:44772)

    C->>S: POST /v1/sandboxes (templateId / image / snapshotId, env)
    S->>W: get_snapshot(namespace, poolRef) [O(1) Informer memory]
    W-->>S: PoolSnapshot(is_resource_aware, resources, action_handlers)
    S->>S: ExecdTokenManager.mint_env_value() -> sha256:<kid>:<nonce>:<hex>
    S->>FP: CreateSandbox(action_bindings=[egress, runtime-init], completion=READY)
    FP->>FL: Persist Sandbox CR & dispatch CreateSandbox
    FL->>FL: EnsureSandbox (slot IP attached; Ready=false)
    Note over FP,FL: ResolveEndpoint rejects external access while Ready == false
    FL->>EG: SET_BINDING (install deny-first subject rules)
    FL->>EG: LIFECYCLE_HOOK (sandbox.data-plane-ready -> activate egress policy)
    FL->>CMD: execve argv template (stdin: binding.input)
    CMD->>EX: POST http://<slot.IP>:44772/internal/init (future: vsock exec)
    EX->>EX: Atomically apply RuntimeBinding -> start/keep entrypoint
    EX-->>CMD: 200 OK ({"status": "initialized"})
    CMD-->>FL: exit 0
    FL->>FL: runtime-init ActionReady -> Aggregate Ready = true
    FL-->>FP: SandboxInfo (Ready=true)
    FP-->>S: SandboxInfo (Ready=true)
    S-->>C: 202 Accepted (body status Running; Pending on cold image pull, converging in background)
```

### Contract summary

| Concern | Authoritative Owner | Mechanism |
| --- | --- | --- |
| Pool capability & resource mode discovery | `OpenSandbox Server` (`PoolCRReader`) | Shared `WorkloadInformer` (`LIST/WATCH` on `sandboxpools.sandbox.fast.io/v1alpha2`) |
| Per-sandbox init intent | `OpenSandbox Server` (`create_mapping.py`) | `CreateSandboxRequest.action_bindings` (`handler: "runtime-init"`, JSON `input`) |
| Stateless `execd` token verifier | `OpenSandbox Server` (`ExecdTokenManager`, PR #3080) | `"accessTokenHash": "sha256:<hex>"` + `"accessTokenVerifier": "sha256:<kid>:<nonce>:<hex>"` in `ActionBinding.input`; `GetEndpoint(44772)` calls `resolve_raw_token` from cached `Sandbox` CR |
| Near-end transport & execution | `SandboxPool` & `Fastlet` (`fast-sandbox`) | `SandboxPool.spec.actionHandlers[].exec` (`command[]`, `timeoutSeconds`, `maxRetries`); fixed stdin input, fixed `Restored || Recovered` skip, bounded retries then sandbox failure |
| `/internal/init` endpoint & Ingress protection | `execd` ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)) & `ingress` ([PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111)) | Reused unchanged (`403` at Ingress on port `44772`; 1 MiB body limit, `500` binding rollback, and optional `--init-auth` in `execd`) |

### Notes/Constraints/Caveats

- **Pool-capability gates for template creates (`request.env` and deferred entrypoints)**:
  - **Implemented validation preparation**: `api/schema.py` allows `templateId` together with `env`. `FastSandboxService` checks the selected pool's `runtime-init` capability before mapping; non-empty env on a pool without that handler returns `400 INVALID_PARAMETER` with pool-capability guidance. Empty or omitted env passes this gate. When SandboxPool reads fall back to `GetPool` after RBAC denial, handler capability cannot be established and non-empty env is rejected. The public lifecycle spec and regenerated SDK descriptions reflect this gate.
  - **Pending delivery**: On a capable pool, passing validation does not yet apply env to the guest. Template mapping must emit a `runtime-init` binding before this can be released as runtime-env support; high-level SDK template-create options and delivery tests must then be added.
  - Conversely, if a template was built with `"entrypointPrewarmed": false` (`EXECD_RUNTIME_INIT=1`, so `execd` waits idle for `/internal/init` before starting `entrypoint`) and targets a pool *without* `"runtime-init"`, `FastSandboxService` will reject the request with `400 INVALID_PARAMETER` rather than omitting the binding and stranding the sandbox with an unstarted `entrypoint`.
- **`ActionBinding.input` size bound & create-only immutability**: `fast-sandbox` caps each `ActionBinding.input` (64 KiB), enforces create-only immutability on `exec` `ActionBindings` (rejecting post-create `UpdateSandbox` additions, removals, or modifications), and `execd` caps `/internal/init` at 1 MiB ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)); `create_mapping.py` validates the serialized `runtime-init` binding size up front and returns `400 INVALID_PARAMETER` before calling `FastPath`.
- **`Pause -> Resume`, `Fastlet` restart recovery, and `Full` snapshot clone**:
  - **`Pause -> Resume` (`Restored == true`) & in-place `Fastlet` restart (`Recovered == true`)**: Normally, both restore or adopt an already-running sandbox whose `RuntimeBinding` is active in `execd` memory. `SandboxManager.Recover` stamps an in-memory `Recovered = true` flag on recovered sandboxes (zero disk files or extra volume mounts), and `ActionHandler.exec` unconditionally skips re-execution whenever `Restored || Recovered`, marking the binding `ActionReady` immediately. Known edge window: if `Fastlet` crashes during the brief millisecond window after `EnsureSandbox` boots a VM but before `runtime-init` finishes, `Recovered` will skip `runtime-init` on restart; this narrow crash window is accepted rather than maintaining per-sandbox disk state.
  - **`Full` snapshot clone (`CreateSandbox` from `snapshotId`)**: Boots as a new sandbox (`Restored == false && Recovered == false`) with `preserveRuntimeState: true` and a freshly minted `accessTokenHash`, so `Fastlet` runs `runtime-init` once to rotate identity, token hash, `envs`, and telemetry while keeping live snapshot processes running (with `EXECD_INIT_AUTH` left at its default `false`; processes inside the guest already execute within the same single-sandbox trust boundary, while external access to `:44772/internal/init` is blocked at Ingress).

### Risks and Mitigations

- **Server rollout ahead of Helm RBAC update (`403` on `sandboxpools`)**: If the `opensandbox-server` image upgrades before the Helm chart adds `sandboxpools` `get/list/watch` to its `ClusterRole`, `PoolCRReader` catches `ApiException(status=403)`, logs a structured warning, and falls back to `FastPath.GetPool` for `is_resource_aware` and `resources` (treating `action_handlers` as empty), preserving existing traffic while failing closed (`400`) on deferred-entrypoint templates that strictly require `runtime-init`.
- **Pre-existing Golden Templates (pre-`/internal/init` `execd` vs pre-OSEP-0035 `/internal/init`-capable `execd`)**:
  - Golden VM snapshots restore the exact `execd` binary frozen in guest memory at template-build time (`spec.execd`). Templates built **before `execd` added `POST /internal/init`** do not expose the endpoint at all and would fail with HTTP `404` (sandbox failure after bounded retry exhaustion) if scheduled onto a pool that emits `runtime-init`; operators must either rebuild those legacy templates with a current `execd` image before adding `runtime-init` to an existing pool, or roll out `runtime-init` on a new `SandboxPool` and migrate traffic once templates are rebuilt.
  - Templates already built with an `/internal/init`-capable `execd` image (`EXECD_INIT=1`) but predating this OSEP simply lack the `"entrypointPrewarmed"` flag in `spec_json`; the server defaults missing metadata to `entrypointPolicy = "keep"`, applying `RuntimeBinding` (`sandboxId`, `accessTokenHash`, `envs` for new `/command` executions) without restarting the already-running entrypoint.
- **Transient guest readiness delay, lost-response retry, and deterministic init error**: All command failures retry locally within the configured bound, including `curl --fail-with-body` exit `22` on HTTP `4xx/5xx`, TCP connect refusal (`7`), and transfer timeout (`28`). `maxRetries` counts retries after the initial attempt (`10` permits at most `11` attempts). Exhaustion tears down the sandbox, deletes its newly created `Sandbox` CR, and returns synchronous `FailedPrecondition` without penalizing the healthy `Fastlet` or trying another node. If `execd` applied `/internal/init` just before a response timeout (`28`), an ordinary in-process retry runs while route resolution remains gated: `RuntimeInitManager.Apply` serializes under `m.mu`, performing an atomic `RuntimeBinding` swap when `preserveRuntimeState=true` and restarting the not-yet-exposed `entrypoint` when `preserveRuntimeState=false`. This does not remove the accepted crash/recovery window.
- **Definitive failure versus uncertain transport outcome**: The server's `_create_sandbox_sync` must catch `FastPathFailedPrecondition` separately from generic query-based recovery and map it directly to HTTP `409`. Only uncertain transport failures should trigger a lookup to recover a possibly successful create; a definitive init failure must not be converted into success or a misleading not-found result by that lookup.

## Design Details

### 1. Responsibility boundaries and prior art

| Building Block | Provided By | Usage / Delta in This OSEP |
| --- | --- | --- |
| Stateless HMAC `execd` tokens (`[execd].auth_signing_key_file`) | [PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080) | Reuse `ExecdTokenManager.mint_env_value` and `resolve_raw_token` in `FastSandboxService`; persist verifier metadata in `runtime-init` `ActionBinding.input`; attach `X-EXECD-ACCESS-TOKEN` on `fsb` `GetEndpoint(44772)`. |
| `POST /internal/init` hardening (`1 MiB` cap, `500` rollback, `EXECD_INIT_AUTH` strip, token rotation) | [PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103) | Reused **unchanged** in `components/execd`; `FastSandboxService` enforces the same reserved-env blacklist (`EXECD_INIT_AUTH`, `EXECD_RUNTIME_INIT`, etc.). |
| External Ingress block on `:44772/internal/init` (`403` pre-wake) | [PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111) | Reused **unchanged** in `components/ingress`. |
| `ResourceAware` pool mode & default limit lookup | [OSEP-0026](0026-resource-oversubscription-accounting-for-fast-sandbox.md) | Migrate `FastSandboxService` from `FastPath.GetPool` + `_pool_mode_cache` to `PoolCRReader` (`WorkloadInformer` watch on `SandboxPool`). |

### 2. Unified `SandboxPool` watch (`PoolCRReader`)

#### RBAC (`manifests/charts/server/templates/rbac.yaml`)

`opensandbox-server` already holds `get`, `list`, and `watch` on `sandboxes`, `sandboxtemplates`, and `sandboxsnapshots` in `apiGroups: ["sandbox.fast.io"]`. Add read-only watch access to `sandboxpools`:

```yaml
  - apiGroups: ["sandbox.fast.io"]
    resources: ["sandboxpools"]
    verbs: ["get", "list", "watch"]
```

#### `PoolCRReader` and `PoolSnapshot` (`services/fast_sandbox/cr_reader.py` & `service.py`)

Add `PoolCRReader` alongside `SandboxCRReader`, sharing the same `K8sClient` instance on `FastSandboxService` so informer lifecycle (`stop_informers` on shutdown) and Kubernetes API configuration are managed once:

```python
@dataclass(frozen=True)
class PoolSnapshot:
    name: str
    namespace: str
    is_resource_aware: bool
    resources: dict[str, str]          # {"cpu": ..., "memory": ..., "pids": ...}
    action_handlers: tuple[str, ...]   # ordered spec.actionHandlers[].name
```

- **Read path**: Calls `K8sClient.get_custom_object("sandbox.fast.io", "v1alpha2", namespace, "sandboxpools", pool_ref, use_cache=True)`, which lazily starts a per-namespace `WorkloadInformer` (`LIST/WATCH`) and serves synced reads from memory in `O(1)` (`0` network RTT).
- **Cache-miss & `404` semantics**: When `pool_ref` is absent from a synced informer (e.g., a newly created `SandboxPool` whose `ADDED` watch event has not arrived yet), `K8sClient.get_custom_object` falls through to a direct `get_namespaced_custom_object` call; a genuine `404` raises `HTTPException(404, code=FSB_POOL_NOT_FOUND)` identically to `_pool_info` today.
- **Exact `ResourceMode` parity with `fast-sandbox`**: `is_resource_aware` evaluates to `True` if and only if `spec.sandboxResources.mode == "ResourceAware"` and `cpu`, `memory`, and `overcommitRatio` are all present and non-empty (matching `SandboxResourceProfile.ResourceMode()` in `fast-sandbox/api/v1alpha2/resource_profile.go`); all other shapes fail safe to `False`.
- **Single-pass create mapping**: Normalizing `pool_ref` up front and reading `PoolSnapshot` once eliminates both `self._pool_mode_cache` (`_POOL_MODE_CACHE_TTL_SECONDS`) on template creates and the synchronous `self._fastpath.get_pool(...)` RPC on every image/snapshot create, collapsing `_create_sandbox_sync`'s two-pass `map_*_create_request` calls into a single pass.
- **Rolling-upgrade `403` fallback**: On `ApiException(status=403)` from `sandboxpools`, `PoolCRReader` logs a rate-limited warning and delegates to `self._fastpath.get_pool(namespace, pool_ref)` to construct `PoolSnapshot(..., action_handlers=())`.

### 3. Template build modes and `ActionBinding` mapping

#### Template build classification (`services/templates/template_service.py`)

When `runtime.execd_run_as_init` is `True` (default since OSEP-0018 R-f), `_build_crd` and `spec_json` will distinguish two template startup shapes without adding public API fields. The build must persist `entrypointPrewarmed` consistently with the actual `EXECD_RUNTIME_INIT` setting used in the built template: `false` for deferred initialization, `true` for prewarmed execution. Create-time classification must read that persisted build fact, not reconstruct it from later server configuration or readiness settings:

| Template Shape | Condition | Build-Time Env Injected | Persisted `spec_json` Flag | Create-Time `entrypointPolicy` |
| --- | --- | --- | --- | --- |
| **Standard (Deferred Entrypoint)** | `readiness.probe` is `None` | `EXECD_INIT=1`, `EXECD_RUNTIME_INIT=1` | `"entrypointPrewarmed": false` | `"restart"` (`execd` boots idle in the Golden snapshot; `Fastlet` `runtime-init` injects `envs` and launches `entrypoint` at create time) |
| **Prewarmed Workload** | Custom `readiness.probe` configured | `EXECD_INIT=1` | `"entrypointPrewarmed": true` | `"keep"` (the Golden builder waits for the user service's readiness probe before snapshotting; `runtime-init` binds identity/token/env for new sessions while keeping the prewarmed process warm in memory) |

Legacy template rows missing `"entrypointPrewarmed"` default to `True` (`entrypointPolicy = "keep"`), preserving backward compatibility for `/internal/init`-capable templates built prior to this OSEP (templates built before `execd` supported `POST /internal/init` must be rebuilt before targeting a `runtime-init`-enabled pool; see `Upgrade & Migration Strategy`). If `"entrypointPrewarmed"` is explicitly `false` (`EXECD_RUNTIME_INIT=1`) and the target `SandboxPool` does not advertise `"runtime-init"` in `action_handlers`, `FastSandboxService` will reject the create with HTTP `400 INVALID_PARAMETER` based on that persisted metadata and pool capability.

#### `ActionBinding` payload and source matrix (`services/fast_sandbox/create_mapping.py`)

1. **Reserved env validation**: Reject `None` values and case-insensitively reject the complete set of platform-owned `execd` configuration variables (`isolation.ExecdConfigEnvBlacklist()` plus init flags: `EXECD_ACCESS_TOKEN`, `EXECD_INIT`, `EXECD_RUNTIME_INIT`, `EXECD_INIT_AUTH`, `EXECD_ISOLATION_CONFIG`, `EXECD_LIFECYCLE_CONFIG`, `OPENSANDBOX_LIFECYCLE`, `EXECD_ENVS`, `JUPYTER_HOST`, `JUPYTER_TOKEN`) in `request.env` with HTTP `400 INVALID_PARAMETER`, matching `execd`'s `validateInitEnvs` and the Docker/Kubernetes services ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)) so invalid user env keys never reach `Fastlet` or trigger sandbox teardown after retry exhaustion.
2. **Handler ordering**: `create_mapping.py` emits `"egress"` followed by `"runtime-init"` whenever `"runtime-init" in pool_snapshot.action_handlers`:

```json
{
  "sandboxId": "fsb-8a91c2",
  "generation": 1,
  "accessTokenHash": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "accessTokenVerifier": "sha256:v1:9fK3mP2xQ8vL1nR4wT7yZ0:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "entrypointPolicy": "restart",
  "preserveRuntimeState": false,
  "envs": {
    "APP_ENV": "production"
  },
  "telemetry": {
    "attributes": {
      "namespace": "tenant-a",
      "pool": "firecracker-pool"
    }
  }
}
```

`execd`'s standard `json.Decoder` decodes `accessTokenHash` directly into `RuntimeInitRequest.AccessTokenHash` with zero changes and ignores the auxiliary `accessTokenVerifier` key used by `opensandbox-server` on `GetEndpoint(44772)`.

3. **Parameter derivation by create source**:

| Create Source | `create.envs` on `FastPath` | `preserveRuntimeState` | `entrypointPolicy` | Behavior |
| --- | --- | --- | --- | --- |
| **Golden Template (Standard)** | Empty (guest restored from `vmstate`) | `false` | `"restart"` | Injects `request.env`, binds token/identity, launches `entrypoint` |
| **Golden Template (Prewarmed / Legacy)** | Empty | `false` | `"keep"` | Preserves prewarmed process; binds token/identity and `envs` for `/command`, `/code`, `/pty` |
| **OCI Image (`container`/`gvisor`/`kata`)** | Populated from `request.env` | `false` | `"keep"` | Container runtime sets initial env; `runtime-init` atomically binds token/identity/telemetry |
| **`Full` Snapshot Restore** | Populated from `request.env` | `true` | `"keep"` | Preserves live memory processes and sessions; rotates `sandboxId`, `accessTokenHash`, `envs`, and telemetry to the new clone |
| **`RootfsOnly` Snapshot Restore** | Populated from `request.env` | `false` | `"restart"` | Fresh cold boot from snapshot rootfs; applies clean initialization and launches `entrypoint` |

### 4. Stateless `execd` token derivation on Fast Sandbox

When `[execd].auth_signing_key_file` ([PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080)) is configured on `opensandbox-server`:

- **At `create_sandbox`**:
  - If `"runtime-init" in pool_snapshot.action_handlers`, `FastSandboxService` calls `self._execd_token_manager.mint_env_value()` to obtain `verifier = "sha256:<kid>:<nonce>:<verifier_hex>"`, setting `"accessTokenHash": f"sha256:{verifier.rsplit(':', 1)[-1]}"` (for `execd`) and `"accessTokenVerifier": verifier` (for stateless endpoint derivation) inside `ActionBinding.input`.
  - Because `Sandbox.spec.actionBindings` is persisted atomically by `FastPath.CreateSandbox`, no post-create `PATCH` is needed and idempotent `CreateSandbox` retries carry the exact same payload.
- **At `get_sandbox_endpoint(sandbox_id, port=44772)` (and server proxy guard)**:
  - `FastSandboxService` reads the informer-cached `Sandbox` CR via `SandboxCRReader.get(namespace, sandbox_id)`, extracts `accessTokenVerifier` from `spec.actionBindings[handler=="runtime-init"].input`, derives `raw_token = self._execd_token_manager.resolve_raw_token(verifier)`, and merges `build_execd_auth_headers(raw_token)` into `Endpoint.headers`.
  - Matching [PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080), `use_server_proxy=true` for port `44772` returns `400 INVALID_PARAMETER` when an `execd` token verifier is bound on the sandbox.

### 5. Impact on `fast-sandbox` (`ActionHandler.exec`)

The upstream PR #269 baseline adds a generic `SandboxPool` exec handler and an optional Protobuf sensitive-input channel, without `execd`-specific logic. This OSEP's default path will reuse existing binding fields; it does not imply that upstream made zero Protobuf changes.

- **`SandboxPool` `ActionHandler` schema (`api/v1alpha2/sandboxpool_types.go`)**:
  - `ActionHandler` selects either existing `targetHTTPPort` (Pod-loopback HTTP sidecar, e.g., `egress`) or `exec`. `ExecActionHandler` exposes only:
    - `command: []string` (required `argv` templates executed directly, no implicit shell).
    - `timeoutSeconds` (default `15`).
    - `maxRetries` (number of retries after the initial attempt).
  - Binding input is always delivered on stdin. `Restored || Recovered` always skips execution and marks the binding `ActionReady`. All command failures receive bounded retries; exhaustion fails the sandbox, deletes the newly created CR, and returns synchronous `FailedPrecondition` without retrying another node. These are fixed behaviors, not configurable policies.
  - Controlled `text/template` (`missingkey=error`) exposes platform-trusted sandbox, revision, and attachment metadata, including `{{.Attachment.Network.IP}}`, but never interpolates `binding.input` into command arguments. No vsock field is currently provided; supporting it requires runtime-driver and template-context extensions. `exec` `ActionBindings` are create-only and immutable after sandbox creation.
  - `ActionManager` keeps execution state in memory (`SandboxManager.Recover` stamps `Recovered = true` on surviving sandboxes); no per-sandbox completion file closes the accepted recovery crash window.
- **Optional sensitive-input channel**: Upstream adds `ActionBinding.sensitive_input = 3` in Protobuf for `{{.Sensitive.File}}`. This OSEP defaults to `EXECD_INIT_AUTH=false` and passes only token hash/verifier metadata in ordinary `ActionBinding.input`, not a raw token or init-auth secret, so it does not use this channel. Adopting the channel would require generated client stubs. Its temporary-file implementation prefers `/dev/shm` but can fall back to `os.CreateTemp` in the default temporary directory; it cannot guarantee that secret material never reaches disk.

#### Reference `SandboxPool` configuration (HTTP today)

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: firecracker-opensandbox-pool
spec:
  actionHandlers:
    - name: egress
      targetHTTPPort: 18080
      hooks: [sandbox.runtime-ready, sandbox.data-plane-ready]
    - name: runtime-init
      hooks: [sandbox.data-plane-ready]
      exec:
        timeoutSeconds: 15
        maxRetries: 10
        command:
          - /usr/bin/curl
          - -sS
          - --fail-with-body
          - --max-time
          - "10"
          - -X
          - POST
          - -H
          - "Content-Type: application/json"
          - --data-binary
          - "@-"
          - "http://{{.Attachment.Network.IP}}:44772/internal/init"
```

Direct command execution over `virtio-vsock` is a future transport option, not a capability of today's schema or template context. It first requires a suitable guest agent, runtime-driver support, and explicit template-context extension for the host UDS path; only after those prerequisites could operators substitute a vsock helper in `exec.command` while retaining the binding payload.

## Test Plan

- **Unit — `PoolCRReader` & `FastSandboxService`**:
  - `WorkloadInformer` real-time updates when `spec.actionHandlers` adds/removes `"runtime-init"` and when a same-name `SandboxPool` is recreated with a flipped `LegacySlot` / `ResourceAware` mode.
  - Zero `FastPath.GetPool` gRPC calls on steady-state Template, Image, and Snapshot creates; exact parity with `OSEP-0026` resource validation; `404 FSB_POOL_NOT_FOUND` on missing pools; `403` RBAC fallback to `FastPath.GetPool`.
  - `_create_sandbox_sync` maps `FastPathFailedPrecondition` directly to HTTP `409` without query recovery; only uncertain transport failures enter query recovery, including a lost response after a successful create.
- **API / SDK contract and template metadata**:
  - `api/schema.py` accepts `templateId` with `env`; `FastSandboxService` accepts it on capable pools and returns `400` on pools lacking `runtime-init`. Lifecycle spec and affected SDK validation/descriptions preserve this conditional behavior end to end.
  - Successful POST creates return HTTP `202` with body status `Running` or `Pending`, never HTTP `201`.
  - Persisted `entrypointPrewarmed` matches the actual build-time `EXECD_RUNTIME_INIT`; later configuration changes do not reclassify an existing template. Legacy rows without the flag default to `true`.
- **Unit — `create_mapping.py` & stateless `execd` auth**:
  - `ActionBinding` ordering (`[egress, runtime-init]`), source-mode matrix (`Standard Template`, `Prewarmed Template`, `Legacy Template`, `OCI Image`, `Full Snapshot`, `RootfsOnly Snapshot`), complete reserved env blacklist rejection (`400`, including `JUPYTER_HOST` and `JUPYTER_TOKEN`), payload size cap rejection (`400`), template `request.env` rejection (`400`) on pools lacking `"runtime-init"`, and deferred-entrypoint template (`entrypointPrewarmed=false`) rejection (`400`) on pools lacking `"runtime-init"`.
  - `[execd].auth_signing_key_file` integration: `accessTokenHash` + `accessTokenVerifier` embedded in `ActionBinding.input`, `GetEndpoint(44772)` returning valid `X-EXECD-ACCESS-TOKEN` via `ExecdTokenManager.resolve_raw_token`, and `use_server_proxy=true` rejected with `400`.
- **Integration / KVM E2E (`scripts/fast-sandbox-env`)**:
  - Standard template (`EXECD_RUNTIME_INIT=1`): `request.env` visible inside both the launched `entrypoint` and `/command`, OTel/identity reflects new `fsb-*` ID, `X-EXECD-ACCESS-TOKEN` enforced on `/command`.
  - Prewarmed template (`readiness.probe`): prewarmed PID survives across create (`entrypointPolicy: "keep"`), `/command` sees runtime `env` and bound token.
  - Cold-boot `202 Pending`, `Full` snapshot clone (distinct `sandboxId`, rotated `X-EXECD-ACCESS-TOKEN`, preserved memory state), and `Pause -> Resume` (fixed `Restored || Recovered` skip preserves live state without re-running `runtime-init`).
  - All command failures, including curl `22` on `4xx/5xx`, retry within `maxRetries` after the initial attempt; exhaustion fails the sandbox, deletes the newly created CR, and returns synchronous `FailedPrecondition` without node failover. Use the `-sS --fail-with-body` example without conflicting curl flags.
  - Fastlet recovery skips exec unconditionally, including the accepted crash window between VM boot and completed initialization; do not assert fail-closed protection for that window.
  - Egress action readiness establishes policy and subject only; vault credentials become available through the separate post-create API, not as an entrypoint-start guarantee.
  - Security assertions: `/internal/init` blocked via Ingress (`403`, PR #3111); shell metacharacters in `request.env` (`$(touch /tmp/pwned)`, quotes, newlines) passed safely over `stdin` without execution.

## Drawbacks

- `opensandbox-server`'s `ClusterRole` gains read-only `get/list/watch` access to `sandboxpools.sandbox.fast.io` (consistent with its existing watch permissions on `sandboxes`, `sandboxtemplates`, and `sandboxsnapshots`).
- Forking a short-lived `curl` (or `vsock` helper) process in `Fastlet` per sandbox boot adds ~1–3 ms of local `fork/exec` overhead on the host — negligible relative to network setup and far smaller than a cross-node control-plane HTTP round-trip, while keeping `fast-sandbox` completely protocol-agnostic.

## Alternatives

- **Call `POST /internal/init` remotely from `opensandbox-server`** — rejected: opens a pre-init data-plane exposure window, conflicts with [PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111), and misses `202 Pending` cold boots.
- **Bake a dedicated HTTP `ActionHandler` sidecar Pod container into every `SandboxPool`** — rejected: forces operators to deploy, monitor, and version an extra sidecar container in every Fastlet Pod just to relay one local HTTP POST, whereas `ActionHandler.exec` runs natively from `Fastlet` with `curl` today and `vsock` tomorrow.
- **Add `execd`-specific HTTP fields to `Sandbox` CRD or `FastPath` Protobuf** — rejected: couples `fast-sandbox` to `execd`'s wire format and prevents evolving from HTTP to `vsock` without CRD/Protobuf churn.
- **Expose `action_handlers` on `FastPath.GetPool` (`PoolInfo`) instead of watching `SandboxPool` CRs** — rejected: `spec.actionHandlers` is mutable on live pools (unlike `sandboxResources`), so a gRPC cache either suffers stale TTL windows or pays per-create RPC latency, and requires cross-repo Protobuf + Python stub synchronization.

## Infrastructure Needed

- Implementation PR in `opensandbox-group/fast-sandbox` adding `ActionHandler.exec` (plus `/usr/bin/curl` present in the `fastlet` runtime image).
- Vendored `fast-sandbox-crds.yaml` sync in `manifests/charts/base/files/fast-sandbox-crds.yaml` once `ActionHandler.exec` lands upstream.

## Upgrade & Migration Strategy

1. **Phase 1 — Prerequisites**: Merge [PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103) and [PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111) (no further `execd` or `ingress` changes needed), and ensure the configured template `execd` image (`spec.execd`) includes `POST /internal/init`.
2. **Phase 2 — `fast-sandbox` `ActionHandler.exec`**: Merge `ActionHandler.exec` in `fast-sandbox` and sync `manifests/charts/base/files/fast-sandbox-crds.yaml`. Existing `SandboxPool` resources omit `runtime-init` and behave identically.
3. **Phase 3 — Per-pool `runtime-init` handler enablement (before deferred-template builds)**:
   - Rebuild any legacy Golden Templates on target pools whose frozen `execd` binary predates `POST /internal/init` (or create a new `SandboxPool` for `/internal/init`-capable templates).
   - Add the `runtime-init` `ActionHandler.exec` entry to `SandboxPool.spec.actionHandlers` pool by pool. The pool controller rolls `Fastlet` Pods to the new `FastletRevision`; existing `opensandbox-server` creates that do not attach a `runtime-init` `ActionBinding` continue running unaffected because `Fastlet` only invokes handlers referenced in `Sandbox.spec.actionBindings`.
4. **Phase 4 — Server, Helm RBAC, and API/SDK alignment**: Apply the `ClusterRole` rule (`sandboxpools`: `get`, `list`, `watch`) and deploy server support for conditional `runtime-init` bindings, stateless auth, direct `FastPathFailedPrecondition` → HTTP `409` mapping, and deferred-template capability checks alongside `PoolCRReader`. Allow `templateId` + `env` in `api/schema.py`, gate support in `FastSandboxService`, and align `specs/sandbox-lifecycle.yml` and SDK consumer validation/descriptions. Persist template classification from the actual build-time `EXECD_RUNTIME_INIT` setting before enabling deferred-template builds. Target pools already advertise `runtime-init`, so newly built standard templates (`EXECD_RUNTIME_INIT=1`, `"entrypointPrewarmed": false`) receive bindings at create time, while legacy records default to `entrypointPrewarmed=true` and `entrypointPolicy = "keep"`. Deferred templates on an unmigrated pool fail with HTTP `400`. Pool discovery/RBAC and template-env capability validation are implemented; binding delivery and the other items remain rollout design, not completed work. Do not publish validation preparation as usable runtime-env support.
5. **Rollback**: Roll back `opensandbox-server` (and rebuild any deferred templates created with `EXECD_RUNTIME_INIT=1` if retiring `runtime-init`), then switch traffic to a `SandboxPool` without `runtime-init` or neutralize an existing pool's handler for `entrypointPrewarmed: true` / image workloads by setting `exec.command` to `["/bin/true"]` (since `ValidateSandboxPoolUpdate` forbids deleting an already-declared `ActionHandler` name from an active pool).
