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

This OSEP wires per-sandbox runtime initialization on Fast Sandbox **entirely at the near end (`Fastlet`) with zero `opensandbox-server` → `execd` network calls**, and **zero changes to `components/execd`, `components/ingress`, `FastletProxy`, `Sandbox` CRD, or `FastPath` Protobuf**:

1. **Per-sandbox payload via `ActionBinding`**: `opensandbox-server` packs the standard `RuntimeInitRequest` JSON (`sandboxId`, `generation`, 2-part `accessTokenHash`, `entrypointPolicy`, `preserveRuntimeState`, `envs`, `telemetry`) into `CreateSandboxRequest.action_bindings` (`handler: "runtime-init"`, ordered after `"egress"`).
2. **Pool-owned command-line template (`ActionHandler.exec`)**: A small additive change in `fast-sandbox` lets a `SandboxPool` declare a host-local `execve` `argv` template (`stdin: BindingInput`, `skipOnRestore: true`) — running plain `curl` against `http://{{.Attachment.Network.IP}}:44772/internal/init` today, and cleanly evolving to direct in-VM command execution over `virtio-vsock` once runtime drivers expose the host vsock UDS path, without any `OpenSandbox` or CRD change. Because `FastPath.ResolveEndpoint` already gates route-credential issuance on aggregate `Ready` (all `ActionBindings` `ActionReady`), external traffic is fail-closed until initialization succeeds across synchronous creates, `202 Pending` async cold boots, and `Full` / `RootfsOnly` snapshot restores.
3. **Unified `SandboxPool` Informer watch (`PoolCRReader`)**: `opensandbox-server` watches `sandboxpools.sandbox.fast.io/v1alpha2` via the shared `K8sClient` `WorkloadInformer`, unifying real-time `actionHandlers` capability discovery with [OSEP-0026](0026-resource-oversubscription-accounting-for-fast-sandbox.md) `ResourceAware` mode and default pool resource lookups — eliminating both the 15-minute `_pool_mode_cache` and per-create `FastPath.GetPool` gRPC round-trips.

## Motivation

### Why Fast Sandbox needs runtime initialization

On `fsb`, `SandboxTemplate` Golden Images capture a running Firecracker VM after `execd` has already booted as PID 1 ([OSEP-0018](0018-execd-as-sandbox-init.md)), and `Full` snapshots capture live workload memory. At create or clone time:

- **Template creates cannot inject per-sandbox `env` or `OPENSANDBOX_ID` via OCI config**: the guest process tree is restored from `vmstate`, so `create_mapping.py` currently rejects `request.env` on template creates and leaves `execd` reporting the build-time placeholder identity in OTel metrics and eBPF audit records.
- **Stateless `execd` authentication ([PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080)) does not yet cover `fsb`**: Kubernetes `BatchSandbox` / `AgentSandbox` workloads receive `EXECD_ACCESS_TOKEN="sha256:<kid>:<nonce>:<verifier>"` through Pod env, which does not apply to restored `fsb` VMs or cloned `Full` snapshots (where a clone would otherwise inherit the source sandbox's in-memory token).

### Why `opensandbox-server` must not call `execd` remotely

Having `opensandbox-server` call `POST /internal/init` over the data plane after `CreateSandbox` returns breaks down across three structural boundaries:

1. **Pre-init exposure window (breaks fail-closed)**: Reaching `execd` from the server through the data plane requires the sandbox route to become externally routable *before* `accessTokenHash` and runtime `env` are installed, directly conflicting with [PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111) (which blocks `/internal/init` at Ingress).
2. **Async cold boot (`202 Pending`)**: When a cold Fastlet parks a sandbox for background image/snapshot pull (`parkForImageDelivery`), `CreateSandbox` returns `Pending` immediately; `opensandbox-server` is no longer on the call stack when the VM eventually boots.
3. **Cross-hop latency and half-initialized orphans**: An extra control-plane → proxy → node → guest RTT adds tail latency to sub-100ms Firecracker restores and leaves `Running` but uninitialized sandboxes behind on transient network blips.

### Goals

- **Zero control-plane callback**: Execute `runtime-init` locally from `Fastlet` before a sandbox becomes `Ready` or routable via `FastPath.ResolveEndpoint`.
- **Reuse `execd` and `ingress` unchanged**: Consume existing `POST /internal/init` ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)) and Ingress `:44772/internal/init` blocking ([PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111)) with zero `execd` or `ingress` code changes.
- **Protocol- and transport-decoupled `fast-sandbox` surface**: Carry per-sandbox init data in existing `ActionBinding.input`; keep `fast-sandbox` agnostic to `execd` HTTP paths and JSON schemas via a minimal command-line `ActionHandler.exec` template on `SandboxPool` (supporting HTTP via `curl` today and `vsock` command execution later).
- **Watch-driven pool discovery + `OSEP-0026` unification**: Replace `FastPath.GetPool` polling/TTL caching in `opensandbox-server` with a shared `WorkloadInformer` watch on `SandboxPool` CRs covering `actionHandlers`, `ResourceAware` mode, and pool default resources.
- **Extend stateless HMAC `execd` authentication ([PR #3080](https://github.com/opensandbox-group/OpenSandbox/pull/3080)) to `fsb`**: Bind one-way `sha256:<hex>` digests at create/clone time via `ExecdTokenManager` and return derived `X-EXECD-ACCESS-TOKEN` headers on `GetEndpoint(44772)` with zero plaintext tokens in etcd, CRDs, or node disk.

### Non-Goals

- **Public `entrypointPolicy` override field on `CreateSandboxRequest`**: Entrypoint policy (`"restart"` vs `"keep"`) is derived deterministically by the server from the template/snapshot source kind in Phase 1.
- **Sandbox lifecycle hooks ([OSEP-0020](0020-sandbox-lifecycle-hooks.md)) on `fsb`**: `preStart`, `periodic`, `prePause`, `postResume`, and `preTerminate` evolve independently and remain rejected on `fsb` in Phase 1.
- **Credential Vault transport changes**: Secret material continues to use the [OSEP-0022](0022-multi-sandbox-egress-control-plane.md) handler-relay channel and never enters `ActionBinding`.
- **Any `execd`, `ingress`, `FastletProxy`, `Sandbox` CRD, or `FastPath` Protobuf modification**: All changes in `OpenSandbox` stay inside `opensandbox-server` and its Helm `ClusterRole`.

## Requirements

- **Fail-closed readiness**: `FastPath.ResolveEndpoint` must reject route resolution (`FailedPrecondition`) until `runtime-init` succeeds; deterministic init rejection (`4xx/5xx` from `execd`) must fail the sandbox immediately rather than retrying forever.
- **No command injection**: User-supplied `request.env` keys/values may contain arbitrary shell metacharacters, newlines, or quotes; `Fastlet` must pass `ActionBinding.input` strictly over `stdin` to direct `execve` `argv` arrays, never interpolating `binding.input` into command arguments or invoking a shell.
- **No plaintext token persistence**: Only irreversible `sha256` verifiers may appear in `Sandbox.spec.actionBindings`.
- **Preserve checkpoint state on `Pause -> Resume`**: Resuming a paused sandbox restores its already-initialized `execd` memory (`RuntimeBinding`, `accessTokenHash`, and running user processes); `Fastlet` must not re-execute one-shot `runtime-init` on checkpoint restore (`skipOnRestore: true`).
- **Zero-downtime pool-scoped rollout**: Pools without a `"runtime-init"` `ActionHandler` continue serving existing workloads unchanged; pools opting into `"runtime-init"` require Golden Templates built with an `/internal/init`-capable `execd` image, and deferred-entrypoint templates (`EXECD_RUNTIME_INIT=1`) fail fast if targeted at a pool lacking `"runtime-init"`.

## Proposal

### Flow diagrams

Ordering `actionBindings` as `[egress, runtime-init]` on `sandbox.data-plane-ready` guarantees that **egress policy and credential vault activate before `runtime-init` launches the user `entrypoint`**, while **`FastPath.ResolveEndpoint` stays gated (`Ready=false`) until `runtime-init` exits `0`**:

```mermaid
sequenceDiagram
    participant C as SDK / Caller
    participant S as OpenSandbox Server
    participant W as PoolCRReader (K8s Watch)
    participant FP as FastPath / Controller
    participant FL as Fastlet (ActionManager)
    participant EG as Egress Handler (Loopback)
    participant CMD as Exec Runner (curl / vsock)
    participant EX as Guest execd (:44772)

    C->>S: POST /v1/sandboxes (templateId / image / snapshotId, env)
    S->>W: get_snapshot(namespace, poolRef) [O(1) Informer memory]
    W-->>S: PoolSnapshot(is_resource_aware, resources, action_handlers)
    S->>S: ExecdTokenManager.mint_env_value() -> sha256:<kid>:<nonce>:<hex>
    S->>FP: CreateSandbox(action_bindings=[egress, runtime-init], completion=READY)
    FP->>FL: Persist Sandbox CR & dispatch CreateSandbox
    FL->>FL: EnsureSandbox (slot IP / vsock attached; Ready=false)
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
    S-->>C: 201 Created (or 202 Pending on cold image pull, converging identically in background)
```

### Contract summary

| Concern | Authoritative Owner | Mechanism |
| --- | --- | --- |
| Pool capability & resource mode discovery | `OpenSandbox Server` (`PoolCRReader`) | Shared `WorkloadInformer` (`LIST/WATCH` on `sandboxpools.sandbox.fast.io/v1alpha2`) |
| Per-sandbox init intent | `OpenSandbox Server` (`create_mapping.py`) | `CreateSandboxRequest.action_bindings` (`handler: "runtime-init"`, JSON `input`) |
| Stateless `execd` token verifier | `OpenSandbox Server` (`ExecdTokenManager`, PR #3080) | `"accessTokenHash": "sha256:<hex>"` + `"accessTokenVerifier": "sha256:<kid>:<nonce>:<hex>"` in `ActionBinding.input`; `GetEndpoint(44772)` calls `resolve_raw_token` from cached `Sandbox` CR |
| Near-end transport & execution | `SandboxPool` & `Fastlet` (`fast-sandbox`) | `SandboxPool.spec.actionHandlers[].exec` (`argv` template + `stdin: BindingInput` + `skipOnRestore: true` + `nonRetryableExitCodes` + `failurePolicy: FailSandbox`) |
| `/internal/init` endpoint & Ingress protection | `execd` ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)) & `ingress` ([PR #3111](https://github.com/opensandbox-group/OpenSandbox/pull/3111)) | Reused unchanged (`403` at Ingress on port `44772`; 1 MiB body limit, `500` binding rollback, and optional `--init-auth` in `execd`) |

### Notes/Constraints/Caveats

- **Pool-capability gates for template creates (`request.env` and deferred entrypoints)**:
  - On a `SandboxPool` that declares `"runtime-init"`, template-mode `POST /sandboxes` accepts `request.env`; if a caller supplies `request.env` on a template create targeting a pool *without* `"runtime-init"`, the server returns `400 INVALID_PARAMETER` with clear pool-capability guidance instead of silently dropping the variables.
  - Conversely, if a template was built with `"entrypointPrewarmed": false` (`EXECD_RUNTIME_INIT=1`, so `execd` waits idle for `/internal/init` before starting `entrypoint`) and targets a pool *without* `"runtime-init"`, `create_mapping.py` immediately rejects the request with `400 INVALID_PARAMETER` rather than omitting the binding and stranding the sandbox with an unstarted `entrypoint`.
- **`ActionBinding.input` size bound & create-only immutability**: `fast-sandbox` caps each `ActionBinding.input` (64 KiB), enforces create-only immutability on `exec` `ActionBindings` (rejecting post-create `UpdateSandbox` additions, removals, or modifications), and `execd` caps `/internal/init` at 1 MiB ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)); `create_mapping.py` validates the serialized `runtime-init` binding size up front and returns `400 INVALID_PARAMETER` before calling `FastPath`.
- **`Pause -> Resume`, `Fastlet` restart recovery, and `Full` snapshot clone**:
  - **`Pause -> Resume` (`Restored == true`) & in-place `Fastlet` restart (`Recovered == true`)**: Both restore or adopt an already-running sandbox whose `RuntimeBinding` is already active in `execd` memory. `SandboxManager.Recover` stamps an in-memory `Recovered = true` flag on recovered sandboxes (zero disk files or extra volume mounts), and `skipOnRestore: true` on `ActionHandler.exec` skips re-execution whenever `Restored || Recovered`, marking the binding `ActionReady` immediately. Known edge window: if `Fastlet` crashes during the brief millisecond window after `EnsureSandbox` boots a VM but before `runtime-init` finishes, `Recovered` will skip `runtime-init` on restart; this narrow crash window is accepted rather than maintaining per-sandbox disk state.
  - **`Full` snapshot clone (`CreateSandbox` from `snapshotId`)**: Boots as a new sandbox (`Restored == false && Recovered == false`) with `preserveRuntimeState: true` and a freshly minted `accessTokenHash`, so `Fastlet` runs `runtime-init` once to rotate identity, token hash, `envs`, and telemetry while keeping live snapshot processes running (with `EXECD_INIT_AUTH` left at its default `false`; processes inside the guest already execute within the same single-sandbox trust boundary, while external access to `:44772/internal/init` is blocked at Ingress).

### Risks and Mitigations

- **Server rollout ahead of Helm RBAC update (`403` on `sandboxpools`)**: If the `opensandbox-server` image upgrades before the Helm chart adds `sandboxpools` `get/list/watch` to its `ClusterRole`, `PoolCRReader` catches `ApiException(status=403)`, logs a structured warning, and falls back to `FastPath.GetPool` for `is_resource_aware` and `resources` (treating `action_handlers` as empty), preserving existing traffic while failing closed (`400`) on deferred-entrypoint templates that strictly require `runtime-init`.
- **Pre-existing Golden Templates (pre-`/internal/init` `execd` vs pre-OSEP-0035 `/internal/init`-capable `execd`)**:
  - Golden VM snapshots restore the exact `execd` binary frozen in guest memory at template-build time (`spec.execd`). Templates built **before `execd` added `POST /internal/init`** do not expose the endpoint at all and would fail with HTTP `404` (`FailSandbox`) if scheduled onto a pool that emits `runtime-init`; operators must either rebuild those legacy templates with a current `execd` image before adding `runtime-init` to an existing pool, or roll out `runtime-init` on a new `SandboxPool` and migrate traffic once templates are rebuilt.
  - Templates already built with an `/internal/init`-capable `execd` image (`EXECD_INIT=1`) but predating this OSEP simply lack the `"entrypointPrewarmed"` flag in `spec_json`; the server defaults missing metadata to `entrypointPolicy = "keep"`, applying `RuntimeBinding` (`sandboxId`, `accessTokenHash`, `envs` for new `/command` executions) without restarting the already-running entrypoint.
- **Transient guest readiness delay, lost-response retry, and deterministic init error**: `curl --fail-with-body` returns exit code `22` on HTTP `4xx/5xx` (mapped via `nonRetryableExitCodes: [22]` + `failurePolicy: FailSandbox` to immediate sandbox teardown, `Sandbox` CR cleanup, and direct `FailedPrecondition` failure back to the caller without penalizing the `Fastlet` node or retrying other candidates), whereas TCP connect refusal (`7`) or transfer timeout (`28`) retries locally with backoff inside `Fastlet`. If `execd` applied `/internal/init` just before a response timeout (`28`), the retry executes while `FastPath.ResolveEndpoint` is still fail-closed (`Ready=false`, zero external traffic or user sessions): `RuntimeInitManager.Apply` serializes under `m.mu`, performing a pure atomic `RuntimeBinding` swap when `preserveRuntimeState=true` and cleanly restarting the not-yet-exposed `entrypoint` when `preserveRuntimeState=false`.

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

When `runtime.execd_run_as_init` is `True` (default since OSEP-0018 R-f), `_build_crd` and `spec_json` distinguish two template startup shapes without adding public API fields:

| Template Shape | Condition | Build-Time Env Injected | Persisted `spec_json` Flag | Create-Time `entrypointPolicy` |
| --- | --- | --- | --- | --- |
| **Standard (Deferred Entrypoint)** | `readiness.probe` is `None` | `EXECD_INIT=1`, `EXECD_RUNTIME_INIT=1` | `"entrypointPrewarmed": false` | `"restart"` (`execd` boots idle in the Golden snapshot; `Fastlet` `runtime-init` injects `envs` and launches `entrypoint` at create time) |
| **Prewarmed Workload** | Custom `readiness.probe` configured | `EXECD_INIT=1` | `"entrypointPrewarmed": true` | `"keep"` (the Golden builder waits for the user service's readiness probe before snapshotting; `runtime-init` binds identity/token/env for new sessions while keeping the prewarmed process warm in memory) |

Legacy template rows missing `"entrypointPrewarmed"` default to `True` (`entrypointPolicy = "keep"`), preserving backward compatibility for `/internal/init`-capable templates built prior to this OSEP (templates built before `execd` supported `POST /internal/init` must be rebuilt before targeting a `runtime-init`-enabled pool; see `Upgrade & Migration Strategy`). If `"entrypointPrewarmed"` is explicitly `false` (`EXECD_RUNTIME_INIT=1`) and the target `SandboxPool` does not advertise `"runtime-init"` in `action_handlers`, `create_mapping.py` rejects the create with HTTP `400 INVALID_PARAMETER`.

#### `ActionBinding` payload and source matrix (`services/fast_sandbox/create_mapping.py`)

1. **Reserved env validation**: Reject `None` values and case-insensitively reject the complete set of platform-owned `execd` configuration variables (`isolation.ExecdConfigEnvBlacklist()` plus init flags: `EXECD_ACCESS_TOKEN`, `EXECD_INIT`, `EXECD_RUNTIME_INIT`, `EXECD_INIT_AUTH`, `EXECD_ISOLATION_CONFIG`, `EXECD_LIFECYCLE_CONFIG`, `OPENSANDBOX_LIFECYCLE`, `EXECD_ENVS`, `JUPYTER_HOST`, `JUPYTER_TOKEN`) in `request.env` with HTTP `400 INVALID_PARAMETER`, matching `execd`'s `validateInitEnvs` and the Docker/Kubernetes services ([PR #3103](https://github.com/opensandbox-group/OpenSandbox/pull/3103)) so invalid user env keys never reach `Fastlet` or trigger `FailSandbox` teardown.
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

The `fast-sandbox` side requires only a small, localized extension to `SandboxPool` `ActionHandlers` (`api/v1alpha2/sandboxpool_types.go` and `internal/fastlet/action`), with zero `execd`-specific logic, zero `FastletProxy` changes, and zero `FastPath` Protobuf changes:

- **`SandboxPool` `ActionHandler` schema (`api/v1alpha2/sandboxpool_types.go`)**:
  - `ActionHandler` becomes a strict `oneOf` between existing `targetHTTPPort` (Pod-loopback HTTP sidecar, e.g., `egress`) and `exec`:
    - `command: []string` (required, 1..64 `argv` templates executed directly via `execve`, no implicit shell), `cleanupCommand: []string` (optional for `REMOVE_BINDING`; omitted = no-op delete), `env: []ExecActionEnvVar`.
    - `stdin: None | BindingInput | Envelope` (`runtime-init` uses `BindingInput`).
    - `skipOnRestore: bool` (when `true`, both checkpoint restores on `Pause -> Resume` and in-place `Fastlet` daemon restarts mark the binding `ActionReady` in memory without re-running `command`, via `Restored || Recovered`).
    - `timeoutSeconds` (default `15`), `nonRetryableExitCodes: []int32`, `maxRetries: int32`, `failurePolicy: Retry | FailSandbox` (`FailSandbox` tears down the sandbox, deletes a newly created `Sandbox` CR, and fails synchronous `CreateSandbox` directly with `FailedPrecondition` without penalizing the healthy `Fastlet` or retrying other candidates).
  - Controlled `text/template` (`missingkey=error`) exposes only platform-trusted metadata (`{{.Sandbox.UID}}`, `{{.Sandbox.Name}}`, `{{.Sandbox.Namespace}}`, `{{.Revision.InstanceGeneration}}`, `{{.Revision.SpecGeneration}}`, `{{.Revision.RuntimeInstanceID}}`, `{{.Revision.AttachmentID}}`, `{{.Revision.RouteGeneration}}`, `{{.Attachment.Restored}}`, `{{.Attachment.Recovered}}`, and `{{.Attachment.Network.{IP,Gateway,NetNSPath,PrivateCIDR,HostVeth}}}`, with `Attachment.VSock` reserved until runtime drivers wire host UDS paths) — **never** `binding.input`. `exec` `ActionBindings` are create-only and immutable after sandbox creation.
  - `ActionManager` keeps `exec` state purely in memory (`SandboxManager.Recover` stamps `Recovered = true` on surviving sandboxes, requiring zero local state files or extra volume mounts; see `Notes/Constraints/Caveats`).

#### Reference `SandboxPool` configuration (HTTP today, `vsock` ready)

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
        stdin: BindingInput
        skipOnRestore: true
        timeoutSeconds: 15
        nonRetryableExitCodes: [22]   # curl --fail-with-body exit code on HTTP 4xx/5xx
        maxRetries: 10
        failurePolicy: FailSandbox
        command:
          - /usr/bin/curl
          - -fsS
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

When a future guest agent and runtime driver expose direct command execution over `virtio-vsock`, operators only swap `exec.command` in `SandboxPool` (once the host vsock UDS path template field is wired), with zero changes to `OpenSandbox` or `Sandbox` CRs.

## Test Plan

- **Unit — `PoolCRReader` & `FastSandboxService`**:
  - `WorkloadInformer` real-time updates when `spec.actionHandlers` adds/removes `"runtime-init"` and when a same-name `SandboxPool` is recreated with a flipped `LegacySlot` / `ResourceAware` mode.
  - Zero `FastPath.GetPool` gRPC calls on steady-state Template, Image, and Snapshot creates; exact parity with `OSEP-0026` resource validation; `404 FSB_POOL_NOT_FOUND` on missing pools; `403` RBAC fallback to `FastPath.GetPool`.
- **Unit — `create_mapping.py` & stateless `execd` auth**:
  - `ActionBinding` ordering (`[egress, runtime-init]`), source-mode matrix (`Standard Template`, `Prewarmed Template`, `Legacy Template`, `OCI Image`, `Full Snapshot`, `RootfsOnly Snapshot`), complete reserved env blacklist rejection (`400`, including `JUPYTER_HOST` and `JUPYTER_TOKEN`), payload size cap rejection (`400`), template `request.env` rejection (`400`) on pools lacking `"runtime-init"`, and deferred-entrypoint template (`entrypointPrewarmed=false`) rejection (`400`) on pools lacking `"runtime-init"`.
  - `[execd].auth_signing_key_file` integration: `accessTokenHash` + `accessTokenVerifier` embedded in `ActionBinding.input`, `GetEndpoint(44772)` returning valid `X-EXECD-ACCESS-TOKEN` via `ExecdTokenManager.resolve_raw_token`, and `use_server_proxy=true` rejected with `400`.
- **Integration / KVM E2E (`scripts/fast-sandbox-env`)**:
  - Standard template (`EXECD_RUNTIME_INIT=1`): `request.env` visible inside both the launched `entrypoint` and `/command`, OTel/identity reflects new `fsb-*` ID, `X-EXECD-ACCESS-TOKEN` enforced on `/command`.
  - Prewarmed template (`readiness.probe`): prewarmed PID survives across create (`entrypointPolicy: "keep"`), `/command` sees runtime `env` and bound token.
  - Cold-boot `202 Pending`, `Full` snapshot clone (distinct `sandboxId`, rotated `X-EXECD-ACCESS-TOKEN`, preserved memory state), and `Pause -> Resume` (`skipOnRestore: true` preserves live state without re-running `runtime-init`).
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
4. **Phase 4 — `opensandbox-server` & Helm RBAC upgrade**: Apply the `ClusterRole` rule (`sandboxpools`: `get`, `list`, `watch`) and deploy `opensandbox-server` with `PoolCRReader`, conditional `runtime-init` `ActionBinding` assembly, and deferred-template capability checks. Target pools already advertise `runtime-init`, so newly built standard templates (`EXECD_RUNTIME_INIT=1`, `"entrypointPrewarmed": false`) immediately receive `runtime-init` bindings at create time, while pre-existing `/internal/init`-capable templates default to `entrypointPolicy = "keep"`. Any attempt to instantiate a deferred-entrypoint template on an unmigrated pool fails fast with HTTP `400` rather than stranding an unstarted entrypoint.
5. **Rollback**: Roll back `opensandbox-server` (and rebuild any deferred templates created with `EXECD_RUNTIME_INIT=1` if retiring `runtime-init`), then switch traffic to a `SandboxPool` without `runtime-init` or neutralize an existing pool's handler for `entrypointPrewarmed: true` / image workloads by setting `exec.command` to `["/bin/true"]` (since `ValidateSandboxPoolUpdate` forbids deleting an already-declared `ActionHandler` name from an active pool).
