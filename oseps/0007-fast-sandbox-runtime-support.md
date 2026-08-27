---
title: Sandbox Fleets Runtime (fast-sandbox backend)
authors:
  - "@fengcone"
  - "@Pangjiping"
creation-date: 2026-02-08
last-updated: 2026-07-30
status: provisional
---
# OSEP-0007: Sandbox Fleets Runtime (fast-sandbox backend)

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Why Fast-Sandbox is Fast](#why-fast-sandbox-is-fast)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [The `fleets` runtime type and API reuse model](#the-fleets-runtime-type-and-api-reuse-model)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [How Fast-Sandbox Reduces Creation-Path Overhead](#how-fast-sandbox-reduces-creation-path-overhead)
  - [Kubernetes Ecosystem Integration](#kubernetes-ecosystem-integration)
- [Integration Conditions & Feasibility](#integration-conditions--feasibility)
  - [Lifecycle](#lifecycle-integration)
  - [execd Injection](#execd-injection)
  - [Ingress / Endpoint Access](#ingress--endpoint-access)
  - [Egress / Network Policy](#egress--network-policy)
- [Construction Phases](#construction-phases)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)

<!-- /toc -->

## Summary

Introduce a new OpenSandbox backend type, **`sandbox fleets`** (runtime `type = "fleets"`), whose sole implementation is backed by [fast-sandbox](https://github.com/opensandbox-group/fast-sandbox). A fleet runs many sandboxes as isolated runtimes (container / gVisor / Kata) inside pre-warmed **Fastlet** pods, reached through fast-sandbox's gRPC Fast-Path control plane and its authenticated proxy data plane. The architecture removes the per-sandbox K8s scheduler, watch-propagation, and kubelet path for latency-sensitive workloads; any cross-backend latency claim still requires a reproducible OpenSandbox end-to-end benchmark.

`fleets` is **additive and parallel** to the existing `docker` and `kubernetes` backends; it does **not** replace the pod-per-sandbox `kubernetes` backend. The integration is deliberately scoped:

- **Create** is a *simplified* subset of `CreateSandboxRequest`. Pod-identity-dependent fields (`volumes`, `platform` node-selectors, `resource_requests`, `credential_proxy`, `snapshot_id`, pause/resume) are explicitly rejected; `network_policy` and `secure_access` are staged (contract kept, enforced in phase 1b).
- **Lifecycle** (get / delete / renew-expiration / list / metadata), **execd** (exec / file), and **egress** (`network_policy`) reuse the *existing public API contracts* unchanged, so upstream SDKs are unaffected.

**Implementation baseline and current performance evidence**:

- This revision is aligned to fast-sandbox master [`aac0c2c`](https://github.com/opensandbox-group/fast-sandbox/tree/aac0c2c80f08a8efa455f057ff7323a8248b558d), which implements **FastPath v2** and the `sandbox.fast.io/v1alpha2` CRDs. FastPath v2 carries initial expiry and metadata atomically in Create, returns metadata/expiry in lifecycle reads, supports metadata filtering and explicit deletion, waits directly on Fastlet readiness, and resolves named Infra Components or raw user ports.
- The repository still explicitly has **no release-grade Sandbox Create benchmark**. Its [dated engineering baseline](https://github.com/opensandbox-group/fast-sandbox/blob/aac0c2c80f08a8efa455f057ff7323a8248b558d/docs/guides/performance.md) measured 20 concurrency-1, warm-image runc creates through `RuntimeReady`: mean **76.02 ms**, p50 **75.95 ms**, and p95 **83.15 ms**. Those measurements describe base revision `42fe03549598c3ab730b989c7757634b486697cf`, not `aac0c2c`.
- That baseline excludes Infra readiness, route publication, `DataPlaneReady`, and the OpenSandbox server/gateway path. It is evidence about the current fast-sandbox implementation, not a `fleets` release target or a comparison with the BatchSandbox pool.

> **Correction from earlier drafts**: fast-sandbox does **not** implement a "Fast Mode" (container-first / async-CRD / eventual-consistency) path. Every create ranks in-memory candidates, persists one Sandbox CRD containing the complete initial intent, and only then performs atomic Fastlet admission/runtime creation. Here, **CRD-first** means durable intent precedes runtime creation; it does not mean the CRD write precedes candidate ranking. The gRPC entry avoids the K8s *scheduler* and *watch propagation*, not the CRD/etcd write. There is consequently no `strong` / `fast` consistency switch, request field, or CRD label in this proposal. This OSEP also uses the real fast-sandbox terminology (**Fastlet** / **SandboxPool**), not the "Agent" / "AgentPool" terms from earlier drafts.

> **Note**: The observations above assume the container image and runtime artifacts are already cached on the Fastlet's host node. Cache misses add pull/unpack work and must be reported separately.

## Motivation

OpenSandbox currently supports Docker and Kubernetes runtimes. The Kubernetes runtime provides scalability, but its per-sandbox path can include an API write, scheduler and watch propagation, kubelet reconciliation, container runtime startup, and an image pull on a cache miss. Their costs vary materially by cluster and workload, so this OSEP does not assign them universal latency values.

### OpenSandbox's Existing Pool Optimization

OpenSandbox's Kubernetes runtime already supports a **pool-based optimization** via the `poolRef` field in BatchSandbox CRD. When `poolRef` is specified:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: my-sandbox
spec:
  poolRef: my-pool              # Reference to pre-warmed pool
  taskTemplate:
    spec:
      process:
        command: ["python", "app.py"]
```

**How it works**:

- Users create a pool of pre-provisioned pods (managed by BatchSandbox controller)
- When creating a sandbox, OpenSandbox assigns a task from the pool
- Only `entrypoint` and `env` are customizable; image and resources are pre-defined
- Controller and OpenSandbox Server watch K8s API for state changes

**Performance with pool**:

- Eliminates scheduler wait and pod startup time
- Still requires K8s API write + watch propagation overhead
- Image must be pre-pulled in pool pods
- No reproducible report currently establishes a portable allocation-latency number; the comparison benchmark in this OSEP must measure both backends under the same environment and readiness boundary

This is an effective optimization for many use cases. However, fast-sandbox aims to push latency even lower through additional innovations described below.

For AI Agent and Serverless scenarios that require rapid sandbox provisioning, removing scheduler/watch/kubelet work from the per-sandbox hot path is valuable even though fast-sandbox retains a synchronous CRD write.

### Why Fast-Sandbox is Fast

fast-sandbox reduces creation-path overhead through three key design choices:

**Comparison: OpenSandbox Pool vs fast-sandbox**


| Aspect                          | OpenSandbox BatchSandbox Pool                      | fast-sandbox (fleets)                       |
| ------------------------------- |----------------------------------------------------|---------------------------------------------|
| **Allocation mechanism**        | K8s API write → Controller watch → Task assignment | gRPC → in-memory Top-K → CRD write → Fastlet admission |
| **Latency (with cached image)** | No comparable release-grade report                 | No comparable release-grade report          |
| **Scheduling**                  | K8s Scheduler places pool pods (one-time)          | In-memory Top-K registry with image affinity |
| **Image awareness**             | Pool pods have fixed image                         | Registry ranks by image cache availability  |
| **Customization**               | entrypoint, env only                               | entrypoint, env, image per request          |
| **Container creation**          | pre-warmed                                         | Direct containerd socket inside Fastlet     |
| **Consistency**                 | Durable K8s state is the source of truth            | Sandbox CRD is persisted synchronously before runtime creation |
| **Failure recovery**            | K8s Controller reconciliation                      | NodeJanitor cleanup + Manual/AutoRecreate policy |

Both approaches use pre-provisioned resource pools to eliminate cold start overhead. fast-sandbox's key advantage is bypassing the K8s **scheduler and watch propagation** for container placement while still committing durable intent through a CRD write.

#### 1. gRPC Fast-Path Allocation, Bypassing the K8s Scheduler

Traditional K8s sandbox creation follows this control flow:

```
Client → K8s API Server → etcd → Scheduler → etcd → Kubelet → Container Runtime
```

fast-sandbox uses a gRPC Fast-Path that is **CRD-first for every create** — it does not bypass etcd, but it bypasses the scheduler queue and watch propagation:

```
Client → gRPC Fast-Path → in-memory Top-K candidate ranking
       → K8s API (Sandbox CRD write, IO 1)
       → atomic Fastlet admission/create (IO 2) → containerd
```

**With uncached image**: additional image pull time applies.

The Fast-Path Server maintains an **in-memory registry** for placement, eliminating:

- scheduler queue wait time
- watch propagation delays

It does **not** eliminate the CRD/etcd write — that write (IO 1) is on the synchronous happy path and precedes the Fastlet create (IO 2). The durable CRD is the source of truth; there is no eventual-consistency "fast mode".

#### 2. In-Memory Top-K Scheduling with Image Affinity

fast-sandbox's registry ranks candidate Fastlets (it does not use a single additive score). Ordering, in priority:

```
1. image-cache hit (Fastlets with the image cached rank first)
2. lower normalized load (used / capacity)
3. stable hash tiebreak (request stable key + Fastlet ID)
```

Key characteristics:

- **In-memory placement**: No disk I/O, no database queries
- **Image affinity**: Prioritizes Fastlets with cached images
- **Atomic admission**: The selected Fastlet is the authority that consumes a capacity slot; the registry ranking is advisory, and Fastlet atomic admission is final
- **Top-K with retry**: The Fast-Path picks the top candidate and can retry the next candidate on rejection

This is fundamentally different from the K8s scheduler which:

- Runs as a separate process with IPC overhead
- Doesn't track image cache state
- Schedules pods without considering image availability

#### 3. Kubernetes Ecosystem Reuse with Direct Containerd Access

fast-sandbox achieves speed while maintaining K8s compatibility:


| Aspect                     | fast-sandbox Approach                                          | K8s Benefit                               |
| -------------------------- | -------------------------------------------------------------- | ----------------------------------------- |
| **Resource Accounting**    | Fastlet Pods tracked in K8s                                    | Resource visibility via`kubectl get pods` |
| **Scheduling Constraints** | Node selectors, taints, tolerations on the Fastlet Pod         | K8s scheduler places Fastlet Pods optimally |
| **Container Creation**     | Direct containerd socket access (bypasses kubelet)             | Removes kubelet from the per-sandbox path |
| **Security Containers**    | Supports gVisor/Kata Containers via containerd runtime handler | Same workflow, different runtime class    |
| **Network Namespace**      | Each sandbox gets its own netns + private IP inside the Fastlet Pod | K8s CNI plugins carry the Fastlet Pod's traffic |

The key insight: **use K8s for what it's good at** (resource accounting, cluster management, scheduling constraints at the Fastlet-pool granularity), but **bypass the K8s scheduler for the hot path** (container placement + creation).

### Goals

- Add a `fleets` runtime type (`config.runtime.type = "fleets"`) implemented as a new `FleetSandboxService` (both `SandboxService` and `ExtensionService`, **not** a Kubernetes `WorkloadProvider`)
- Reuse the existing lifecycle API (`get` / `delete` / `renew-expiration` / `list` / `metadata`) with no changes to routes or SDKs
- Reuse the existing execd exec/file access pattern (`get_endpoint(id, 44772)` → in-sandbox execd HTTP) unchanged
- Reuse the existing egress contract (`network_policy` at create; `egress-api.yaml` `/policy` at runtime) only in separately gated phase 1b, after per-sandbox enforcement exists on fast-sandbox
- Provide a simplified Create that maps a well-defined subset of `CreateSandboxRequest` to fast-sandbox's gRPC `CreateSandbox`, and cleanly rejects unsupported fields
- Demonstrate lower p50 and p95 user-visible creation latency than the `kubernetes` pool backend, measured from SDK create start until `Running` and the execd endpoint are usable under the same environment, cache state, workload, and concurrency; this OSEP sets no universal absolute-millisecond threshold
- Provide flexible deployment: users can bring their own fast-sandbox or use OpenSandbox-provided charts

### Non-Goals

- Replacing or removing the existing Docker or Kubernetes runtimes
- Supporting `volumes` (PVC / host / ossfs), `platform` node-selectors, `resource_requests`, `credential_proxy`, or `snapshot_id` on `fleets`
- Supporting `pause` / `resume` / snapshot on `fleets` (fast-sandbox states these as explicit non-goals)
- Implementing `fleets` as a Kubernetes `WorkloadProvider`
- Implementing a full Kubernetes operator for fast-sandbox (it has its own controller)
- Changing the OpenSandbox sandbox lifecycle API or SDKs in a breaking way
- Direct management of fast-sandbox `Sandbox` / `SandboxPool` CRDs or Fastlet pods (owned by the fast-sandbox controller)

## Requirements

- Must register as a new `SandboxService` under `config.runtime.type = "fleets"`; must not modify the `WorkloadProvider` contract
- Must also implement `ExtensionService`, because OpenSandbox startup unconditionally requires it for renew-on-access behavior
- Must map `fleets` to `NoopSnapshotRuntime` in `create_snapshot_runtime()` so the server can start while snapshot operations remain explicitly unsupported
- Must not change the public lifecycle, execd, or egress API contracts or the SDKs
- Simplified Create must reject unsupported fields with a clear, actionable error rather than silently ignoring them
- Must return a stable ingress-gateway endpoint handle for ports 44772 (execd), 18080 (the SDK's eagerly requested egress handle), and arbitrary user ports without requiring the sandbox route to be ready. In phase 1a, requests through the 18080 handle return a clear unsupported response; phase 1b makes that route functional only after its policy-manager contract exists
- Must extend the ingress provider/proxy contract to carry the requested port, complete upstream URL/path, upstream-only headers, and route expiry; adding only a provider is insufficient
- Per-sandbox egress must be enforced (not advisory) once `network_policy` is accepted; phase 1a rejects it, and a separately gated phase 1b may accept it only after the cross-repository enforcement contract is implemented
- Must handle status mapping between fast-sandbox and OpenSandbox states
- Must preserve actual NotFound semantics: a missing fast-sandbox CRD maps to OpenSandbox HTTP 404, while a retained `Stopped`/expired CRD maps to `Terminated`
- Must preserve OpenSandbox list semantics, including state filtering, page/pageSize, totalItems, totalPages, and hasNextPage, even though FastPath v2 exposes metadata filtering plus continue-token pagination
- Must preserve tenant isolation: when `[tenants]` is configured, every namespaced FastPath call (create/get/list/update/delete/endpoint) must resolve the current tenant to a fast-sandbox namespace. The authenticated namespace must also survive beyond the request context in stable gateway routes and renew intents; locks, throttles, and caches must key on `(namespace, sandbox_id)`. If this complete mapping is not implemented, `fleets` must **reject** tenant configuration
- gRPC reachability from the OpenSandbox Server to the fast-sandbox Fast-Path Server is required

## Proposal

Introduce a new backend type **`sandbox fleets`**, implemented as a `FleetSandboxService` that communicates with the fast-sandbox Fast-Path Server via the gRPC Fast-Path API. It is selected by `config.runtime.type = "fleets"` and registered alongside `docker` and `kubernetes` in `server/opensandbox_server/services/factory.py`.

> **Why a new backend type, not a new `WorkloadProvider`**: OpenSandbox has two abstraction layers — the top-level `SandboxService` (selected by `config.runtime.type`; the seam for `docker` / `kubernetes`) and the Kubernetes-internal `WorkloadProvider` (`batchsandbox` / `agent-sandbox`). The `WorkloadProvider` ABC is saturated with K8s semantics (namespace, CR metadata, pod-spec mutation) and cannot host a separate gRPC control plane. fast-sandbox is therefore a new `SandboxService`. This choice is what lets the lifecycle routes, exec/file access, and egress access patterns be reused unchanged, because all of them funnel through the `SandboxService` ABC and the `get_endpoint` + in-sandbox HTTP contracts rather than through pod semantics.

**Architecture Overview**:

```
+-------------------------------------------------------------------------+
|                        OpenSandbox Control Plane                        |
+-------------------------------------------------------------------------+
|                                                                         |
|   lifecycle routes ---> SandboxService (ABC)                            |
|                              |                                          |
|                     FleetSandboxService                                 |
|                     |                    |                              |
|         gRPC FastPathService (9090)   get_endpoint()                    |
|                     |                    | (via ingress gateway)        |
|                     v                    v                              |
|   +-----------------------+     +----------------------+                |
|   | fast-sandbox          |     | ingress gateway      |                |
|   | Fast-Path Server      |     | (namespace +         |                |
|   +----------+------------+     |  sandbox_id + port)  |                |
|              |                  +----------+-----------+                |
|              |                             |                            |
|              | CRD-first + in-memory       | Sandbox Proxy /            |
|              | Top-K placement             | Fastlet Proxy              |
|              v                             v                            |
|   +---------------------------------------------------+                 |
|   | Fastlet Pod (K8s Managed)                         |                 |
|   |   Fastlet control + Fastlet Proxy sidecar         |                 |
|   |   many sandbox runtimes via direct containerd     |                 |
|   |   each: own netns + private IP; execd :44772      |                 |
|   +---------------------------------------------------+                 |
|                                                                         |
+-------------------------------------------------------------------------+
                                ^
                                | K8s API Server (Fastlet Pod mgmt + Sandbox CRD)
                                |
+-------------------------------------------------------------------------+
|                    Kubernetes Control Plane (CRD path)                  |
|  - Fastlet Pod lifecycle (create/monitor/delete)                        |
|  - Sandbox / SandboxPool CRDs (durable intent, reconciliation, audit)   |
|  - Resource accounting (visible in kubectl); scheduling constraints     |
+-------------------------------------------------------------------------+
```

**Data Flow Comparison** (assuming cached image):

```
Standard K8s Runtime:
OpenSandbox Server → K8s API → etcd → Scheduler → etcd → Kubelet → containerd

Sandbox Fleets (fast-sandbox, CRD-first — the only path):
OpenSandbox Server → gRPC Fast-Path → in-memory Top-K → K8s API (CRD write)
                   → atomic Fastlet admission → containerd
      (scheduler + watch propagation bypassed; latency must be measured end-to-end)
```

### The `fleets` runtime type and API reuse model

The three "reused" API areas reuse *different* things. Being precise here avoids the biggest integration trap: **there is no server-side exec or egress API endpoint.** exec/file and egress runtime control are HTTP contracts that clients speak *directly to components inside the sandbox*; the server's only role is `get_endpoint`.

| Area | What is reused | What must be built for `fleets` |
| --- | --- | --- |
| Lifecycle (get/delete/renew/list/metadata) | Public routes + `SandboxService` ABC remain unchanged | Map to FastPath v2; adapt OpenSandbox state/page pagination and error semantics |
| execd exec/file | `specs/execd-api.yaml` (client → in-sandbox execd:44772) is backend-agnostic | Declare the Pool Infra Component `execd`; map public port 44772 to `component_name = "execd"` |
| egress `network_policy` | `NetworkPolicy` schema + `egress-api.yaml` `/policy` (client → egress proxy:18080) | Phase 1a returns the SDK-required stable 18080 handle but policy traffic is unsupported; phase 1b delivers/enforces policy and activates the assignment-fenced route |
| Endpoint resolution | `SandboxService.get_endpoint` → public stable `Endpoint` remains unchanged | Issue an authenticated tenant-scoped handle before readiness; extend ingress to resolve the complete FastPath upstream route lazily on traffic |

**Simplified Create.** The `fleets` Create is a subset of the full `CreateSandboxRequest`. The table below classifies every field as **kept** (mapped through), **downgraded** (accepted but semantics change), **staged** (contract kept, enforced in a later phase), or **rejected** (HTTP 400 with a clear message). The common thread among rejected fields is that they assume *1 sandbox = 1 dedicated K8s Pod* (own rootfs, node, volume set, netns), which does not hold in the shared-Fastlet model.

| `CreateSandboxRequest` field | fleets | Mapping / reason |
| --- | --- | --- |
| `image.uri` | **kept, required** | → FastPath v2 `CreateRequest.image`. Unlike the existing BatchSandbox pool mode, a fast-sandbox `SandboxPool` does not define the workload image, so fleets rejects a request that omits it even when `extensions.poolRef` is set |
| `entrypoint` | **kept, required** | → `command` / `args`. A fast-sandbox Pool defines Infra Components and resources, not the user process |
| `env` | **kept** | → `envs`; null values are rejected because FastPath v2 uses `map<string,string>` |
| `timeout` | **kept** | Convert the relative duration once to an absolute `expires_at_unix_seconds`; it is included in the first idempotent Create/CRD write and must be reused unchanged on retries |
| `metadata` | **kept** | → FastPath v2 `metadata` in the first Create. Keys/values must satisfy the existing OpenSandbox/Kubernetes label validation |
| `extensions.poolRef` | **kept** | Preserve the existing public camelCase key and translate it only inside the backend to FastPath `pool_ref` (else use `default_pool_ref`) |
| `extensions["access.renew.extend.seconds"]` | **kept** | Persist under a fleets-reserved, DNS-safe FastPath metadata key, strip it from public metadata, and expose it through `ExtensionService.get_access_renew_extend_seconds()` |
| Other `extensions` keys | **rejected unless explicitly supported** | Prevent silent loss of opaque or pod-specific options, including `bootstrap.execd.isolation` |
| `resource_limits` | **downgraded** | fast-sandbox enforces the pool's immutable `sandboxResources`; the request value is validated for pool compatibility, not applied per-sandbox |
| `network_policy` | **staged (1b)** | Contract kept; rejected in phase 1a, enforced per-slot netns in phase 1b (see [Egress / Network Policy](#egress--network-policy)) |
| `secure_access` | **staged (1b)** | Naturally aligned: fast-sandbox already returns `required_headers` with a short-lived Ed25519 bearer per `ResolveEndpoint`. Server-issued access headers are layered on the gateway route by the fleets ingress adapter; deferred to 1b (see [Ingress / Endpoint Access](#ingress--endpoint-access)) |
| `image.auth` | **rejected** | Private-registry credentials are not carried to fast-sandbox; an authenticated image is rejected rather than attempting an unauthenticated pull (future: map to a pool-level imagePullSecret) |
| `snapshot_id` | **rejected** | No snapshot capability in fast-sandbox (explicit non-goal) |
| `platform` | **rejected** | No per-sandbox node scheduling; scheduling is per Fastlet pool |
| `resource_requests` | **rejected** | No per-sandbox K8s requests / Burstable QoS; resources are fixed by `SandboxPool.spec.sandboxResources` |
| `credential_proxy` | **rejected (all phases)** | Rides the per-pod egress mitmproxy sidecar, which has no place in the shared-Fastlet model. Rejected even after 1b accepts `network_policy`, so it is never silently ignored |
| `volumes` | **rejected** | Fastlet child containers cannot receive dynamic PVC/CSI mounts |

In short, `fleets` Create keeps the "**what to run**" fields (image / entrypoint / env) plus "**which pool, how long, what tags**" (`poolRef` / timeout / metadata), and drops or stages the pod-level isolation / storage / snapshot / signed-network fields. The fast-sandbox v2 Create persists image, absolute expiry, metadata, Pool, command, environment, failure defaults, and assignment atomically in the initial CRD write; there is no follow-up Update or create rollback for these fields.

### Notes/Constraints/Caveats

- The fast-sandbox control plane (Fast-Path Servers, Reconcilers, Sandbox Proxy) and Fastlet pools + NodeJanitor must be deployed separately (by the user or via OpenSandbox-provided Helm charts)
- fast-sandbox uses its own CRD types (`Sandbox`, `SandboxPool`, group `sandbox.fast.io/v1alpha2`) - OpenSandbox does not manipulate these directly
- gRPC communication requires network reachability from OpenSandbox Server to the fast-sandbox Fast-Path Server
- execd is injected via fast-sandbox's **Infra Component** mechanism (see [execd Injection](#execd-injection)), not the K8s init-container copy used by the pod backend
- Because all sandboxes in a Fastlet pod share that pod's K8s network identity (SNAT to one pod IP), **standard Kubernetes NetworkPolicy cannot express per-sandbox egress** for fleets; phase 1b must enforce it inside each sandbox's netns (see [Egress / Network Policy](#egress--network-policy))
- **Tenant isolation** relies on fast-sandbox namespaces: `ListSandboxes` is namespace-only, so a shared namespace would expose tenants to each other. `FleetSandboxService` must map each OpenSandbox tenant to a distinct namespace on every call and carry an authenticated namespace claim into stable routes and background renew work, or reject `[tenants]` configuration outright (phase 1a)

### Risks and Mitigations


| Risk                                                                     | Mitigation                                                                                                                         |
| ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| Fast-Path Server becomes a single point of failure                       | Fast-Path Servers are multi-active; retry only with the same sandbox ID and absolute expiry, then read the durable intent to disambiguate post-persistence failures |
| gRPC API changes in fast-sandbox could break integration                 | Version pinning in deployment; compatibility matrix documentation                                                                  |
| Network partition between OpenSandbox Server and fast-sandbox Fast-Path  | Configurable timeouts; health check endpoint integration                                                                           |
| State drift if sandboxes are managed outside OpenSandbox                 | OpenSandbox tracks sandbox IDs; periodic state reconciliation via gRPC GetSandbox                                                  |
| SDK eagerly fetches execd and egress endpoints before health polling     | `get_endpoint` returns a stable tenant-scoped gateway URL without resolving FastPath; the gateway resolves lazily and returns retryable 503 while a supported target is Pending |
| Background renew workers have no request tenant context                  | Carry an authenticated namespace in the stable route and renew intent; validate and bind it for the full renew operation; key all deduplication state by namespace + sandbox ID |
| FastPath pagination does not match OpenSandbox page/total semantics       | Phase 1a follows FastPath continue tokens to exhaustion, maps/filter states, then computes the requested page and totals; optimize only with a later indexed API |
| FastPath v2 `GetSandbox` does not normalize Kubernetes NotFound at `aac0c2c` | Fix the upstream handler to return gRPC `codes.NotFound`; the fleets adapter maps only that code to HTTP 404 |
| "Reusing egress" misread as reusing a server API, underestimating cost  | This OSEP states explicitly that egress enforcement is bespoke; only the HTTP contract + endpoint/token plumbing are reused        |
| Carrying `network_policy` needs a proto/CRD change on fast-sandbox       | Additive, backward-compatible field; gated behind an "ask first" review with fast-sandbox maintainers                             |
| gVisor/Kata do not honor host-netns egress rules                         | Phase 1b restricts `network_policy` to the `container` (runc) runtime; reject egress on gVisor/Kata                                |
| Orphaned sandboxes on Fastlet/node loss                                  | fast-sandbox NodeJanitor performs fenced cleanup; the phase-1a backend keeps FastPath's default `MANUAL` failure policy             |
| Users expect volumes/snapshot on fleets                                  | Simplified Create rejects them with a clear message pointing to the `kubernetes` backend                                          |

## Design Details

### How Fast-Sandbox Reduces Creation-Path Overhead

The fast-sandbox architecture is built around three performance-critical design choices:

#### 1. Bypassing the K8s Scheduler for the Hot Path (CRD write retained)

```
┌──────────────────────────────────────────────────────────────────────────┐
│              CRD-first Creation Flow (image cached, happy path)           │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          |
│  Prerequisite: Image is cached on the Fastlet's host node (containerd)   │
│                                                                          |
│  1. OpenSandbox Server → gRPC CreateSandbox request                      │
│                                                                          |
│  2. In-memory Top-K placement (registry-only, no K8s API)                │
│     • Filter by pool, namespace, runtime/profile, capacity               │
│     • Rank by: image-hit, then used/capacity, then stable hash           │
│                                                                          |
│  3. IO 1: Sandbox CRD write to K8s API / etcd                            │
│     • Durable intent + idempotency by request_id                         │
│                                                                          |
│  4. IO 2: atomic Fastlet admission → runtime create/start (cached image) │
│     • Direct socket access to host containerd                            │
│     • No image pull (cached); sandbox gets its own netns + private IP    │
│                                                                          |
│  5. Fast-Path returns {sandbox_uid, sandbox_name, fastlet_pod}           │
│     • Returns at RuntimeReady; endpoints resolved later via ResolveEndpoint │
│                                                                          |
│  Measure: client-observed Create through RuntimeReady                    │
│  Do not infer this total by adding estimates from different runs.        │
│                                                                          |
│  If image is NOT cached: image pull time is added to step 4              │
└──────────────────────────────────────────────────────────────────────────┘
```

The difference is steps 2-4 of the K8s path (scheduler queue + watch propagation + kubelet). fast-sandbox keeps the etcd write (as IO 1) but replaces the scheduler/watch/kubelet steps with in-memory placement + a direct Fastlet create.

The current public engineering baseline found that warm runc runtime work, not candidate ranking, dominated the tested path: mean RuntimeDriver work was 67.76 ms of a 76.02 ms client-observed Create, including 21.95 ms in `NewContainer`, 36.87 ms in `NewTask`, and 7.89 ms in `Start`. These are nested observations from one dated environment, not budgets for this diagram.

#### 2. Registry Top-K Ranking

The registry does not compute a single additive score; it hard-filters candidates then sorts them. Simplified from the real `TopK` in `internal/controlplane/placement/registry.go`:

```go
// Hard filter: namespace, pool, readiness, capacity, runtime/profile match.
// Then sort the survivors:
sort(candidates, func(a, b) bool {
    if a.imageHit != b.imageHit {
        return a.imageHit          // image-cache hit ranks first
    }
    if a.used*b.capacity != b.used*a.capacity {
        return a.used*b.capacity < b.used*a.capacity   // lower normalized load
    }
    return stableHash(reqKey, a.id) < stableHash(reqKey, b.id)  // stable tiebreak
})
// Return top K; Fastlet atomic admission is the final authority on capacity.
```

The repository currently provides `BenchmarkRegistryTopK1000` for same-machine regression comparisons, but publishes no raw result that supports a portable `100 Fastlets` or `1000 Fastlets` latency claim. This microbenchmark excludes Kubernetes, Fastlet admission, runtime/network creation, Infra readiness, and routing; it must not be presented as Sandbox Create latency.

#### 3. Direct Containerd Integration

Fastlet Pods run with access to the host containerd socket and create sandbox containers directly:

```go
// fast-sandbox internal/runtime/containerd/driver.go (illustrative)

client, _ := containerd.New("/run/containerd/containerd.sock",
    containerd.WithDefaultNamespace("k8s.io"))

// Direct container creation - bypasses kubelet entirely
container, _ := client.NewContainer(
    ctx, sandboxID,
    containerd.WithImage(image),                 // Already cached
    containerd.WithNewSnapshot(...),             // Snapshot setup still runs with a cached image
    // runc by default; "io.containerd.runsc.v1" (gVisor) or Kata shim per pool runtime
    oci.WithLinuxNamespace(networkNamespace),    // sandbox's own netns
)

task, _ := container.NewTask(ctx, cio.NewCreator(...))
task.Start(ctx)
```

This approach:

- Eliminates kubelet reconciliation from the per-sandbox creation path
- Enables image cache reuse (the Fastlet Pod shares the node's containerd image store)
- Supports alternative runtimes (gVisor via `runsc`, Kata) via the pool's immutable runtime handler

### Kubernetes Ecosystem Integration

Despite bypassing the K8s scheduler for the hot path, fast-sandbox maintains full compatibility:

#### Resource Accounting via K8s Pods

Fastlet Pods are normal K8s Pods:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fast-sandbox-fastlet-node-1
  labels:
    app: fast-sandbox-fastlet
    pool-ref: default-pool
spec:
  containers:
  - name: fastlet
    image: fast-sandbox/fastlet:latest
    resources:
      requests:
        cpu: "2000m"
        memory: "4Gi"
      limits:
        cpu: "4000m"
        memory: "8Gi"
    volumeMounts:
    - name: containerd-socket
      mountPath: /run/containerd/containerd.sock
  volumes:
  - name: containerd-socket
    hostPath:
      path: /run/containerd/containerd.sock
```

These Pods are visible in `kubectl get pods` and count against:

- Node resource allocation (visible to cluster autoscaler)
- Resource quotas (namespace limits enforced)
- Scheduler decisions (node affinity, taints, tolerations)

#### CRD for Reconciliation and Auditing

fast-sandbox defines two CRDs:

```yaml
# SandboxPool - manages Fastlet Pod lifecycle (fields illustrative; see fast-sandbox CRD)
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: default-pool
  namespace: fast-sandbox
spec:
  capacity:
    poolMin: 2
    poolMax: 10
    bufferMin: 1
    bufferMax: 3
  maxSandboxesPerPod: 5
  runtime: container               # immutable: container | gvisor | kata-qemu | kata-clh | ...
  sandboxResources:                # immutable per-sandbox profile, enforced by Fastlet
    cpu: "500m"
    memory: "512Mi"
    pids: 256
  infraComponents:
    - name: execd
      artifact:
        source:
          image:
            reference: ghcr.io/opensandbox/execd@sha256:<digest>
        mappings:
          - sourcePath: /execd
            targetPath: /.fast/components/execd/execd
      process:
        command:
          - /.fast/components/execd/execd
          - --port
          - "44772"
        restartPolicy: OnFailure
        healthCheck:
          httpGet:
            path: /ping
          timeoutSeconds: 10
      endpoint:
        protocol: HTTP
        port: 44772
  warmImages:                      # asynchronously pre-pulled, protected from cache GC
    - python:3.11
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:latest
        volumeMounts:
        - name: containerd-socket
          mountPath: /run/containerd/containerd.sock
      volumes:
      - name: containerd-socket
        hostPath:
          path: /run/containerd/containerd.sock

---
# Sandbox - durable intent + audit trail (created by the Fast-Path or declaratively)
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: my-sandbox                 # = request_id (OpenSandbox sandbox_id)
  namespace: fast-sandbox
  labels:
    sandbox.fast.io/created-by: fastpath
    metadata.sandbox.fast.io/team: agents
spec:
  image: python:3.11
  poolRef: default-pool
  command: ["python", "-m", "http.server", "8000"]
  expireTime: "2026-07-30T12:00:00Z"
  failurePolicy: Manual               # FastPath v2 default; AutoRecreate is available upstream
  recoveryTimeoutSeconds: 60
status:
  runtimeState: Ready               # ObservedState: Pending/Creating/Ready/Draining/Stopped/Failed/Unavailable
  dataPlaneState: Ready
  components:
    - name: execd
      state: Ready
      protocol: HTTP
      port: 44772
  assignment:
    fastletName: fast-sandbox-fastlet-node-1
    fastletPodUID: ...
    nodeName: node-1
```

Note: the `Sandbox` CRD carries **no `exposedPorts` field and no inline endpoints** — endpoints are resolved on demand via `ResolveEndpoint`, which returns an authenticated proxy route. There is a single `created-by: fastpath` label (no fast/strong variants).

These CRDs serve as:

- **Durable intent + audit trail**: the CRD is the source of truth; the Fast-Path writes it first (IO 1)
- **Self-healing**: leader-elected Reconcilers converge state and clean up orphaned sandboxes
- **Observability**: Standard K8s tools (kubectl, metrics-server) work

#### Security Container Support

fast-sandbox supports gVisor/Kata Containers via the pool's immutable runtime handler:

```
container   → io.containerd.runc.v2
gvisor      → io.containerd.runsc.v1
kata-qemu   → containerd Kata shim (QEMU)
kata-clh    → containerd Kata shim (Cloud Hypervisor)
```

The runtime is a `SandboxPool` field (immutable per pool), so OpenSandbox selects isolation level by targeting a pool, without changing the integration layer.

> **Caveat for egress**: gVisor and Kata run their network stack in a user-space kernel / guest VM, so host-netns iptables rules do not reliably filter their egress. Phase 1b restricts per-sandbox `network_policy` to the `container` (runc) runtime unless another runtime is separately proven (see [Egress / Network Policy](#egress--network-policy)).

#### NodeJanitor: Orphan Cleanup

Because a sandbox is bound to one Fastlet Pod, orphaned containerd resources can arise if:
- The Fastlet Pod is unexpectedly deleted (crash, node drain, eviction)
- The `Sandbox` CRD is deleted while a container still exists
- A CRD is recreated with a new UID (UID mismatch)

fast-sandbox provides a **NodeJanitor DaemonSet** on each node that performs fenced cleanup a lost Fastlet can no longer do.

**How NodeJanitor detects orphans:**

| Orphan Type | Detection Method | Cleanup Trigger |
|-------------|-------------------|-----------------|
| Fastlet Pod disappeared | Pod UID not found in K8s API | After orphan timeout |
| Sandbox CRD deleted | CRD not found | After orphan timeout |
| UID mismatch (recreated CRD) | Container label ≠ CRD UID | After orphan timeout |

**Scan process (per node):** enumerate fast-sandbox-managed containerd resources from durable per-slot state, perform a fresh Kubernetes ownership check and an orphan-age check, and only then tear down the task/container, snapshot, network namespace, and any Infra state.

**Note for the egress work (phase 1b):** in-netns rules are reaped automatically when the slot's netns is deleted, so they need no janitor change; any out-of-netns state (e.g. a host-side DNS proxy or ipset) would require extending NodeJanitor.

### Configuration Extension

Add `FleetsRuntimeConfig` to `server/opensandbox_server/config.py`:

```python
class FleetsRuntimeConfig(BaseModel):
    """sandbox fleets (fast-sandbox) runtime configuration."""

    fastpath_endpoint: str = Field(
        default="fast-sandbox-fastpath.opensandbox.svc:9090",
        description="fast-sandbox Fast-Path Server gRPC endpoint.",
    )
    default_pool_ref: str = Field(
        default="default-pool",
        description="Default SandboxPool when extensions.poolRef is unset.",
    )
    execd_component_name: str = Field(
        default="execd",
        description="Pool Infra Component used for public execd port 44772.",
    )
    endpoint_access_mode: Literal["central_proxy", "direct_fastlet_proxy"] = Field(
        default="central_proxy",
        description="FastPath v2 endpoint mode used by the trusted ingress.",
    )
    require_ingress_gateway: bool = Field(
        default=True,
        description="fleets resolves endpoints via the ingress gateway only.",
    )
```

Update `AppConfig` to include the new config block and validation logic.

### TOML Configuration Example

```toml
[server]
host = "0.0.0.0"
port = 8080
api_key = "your-secret-key"

[runtime]
type = "fleets"

[fleets]
fastpath_endpoint = "fast-sandbox-fastpath.fast-sandbox-system.svc:9090"
default_pool_ref = "default-pool"
execd_component_name = "execd"
endpoint_access_mode = "central_proxy"
require_ingress_gateway = true
```

### New Code Structure

```
server/opensandbox_server/services/fleets/
├── __init__.py
├── fleet_service.py         # New: FleetSandboxService(SandboxService, ExtensionService)
├── fastpath_client.py       # New: gRPC client wrapper for fast-sandbox FastPathService v2
├── create_mapping.py        # New: CreateSandboxRequest subset → v2 CreateRequest; field rejection
└── status_mapping.py        # New: fast-sandbox Sandbox status → OpenSandbox states
# Modified: server/opensandbox_server/services/factory.py (register "fleets")
# Modified: server/opensandbox_server/services/snapshot_runtime_factory.py (fleets → NoopSnapshotRuntime)
# Modified: components/ingress provider/proxy contract + fleets provider
# Modified: components/ingress + server renew-intent schemas/consumers (authenticated namespace propagation)
```

### API Mapping


| OpenSandbox API (`SandboxService`)      | fast-sandbox gRPC                    | Notes                                          |
| --------------------------------------- | ------------------------------------ | ---------------------------------------------- |
| `POST /sandboxes` (simplified)          | v2 `CreateSandbox`, then bounded `WaitSandboxReady(data_plane=true)` | Initial expiry/metadata are atomic. If the readiness wait expires, return the accepted sandbox as `Pending`; the SDK can still obtain stable lazy gateway endpoints and continue its health loop |
| `GET /sandboxes/{id}`                   | v2 `GetSandbox`                      | `SandboxInfo` already includes metadata + expiry. gRPC NotFound maps to HTTP 404 |
| `GET /sandboxes` (list)                 | v2 `ListSandboxes`                   | Follow continue tokens, then apply OpenSandbox state filtering and page/total response semantics |
| `DELETE /sandboxes/{id}`                | `GetSandbox` + v2 `DeleteSandbox`    | Preflight preserves public 404; accepted delete remains async/finalizer-driven |
| `POST /sandboxes/{id}/renew-expiration` | v2 `UpdateSandbox(expires_at_unix_seconds)` | Absolute expiry is persisted in the CRD |
| `PATCH /sandboxes/{id}/metadata`        | v2 `UpdateSandbox(metadata_upsert, metadata_delete_keys)` | Direct RFC 7396 mapping without read-modify-write |
| `GET /sandboxes/{id}/endpoints/{port}`  | No readiness-dependent FastPath call | Return a stable, tenant-scoped gateway URL even while Pending. The gateway lazily maps 44772 to component `execd`, ordinary ports to raw-port targets, and refreshes `X-Fast-Sandbox-Route-Credential` internally |
| Gateway request for a supported target  | v2 `ResolveEndpoint`                 | Resolve/cache the current upstream route at request time; return retryable 503 while Pending or on a bounded readiness timeout |
| Gateway request on port 18080 in phase 1a | No FastPath route                    | Endpoint discovery succeeds for SDK compatibility, but an actual egress-policy request returns HTTP 501 until phase 1b |
| diagnostics (logs/inspect/events)       | `GetSandboxDiagnostics`              | Lifecycle events only; no process stdout/stderr |
| `pause` / `resume`                      | (unsupported)                        | Clear "unsupported on fleets" error |
| snapshot API                            | OpenSandbox `NoopSnapshotRuntime`     | Server starts successfully; snapshot operations return the existing unsupported/no-op result |

### Request Parameter Mapping

```python
# OpenSandbox CreateSandboxRequest (accepted subset) → fast-sandbox CreateRequest
{
    "image": {"uri": "python:3.11"},               # → image
    "entrypoint": ["python", "-m", "http.server"],  # → command (+ args)
    "env": {"PYTHONUNBUFFERED": "1"},              # → envs
    "resource_limits": {"cpu": "500m"},            # → validated against SandboxPool profile
    "timeout": 3600,                              # → one absolute expires_at_unix_seconds in Create
    "network_policy": {...},                       # → new additive CreateRequest field (phase 1b)
    "metadata": {...},                             # → CreateRequest.metadata
    "extensions": {"poolRef": "default-pool"},     # public key → FastPath pool_ref
    # request_id (idempotency key + Sandbox CRD name) = OpenSandbox sandbox_id
}
# Rejected (HTTP 400): volumes, platform, resource_requests, credential_proxy, snapshot_id, image.auth
# Staged (contract kept, enforced in phase 1b): network_policy, secure_access
```

### Status Mapping


| fast-sandbox `Sandbox` state                        | OpenSandbox State |
| --------------------------------------------------- | ----------------- |
| RuntimeState Ready + DataPlaneState Ready           | Running           |
| Pending / Creating                                  | Pending           |
| Draining (delete in progress)                       | Stopping          |
| Stopped with `RuntimeReady=False, reason=Expired` (CRD retained) | Terminated |
| Stopped (CRD retained)                              | Terminated        |
| Failed / Unavailable                                | Failed            |
| Actual missing CRD / gRPC `codes.NotFound`          | HTTP 404 (no synthetic Sandbox object) |

fast-sandbox splits `RuntimeReady` (runtime up) from `DataPlaneReady` (route + Infra published). OpenSandbox reports **Running only when both are Ready**, matching the existing "endpoint usable" expectation.

> **Important**: On expiry, fast-sandbox's reconciler sets `runtimeState=Stopped` and a `RuntimeReady=False, reason=Expired` Condition while retaining the CRD. That retained object maps to `Terminated`; it is distinct from an actual missing CRD, which maps to HTTP 404. `Draining` maps to `Stopping`.

### Extensions Field Support

The `extensions` field in `CreateSandboxRequest` supports fleets-specific options:


| Extension Key | Type | Description |
| --- | --- | --- |
| `poolRef` | string | Existing OpenSandbox key; selects the fast-sandbox `SandboxPool` and is translated internally to `pool_ref` |
| `access.renew.extend.seconds` | decimal string | Existing renew-on-access setting; persisted durably under a fleets-reserved metadata key and served through `ExtensionService` |

All other extension keys are rejected in phase 1a instead of being silently ignored. Fast-sandbox `failure_policy` is not exposed as a new OpenSandbox extension; the backend uses FastPath v2's default `MANUAL` policy and 60-second recovery timeout.

## Integration Conditions & Feasibility

This section records, per integration area, whether fast-sandbox **today** provides what the `fleets` backend needs, and — where it does not — the concrete feasibility plan. Verdicts are based on the current fast-sandbox source, not on documentation aspiration.

### Lifecycle integration

**Verdict: READY at the FastPath v2 field/operation layer; CONDITIONAL at the OpenSandbox adapter layer.** fast-sandbox master `aac0c2c` implements the previously missing expiry, metadata, filtering, deletion, readiness, and endpoint fields. Phase 1a no longer requires those additive lifecycle fields, but it must adapt errors, pagination, extensions, and asynchronous readiness correctly.

The gRPC `FastPathService` (`CreateSandbox` / `GetSandbox` / `ListSandboxes` / `DeleteSandbox` / `UpdateSandbox` / `GetSandboxDiagnostics`) covers the `SandboxService` lifecycle surface, but the semantics differ from the pod backend:

| Method | Condition today | Plan |
| --- | --- | --- |
| Create | Idempotent by `request_id`; v2 Create includes absolute expiry + metadata in the first CRD write and returns at `RuntimeReady` | Use OpenSandbox `sandbox_id` as `request_id`; reuse the same absolute expiry on retry; wait for `DataPlaneReady` separately |
| Get | `SandboxInfo` returns metadata, expiry, image, pool, states, assignment and component data | Map the complete lifecycle response; normalize missing CRD to HTTP 404 |
| Renew-expiration | `UpdateSandbox(expires_at_unix_seconds)` persists absolute expiry; expiry becomes retained `Stopped` + `reason=Expired` | Map the retained object to `Terminated` |
| Metadata | `metadata_upsert` + `metadata_delete_keys` are implemented | Split RFC 7396 non-null/null entries directly; protect both OpenSandbox and fast-sandbox reserved keys |
| Delete | **Async** (finalizer-driven teardown); FastPath Delete itself is idempotent for NotFound | Preflight Get for the public DELETE 404 contract, then submit delete and poll Get until 404 |
| Diagnostics | `GetSandboxDiagnostics` returns **lifecycle events only, not stdout/stderr** | Back `inspect`/`events`; command output flows through execd, not this RPC |
| List | Namespace + metadata AND-filter + continue token; items include metadata/expiry | Follow all bounded pages, map/filter OpenSandbox states, then compute page/pageSize/totalItems/totalPages |

**Gaps and feasibility:**

1. **OpenSandbox list contract** — FastPath's continue-token pagination cannot directly return OpenSandbox's page number and total counts, and FastPath does not filter mapped OpenSandbox states. The phase-1 adapter follows FastPath pages to exhaustion, applies state mapping/filtering, then produces the requested page and totals. This is correct but O(total); a future indexed FastPath API may optimize it without changing the public OpenSandbox route.
2. **NotFound normalization** — At `aac0c2c`, `GetSandbox` returns the raw Kubernetes Get error instead of passing it through `grpcKubernetesError`, unlike other v2 handlers. fast-sandbox must normalize this to gRPC `codes.NotFound`; the OpenSandbox adapter must not infer NotFound from error strings.
3. **ExtensionService durability and tenant scope** — Store `access.renew.extend.seconds` under a reserved DNS-safe FastPath metadata key, remove that internal key from public metadata/list filters, and implement `get_access_renew_extend_seconds()` by reading it from FastPath. Stable routes and renew intents carry the authenticated namespace so background workers can select the same FastPath object after the request context is gone. This survives OpenSandbox restarts without server-memory ownership state.
4. **Pending must remain SDK-usable** — The Python SDK fetches both 44772 and 18080 immediately after Create and only then starts execd health polling. Therefore `get_endpoint` must not call `ResolveEndpoint` or require `DataPlaneReady`. It returns stable tenant-scoped gateway handles; the gateway lazily resolves supported upstream targets and returns retryable 503 while they are not ready. Port 18080 discovery also succeeds in phase 1a, while actual policy requests return 501.
5. **Post-persistence Create failures** — FastPath may return an error after durable intent exists. On an ambiguous Create error, the adapter reads the same namespaced sandbox ID: if found, it returns an accepted `Pending` response and lets reconciliation continue through the same lazy gateway handles; if absent, it maps the original error. It never retries with a new ID or a recomputed expiry.
6. **Sandbox logs (`get_sandbox_logs`) not implementable via execd** — `specs/execd-api.yaml` only exposes `/command/{id}/logs` for a known detached command ID; it has no endpoint for the sandbox entrypoint or an arbitrary container. The lifecycle diagnostics route has no command ID. *Plan (phase 1)*: `get_sandbox_logs` returns a clear **"unsupported on fleets"** error; `inspect`/`events` are backed by `GetSandboxDiagnostics` (lifecycle events only). A Fastlet/containerd log API or a backend extension is a future item, not a phase-1 claim.
7. **Delete/expiry are eventual, not synchronous** — `FleetSandboxService` preserves poll-for-state semantics via status mapping (`Stopped` with expired reason → `Terminated`, `Draining → Stopping`, actual NotFound → HTTP 404).

### execd Injection

**Verdict: READY, Pool configuration required.**

fast-sandbox v1alpha2 replaces the old named `infraProfile` catalog with inline, immutable `SandboxPool.spec.infraComponents[]`. The canonical sample declares a component named `execd` with:

- an OCI artifact reference pinned by `@sha256:`;
- `/execd` mapped read-only to `/.fast/components/execd/execd`;
- a supervised process on port 44772;
- readiness `GET /ping`;
- one named HTTP endpoint.

Fastlet prepares the artifact revision before admission, `sandbox-init` starts execd and the user process concurrently, and Fastlet Proxy publishes the named route only after the health check passes. The official OpenSandbox Go SDK exec/file flow is exercised by fast-sandbox's integration tests.

**What OpenSandbox must provide:**

1. Provide or reference a `SandboxPool` whose current Infra revision contains the named `execd` component.
2. Pin the production execd artifact by immutable OCI digest and configure namespace-scoped fast-sandbox Registry credentials when the artifact is private.
3. Map public OpenSandbox port 44772 to FastPath v2 `component_name = "execd"`; raw-port resolution of 44772 is deliberately rejected because component ports are reserved.

Execd is started **without** `EXECD_ACCESS_TOKEN`; neither gateway injects `X-EXECD-ACCESS-TOKEN`. Fast Sandbox protects its upstream hop with `X-Fast-Sandbox-Route-Credential`, OpenSandbox independently protects its public gateway, and application `Authorization` is preserved.

Readiness contract: `RuntimeReady` (FastPath Create returns) is distinct from `ComponentReady("execd")` and `DataPlaneReady` (all Pool-declared components ready). OpenSandbox reports Running only at `DataPlaneReady`.

### Ingress / Endpoint Access

**Verdict: FEASIBLE, but requires a target-aware, tenant-scoped, lazy-resolving ingress provider/proxy contract, not only a fleets provider.** `get_endpoint` returns the stable public gateway route without consulting FastPath readiness. The gateway calls FastPath v2 `ResolveEndpoint` only when traffic arrives.

FastPath v2 accepts a `SandboxReference` by namespace/name or UID and an `EndpointTarget` by component name or raw port. It returns the resolved protocol/port, a complete `proxy_endpoint`, `required_headers` containing `X-Fast-Sandbox-Route-Credential`, `route_generation`, and credential expiry. Component routes use `/v2/sandboxes/{uid}/components/{name}`; raw ports use `/v2/sandboxes/{uid}/ports/{port}`.

**Why the current ingress component cannot be reused unchanged**: `components/ingress` supports only BatchSandbox / AgentSandbox providers; `Provider.GetEndpoint(sandboxId)` receives no port; `EndpointInfo` carries only a host + secure-access token; and `resolveRealHost` always constructs `endpoint:port`. That contract cannot carry a scheme, path, upstream-only header, or expiry, so it cannot represent either FastPath v2 route form.

**The fleets ingress adapter (phase 1a work item)**:

- `FleetSandboxService.get_endpoint()` authenticates the current tenant and returns a **stable, backend-neutral gateway URL** containing an opaque or signed route scope bound to `(namespace, sandbox_id, port)`. It does not call `ResolveEndpoint`, wait for `DataPlaneReady`, or expose a FastPath credential. Consequently the SDK can fetch its eagerly requested 44772 and 18080 handles even when Create returned `Pending`. This integrity-protected routing scope is internal plumbing and does not replace the separate public `secure_access` authorization policy.
- The gateway verifies the route scope before lookup and passes the verified namespace, sandbox ID, and requested port to the fleets provider. Namespace must never come from an unsigned caller header or an unverified URL segment.
- Change the provider lookup to receive that full target: conceptually `ResolveEndpoint(ctx, namespace, sandboxID, port)`, not `GetEndpoint(sandboxID)`.
- Extend `EndpointInfo` to carry the complete upstream route: scheme + authority + base path, upstream-only headers, route expiry, plus the existing OpenSandbox public secure-access metadata.
- Add the **fleets provider** on top of that contract. For port 44772 it lazily requests `component_name="execd"`; for ordinary user ports it requests a raw-port target. It selects `CENTRAL_PROXY` by default, or `DIRECT_FASTLET_PROXY` only when the ingress can reach Fastlet Pod IPs and NetworkPolicy restricts that path.
- While a supported target is Pending, use a bounded `ResolveEndpoint(wait_until_ready=true)` or return HTTP 503 with `Retry-After`; this lets the existing SDK execd health loop retry without treating endpoint discovery itself as Create failure. Do not translate Pending into 404.
- Stop unconditionally building `endpoint:port`. Join the incoming suffix and query onto the resolved base path, preserve HTTP/SSE/WebSocket/file streaming, remove caller-supplied values for reserved upstream headers, and inject `X-Fast-Sandbox-Route-Credential` only on the upstream hop.
- Cache the FastPath route by `(namespace, sandbox_id, port)` only until before its expiry; refresh on expiry, reassignment, Pod failure, or a route-stale response. Do not blindly replay a non-idempotent or streaming request after refresh.
- Port 18080 still receives a stable handle in phase 1a because the SDK fetches it during construction, but the gateway returns HTTP 501 for actual policy operations until phase 1b installs the policy manager.

**Tenant-scoped renew-on-access**:

- Extend both the Go ingress renew-intent schema and the server-proxy work item with the authenticated namespace. The external ingress obtains it only from the verified stable route scope; the server proxy captures it from the authenticated tenant context before scheduling background work.
- Validate an intent namespace against the active `TenantProvider`, then bind that tenant context for the complete `get_sandbox` → `get_access_renew_extend_seconds` → `renew_expiration` sequence. Do not discover ownership by scanning every namespace for a matching sandbox ID.
- Key ingress publish throttles, consumer locks/LRU state, route caches, and other deduplication state by `(namespace, sandbox_id)`, not `sandbox_id`, so tenants with equal IDs cannot suppress or redirect each other's renewals.
- Treat missing, unknown, or mismatched namespace claims as invalid intent and do not fall back to the default namespace.

**Integration facts to design around:**

1. **No server-side UID cache is required** — FastPath v2 accepts `namespaced_name`; OpenSandbox uses its stable sandbox ID as the CRD name and supplies the tenant-resolved namespace on every lookup.
2. **Ephemeral, instance-fenced credentials** — the route credential is short-lived and fenced on the Sandbox/assignment/route identity plus the component or raw port target. The **ingress adapter** (not the SDK) re-resolves behind the stable gateway URL.
3. **Pending is a routable public handle, not a ready upstream** — creating the stable URL succeeds before route publication; only traffic resolution is readiness-dependent.
4. **HTTP only** — the transparent proxy supports HTTP/SSE/WebSocket-over-HTTP; raw TCP is not supported. (execd:44772 and egress:18080 are HTTP, so this is fine.)
5. **Application authentication is separate** — Fast Sandbox does not consume `Authorization`; OpenSandbox removes its own public secure-access proof, Fastlet Proxy removes `X-Fast-Sandbox-Route-Credential`, and the application header survives.
6. **Component ports are reserved** — a raw request for 44772 fails when the Pool declares `execd`; the adapter must resolve the logical name. Other 1–65535 ports are raw HTTP targets unless reserved by another component.

### Egress / Network Policy

**Verdict: NOT READY on current master; technically feasible for runc only after a separately reviewed phase 1b. It is not a reuse of Kubernetes NetworkPolicy.**

fast-sandbox `aac0c2c` programs only NAT `MASQUERADE` plus sibling `REJECT`. FastPath v2, the v1alpha2 `Sandbox` CRD, the Fastlet protocol, and `Slot` do **not** carry a network policy. There is also no FQDN filtering, DNS interception, nftables policy manager, or authenticated runtime policy endpoint. Therefore phase 1a rejects both create-time `network_policy` and runtime `/policy` mutation.

A viable phase-1b design has these prerequisites:

- **Policy delivery (public, additive change)**: carry `network_policy` through FastPath v2 `CreateRequest` → v1alpha2 `SandboxSpec` → Fastlet protocol `SandboxSpec` → the bound `Slot`. Because this changes a public proto and CRD, it requires explicit fast-sandbox maintainer review and generated-output updates.
- **Bind-time enforcement**: slots are pre-warmed before an owner or policy is known, so policy cannot be installed in `Prepare()`. Add a fail-closed `ApplyPolicy` step after `Acquire` has bound the owner and before the runtime network is released to the workload. A partial install must roll back the slot or make it unavailable.
- **Per-netns enforcement**: for runc, install default-drop OUTPUT rules, CIDR rules, and DNS-learned allow sets inside the slot netns. Netns deletion naturally removes in-netns rules; any host-side DNS or set state must be keyed by assignment identity and reaped by NodeJanitor.
- **DNS mediation**: each slot currently copies the host `resolv.conf`. FQDN policy requires a new per-sandbox DNS listener and a sandbox `resolv.conf` that points to it, with bounded TTL and stale-entry behavior defined.
- **Runtime restriction**: the current network driver provides sufficient evidence only for runc. Kata moves the interface into a guest network stack, and gVisor behavior needs separate validation. Phase 1b must reject policy on unsupported runtimes.
- **Authenticated runtime mutation**: an unprivileged inline Infra Component cannot mutate its host netns merely by listening on port 18080. The preferred design is a Fastlet-owned policy manager with an authenticated, assignment-fenced per-sandbox facade routed through the gateway. Its authorization, optimistic concurrency, PATCH/DELETE semantics, restart recovery, and cleanup must be specified before exposing OpenSandbox's `/policy` API. A privileged in-sandbox component is not assumed by this OSEP.

Until that control contract exists, the ingress adapter resolves execd and ordinary user ports normally. It still issues the SDK-compatible stable handle for port 18080, but actual phase-1a policy requests through that handle return HTTP 501. Phase 1b replaces that response with the authenticated policy-manager route.

## Construction Phases

Phase 1a is the initial `fleets` release. Phase 1b is a separately gated cross-repository follow-up and is not implied by phase-1a acceptance. In phase 1a, `network_policy` and `secure_access` are rejected rather than silently ignored.

### Phase 1a — Service seam, lifecycle, execd, ingress

- Add `FleetSandboxService(SandboxService, ExtensionService)`, register `"fleets"` in `factory.py`, and add `FleetsRuntimeConfig`.
- Map `fleets` to `NoopSnapshotRuntime` during server startup; snapshot, pause, and resume operations remain explicitly unsupported.
- Implement a FastPath v2 client for Create / Get / Delete / Update / List / Diagnostics / WaitSandboxReady / ResolveEndpoint and Pool discovery. No new lifecycle request or response fields are required on current fast-sandbox master.
- Map Create atomically: public `extensions["poolRef"]`, image, command/args, string-valued environment, absolute expiry, public metadata, and the reserved renew-on-access value all go into the first Create request. Preserve the same request ID, normalized intent, and absolute expiry across retries.
- On an ambiguous Create error, Get the same namespaced sandbox ID; return accepted `Pending` when durable intent exists, otherwise map the original error. Its stable 44772/18080 gateway handles remain discoverable while Pending; do not delete valid durable intent or retry under a new ID.
- Reject `volumes` / `platform` / `resource_requests` / `credential_proxy` / `snapshot_id` / `image.auth` / null environment values, and reject `network_policy` / `secure_access` until phase 1b. No unsupported field is silently ignored.
- **Tenant handling**: map each OpenSandbox tenant to a fast-sandbox namespace on every namespaced call. Bind `(namespace, sandbox_id, port)` into an authenticated stable route; propagate namespace through renew intents; validate and restore tenant context in background renewal; use composite namespace/ID keys for locks, throttles, and caches. Do not scan namespaces by sandbox ID. Reject `[tenants]` configuration for fleets if any part of this path is not implemented.
- **Ingress contract and fleets provider**: `get_endpoint` returns the stable tenant-scoped route without FastPath readiness. On traffic, extend provider lookup and proxying to consume namespace + requested port, the complete upstream URL/base path, upstream-only headers, and expiry. Map 44772 to named component `execd`; use raw-port targets for ordinary user ports; preserve application `Authorization`; inject only `X-Fast-Sandbox-Route-Credential` upstream. Return retryable 503 while supported targets are Pending and phase-1a HTTP 501 for actual port-18080 policy calls. No sandbox-ID-to-UID store is required.
- **Execd**: require an inline v1alpha2 `infraComponents[]` entry named `execd`, pinned by OCI digest, with the documented binary mapping, process, health check, and named endpoint. Verify exec/file end to end through the SDK.
- **Lifecycle semantics**: implement get/delete/renew/list/metadata; keep reserved metadata private; exhaust FastPath pages before state filtering and OpenSandbox page/total calculation; preflight Delete to preserve public 404; map retained `Stopped/Expired` to `Terminated`, `Draining` to `Stopping`, and actual NotFound to HTTP 404.
- Normalize fast-sandbox `GetSandbox` Kubernetes NotFound to gRPC `codes.NotFound`; do not string-match backend errors in OpenSandbox.
- Return a clear unsupported error for sandbox logs; back `inspect`/events only with lifecycle diagnostics.
- **Exit criteria**: server starts under `fleets`; full SDK flow (create → exec → file → delete) passes on a Kind cluster without SDK changes, including a Create that initially returns Pending; tenant-scoped background renewal, equal IDs in different namespaces, list pagination/totals, extensions, unsupported-field rejection, and actual-NotFound behavior are verified.

### Phase 1b — Per-sandbox egress enforcement + secure access (separately gated)

- Additive `network_policy` field on fast-sandbox `CreateRequest` (proto), `Sandbox` CRD, Fastlet protocol `SandboxSpec`, and `Slot` (ask-first review with fast-sandbox maintainers).
- New "apply on bind" driver step invoked from `Acquire` (resolves the pre-warm timing blocker); port `components/egress` nft + DNS logic into the per-slot netns.
- Add the Fastlet-owned, assignment-fenced policy manager and authenticated per-sandbox `/policy` facade on port 18080; validate its GET/PATCH/DELETE behavior against `egress-api.yaml`.
- Restrict `network_policy` to the `container` (runc) runtime; reject egress on gVisor/Kata.
- Extend NodeJanitor for any out-of-netns state (DNS proxy / ipset).
- Flip Create to accept `network_policy` only after enforcement, restart recovery, mutation authorization, and cleanup tests pass.
- **Secure access**: accept `secure_access`; layer server-issued access headers onto the ingress-gateway route, reusing fast-sandbox's `required_headers` / short-lived Ed25519 credential model (OSEP-0011 semantics).
- **Exit criteria**: a fleets sandbox with a deny-by-default FQDN allowlist blocks non-allowed egress and permits allowed FQDNs, end-to-end; co-located sandboxes with different policies do not interfere; a `secure_access` sandbox returns access headers and rejects unauthenticated endpoint access.

## Test Plan

- **Unit Tests**: FastPath v2 wrapper; atomic Create mapping and idempotent retry; ambiguous post-persistence recovery; Pending-safe stable endpoint discovery; lazy ResolveEndpoint 503/refresh behavior; phase-1a 18080 handle/501 behavior; authenticated namespace propagation; namespace+ID lock/throttle/cache keys; `ExtensionService` persistence/filtering; status/error mapping; state filtering and page/total calculation
- **Startup Tests**: `fleets` satisfies the unconditional `ExtensionService` requirement and selects `NoopSnapshotRuntime`; server startup succeeds while snapshot operations remain unsupported
- **Integration Tests**: Deploy fast-sandbox in a Kind cluster; test create/get/delete/renew/list/metadata flows, actual NotFound, retained expiry, multi-page list behavior, Pending endpoint discovery followed by named execd readiness, and tenant-scoped background renewal
- **E2E Tests**: Full OpenSandbox SDK flow using the `fleets` runtime, asserting behavior identical to the pod backend for lifecycle + exec/file
- **Egress Tests (phase 1b)**: fail-closed bind, deny-by-default block, FQDN allowlist permit/expiry, per-sandbox isolation between co-located sandboxes, authenticated and assignment-fenced `/policy` GET/PATCH/DELETE, restart recovery/cleanup, and unsupported-runtime rejection
- **Performance Tests**: create latency and density vs the `kubernetes` pool backend, reported per fast-sandbox methodology (commit/env/runtime/cache-state/concurrency/percentiles); compare the user-visible `Running` + execd-usable milestone separately from fast-sandbox's internal `RuntimeReady`

### Test Scenarios

1. Basic lifecycle: create → status query → delete (delete is async; poll for NotFound)
2. NotFound distinction: an unknown ID returns HTTP 404; an expired but retained CRD returns `Terminated`
3. Expiration: initial absolute expiry is present in the first Create; renewal is eventual; an expired sandbox reaches `Terminated`
4. Idempotency: a retry reuses request ID, normalized intent, and absolute expiry; a changed intent conflicts
5. Ambiguous Create failure: when the first CRD write succeeded, the adapter reads the same ID and returns accepted `Pending`; SDK retrieval of stable 44772 and 18080 handles succeeds, execd health polling tolerates lazy-route 503 until ready, and no duplicate creation or premature deletion occurs
6. Simplified Create: `volumes` / `platform` / `resource_requests` / `credential_proxy` / `snapshot_id` / `image.auth` and null environment values are rejected; `network_policy` / `secure_access` are rejected in 1a and accepted only after 1b exits
7. Tenant isolation: a tenant cannot see/operate another tenant's sandboxes via list, ID, stable route, or renew intent. Two namespaces with the same sandbox ID retain independent route caches, publish throttles, locks, extension reads, and renewals; missing/forged namespace claims are rejected without default-namespace fallback
8. Metadata PATCH with a `null` value deletes the key through `metadata_delete_keys`, preserving reserved keys; get/list return public metadata after a server restart
9. Extensions: pool selection uses public `extensions["poolRef"]`; renew-on-access is durable through `ExtensionService`; a background renew intent restores its authenticated tenant namespace for get/extension/renew; internal metadata is hidden from get/list/filter results
10. List compatibility: FastPath continue-token pages are exhausted, mapped state filters are applied, and page/pageSize/totalItems/totalPages/hasNextPage match the public contract
11. Ingress adapter: endpoint discovery performs no readiness-dependent FastPath call; SDK reaches named execd and raw user ports through the stable tenant-scoped gateway URL after Pending; base paths and queries are preserved; the route credential rotates internally; application `Authorization` survives; phase-1a port 18080 discovery succeeds while policy traffic returns 501
12. Server startup: fleets selects `NoopSnapshotRuntime`, implements `ExtensionService`, and returns clear unsupported errors for logs/pause/resume/snapshot
13. Image affinity: record candidate/cache-hit state for repeated creates and report it separately from end-to-end latency
14. Failure: FastPath unavailable and invalid `poolRef`
15. Concurrent sandbox creation (stress test)
16. Egress (phase 1b): fail-closed bind, deny-by-default, allowed FQDN, policy mutation fencing, restart cleanup, and co-located policy isolation

### Performance Benchmarks

The figures below are the current fast-sandbox engineering evidence, not `fleets` targets:

| Scope | Observation | Measurement boundary |
| --- | --- | --- |
| Warm container (`runc`) | 20 samples: mean 76.02 ms, p50 75.95 ms, p95 83.15 ms | Concurrency 1; cached image/artifacts; minimal Infra Component revision; client FastPath Create through `RuntimeReady` |
| gVisor (`runsc`) | 10 samples: mean 644.29 ms | Small diagnostic batch; cached artifacts; Execd readiness excluded |
| Kata Cloud Hypervisor | 10 samples: mean 1,359.59 ms | Small diagnostic batch under nested KVM; Execd readiness excluded |
| Kata QEMU | 10 samples: mean 2,125.58 ms | Small diagnostic batch under nested KVM; Execd readiness excluded |
| BatchSandbox pool vs fleets | No comparable report yet | Must run both through the same OpenSandbox endpoint, environment, cache state, concurrency, and readiness boundary |
| Registry Top-K | No portable latency claim | `BenchmarkRegistryTopK1000` is a scheduler microbenchmark, not Sandbox Create |

Source: fast-sandbox [`aac0c2c` performance guide](https://github.com/opensandbox-group/fast-sandbox/blob/aac0c2c80f08a8efa455f057ff7323a8248b558d/docs/guides/performance.md), whose dated measurements still describe base revision `42fe03549598c3ab730b989c7757634b486697cf`. The current guide explicitly does not claim a release-grade OpenSandbox end-to-end result.

There is no absolute-millisecond exit threshold. The performance goal passes only when the matched OpenSandbox benchmark shows lower fleets p50 and p95 from SDK create start through `Running` and an execd readiness probe than the BatchSandbox pool. fast-sandbox `RuntimeReady` is reported as a separate diagnostic milestone and is not substituted for that user-visible comparison.

The `fleets` acceptance report must record commit SHA and command; hardware, virtualization, Kubernetes, and containerd versions; component replicas; runtime and Infra Component revision; image/cache/network-slot state; concurrency and request rate; start/end milestones; p50/p95/p99/max; failures, admission rejections, and retries. It must report `RuntimeReady`, `DataPlaneReady`, and OpenSandbox client-observed latency separately. Image-affinity and Top-K microbenchmarks remain supporting diagnostics and must not substitute for the end-to-end comparison.

## Drawbacks

- **Added Dependency**: Requires deploying and managing the fast-sandbox control plane (Fast-Path Servers, Reconcilers, Sandbox Proxy), Fastlet pools, and NodeJanitor DaemonSet
- **Feature gap vs. pod backend**: no volumes, no pause/resume/snapshot, no per-sandbox K8s NetworkPolicy, no per-sandbox node scheduling — fleets is a deliberate subset
- **Bespoke egress layer (phase 1b)**: per-sandbox egress is a new network layer with a cross-repo proto/CRD change — the highest-cost part of the integration; gVisor/Kata + egress remains unsupported in phase 1b unless separately proven
- **Operational Complexity**: Teams need to understand both OpenSandbox and fast-sandbox concepts
- **gRPC Protocol**: Introduces gRPC on the server's backend surface (vs pure HTTP/REST)
- **Limited Ecosystem**: fast-sandbox is a newer project with a smaller community than vanilla K8s

## Alternatives

1. **Full replacement of the pod backend**: Rejected — the pod backend's volumes, snapshots, and per-sandbox K8s NetworkPolicy have no equivalent in the shared-Fastlet model
2. **Implement fleets as a K8s `WorkloadProvider`**: Rejected — that ABC is Kubernetes-internal (namespace/CR/pod-spec) and cannot host a separate gRPC control plane
3. **Only the declarative fast-sandbox CRD path (no gRPC)**: Rejected — loses the in-memory-placement latency benefit that motivates fleets
4. **Reuse Kubernetes NetworkPolicy for egress**: Rejected — all sandboxes SNAT to one Fastlet pod IP, so K8s NetworkPolicy cannot distinguish sandboxes
5. **Direct `FastletPodIP:port` endpoints**: Deferred — workable but requires port allocation + auth handling in OpenSandbox; the ingress gateway is backend-neutral and reuses fast-sandbox's existing authenticated proxy chain

## Infrastructure Needed

- **CI/CD**: Kind cluster with fast-sandbox (Fast-Path, Reconcilers, Sandbox Proxy, a Fastlet pool, NodeJanitor) for integration/e2e
- **Documentation**: fleets deployment guide; execd-as-Infra-Component setup; egress enforcement guide (phase 1b); compatibility matrix
- **Helm Charts** (optional): Unified charts deploying OpenSandbox Server + fast-sandbox components
- **Cross-repo coordination**: with fast-sandbox maintainers for `GetSandbox` NotFound normalization and, separately, the phase-1b `network_policy` proto/CRD/Fastlet contract

## Upgrade & Migration Strategy

- **Backwards Compatible**: Default runtime unchanged; `fleets` is opt-in via `config.runtime.type = "fleets"`
- **No Migration**: Existing Docker/Kubernetes runtime users unaffected
- **Enable by Config**: Set `type = "fleets"` and add the `[fleets]` block; deploy fast-sandbox and an ingress gateway wired to its Sandbox Proxy
- **Rollback**: Switch `type` back to `kubernetes` or `docker`; fleets sandboxes are ephemeral (no persistent state to migrate)
- **Choosing a backend**: use `kubernetes` when you need volumes, pause/resume/snapshot, per-sandbox K8s NetworkPolicy, or per-sandbox node scheduling; use phase-1a `fleets` for latency-sensitive, stateless, high-density workloads needing lifecycle + exec/file. Do not select `fleets` for FQDN egress until phase 1b is implemented and enabled.
