---
title: Spec-Driven Pod Snapshot for Pause and Resume
authors:
  - "@ryanzhang-oss"
creation-date: 2026-06-27
last-updated: 2026-06-27
status: draft
---

# OSEP-0015: Spec-Driven Pod Snapshot for Pause and Resume

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Relationship to OSEP-0008](#relationship-to-osep-0008)
  - [Personas and ownership](#personas-and-ownership)
  - [Public API Overview](#public-api-overview)
  - [Kubernetes Resource Overview](#kubernetes-resource-overview)
  - [Example CRs](#example-crs)
  - [Operating modes](#operating-modes)
  - [Seamless restore contract](#seamless-restore-contract)
  - [Component Interaction Overview](#component-interaction-overview)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. BatchSandbox spec field](#1-batchsandbox-spec-field)
  - [2. SandboxSnapshotClass, SandboxSnapshotClaim, and SandboxSnapshotClaimTemplate CRDs](#2-sandboxsnapshotclass-sandboxsnapshotclaim-and-sandboxsnapshotclaimtemplate-crds)
  - [3. Snapshot result recording on `SandboxSnapshot` + inline reference](#3-snapshot-result-recording-on-sandboxsnapshot--inline-reference)
  - [4. Pause state model](#4-pause-state-model)
  - [5. Pause flow](#5-pause-flow)
  - [6. Snapshot Job](#6-snapshot-job)
  - [7. Resume flow](#7-resume-flow)
  - [8. Server mapping and public API compatibility](#8-server-mapping-and-public-api-compatibility)
  - [9. Stable sandbox ID, list, get, delete](#9-stable-sandbox-id-list-get-delete)
  - [10. Configuration](#10-configuration)
  - [11. Security considerations](#11-security-considerations)
  - [12. Potential Go type changes](#12-potential-go-type-changes)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal reworks how Kubernetes-backed pause and resume are expressed inside
the cluster. **The primary change is to inline the pause/resume _action_ onto the
`BatchSandbox` reconciler instead of dispatching a separate "action" custom
resource.** In OSEP-0008 a pause is carried out by creating a dedicated
`SandboxSnapshot` CR that a separate `SandboxSnapshotReconciler` consumes to drive
the snapshot Job. Here the action lives directly on the `BatchSandbox` object: the
existing boolean pause trigger is replaced by `BatchSandbox.spec.operatingMode`,
an enum that carries the requested persistent mode (`Running`, `Freeze`,
`KeepFS`, `Hibernate`), and the `BatchSandbox` reconciler executes the whole
pause/resume inline. `KeepFS` is the implemented filesystem-only mode inherited
from OSEP-0008: it saves the writable filesystems, does not save process memory,
and restarts processes on resume. `Hibernate` is the future full-state mode: it
saves both filesystem and memory/process state, but is not implemented in this
revision. Snapshot-producing modes use `BatchSandbox.spec.snapshotStrategy`,
which references a concrete namespaced `SandboxSnapshotClaim`, or a namespaced
`SandboxSnapshotClaimTemplate` that the server copies into a generated,
sandbox-specific `SandboxSnapshotClaim`. A concrete claim is shared by every
sandbox/pod that references it directly; a template is not shared as the claim,
but is a factory for distinct generated claims, one per sandbox/pod. The final claim resolves to a cluster-scoped
`SandboxSnapshotClass`, following the `PersistentVolumeClaim` -> `StorageClass`
and DRA `ResourceClaimTemplate` patterns.

The `SandboxSnapshot` CR is **not removed** — it is demoted from an *action*
object to a *result recording*: the reconciler writes one `SandboxSnapshot` per
pause to record where the checkpoint artifact landed plus source compatibility,
and `BatchSandbox.status.snapshot` keeps a lightweight reference to it. Where that
result is recorded is a secondary detail; the headline is that the action no
longer requires a separate CR and its own reconciler.

This design follows the upstream
[kubernetes-sigs/agent-sandbox KEP-0694 proposal](https://github.com/kubernetes-sigs/agent-sandbox/pull/762)
("Suspend and Resume + Snapshot Provider API"). At the time of writing, that PR
is still open and the agent-sandbox default-branch CRDs do not yet include the
snapshot strategy fields. The current agent-sandbox API uses
`spec.operatingMode` (`Running`/`Suspended`) instead of a boolean lifecycle
field; this OSEP follows that enum pattern and folds OpenSandbox's pause operation
into the same enum so there is only one lifecycle/operation selector. It also adds
a `StorageClass`-style `SandboxSnapshotClass` for pluggable snapshot backends.
OpenSandbox adds a namespaced `SandboxSnapshotClaim`/`SandboxSnapshotClaimTemplate` layer so
tenant-owned storage locations are unambiguous. A template is a real namespaced
CRD, analogous to DRA `ResourceClaimTemplate`: the server copies it into a
distinct concrete claim for each sandbox/pod that uses it. Backend credentials are
never passed through the API or stored in Kubernetes Secrets; they are supplied
by node/workload identity (e.g., cloud instance roles or workload identity).

This OSEP extends the public `POST /sandboxes/{sandboxId}/pause` request body in
an additive, backward-compatible way. Existing clients may continue sending no
body; the server supplies a default operating mode and default snapshot claim
selection. Callers that need control can optionally select a pause-capable
operating mode (`KeepFS`, `Freeze`, `Hibernate`) and, for snapshot-producing  
modes, either an
existing `SandboxSnapshotClaim`, a `SandboxSnapshotClaimTemplate` plus optional
per-use parameters, or inline claim parameters that the server materializes into
a generated `SandboxSnapshotClaim`. `POST
/sandboxes/{sandboxId}/resume` remains driven by the snapshot result recorded at
pause time; it does not need new required parameters in this revision.

```text
Time ------------------------------------------------------------------------>

Sandbox lifecycle:   [Running]--[Pausing]--[Paused]--[Resuming]--[Running]
                          |                     |
                operatingMode=KeepFS     operatingMode=Running
                + snapshot               (controller recreates Pod
                (controller snapshots     from status.snapshot artifact)
                 writable filesystems)
```

## Motivation

OSEP-0008 delivered pause/resume by committing a sandbox Pod's writable
filesystem to an OCI image. It models the operation as a dedicated `SandboxSnapshot` CR that
the server creates, a `SandboxSnapshotReconciler` consumes, and that drives a
same-node commit Job. The snapshot backend is configured globally through
controller manager startup flags such as registry, credential, and committer
image settings.

That shape works, but it has structural costs:

- **The pause action is a separate CR and reconcile loop.** A pause is expressed
  as a `SandboxSnapshot` *action* object that the server creates and a dedicated
  `SandboxSnapshotReconciler` consumes. That reconciler is a separate atomic
  capability that resolves Pod/Node from the referenced `BatchSandbox`, races with
  scale and eviction, and needs its own finalizer, ownership, and garbage
  collection; the sandbox's own reconciler cannot see or sequence the action
  directly. **Inlining this action is the primary change in this OSEP.**
- **Two objects to keep consistent for one operation.** A paused sandbox's
  lifecycle state is split between `BatchSandbox.spec.pause` and the sibling action
  object, so the server must merge both and keep them consistent on pause, resume,
  and delete.
- **Global, non-pluggable snapshot configuration.** The registry and push
  credentials are cluster-wide controller flags. Operators cannot offer multiple
  snapshot backends (e.g., a fast local registry vs. a remote durable bucket), and
  the choice cannot vary per sandbox.
- **Divergence from the upstream agent-sandbox proposal.** OpenSandbox tracks the
  Kubernetes SIG `agent-sandbox` project (OSEP-0002). The current agent-sandbox
  API uses `spec.operatingMode` instead of a boolean lifecycle field, and PR #762
  layers `spec.suspensionStrategy` + `SandboxSnapshotClass` on top. Aligning now reduces
  future migration cost and lets OpenSandbox adopt additional strategies (Freeze,
  VM/memory checkpoint) under one API if that direction lands.

Making pause/resume part of the `BatchSandbox` reconciler's own desired-state loop
is the idiomatic Kubernetes pattern. It collapses the separate-action-CR model
into one reconciler driven by `spec.operatingMode`, retains `SandboxSnapshot` only
as a per-pause result recording, and makes the backend pluggable through
DRA/PV-style class and claim resources. The public pause API gains optional
operating-mode and claim-selection fields, while empty-body pause requests keep
working through defaults.

### Goals

- **Inline the pause/resume action on the `BatchSandbox` reconciler**, driven by
  `spec.operatingMode` (desired operation enum) with `spec.snapshotStrategy` as
  the backend selector for snapshot-producing modes — removing the separate
  `SandboxSnapshot` *action* CR and its dedicated reconciler. Record the per-pause *result* on a reused
  `SandboxSnapshot` object referenced from `status.snapshot`.
- Introduce a cluster-scoped `SandboxSnapshotClass` CRD plus namespaced
  `SandboxSnapshotClaim` and `SandboxSnapshotClaimTemplate` CRDs, following the Kubernetes DRA
  `DeviceClass`/`ResourceClaim`/`ResourceClaimTemplate` pattern:
  classes carry high-level backend direction, claims carry namespace-local target
  parameters, and templates generate one concrete claim per sandbox/pod.
- Extend the public `POST /sandboxes/{id}/pause` API with an optional typed
  operating mode and snapshot claim selector. Existing empty-body calls remain valid by
  using server/operator defaults. Keep `POST /sandboxes/{id}/resume` simple by
  using the claim and artifact recorded in `BatchSandbox.status.snapshot`.
- Keep the public `sandboxId` stable across pause and resume.
- Preserve the same-node snapshot Job pattern from OSEP-0008; only its *driver*
  changes from a separate `SandboxSnapshot` *action* CR to the `BatchSandbox`
  reconciler reading `spec.operatingMode`. This revision keeps the OSEP-0008
  filesystem-only image commit behavior as `KeepFS`.
- Define `Hibernate` as the future full pod-state mode that captures all
  container filesystems plus in-memory process state, so a resumed sandbox can
  restore running processes, not just files. `Hibernate` is not implemented in
  this revision.
- Leave the API extensible for future strategies and backends without breaking
  changes.

### Non-Goals

- Recovering or rolling back from any snapshot other than the latest. Per-pause
  `SandboxSnapshot` objects are retained as history (useful for audit), but resume
  always restores from the most recent result.
- Implementing automatic, idle-timeout-driven suspension (policy-based triggers
  are future work, consistent with the upstream Phase 2).
- Redesigning the explicit "create snapshot" public API (the `osb-snap-*` flow).
  This OSEP only replaces the *pause/resume* usage of `SandboxSnapshot`.
- Extending the Docker runtime; this proposal targets the Kubernetes runtime.
- Pause/resume for pool-backed sandboxes (created through `extensions.poolRef`,
  where the `BatchSandbox` carries `spec.poolRef` and no `spec.template`). Those
  require solidifying the allocated pod template before the pool pod is released,
  which is coupled to warm-pool allocation and reclaim. This OSEP assumes a
  self-contained `BatchSandbox` with its own `spec.template`; pool-backed
  lifecycle is deferred to a separate warm-pool sandbox lifecycle OSEP.

## Requirements

- `POST /sandboxes/{sandboxId}/pause` must accept enough optional information to
  choose the operating mode and, for snapshot-producing modes, the snapshot
  backend/claim to use. A request with no body must remain valid and resolve
  through defaults.
- `POST /sandboxes/{sandboxId}/resume` must resume from the latest successful
  pause result without requiring the caller to repeat pause-time backend
  parameters.
- The public `sandboxId` must remain stable across pause and resume.
- For `KeepFS`, the snapshot must capture each container writable filesystem and
  push it to a backend resolved from `SandboxSnapshotClaim -> SandboxSnapshotClass`.
  `KeepFS` deletes the Pod after the filesystem snapshot is durable; memory and
  running processes are not preserved.
- `Hibernate` is specified as the future full-state mode: it must capture every
  container filesystem plus in-memory/process state before deleting the Pod, but
  it is not implemented in this revision.
- Basic lifecycle state must be readable from the `BatchSandbox` object alone: the
  pause/resume action, aggregate `status.phase`, and the `status.snapshot`
  reference require no sibling CR lookup to answer `GET /sandboxes/{id}`. Full
  per-container artifact detail lives on the referenced `SandboxSnapshot` result.
- For `KeepFS`, pause must complete the filesystem snapshot and record the artifact
  before the sandbox is reported `Paused` and the Pod is removed.
- At most one pause/resume transition may be in progress for a given sandbox.
- Resume from `KeepFS` must reconstruct the Pod from the current filesystem
  snapshot and `spec.template`; processes restart from the image entrypoint.
  Future `Hibernate` resume restores the recorded full-pod-state snapshot.
- For checkpoint restores, the controller must verify runtime compatibility
  (runtime handler, OS/architecture, kernel/runtime support, and artifact format)
  before starting restore; incompatible restores fail closed with `ResumeFailed`.
- OpenSandbox must keep `sandboxId` and endpoint identities stable across resume,
  but Kubernetes Pod UID/IP and external TCP sessions are not guaranteed to
  survive `KeepFS` or `Hibernate`.
- Backend credentials must never be passed through the public API or stored in
  Kubernetes Secrets; they are provided by node/workload identity in the runtime
  environment.
- The snapshot backend must be selectable per pause via a named `SandboxSnapshotClaim`,
  a `SandboxSnapshotClaimTemplate` reference with optional per-use parameters, or
  inline claim parameters that the server turns into a generated `SandboxSnapshotClaim`. The resolved claim selects a
  `SandboxSnapshotClass`, and a cluster default class must be supported when the claim
  does not specify one.
- The design must remain compatible with the current server path where a
  Kubernetes sandbox maps to a `BatchSandbox` with `replicas = 1`.
- The change must be additive to CRD schemas and must not break clusters that have
  not yet installed the `SandboxSnapshotClass`/`SandboxSnapshotClaim` CRDs. Until those CRDs are
  present, the server must detect their absence and fall back to the OSEP-0008
  `SandboxSnapshot` pause/resume path so existing empty-body `/pause` calls keep
  working unchanged. CRD installation must not be required as a hard preflight
  before the new server/controller can run.

## Proposal

Pause and resume are modeled on one runtime object plus PV/PVC-style
configuration objects:

- `BatchSandbox`: carries the desired persistent mode (`spec.operatingMode`), the
  snapshot config for `KeepFS` and future `Hibernate` (`spec.snapshotStrategy`), and the
  inline snapshot result (`status.snapshot`).
- `SandboxSnapshotClass` (new, cluster-scoped): describes a pluggable snapshot backend
  (type + parameters).
- `SandboxSnapshotClaim` (new, namespaced): selects a `SandboxSnapshotClass` and carries the
  namespace-local storage target for snapshot artifacts.

The server remains the orchestrator of the public lifecycle. The `BatchSandbox`
reconciler owns the in-cluster execution: when it observes
`spec.operatingMode = KeepFS`, it resolves the live Pod/Node, dispatches a
same-node snapshot Job using `spec.snapshotStrategy`, records the resulting
filesystem artifact in `status.snapshot`, and deletes the Pod after the artifact
is durable. Future `Hibernate` uses the same control shape but requires a
checkpoint/restore implementation that persists memory/process state as well.
Resume is the reverse, driven by `spec.operatingMode = Running`.

### Relationship to OSEP-0008

OSEP-0008 is the predecessor. This OSEP keeps its goals (stable `sandboxId`,
state persistence, resource release after pause, same-node snapshot Job) and
changes the in-cluster representation. Filesystem-only pause/resume remains the
implemented behavior under `KeepFS`; `Hibernate` names the future full-state
extension.

| Concern | OSEP-0008 (current) | OSEP-0015 (this proposal) |
|---|---|---|
| Pause trigger | `BatchSandbox.spec.pause` boolean | `BatchSandbox.spec.operatingMode` enum (`Running`/`Freeze`/`KeepFS`/`Hibernate`) |
| Snapshot intent/policy | `SandboxSnapshot` CR (`spec.sandboxName`) | `BatchSandbox.spec.operatingMode` + `BatchSandbox.spec.snapshotStrategy` |
| Snapshot result | `SandboxSnapshot.status` | `BatchSandbox.status.snapshot` |
| Backend config | controller startup flags (global) | `SandboxSnapshotClaim` (namespaced) -> `SandboxSnapshotClass` (cluster-scoped, pluggable) |
| Reconciler | dedicated `SandboxSnapshotReconciler` | `BatchSandboxReconciler` (inline) |
| Sources of truth for `GET` | `BatchSandbox` + `SandboxSnapshot` | `BatchSandbox` only |
| Snapshot Job | same-node image-committer Job | same-node image-committer Job for `KeepFS`; future checkpoint/restore Job for `Hibernate` |
| Public server API | `/pause`, `/resume`, `GET` | same endpoints; `/pause` gains an optional backward-compatible body |

This is the OpenSandbox counterpart of agent-sandbox KEP-0694. The desired-state
enum follows the current agent-sandbox `spec.operatingMode` shape, while the
backend selector adds a namespaced claim layer for OpenSandbox's multi-namespace
storage ownership model:

| agent-sandbox `Sandbox` | OpenSandbox `BatchSandbox` |
|---|---|
| `spec.operatingMode: Running\|Suspended` | `spec.operatingMode: Running\|Freeze\|KeepFS\|Hibernate` |
| `spec.suspensionStrategy.type` | folded into `spec.operatingMode` |
| `spec.suspensionStrategy.hibernate.snapshotClass` | `spec.snapshotStrategy.snapshotClaimName` |
| `SandboxSnapshotClass` (cluster-scoped) | `SandboxSnapshotClaim` (namespaced) -> `SandboxSnapshotClass` (cluster-scoped) |

OpenSandbox keeps its existing `pause`/`resume` vocabulary instead of upstream's
`suspend`/`resume`, but uses an enum rather than a boolean for the CRD field.
Because OpenSandbox may also adopt future `Freeze`, it uses `operatingMode` as the
single persistent-mode selector instead of adding a second operation/strategy
enum. A checkpoint-only snapshot is action-shaped rather than
persistent-mode-shaped and is left to a future OSEP. The backend selector is adapted to OpenSandbox's
multi-namespace storage targeting needs by inserting a `SandboxSnapshotClaim` layer
between `BatchSandbox` and `SandboxSnapshotClass`, analogous to PVCs sitting between
Pods and `StorageClass`.

### Personas and ownership

This mirrors Kubernetes DRA's split between device owners, cluster admins, and
workload operators:

| Persona | Owns | Responsibilities |
|---|---|---|
| System / cluster admin | `SandboxSnapshotClass`, snapshot controller installation, snapshotter image, cluster RBAC | Defines available backend classes such as `local`, `blobstore`, and `s3`; sets cluster defaults; controls privileged infrastructure, backend endpoints, and the node/workload identity used to reach them. |
| Tenant / namespace operator | `SandboxSnapshotClaim`, `SandboxSnapshotClaimTemplate` | Chooses shared namespace storage claims (bucket/container/region/prefix) and optionally offers templates that generate one concrete claim per sandbox/pod. Backend credentials come from runtime identity, not from objects they create. |
| Application developer / API caller | `POST /pause` request fields | Usually sends no body and relies on defaults. Advanced callers select `KeepFS`, choose a shared claim or a per-sandbox/pod template for snapshot-producing modes, or provide an inline claim spec when policy allows it. |
| OpenSandbox server | Public API validation and materialization | Validates request shape, applies defaults, reuses shared `SandboxSnapshotClaim` objects by name, creates one generated `SandboxSnapshotClaim` per sandbox/pod from templates/inline parameters, and patches `BatchSandbox.spec.operatingMode` plus `BatchSandbox.spec.snapshotStrategy.snapshotClaimName`. |
| BatchSandbox controller | In-cluster execution | Resolves the referenced claim/class, runs the snapshot Job, records `status.snapshot`, and recreates/restores the Pod on resume. |

`SandboxSnapshotClass` should not be created by tenants or end users. It is the
admin-owned catalog of supported snapshot backends. `SandboxSnapshotClaim` and
`SandboxSnapshotClaimTemplate` are the namespace boundary where tenant-specific storage
locations live.

### Public API Overview

```text
POST /sandboxes/{sandboxId}/pause   -> optional body: operating mode + snapshot claim selector; return 202
POST /sandboxes/{sandboxId}/resume  -> patch BatchSandbox (operatingMode=Running), return 202
GET  /sandboxes/{sandboxId}         -> Running / Pausing / Paused / Resuming (from BatchSandbox)
```

There is no new public endpoint. The current OpenAPI definition in
`specs/sandbox-lifecycle.yml` defines `POST /sandboxes/{sandboxId}/pause` and
`POST /sandboxes/{sandboxId}/resume` without request bodies, and the FastAPI
routes in `server/opensandbox_server/api/lifecycle.py` currently accept only the
path parameter and `X-Request-ID`. This OSEP updates those definitions to accept
an optional `PauseSandboxRequest` body. Omitting the body, sending `{}`, or using
old SDKs remains valid:

```json
{}
```

The server resolves that to configured defaults, for example
`operatingMode = "KeepFS"` plus the namespace default `SandboxSnapshotClaim`. New
clients can opt in to explicit control:

```json
{
  "operatingMode": "KeepFS",
  "snapshotStrategy": {
    "snapshotClaimName": "default-claim"
  }
}
```

or ask the server to copy a template into an individual generated claim:

```json
{
  "operatingMode": "KeepFS",
  "snapshotStrategy": {
    "snapshotClaimTemplate": {
      "name": "tenant-s3",
      "parameters": {
        "bucket": "tenant-a-sandbox-abc123"
      }
    }
  }
}
```

Claim and template selection are intentionally different contracts:

- Reference `snapshotStrategy.snapshotClaimName` when a concrete
  `SandboxSnapshotClaim` already exists and can be reused. This is the normal
  path for namespace defaults and shared tenant storage targets. The claim is
  stable configuration shared by all sandboxes/pods that reference it directly;
  there is one claim object for many sandboxes. The controller reads it directly,
  and each pause records a unique result on a
  `SandboxSnapshot` object rather than mutating the claim.
- Reference `snapshotStrategy.snapshotClaimTemplate` when the caller or
  server wants OpenSandbox to materialize an individual concrete claim for that
  sandbox/pod from namespace policy. This is useful for stamping labels/annotations,
  applying namespace policy, or creating a sandbox-specific storage target, such
  as an S3 bucket, without requiring the tenant to pre-create every claim. The
  template reference is materialization intent on the sandbox, analogous to DRA
  `ResourceClaimTemplate` use. The server copies the template once per sandbox/pod,
  merges any allowed `snapshotClaimTemplate.parameters` into the generated claim's
  `spec.parameters`, creates a distinct concrete `SandboxSnapshotClaim` for that sandbox/pod, then writes that generated
  claim name to `BatchSandbox.spec.snapshotStrategy.snapshotClaimName`; the
  controller consumes only the final claim.
- A request must set at most one of `snapshotClaimName`,
  `snapshotClaimTemplate`, or inline `claim`. If multiple selectors are set,
  the server rejects the request with `400` instead of guessing. Defaults follow
  the same order: configured claim first, configured template second, then an
  inline/generated default only if policy allows it.

The public API never accepts raw credentials and never references Kubernetes
Secrets; backend access is granted to the snapshot Job and resumed Pods through
node/workload identity. Claim/template names are the normal user-facing contract.
`POST /resume` does not require a body because the chosen claim, class, and
artifact are recorded on the latest successful pause.

### Kubernetes Resource Overview

```text
BatchSandbox (existing, extended)
  |- spec.replicas                    # 1 on the public server path
  |- spec.template                    # Pod template
  |- spec.operatingMode               # Running | Freeze | KeepFS | Hibernate
  |- spec.snapshotStrategy            # NEW; used by KeepFS and future Hibernate
  |  |- snapshotClaimName             # name of a shared concrete SandboxSnapshotClaim
  |  `- snapshotClaimTemplate         # template name + per-use parameters copied into one claim per sandbox/pod
  `- status.snapshot                  # NEW, inline snapshot result
     |- phase                         # Pending | Committing | Ready | Failed
     |- operatingMode                 # KeepFS | Hibernate for the current result
     |- snapshotClaimName
     |- snapshotClassName
     |- artifacts[]                   # {containerName, artifactUri, digest, format}
     |- sourcePodName
     |- sourcePodUID
     |- sourceNodeName
     |- runtimeHandler
     |- compatibility                 # {os, architecture, kernelVersion, runtimeName, runtimeVersion, checkpointFormatVersion}
     |- readyAt
     `- message

SandboxSnapshotClass (NEW, cluster-scoped)
  |- type                          # Local | BlobStore | S3
  `- parameters                    # admin defaults (endpoint, local path, ...)

SandboxSnapshotClaim (NEW, namespaced; PVC-like)
  |- spec.snapshotClassName        # name of a SandboxSnapshotClass; empty => cluster default
  |- spec.parameters               # bucket/container/region/prefix/compression, backend-specific
  `- status.snapshotClassName      # resolved class

SandboxSnapshotClaimTemplate (NEW, namespaced; DRA ResourceClaimTemplate-like)
  |- spec.metadata                 # labels/annotations copied to each generated claim
  `- spec.spec                     # SandboxSnapshotClaimSpec copied into each generated claim
```

The paused runtime state is fully described by `BatchSandbox`
(`status.phase = Paused` plus `status.snapshot`). No per-pause sibling snapshot
action object is required to answer `GET /sandboxes/{id}`; `SandboxSnapshotClaim` is
configuration, not the snapshot result.

`SandboxSnapshotClaimTemplate` follows Kubernetes Dynamic Resource Allocation's
`ResourceClaimTemplate` pattern and is a first-class namespaced CRD. It is not
used directly by the snapshot Job, but it can be referenced by
`BatchSandbox.spec.snapshotStrategy`. When the server handles a pause for a
sandbox that references a template, it copies the template into a concrete
`SandboxSnapshotClaim` dedicated to that sandbox/pod, merges any allowed per-use
parameters into the generated claim, and patches
`BatchSandbox.spec.snapshotStrategy.snapshotClaimName` to that generated claim.
The controller consumes only the final claim. Referencing an existing
`snapshotClaimName` skips this materialization step and shares that one concrete
claim across all sandboxes/pods that reference it.

Side-by-side, the developer-facing shape matches the agent-sandbox KEP-0694
proposal in PR #762:

```yaml
# agent-sandbox (KEP-0694)
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
spec:
  operatingMode: Suspended
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotClass: "fast-memory"
---
# OpenSandbox (OSEP-0015)
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
spec:
  operatingMode: KeepFS
  snapshotStrategy:
    snapshotClaimTemplate:
      name: tenant-s3
      parameters:
        bucket: tenant-a-sandbox-abc123
```

### Example CRs

These examples are grounded in the current OpenSandbox CRD facts:

- `BatchSandbox` is namespaced, uses `apiVersion:
  sandbox.opensandbox.io/v1alpha1`, has required `spec.replicas`, and embeds a
  Kubernetes `PodTemplateSpec` under `spec.template`. OSEP-0015 replaces the
  existing boolean pause intent with `spec.operatingMode`.
- `SandboxSnapshot` exists today with only `spec.sandboxName` as its desired-state
  input; OSEP-0015 removes that object from the pause/resume path.
- Agent-sandbox uses `spec.operatingMode` (`Running`/`Suspended`) instead of a
  lifecycle boolean. PR #762's KEP examples add a cluster-wide `SandboxSnapshotClass`,
  `spec.suspensionStrategy.type`, and
  `spec.suspensionStrategy.hibernate.snapshotClass`.

Current OSEP-0008 pause creates a separate snapshot action object like this:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshot
metadata:
  name: sandbox-abc123-pause
  namespace: default
spec:
  sandboxName: sandbox-abc123
```

OSEP-0015 instead preconfigures backend selection with admin-owned classes and
namespace-local claims/templates:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClass
metadata:
  name: local
  annotations:
    snapshot.opensandbox.io/is-default-class: "true"
type: Local
parameters:
  nodePath: /var/lib/opensandbox/snapshots
---
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClass
metadata:
  name: blobstore
type: BlobStore
parameters:
  endpoint: https://blob.example.com
  containerBase: opensandbox-snapshots
---
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClass
metadata:
  name: s3
type: S3
parameters:
  endpoint: https://s3.us-west-2.amazonaws.com
  forcePathStyle: "false"
---
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClaimTemplate
metadata:
  name: tenant-s3
  namespace: default
spec:
  metadata:
    labels:
      opensandbox.io/snapshot-profile: tenant-s3
  spec:
    snapshotClassName: s3
    parameters:
      region: us-west-2
      bucket: sandbox-snapshots
      prefix: tenants/default
      compressionMode: Enabled
      compressionAlgorithm: zstd
---
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClaim
metadata:
  name: default-claim
  namespace: default
spec:
  snapshotClassName: s3
  parameters:
    region: us-west-2
    bucket: opensandbox-snapshots
    prefix: tenants/default/shared
    compressionMode: Enabled
    compressionAlgorithm: zstd
```

A sandbox can request an individual claim by referencing the template. The server
copies the template's metadata and spec into a distinct `SandboxSnapshotClaim`
owned by that sandbox/pod:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: sandbox-abc123
  namespace: default
spec:
  replicas: 1
  operatingMode: KeepFS
  snapshotStrategy:
    snapshotClaimTemplate:
      name: tenant-s3
      parameters:
        bucket: tenant-a-sandbox-abc123
  template:
    metadata:
      labels:
        app: sandbox-abc123
    spec:
      restartPolicy: Never
      containers:
        - name: sandbox
          image: python:3.12
          command: ["sleep", "infinity"]
```

When the server handles that pause, it materializes a concrete claim by copying
the template before patching the `BatchSandbox` to reference the generated claim.
Another sandbox/pod using the same template gets a different concrete claim:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClaim
metadata:
  name: sandbox-abc123-pause-20260627
  namespace: default
  labels:
    opensandbox.io/sandbox-id: sandbox-abc123
    opensandbox.io/generated-from-template: tenant-s3
spec:
  snapshotClassName: s3
  parameters:
    region: us-west-2
    bucket: sandbox-snapshots
    prefix: tenants/default
    compressionMode: Enabled
    compressionAlgorithm: zstd
```

A `KeepFS` pause request is just a normal `BatchSandbox` with
`operatingMode: KeepFS` and, after materialization, the snapshot strategy field
set to the generated claim:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: sandbox-abc123
  namespace: default
spec:
  replicas: 1
  operatingMode: KeepFS
  snapshotStrategy:
    snapshotClaimName: sandbox-abc123-pause-20260627
  template:
    metadata:
      labels:
        app: sandbox-abc123
    spec:
      restartPolicy: Never
      containers:
        - name: sandbox
          image: python:3.12
          command: ["sleep", "infinity"]
```

A running sandbox uses the same `BatchSandbox.spec.operatingMode` field with the
steady-state value `Running`. This is the default state and does not require a
snapshot claim:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: running-sandbox
  namespace: default
spec:
  replicas: 1
  operatingMode: Running
  template:
    metadata:
      labels:
        app: running-sandbox
    spec:
      restartPolicy: Never
      containers:
        - name: sandbox
          image: alpine:3.20
          command: ["sleep", "infinity"]
```

### Operating modes

`spec.operatingMode` dictates *how* the sandbox is physically operated. `Running`
is the normal active state. Snapshot-producing modes differ in what they save:
`KeepFS` saves writable filesystems only, while `Hibernate` saves writable
filesystems plus memory/process state.

| Operating mode | What pause does | Filesystem | Memory | Status this revision |
|---|---|---|---|---|
| `Running` | Keep the Pod running; no pause action is taken. | Kept | Kept | Supported (default and resume target) |
| `Freeze` | Keep the Pod, freeze container cgroups. | Kept | Kept (in node RAM) | Reserved (Kubernetes freeze is future work) |
| `KeepFS` | Capture container writable filesystems via the OSEP-0008 image-commit path, then delete the Pod. | Persisted as filesystem artifact | Lost | **Implemented** (the OSEP-0008 behavior under the new operating-mode API) |
| `Hibernate` | Capture full pod state via a checkpoint/restore-capable snapshot Job, then delete the Pod. | Persisted as checkpoint artifact | Persisted as checkpoint artifact | Reserved (not implemented in this revision) |

`KeepFS` is the default pause mode and requires a resolvable
`spec.snapshotStrategy.snapshotClaimName` on `BatchSandbox` because it produces a
stored artifact. The public API can provide that claim directly, ask the server
to generate one from a template/inline claim, or omit the field and rely on
defaults. `Running` is the steady state written by `/resume` and by defaulted new
sandboxes.
Checkpoint-only snapshotting keeps the Pod running and is action-shaped rather
than persistent-mode-shaped; it is intentionally left to a future OSEP.
`Freeze` and `Hibernate` are reserved so the API does not need to break when they
land. Until they are implemented, the server must reject `operatingMode =
"Freeze"` and `operatingMode = "Hibernate"` with a clear `400`/`501` rather than
patching an unsupported `BatchSandbox.spec.operatingMode`.

### Seamless restore contract

`KeepFS` preserves only writable filesystem state. After resume, the Pod is
recreated from `spec.template` and the filesystem artifact; in-sandbox processes
restart from the image entrypoint. This is the behavior implemented by OSEP-0008.

`Hibernate` is intended to be transparent to the in-sandbox agent process: after
resume, the agent process continues from the checkpointed memory/process state
instead of rerunning the image entrypoint. This requires a runtime
checkpoint/restore implementation (for example CRIU/containerd checkpoint or an
equivalent runtime primitive). A filesystem image commit alone is not sufficient
for `Hibernate`, and `Hibernate` is not implemented in this revision.

For `Hibernate`, the checkpoint artifact must include all containers in the Pod, their writable
filesystems, process trees, memory, open file descriptors, and enough runtime
metadata to restore them on a compatible node. `status.snapshot` records the
artifact references plus runtime compatibility data so resume can fail closed
before creating a broken Pod.

The pause path must not stop the in-sandbox agent or task processes before
checkpointing. It may briefly freeze/quiesce containers through the runtime while
the checkpoint is taken, but the restored process tree must continue from the
checkpointed instruction/memory state rather than from a clean entrypoint.

Seamless here is scoped to the sandbox process model. Kubernetes object identity
is not guaranteed to be identical after `Hibernate`: the restored Pod may have a
new Pod UID and IP, and external TCP connections may need to reconnect.
OpenSandbox keeps the public `sandboxId` and endpoint identities stable and
reroutes them after the restored Pod is Ready.

### Component Interaction Overview

The lifecycle has three moving parts:

1. **Server:** validates the optional pause body, resolves or generates a
   namespace-local `SandboxSnapshotClaim`, then patches `BatchSandbox.spec.operatingMode`
  and `spec.snapshotStrategy`.
2. **Controller:** resolves the claim/class, runs the same-node snapshot Job, and
   records a single checkpoint result in `BatchSandbox.status.snapshot`.
3. **Runtime:** commits and restores writable filesystems for `KeepFS`; future
  `Hibernate` adds memory/process checkpoint/restore. Backend access comes from
  node/workload identity.

`KeepFS` deletes the Pod only after `status.snapshot.phase=Ready`. `Resume` sets
`operatingMode=Running`; the controller restores the filesystem artifact from
`status.snapshot` when the Pod is absent, or treats the request as complete when
the sandbox is already Running. Future `Hibernate` uses the same lifecycle state
but restores memory/process state too.

### Notes/Constraints/Caveats

- This proposal targets the public server path where a sandbox maps to a
  `BatchSandbox` with `replicas = 1`. The single-replica assumption is unchanged
  from OSEP-0008.
- `status.snapshot` retains exactly one snapshot result (the latest). Re-pausing
  replaces it. Multi-snapshot history is maintined by the `SandboxSnapshot` CR; However, we only resume from the latest
  snapshot. The details of managing multiple snapshots remains out of scope.
- For `KeepFS` and future `Hibernate`, the snapshot reference recorded in `status.snapshot` must be
  stable and durable enough to survive Pod deletion, because resume relies on it
  after the Pod is gone.
- Backend authentication is expected to be provided by node/workload identity
  rather than Kubernetes Secrets: the snapshot Job and resumed Pods inherit access
  from their node or service-account identity binding. This is guidance for the
  provider that implements a `SandboxSnapshotClass`/`SandboxSnapshotClaim` backend,
  not a mechanism enforced by this proposal. It is the implementor's
  responsibility to wire up identity-based access and to ensure no static or
  long-lived secret is embedded in the class/claim parameters or otherwise
  required by the backend. For example, on Azure the cluster admin binds the
  snapshot committer's Kubernetes ServiceAccount to an Azure Entra ID (workload
  identity) and assigns it the appropriate role (such as `Storage Blob Data
  Contributor`) on the target storage account. The committer can then upload the
  files produced by `nerdctl save` directly to Azure Blob Storage using that
  federated identity, without any token, connection string, or password in the
  `SandboxSnapshotClass`/`SandboxSnapshotClaim` parameters.
- The privileged same-node Job pattern from OSEP-0008 is reused. `KeepFS` uses
  the image-only committer path; future `Hibernate` requires a
  checkpoint/restore-capable snapshotter.
- Because the snapshot result lives on `BatchSandbox.status`, deleting the
  `BatchSandbox` removes the paused state automatically; no per-pause result CR
  cleanup is needed. `SandboxSnapshotClaim` is reusable namespace configuration.
- `spec.operatingMode` is the desired persistent mode enum. Missing values default
  to `Running`; the server writes `KeepFS` for default `/pause` and `Running`
  for `/resume`.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Pod is deleted before the snapshot is durable | Only delete the Pod after `status.snapshot.phase == Ready`; resume validates the recorded artifact. |
| Snapshot Job lands on the wrong node | Record `status.snapshot.sourceNodeName` and pin the Job Pod to that node, as in OSEP-0008. |
| Restore lands where a node-local artifact is unreachable | For node-local backends (`type: Local`), pin the restored Pod to `status.snapshot.sourceNodeName`; fail closed with `ResumeFailed` if that node is gone or unschedulable. Remote backends need no pinning. |
| Missing/invalid `SandboxSnapshotClaim` or `SandboxSnapshotClass` on pause | Validate the referenced claim and resolved class before deleting the Pod; on failure set `status.snapshot.phase = Failed` and keep the Pod running. |
| Concurrent pause/resume requests cause flapping | Gate transitions on `status.snapshot.phase`, `status.observedGeneration`, and conditions for the latest `operatingMode`/`snapshotStrategy` generation; reject overlapping transitions with `409`. |
| Status object grows unbounded | `status.snapshot` is a single fixed-shape struct with one artifact list; no history is appended. |
| Backend config drift between sandboxes | Centralize backend config in `SandboxSnapshotClass`; sandboxes reference a namespace-local `SandboxSnapshotClaim` by name only. |
| Clusters without the new CRDs | The CRDs are additive; until they are installed, the server detects their absence and falls back to the OSEP-0008 `SandboxSnapshot` pause/resume path, so existing `/pause`/`/resume`/`GET` calls keep working unchanged. The new inline path activates automatically once the CRDs are present. |

## Design Details

### 1. BatchSandbox spec field

Extend `BatchSandboxSpec` (in `apis/sandbox/v1alpha1/batchsandbox_types.go`) with a
single desired operation enum and direct snapshot config. This follows the
current agent-sandbox `spec.operatingMode` pattern instead of adding or retaining
a boolean lifecycle field, but folds OpenSandbox's operation enum into
`operatingMode` so there is one selector instead of both `operatingMode` and
a separate `strategy.type`.

```go
// BatchSandboxOperatingMode defines the desired operational state.
// Mirrors agent-sandbox spec.operatingMode while preserving OpenSandbox wording.
// +kubebuilder:validation:Enum=Running;Freeze;KeepFS;Hibernate
type BatchSandboxOperatingMode string

const (
    // BatchSandboxOperatingModeRunning indicates the sandbox should be running.
    BatchSandboxOperatingModeRunning BatchSandboxOperatingMode = "Running"
    // BatchSandboxOperatingModeFreeze keeps the Pod and freezes container cgroups.
    // Reserved; not implemented for Kubernetes in this revision.
    BatchSandboxOperatingModeFreeze BatchSandboxOperatingMode = "Freeze"
    // BatchSandboxOperatingModeKeepFS snapshots writable filesystems and deletes the Pod.
    BatchSandboxOperatingModeKeepFS BatchSandboxOperatingMode = "KeepFS"
    // BatchSandboxOperatingModeHibernate snapshots filesystems and memory, then deletes the Pod.
    // Reserved; not implemented for Kubernetes in this revision.
    BatchSandboxOperatingModeHibernate BatchSandboxOperatingMode = "Hibernate"
)

// SnapshotClaimTemplateSelector asks the server to materialize a template into a
// generated claim for this sandbox/pod. Parameters are merged into the generated
// claim's spec.parameters after copying the template, subject to policy.
type SnapshotClaimTemplateSelector struct {
  // Name references a SandboxSnapshotClaimTemplate in the BatchSandbox namespace.
  Name string `json:"name"`

  // Parameters supplies per-use backend values, such as an S3 bucket or prefix.
  // +optional
  Parameters map[string]string `json:"parameters,omitempty"`
}

// SnapshotStrategy holds the snapshot config used when OperatingMode is KeepFS
// or, in a future revision, Hibernate.
type SnapshotStrategy struct {
    // SnapshotClaimName references a namespaced SandboxSnapshotClaim that selects a
    // SandboxSnapshotClass and storage target. Set directly by the server, or
    // after it materializes SnapshotClaimTemplate.
    // +optional
    SnapshotClaimName string `json:"snapshotClaimName,omitempty"`

    // SnapshotClaimTemplate references a SandboxSnapshotClaimTemplate and optional
    // per-use parameters. The server copies the template into a concrete
    // SandboxSnapshotClaim for this sandbox/pod, merges allowed parameters, and
    // writes the generated claim name back to SnapshotClaimName.
    // +optional
    SnapshotClaimTemplate *SnapshotClaimTemplateSelector `json:"snapshotClaimTemplate,omitempty"`
}

type BatchSandboxSpec struct {
    // ... existing fields ...

    // OperatingMode is the desired operation written by Server and executed by
    // Controller. Missing values default to Running.
    // +kubebuilder:default=Running
    // +kubebuilder:validation:Enum=Running;Freeze;KeepFS;Hibernate
    // +optional
    OperatingMode BatchSandboxOperatingMode `json:"operatingMode,omitempty"`

    // SnapshotStrategy describes where snapshot artifacts are written and read.
    // Consulted when OperatingMode is KeepFS or future Hibernate. When nil, the
    // controller falls back to the server-configured or cluster default
    // SandboxSnapshotClaim.
    // +optional
    SnapshotStrategy *SnapshotStrategy `json:"snapshotStrategy,omitempty"`
}
```

Rules:

- `spec.operatingMode` is the single persistent-mode selector. Checkpoint-only
  snapshotting is an action rather than a persistent mode and is left to a future
  OSEP.
- `KeepFS` requires a resolvable `SandboxSnapshotClaim` (explicit
  `snapshotStrategy.snapshotClaimName`, server-generated from a public
  `snapshotClaimTemplate`/inline request, or server-configured default); the
  claim must resolve to a `SandboxSnapshotClass`. Otherwise the pause fails with a clear
  condition.
- `KeepFS` deletes the Pod and reports public `Paused` only after the filesystem
  snapshot artifact is durable.
- `Hibernate` follows the same claim/class selection contract but is not
  implemented until checkpoint/restore support can save memory/process state.
- `snapshotClaimName` points at a concrete `SandboxSnapshotClaim` that may be
  shared by many sandboxes/pods. `snapshotClaimTemplate` is different: it is a
  server materialization input for creating one concrete claim per sandbox/pod.
  Its `parameters` map supplies per-use values, such as `bucket`, that are merged
  into the generated claim's `spec.parameters` after copying the template.
  After generating that individual claim, the server writes its name to
  `snapshotClaimName`; the controller consumes only that concrete claim and does
  not read the template.
- `Running` and `Freeze` ignore `spec.snapshotStrategy`. `KeepFS` consults it;
  future `Hibernate` will consult it too.

### 2. SandboxSnapshotClass, SandboxSnapshotClaim, and SandboxSnapshotClaimTemplate CRDs

Introduce a cluster-scoped `SandboxSnapshotClass` plus namespaced `SandboxSnapshotClaim` and
`SandboxSnapshotClaimTemplate` resources under `sandbox.opensandbox.io/v1alpha1`. The
split mirrors Kubernetes DRA:

- `SandboxSnapshotClass` is the admin-owned class of backend. It answers "what kind of
  snapshot backend is this?" (`Local`, `BlobStore`, `S3`).
- `SandboxSnapshotClaim` is the namespace-local request for a concrete storage target.
  It carries bucket/container/region/prefix-like parameters and can be shared by
  every sandbox/pod that references it directly; backend access is granted by
  node/workload identity, not by the claim.
- `SandboxSnapshotClaimTemplate` is a namespaced template that the server copies into a
  generated `SandboxSnapshotClaim`, analogous to DRA `ResourceClaimTemplate`. It is used
  when the namespace operator wants a reusable policy that produces a separate
  concrete claim for each individual sandbox/pod instead of sharing one claim or
  pre-creating every concrete claim by hand.

`SandboxSnapshotClaim.status` is owned by a lightweight claim-resolution path in the
same controller manager. It resolves the referenced/default `SandboxSnapshotClass` and
records `Pending`/`Bound`/`Failed` independently of any individual pause. The
snapshot execution result still lives only on `BatchSandbox.status.snapshot`.

```go
// +kubebuilder:validation:Enum=Local;BlobStore;S3
type SnapshotBackendType string

const (
    SnapshotBackendLocal     SnapshotBackendType = "Local"
    SnapshotBackendBlobStore SnapshotBackendType = "BlobStore"
    SnapshotBackendS3        SnapshotBackendType = "S3"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=snapclass
// +kubebuilder:printcolumn:name="TYPE",type="string",JSONPath=".type"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// SandboxSnapshotClass describes a pluggable snapshot backend. It is cluster-scoped and
// selected by namespaced SandboxSnapshotClaim objects.
type SandboxSnapshotClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    // Type is the high-level backend kind exposed to operators.
    // +kubebuilder:validation:Enum=Local;BlobStore;S3
    // +kubebuilder:validation:Required
    Type SnapshotBackendType `json:"type"`

    // Parameters holds admin-owned, backend-specific defaults such as
    // endpoints, local base paths, or compatibility flags. Tenant-specific
    // bucket/container/prefix fields belong on SandboxSnapshotClaim.
    // +optional
    Parameters map[string]string `json:"parameters,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotClassList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SandboxSnapshotClass `json:"items"`
}

// SandboxSnapshotClaimPhase indicates whether a claim resolved its class.
// +kubebuilder:validation:Enum=Pending;Bound;Failed
type SandboxSnapshotClaimPhase string

const (
    SandboxSnapshotClaimPhasePending SandboxSnapshotClaimPhase = "Pending"
    SandboxSnapshotClaimPhaseBound   SandboxSnapshotClaimPhase = "Bound"
    SandboxSnapshotClaimPhaseFailed  SandboxSnapshotClaimPhase = "Failed"
)

// SandboxSnapshotClaimSpec is namespaced configuration for one or more sandboxes.
type SandboxSnapshotClaimSpec struct {
    // SnapshotClassName selects the cluster-scoped SandboxSnapshotClass. When empty,
    // the controller uses the annotated cluster default SandboxSnapshotClass.
    // +optional
    SnapshotClassName string `json:"snapshotClassName,omitempty"`

    // Parameters holds namespace-owned, backend-specific target information.
    // Examples: region, bucket, container, prefix, storageAccount, repository,
    // compressionMode, or compressionAlgorithm.
    // +optional
    Parameters map[string]string `json:"parameters,omitempty"`
}

// SandboxSnapshotClaimStatus records class resolution. Snapshot execution status stays
// on BatchSandbox.status.snapshot, not on the claim.
type SandboxSnapshotClaimStatus struct {
    // Phase indicates whether the claim resolved its SandboxSnapshotClass.
    // +optional
    Phase SandboxSnapshotClaimPhase `json:"phase,omitempty"`

    // SnapshotClassName is the resolved SandboxSnapshotClass, including defaulting.
    // +optional
    SnapshotClassName string `json:"snapshotClassName,omitempty"`

    // Message carries human-readable resolution errors.
    // +optional
    Message string `json:"message,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snapclaim
// +kubebuilder:printcolumn:name="CLASS",type="string",JSONPath=".status.snapshotClassName"
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// SandboxSnapshotClaim is a namespaced PVC-like claim for snapshot backend access.
type SandboxSnapshotClaim struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   SandboxSnapshotClaimSpec   `json:"spec,omitempty"`
    Status SandboxSnapshotClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotClaimList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SandboxSnapshotClaim `json:"items"`
}

// SandboxSnapshotClaimTemplateSpec mirrors DRA ResourceClaimTemplate: metadata is
// copied to each generated claim, and Spec is the SandboxSnapshotClaimSpec to copy.
type SandboxSnapshotClaimTemplateSpec struct {
    // +optional
    Metadata metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec SandboxSnapshotClaimSpec `json:"spec"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=snapclaimtmpl
type SandboxSnapshotClaimTemplate struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec SandboxSnapshotClaimTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotClaimTemplateList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SandboxSnapshotClaimTemplate `json:"items"`
}
```

A cluster default is selected with an annotation, mirroring `StorageClass`.
Claims and templates may omit `snapshotClassName` to use that default:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClass
metadata:
  name: default
  annotations:
    snapshot.opensandbox.io/is-default-class: "true"
type: S3
parameters:
  endpoint: https://s3.us-west-2.amazonaws.com
---
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshotClaimTemplate
metadata:
  name: default-template
  namespace: sandbox-tenant-a
spec:
  metadata:
    labels:
      opensandbox.io/generated-from-template: default-template
  spec:
    snapshotClassName: default
    parameters:
      region: us-west-2
      bucket: tenant-a-snapshots
      prefix: sandboxes
      compressionMode: Enabled
      compressionAlgorithm: zstd
```

This revision implements one snapshot mechanism: a same-node filesystem snapshot
Job for `KeepFS`, using the OSEP-0008 image-commit path. `Hibernate` is reserved
for a future same-node full-pod-state checkpoint Job. The `type` enum is the extension point for future backends without
changing the `BatchSandbox`
API. A concrete claim can be pre-created by a tenant operator and shared by all
sandboxes/pods that reference it directly. A template is not shared as the claim;
instead, the server copies it into a generated concrete claim for each individual
sandbox/pod that uses the template. The server must not mutate a shared existing
`SandboxSnapshotClaim` to satisfy one sandbox's pause request; when
sandbox-specific values are needed, or when a sandbox references
`snapshotClaimTemplate`, it materializes a concrete claim for that sandbox/pod and points
`BatchSandbox.spec.snapshotStrategy.snapshotClaimName` at that generated claim.
The claim is still configuration, not the snapshot result, and does not replace
`BatchSandbox.status.snapshot`. Per-pause history and artifact identity come
from the `SandboxSnapshot` result objects, so implementations should not create
a fresh claim for every pause unless backend retention or isolation policy truly
requires a distinct storage target per pause.

### 3. Snapshot result recording on `SandboxSnapshot` + inline reference

The per-pause result is recorded on the existing, namespaced `SandboxSnapshot`
object, reused here as the bound result (analogous to a `PersistentVolume` /
`VolumeSnapshotContent`). The `BatchSandbox` reconciler creates **one
`SandboxSnapshot` per pause**, owns it with an `ownerReference`, and writes the
snapshot detail into its `status`. `BatchSandbox.status.snapshot` keeps
only a lightweight **reference** to the current result, so basic lifecycle
`GET /sandboxes/{id}` (Running/Pausing/Paused/Resuming) needs no sibling read;
fetching per-container artifact detail reads the referenced `SandboxSnapshot`.

Reusing `SandboxSnapshot` gives a natural per-pause **history** (one object per
pause, garbage-collected with the sandbox) without growing an unbounded array on
`BatchSandbox`. Resume still consumes only the latest referenced result;
recovery from an older snapshot remains a Non-Goal.

`SandboxSnapshot` is evolved **additively** from its OSEP-0008 shape: every new
field is optional and the existing `Succeed` phase value is kept (no `Ready`
rename, so no breaking enum change). Its role changes from an action trigger to a
recording — the `BatchSandbox` reconciler drives the whole flow inline and
*writes* this object, so `spec.sandboxName` is now a back-reference rather than a
Job trigger.

```go
// EXISTING enum, unchanged (note: "Succeed", not "Ready"):
// +kubebuilder:validation:Enum=Pending;Committing;Succeed;Failed
// type SandboxSnapshotPhase string

// SandboxSnapshotSpec is write-once binding identity (no user desired-state).
type SandboxSnapshotSpec struct {
    // SandboxName is the source BatchSandbox (same namespace). EXISTING (required).
    SandboxName string `json:"sandboxName"`
    // SnapshotClaimName is the SandboxSnapshotClaim used for this snapshot,
    // resolving the class + storage target. ADD (optional; empty for the
    // OSEP-0008 filesystem-image path).
    // +optional
    SnapshotClaimName string `json:"snapshotClaimName,omitempty"`
    // OperatingMode is the mode that produced this snapshot (KeepFS | Hibernate).
    // ADD (optional).
    // +optional
    OperatingMode BatchSandboxOperatingMode `json:"operatingMode,omitempty"`
}

// ContainerSnapshot keeps the OSEP-0008 image fields for KeepFS and adds
// checkpoint fields for future Hibernate.
type ContainerSnapshot struct {
    ContainerName string `json:"containerName"`        // EXISTING
    // ImageURI/ImageDigest are the OSEP-0008 filesystem-image path (now optional).
    // +optional
    ImageURI string `json:"imageUri,omitempty"`        // EXISTING (relaxed to optional)
    // +optional
    ImageDigest string `json:"imageDigest,omitempty"`  // EXISTING
    // ArtifactURI/Format are the future Hibernate full-pod-state checkpoint artifact.
    // +optional
    ArtifactURI string `json:"artifactUri,omitempty"`  // ADD
    // +optional
    Format string `json:"format,omitempty"`            // ADD — e.g. "oci-checkpoint-v1"
}

// SnapshotCompatibility records the source environment a checkpoint was taken in
// so resume can fail closed before restoring onto an incompatible target. It is
// captured at pause time, while the source Pod/Node still exist.
type SnapshotCompatibility struct {
    // +optional
    OS string `json:"os,omitempty"`
    // +optional
    Architecture string `json:"architecture,omitempty"`
    // +optional
    KernelVersion string `json:"kernelVersion,omitempty"`
    // +optional
    RuntimeName string `json:"runtimeName,omitempty"`
    // +optional
    RuntimeVersion string `json:"runtimeVersion,omitempty"`
    // +optional
    CheckpointFormatVersion string `json:"checkpointFormatVersion,omitempty"`
}

// SandboxSnapshotStatus is the recording (controller-written). It holds all the
// per-pause detail; nothing heavy lives here — artifact bytes stay in the backend.
type SandboxSnapshotStatus struct {
    Phase SandboxSnapshotPhase `json:"phase,omitempty"`          // EXISTING — Pending|Committing|Succeed|Failed
    // +optional
    Containers []ContainerSnapshot `json:"containers,omitempty"`  // EXISTING — per-container refs
    // +optional
    Conditions []SandboxSnapshotCondition `json:"conditions,omitempty"` // EXISTING
    // +optional
    SourcePodName string `json:"sourcePodName,omitempty"`        // EXISTING
    // +optional
    SourceNodeName string `json:"sourceNodeName,omitempty"`      // EXISTING
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"` // EXISTING
    // +optional
    SourcePodUID string `json:"sourcePodUID,omitempty"`          // ADD — rechecked before Pod deletion (see §5)
    // +optional
    SnapshotClassName string `json:"snapshotClassName,omitempty"` // ADD — resolved class
    // +optional
    RuntimeHandler string `json:"runtimeHandler,omitempty"`      // ADD — checkpoint/restore impl needed to restore
    // +optional
    Compatibility *SnapshotCompatibility `json:"compatibility,omitempty"` // ADD
    // +optional
    ReadyAt *metav1.Time `json:"readyAt,omitempty"`              // ADD — when it reached Succeed
}
```

`BatchSandbox.status.snapshot` shrinks to a reference to the current result:

```go
// SnapshotStatusRef is the inline pointer to the current SandboxSnapshot result.
type SnapshotStatusRef struct {
    // Name references the SandboxSnapshot result object bound to the latest pause.
    // +optional
    Name string `json:"name,omitempty"`
    // Phase mirrors the bound SandboxSnapshot phase so basic lifecycle GET needs
    // no sibling read.
    // +optional
    Phase SandboxSnapshotPhase `json:"phase,omitempty"`
    // OperatingMode is the mode that produced the current result.
    // +optional
    OperatingMode BatchSandboxOperatingMode `json:"operatingMode,omitempty"`
    // Message carries human-readable detail, especially on Failed.
    // +optional
    Message string `json:"message,omitempty"`
}

type BatchSandboxStatus struct {
    // ... existing fields ...

    // Snapshot references the current SandboxSnapshot result for the latest pause.
    // The full per-container artifact detail lives on that object.
    // +optional
    Snapshot *SnapshotStatusRef `json:"snapshot,omitempty"`
}
```

The existing `BatchSandboxStatus.Phase` (`Pausing`/`Paused`/`Resuming`) is the
aggregate the server reads; `status.snapshot.phase` mirrors the bound
`SandboxSnapshot` phase, and the referenced `SandboxSnapshot` carries the detail.

### 4. Pause state model

State is derived from the `BatchSandbox` object alone. The Kubernetes CRD keeps
the existing internal steady-state enum value `Succeed`; the public server maps
that to the user-facing `Running` state.

#### Stable states

- `spec.operatingMode` is `Running` and the Pod is Running and Ready ->
  `status.phase = Succeed` (public API: `Running`).
- Pod gone due to `KeepFS` with `status.snapshot.phase == Ready` ->
  `status.phase = Paused`.
- `KeepFS` snapshot failed while the Pod still exists -> `status.phase = Succeed`
  (public API: `Running`) plus a `PauseFailed` condition.

#### Intermediate states

- Checkpoint is in `Pending`/`Committing` -> `Pausing`.
- `KeepFS` snapshot is Ready but Pod cleanup is still pending ->
  `Pausing` with reason `SNAPSHOT_READY_CLEANUP`.
- Pod is being restored from a checkpoint -> `Resuming`.

This reuses the existing `BatchSandboxCondition` types
(`Paused`, `PauseFailed`, `ResumeFailed`, ...) and the existing
`status.phase` enum; only `status.snapshot` is added.

### 5. Pause flow

```text
1. Client  POST /sandboxes/{sandboxId}/pause
           - no body: use defaults
           - or optional body: operatingMode + shared claim, template, or inline claim parameters
2. Server  Resolve/materialize the concrete SandboxSnapshotClaim:
           - existing snapshotClaimName: read the shared concrete claim
           - snapshotClaimTemplate: copy template and merge per-use parameters into one generated claim for this sandbox/pod
           - inline claim: create one generated claim for this sandbox/pod if policy permits it
3. Server  Patch BatchSandbox:
           - spec.operatingMode = KeepFS
           - spec.snapshotStrategy = { snapshotClaimName }   (server-resolved for KeepFS)
4. Ctrl    Observe operatingMode/snapshotStrategy generation; validate:
           - workload exists, replicas == 1
           - for KeepFS: resolve SandboxSnapshotClaim (existing or server-generated)
           - resolve SandboxSnapshotClass from the claim (explicit or cluster default)
5. Ctrl    Resolve live Pod and Node; record sourcePodName/sourceNodeName
6. Ctrl    status.snapshot.operatingMode = KeepFS, phase = Pending
7. Ctrl    Create same-node snapshot Job from SandboxSnapshotClaim + SandboxSnapshotClass config
8. Job     Commit writable filesystems, write artifact to backend
9. Ctrl    status.snapshot.phase: Pending -> Committing -> Ready,
           artifacts[] populated
10. Ctrl   Delete the Pod, status.phase = Paused.
11. GET    /sandboxes/{sandboxId} returns Paused
```

Failure behavior:

- If checkpoint or upload fails, `status.snapshot.phase = Failed`, the Pod is **not**
  deleted, and a `PauseFailed` condition is set. The sandbox remains `Succeed`
  internally (public API: `Running`).
- Reconciliation is level-triggered: as long as `spec.operatingMode` stays
  `KeepFS`, the controller keeps reconciling toward that desired state and
  re-attempts a `Failed` snapshot on its own (with backoff). The client does
  **not** need to re-issue `POST /pause` to trigger a retry; an unchanged
  `operatingMode`/`snapshot` spec is sufficient because the desired state has not
  been reached. The `observedGeneration` gating only suppresses overlapping,
  in-flight transitions; it does not turn a `Failed` phase into a terminal state.
- In that failure state, `spec.operatingMode` may still be `KeepFS` while
  public `GET` returns `Running`. This is intentional fail-closed behavior: the
  requested pause was not achieved, so a client polling for `Paused` observes the
  `PauseFailed` condition/message and either waits for the controller's retry to
  succeed or clears the intent via `POST /resume`.
- Overlapping pause requests for the same sandbox return `409` while a transition
  is in flight.

### 6. Snapshot Job

The `KeepFS` snapshot Job keeps OSEP-0008's same-node, controller-owned image
commit pattern. Admin-owned backend defaults (endpoint, local path, S3
compatibility flags, and supported artifact format) are read from
`SandboxSnapshotClass`; namespace-owned target inputs (bucket/container/region/
prefix and compression parameters) are read from `SandboxSnapshotClaim`.

The Job is pinned to `status.snapshot.sourceNodeName`, mounts only the runtime
interfaces required by the filesystem commit implementation, authenticates to the
backend through node/workload identity, and writes filesystem artifacts for every
container in the Pod. Future `Hibernate` will require a checkpoint/restore-capable
Job that also records runtime compatibility metadata in `status.snapshot`.

Controller startup flags from OSEP-0008 (`--snapshot-registry`,
`--snapshot-registry-insecure`) are migration-only inputs used to synthesize an
implicit default `SandboxSnapshotClass` and namespace-local `SandboxSnapshotClaim` (see
[Upgrade & Migration Strategy](#upgrade--migration-strategy)). The trusted
snapshotter image remains a controller-level setting, not a user-selectable
field.

### 7. Resume flow

```text
1. Client  POST /sandboxes/{sandboxId}/resume
2. Server  Patch BatchSandbox: spec.operatingMode = Running
3. Ctrl    Observe operatingMode=Running; if Pod absent and the current pause result is KeepFS:
           - build Pod from spec.template
           - for node-local backends, pin the Pod to status.snapshot.sourceNodeName
           - restore writable filesystems from status.snapshot artifacts
           - authenticate to backend via node/workload identity
4. Ctrl    status.phase = Resuming while the Pod starts
5. Ctrl    Pod Running and Ready -> status.phase = Succeed (public API: Running)
```

When the resolved `SandboxSnapshotClass` is a node-local backend (`type: Local` /
`nodePath`), the filesystem artifact only exists on the node that produced it.
Resume must pin the restored Pod to `status.snapshot.sourceNodeName` so the
scheduler cannot place it on a node where the artifact is unreachable. If that
node is gone or unschedulable, resume fails closed with `ResumeFailed` rather
than starting a Pod that cannot find its checkpoint. Remote backends
(`BlobStore`/`S3`) are reachable from any node and do not require node pinning.

### 8. Server mapping and public API compatibility

The server's Kubernetes lifecycle path changes both its internal implementation
and the optional request body for pause. The API remains backward-compatible
because the new body is optional and every new field has a default.

| Public call | OSEP-0008 server action | OSEP-0015 server action |
|---|---|---|
| `POST /pause` with no body | Create `SandboxSnapshot` CR + set `spec.pause` | Resolve defaults, then patch `spec.operatingMode=KeepFS` + `spec.snapshotStrategy` |
| `POST /pause` with body | Not available | Validate `operatingMode`; resolve an existing shared claim or generate one concrete claim for this sandbox/pod from a template/inline claim; patch `spec.operatingMode` and `spec.snapshotStrategy.snapshotClaimName` |
| `POST /resume` | Validate `SandboxSnapshot` Ready, set `spec.pause` | Patch `spec.operatingMode=Running` |
| `GET /sandboxes/{id}` | Merge `BatchSandbox` + `SandboxSnapshot` | Read `BatchSandbox.status` (incl. `status.snapshot`) |
| `DELETE` | Delete `BatchSandbox` + `SandboxSnapshot` | Delete `BatchSandbox` |

**Backward compatibility via CRD detection.** The OSEP-0008 column above is not
removed at rollout. The server detects whether the new
`SandboxSnapshotClass`/`SandboxSnapshotClaim` CRDs are installed (for example via API discovery
or a cached REST mapping). When they are **absent**, the server keeps using the
OSEP-0008 `SandboxSnapshot` pause/resume path and rejects optional pause-body
fields that require the new backend objects, so existing empty-body `/pause`
calls behave exactly as before. When the CRDs are **present**, the server switches
to the inline `spec.operatingMode`/`spec.snapshotStrategy` path. Both paths stay valid
during migration, so CRD installation does not need to be a hard, ordered
preflight; the inline path activates automatically once the CRDs exist.

- `specs/sandbox-lifecycle.yml`: add optional `requestBody` for
  `/sandboxes/{sandboxId}/pause` that references `PauseSandboxRequest`; keep
  `/resume` bodyless in this revision.
- `server/opensandbox_server/api/schema.py`: add Pydantic models for
  `PauseSandboxRequest`, `SnapshotPauseRequest`, and
  inline generated per-sandbox/pod claim options.
- `server/opensandbox_server/api/lifecycle.py`: accept
  `body: PauseSandboxRequest | None = Body(None)` on `pause_sandbox`.
- Kubernetes provider/runtime code: materialize the selected/generated
  `SandboxSnapshotClaim`, then patch the `BatchSandbox`.

Public request shape (sketch):

```yaml
PauseSandboxRequest:
  operatingMode: KeepFS | Hibernate | Freeze
  snapshotStrategy:
    # exactly one of these should be set for KeepFS/Hibernate; all omitted means use defaults
    snapshotClaimName: default-claim
    snapshotClaimTemplate:
      name: tenant-s3
      parameters:
        bucket: tenant-a-sandbox-abc123
    claim:
      snapshotClassName: s3
      parameters:
        region: us-west-2
        bucket: opensandbox-snapshots
        prefix: tenants/default/sandbox-abc123
        compressionMode: Enabled
        compressionAlgorithm: zstd
```

Defaulting rules preserve existing clients:

1. Missing body or missing `operatingMode` defaults to the server's
  `pause.default_operating_mode` (`KeepFS` unless configured otherwise).
2. `KeepFS` with no claim selector uses `pause.snapshot_claim`
   when set.
3. If `pause.snapshot_claim` is empty and `pause.snapshot_claim_template` is set,
   the server generates a claim from that template.
4. If neither claim nor template is configured, the server/controller must
   create or select a namespace-local default claim from legacy snapshot settings
   when those settings exist, preserving the OSEP-0008 migration path. If no
  defaults exist, empty-body `KeepFS` fails as a server
   misconfiguration.

Class resolution precedence is:

1. An explicit `SandboxSnapshotClaim.spec.snapshotClassName` always wins.
2. For generated claims, the inline claim or template value is copied first; if
   still empty, the server may fill `pause.snapshot_class`.
3. If the claim still has no class, the claim resolver uses the
   `snapshot.opensandbox.io/is-default-class: "true"` `SandboxSnapshotClass` annotation.
4. Multiple cluster defaults or no usable default mark the claim `Failed` and
  cause `KeepFS` pause to fail closed before Pod deletion.

### 9. Stable sandbox ID, list, get, delete

- The public `sandboxId` continues to map to a single `BatchSandbox` identity.
  Pause no longer deletes the `BatchSandbox`; it deletes only the Pod and records
  the snapshot inline, so the object identity is stable across pause and resume.
- `GET /sandboxes/{id}` reads one object. Paused sandboxes are ordinary
  `BatchSandbox` objects with `status.phase = Paused`.
- `GET /sandboxes` lists `BatchSandbox` objects directly; paused sandboxes appear
  without a separate snapshot listing.
- `DELETE /sandboxes/{id}` deletes the `BatchSandbox`, which removes the inline
  snapshot state. Backend artifact cleanup remains best-effort and must not block
  deletion success.

> Note: keeping the `BatchSandbox` alive across pause (rather than deleting it as
> OSEP-0008 does) is a deliberate simplification enabled by inline status. It
> removes the need for the server to reconstruct a workload from a snapshot
> template, since the `spec.template` is still present.

### 10. Configuration

Server-side pause configuration provides the defaults that make the public API
backward-compatible. The claim selector and claim-template defaults are optional
migration/defaulting inputs used when the server or controller creates a
namespace-local default claim:

```toml
[pause]
# Used when POST /pause omits operatingMode.
default_operating_mode = "KeepFS"

# Preferred: name of a shared SandboxSnapshotClaim in the sandbox namespace.
snapshot_claim = "default-claim"

# Optional: template used to generate a distinct per-sandbox/pod claim when
# snapshot_claim is empty or a caller explicitly requests a template-backed claim.
snapshot_claim_template = "tenant-s3"

# Whether trusted API callers may submit inline claim specs. Public hosted
# deployments can keep this false and allow only shared claim names or template
# names for generated per-sandbox/pod claims.
allow_inline_claim = false

# Optional: class used when creating/defaulting SandboxSnapshotClaims.
snapshot_class = "default"

# Migration hints (optional). When snapshot_claim is empty and these are set,
# the controller synthesizes an implicit default SandboxSnapshotClass plus a
# namespace-local SandboxSnapshotClaim from them.
snapshot_registry = ""
```

Backend credentials are never read from this config; they are provided by
node/workload identity in the runtime environment.

Controller-side, the snapshot backend flags from OSEP-0008 are retained only to
seed the implicit default `SandboxSnapshotClass` and namespace-local `SandboxSnapshotClaim`.
New deployments should define `SandboxSnapshotClass`, `SandboxSnapshotClaim`, and, when
per-sandbox storage isolation is desired, `SandboxSnapshotClaimTemplate` objects instead.

### 11. Security considerations

The trust model is identical to OSEP-0008: the `KeepFS` snapshot Job mounts the
node container runtime socket and captures writable filesystem state, a
privileged, node-level operation. Future `Hibernate` expands that privileged
capture to memory/process state as well. Operational constraints:

- The snapshot Job image is controller-configured, not user-selectable.
- The snapshot Job spec is not user-extensible.
- `SandboxSnapshotClass` objects are cluster-scoped and managed by cluster
  administrators; backend access is granted by node/workload identity.
- `SandboxSnapshotClaim` objects are namespaced and can be managed by the server,
  namespace operator, or tenant automation. They name only storage targets;
  no credentials are stored, keeping secrets out of the public API and cluster.
- Public hosted deployments should expose only shared claim names or template
  names for generated per-sandbox/pod claims by default.
  Inline claim creation is useful for trusted control planes but should be gated
  by `pause.allow_inline_claim` because it lets callers choose storage locations.
- Checkpoint artifacts contain process memory and may include user data,
  credentials loaded in memory, file descriptors, and application buffers. Backend
  classes must require encryption at rest, tightly scoped access, retention
  limits, and auditable deletion.

### 12. Potential Go type changes

The implementation should express this proposal in Go API types first and then
regenerate CRDs, deepcopy code, clients, and Helm CRD templates from those types
(`make manifests generate`). The following sketch is intentionally human-readable
and omits generated OpenAPI YAML.

#### `batchsandbox_types.go`

`BatchSandboxSpec` replaces the boolean pause field with an `OperatingMode` enum
and adds direct snapshot strategy config. `BatchSandboxStatus` gains a fixed-size inline
snapshot result.

```go
// +kubebuilder:validation:Enum=Running;Freeze;KeepFS;Hibernate
type BatchSandboxOperatingMode string

const (
    BatchSandboxOperatingModeRunning   BatchSandboxOperatingMode = "Running"
    BatchSandboxOperatingModeFreeze    BatchSandboxOperatingMode = "Freeze"
    BatchSandboxOperatingModeKeepFS    BatchSandboxOperatingMode = "KeepFS"
    BatchSandboxOperatingModeHibernate BatchSandboxOperatingMode = "Hibernate"
)

type SnapshotStrategy struct {
    // SnapshotClaimName references a SandboxSnapshotClaim in the BatchSandbox namespace.
    // The server may inject a shared default claim or the generated per-sandbox/pod claim name when this is empty.
    // +optional
    SnapshotClaimName string `json:"snapshotClaimName,omitempty"`

    // SnapshotClaimTemplate references a SandboxSnapshotClaimTemplate in the
    // BatchSandbox namespace plus optional per-use parameters. The server copies
    // it into a concrete SandboxSnapshotClaim for this sandbox/pod before setting
    // SnapshotClaimName.
    // +optional
    SnapshotClaimTemplate *SnapshotClaimTemplateSelector `json:"snapshotClaimTemplate,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Committing;Ready;Failed
type SnapshotPhase string

const (
    SnapshotPhasePending    SnapshotPhase = "Pending"
    SnapshotPhaseCommitting SnapshotPhase = "Committing"
    SnapshotPhaseReady      SnapshotPhase = "Ready"
    SnapshotPhaseFailed     SnapshotPhase = "Failed"
)

type SnapshotArtifact struct {
    ContainerName string `json:"containerName"`
    ArtifactURI   string `json:"artifactUri"`
    // +optional
    Digest string `json:"digest,omitempty"`
    // +optional
    Format string `json:"format,omitempty"`
}

type SnapshotCompatibility struct {
    // +optional
    OS string `json:"os,omitempty"`
    // +optional
    Architecture string `json:"architecture,omitempty"`
    // +optional
    KernelVersion string `json:"kernelVersion,omitempty"`
    // +optional
    RuntimeName string `json:"runtimeName,omitempty"`
    // +optional
    RuntimeVersion string `json:"runtimeVersion,omitempty"`
    // +optional
    CheckpointFormatVersion string `json:"checkpointFormatVersion,omitempty"`
}

type SnapshotStatus struct {
    // +optional
    Phase SnapshotPhase `json:"phase,omitempty"`
    // +optional
    OperatingMode BatchSandboxOperatingMode `json:"operatingMode,omitempty"`
    // +optional
    SnapshotClaimName string `json:"snapshotClaimName,omitempty"`
    // +optional
    SnapshotClassName string `json:"snapshotClassName,omitempty"`
    // +optional
    Artifacts []SnapshotArtifact `json:"artifacts,omitempty"`
    // +optional
    SourcePodName string `json:"sourcePodName,omitempty"`
    // +optional
    SourcePodUID string `json:"sourcePodUID,omitempty"`
    // +optional
    SourceNodeName string `json:"sourceNodeName,omitempty"`
    // +optional
    RuntimeHandler string `json:"runtimeHandler,omitempty"`
    // +optional
    Compatibility *SnapshotCompatibility `json:"compatibility,omitempty"`
    // +optional
    ReadyAt *metav1.Time `json:"readyAt,omitempty"`
    // +optional
    Message string `json:"message,omitempty"`
}

type BatchSandboxSpec struct {
    // ... existing fields ...
    // +kubebuilder:default=Running
    // +optional
    OperatingMode BatchSandboxOperatingMode `json:"operatingMode,omitempty"`
    // +optional
    SnapshotStrategy *SnapshotStrategy `json:"snapshotStrategy,omitempty"`
}

type BatchSandboxStatus struct {
    // ... existing fields ...
    // +optional
    Snapshot *SnapshotStatus `json:"snapshot,omitempty"`
}
```

#### `snapshotclass_types.go`

`SandboxSnapshotClass` is cluster-scoped and contains only backend configuration. It
does not reference Secrets.

```go
// +kubebuilder:validation:Enum=Local;BlobStore;S3
type SnapshotBackendType string

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=snapclass
// +kubebuilder:printcolumn:name="TYPE",type="string",JSONPath=".type"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type SandboxSnapshotClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    // +kubebuilder:validation:Enum=Local;BlobStore;S3
    // +kubebuilder:validation:Required
    Type SnapshotBackendType `json:"type"`

    // +optional
    Parameters map[string]string `json:"parameters,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotClassList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SandboxSnapshotClass `json:"items"`
}
```

#### `snapshotclaim_types.go`

`SandboxSnapshotClaim` is namespaced. It selects a `SandboxSnapshotClass` and carries
namespace-local target parameters; backend access uses node/workload identity.

```go
// +kubebuilder:validation:Enum=Pending;Bound;Failed
type SandboxSnapshotClaimPhase string

const (
    SandboxSnapshotClaimPhasePending SandboxSnapshotClaimPhase = "Pending"
    SandboxSnapshotClaimPhaseBound   SandboxSnapshotClaimPhase = "Bound"
    SandboxSnapshotClaimPhaseFailed  SandboxSnapshotClaimPhase = "Failed"
)

type SandboxSnapshotClaimSpec struct {
    // Empty means use the annotated cluster default SandboxSnapshotClass.
    // +optional
    SnapshotClassName string `json:"snapshotClassName,omitempty"`

    // Backend-specific target settings such as region, bucket/container,
    // prefix, storage account, repository, local path, compressionMode, or
    // compressionAlgorithm.
    // +optional
    Parameters map[string]string `json:"parameters,omitempty"`
}

type SandboxSnapshotClaimStatus struct {
    // +optional
    Phase SandboxSnapshotClaimPhase `json:"phase,omitempty"`
    // +optional
    SnapshotClassName string `json:"snapshotClassName,omitempty"`
    // +optional
    Message string `json:"message,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snapclaim
// +kubebuilder:printcolumn:name="CLASS",type="string",JSONPath=".status.snapshotClassName"
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type SandboxSnapshotClaim struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   SandboxSnapshotClaimSpec   `json:"spec,omitempty"`
    Status SandboxSnapshotClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotClaimList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SandboxSnapshotClaim `json:"items"`
}
```

#### `snapshotclaimtemplate_types.go`

`SandboxSnapshotClaimTemplate` is namespaced and mirrors DRA `ResourceClaimTemplate`.
The server uses it to create distinct concrete `SandboxSnapshotClaim` objects for
individual sandboxes.

```go
type SandboxSnapshotClaimTemplateSpec struct {
    // +optional
    Metadata metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec SandboxSnapshotClaimSpec `json:"spec"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=snapclaimtmpl
type SandboxSnapshotClaimTemplate struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec SandboxSnapshotClaimTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotClaimTemplateList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []SandboxSnapshotClaimTemplate `json:"items"`
}
```

The new root objects must be registered with `SchemeBuilder`, and controller RBAC
markers should be added for reading `SandboxSnapshotClass`, reading/updating
`SandboxSnapshotClaim` status, reading `SandboxSnapshotClaimTemplate`, creating generated
`SandboxSnapshotClaim` objects, using node/workload identity for backend access.

## Test Plan

### Unit tests

- Patching `spec.operatingMode=KeepFS` with `spec.snapshotStrategy` drives
  `status.snapshot.phase` `Pending -> Committing -> Ready` and populates
  `artifacts[]`.
- `POST /pause` with no body remains accepted and resolves to
  `pause.default_operating_mode` plus default shared-claim or template-backed
  generated-claim configuration.
- `POST /pause` with `snapshotClaimName` reuses the named shared
  `SandboxSnapshotClaim` without creating or mutating a claim.
- `POST /pause` with `snapshotClaimTemplate` creates or selects the generated
  `SandboxSnapshotClaim` for that sandbox/pod, merges allowed per-use parameters,
  and patches
  `BatchSandbox.spec.snapshotStrategy.snapshotClaimName` to the generated
  claim name.
- The controller resolves an explicit `snapshotClaimName`, then resolves the
  claim's explicit or default `SandboxSnapshotClass`.
- `KeepFS` pause fails closed (`status.snapshot.phase=Failed`, `PauseFailed`
  condition, Pod retained) when no `SandboxSnapshotClaim` or `SandboxSnapshotClass` resolves.
- `POST /pause` with `operatingMode=Hibernate` is rejected with `400`/`501` until
  checkpoint/restore support implements memory/process-state capture.
- Future `Hibernate` resume fails closed with `ResumeFailed` when the recorded
  `status.snapshot.compatibility` (OS/architecture, kernel/runtime version, or
  checkpoint format), runtime handler, or artifact format is incompatible with the
  target node/runtime.
- The snapshot Job is pinned to `status.snapshot.sourceNodeName`.
- The aggregate `status.phase` returns `Pausing`/`Paused`/`Resuming` consistent
  with `spec.operatingMode` and `status.snapshot.phase`.
- Resume with `spec.operatingMode=Running` re-creates the Pod from the recorded
  `status.snapshot` reference using node/workload identity.
- Overlapping pause/resume transitions are rejected with `409`.
- `GET`/`LIST` answer from `BatchSandbox` alone (no `SandboxSnapshot` read).

### Integration / e2e tests (Kind)

- End-to-end `KeepFS` pause: Running -> filesystem artifact Ready -> Pod
  deleted -> `GET` returns `Paused`, all from one `BatchSandbox`.
- End-to-end resume: Pod restored from the recorded filesystem artifact -> `GET`
  returns `Running`; working directory contents survive and in-memory
  counters/processes restart.
- Repeat pause/resume: `status.snapshot` is replaced; only the latest is kept.
- Delete a paused sandbox: the `BatchSandbox` and its inline snapshot state are
  removed.
- `SandboxSnapshotClaim`/`SandboxSnapshotClass` selection: two claims pointing at different
  classes route the commit to the correct backend/target.
- `SandboxSnapshotClaimTemplate` selection: two sandboxes using the same template
  produce distinct generated claims, one per sandbox/pod, with template values
  copied independently.

### Manual / operator validation

- Confirm the checkpoint artifact lands in the backend target resolved from
  `SandboxSnapshotClaim -> SandboxSnapshotClass`.
- Confirm CPU/memory are released after `KeepFS` pause (Pod deleted).
- Confirm the snapshot Job runs on the source node and commits every sandbox
  container filesystem, including sidecars.

## Drawbacks

- `BatchSandbox` is no longer deleted on pause, so a paused sandbox still occupies
  an API object (but no compute). This is an intentional trade for a single source
  of truth and simpler resume.
- Inline status couples snapshot progress to the `BatchSandbox` object; very large
  status objects are avoided by keeping a single fixed-shape struct with no
  history.
- Introducing `SandboxSnapshotClass`/`SandboxSnapshotClaim`/`SandboxSnapshotClaimTemplate` adds new
  CRDs and RBAC surfaces.
- Only one snapshot is retained; rollback to older states is still impossible.
- The single-replica assumption from OSEP-0008 carries over.

## Alternatives

### Keep the separate `SandboxSnapshot` CR (OSEP-0008)

Rejected for this proposal's goals. It splits one sandbox's state across two
objects, requires a dedicated reconciler and cross-object ownership, and ties the
snapshot backend to global controller flags. This OSEP intentionally collapses it
into the `BatchSandbox` object. `SandboxSnapshot` may still exist for the
*explicit* snapshot API (`osb-snap-*`), which is out of scope here.

### Overload `spec.replicas` (the `/scale` pivot)

Rejected for the same reasons agent-sandbox KEP-0694 rejected it: native tools
(HPA/KEDA) expect `replicas: 0` to mean clean deletion, so overloading it to mean
"hibernate" risks accidental latency, surprise storage cost, and data-loss
ambiguity. `spec.operatingMode` stays the explicit, semantic trigger.

### Strongly-typed `SnapshotProvider` instead of `SandboxSnapshotClass`

Rejected in favor of the class/shared-claim/per-sandbox-template paradigm from
`StorageClass`/PVC and DRA. `SandboxSnapshotClass` keeps a small typed discriminator
(`Local`, `BlobStore`, `S3`) for operator UX, but backend-specific details
stay in `parameters` so new backends do not require a new provider CRD.

### Embed full backend config inline on `BatchSandbox.spec`

Rejected because it would push backend endpoints and storage placement details
into every sandbox spec, duplicate configuration, and leak admin-owned settings
into user-owned objects. `SandboxSnapshotClass` plus `SandboxSnapshotClaim`/`SandboxSnapshotClaimTemplate`
keeps a clean admin/namespace boundary: direct claims are shared namespace-local
storage targets, while templates and inline claim parameters are materialized
into generated per-sandbox/pod claims rather than embedded on
`BatchSandbox.spec`.

## Infrastructure Needed

- `SandboxSnapshotClass`, `SandboxSnapshotClaim`, and `SandboxSnapshotClaimTemplate` CRDs, plus
  controller/server RBAC to read classes/templates and read/update/create claims.
- A snapshot backend reachable from cluster nodes, with access granted via
  node/workload identity (no Kubernetes Secrets).
- A trusted snapshotter/restore image plus node runtime support for checkpointing
  and restoring all containers in a Pod. Clusters must advertise compatible
  runtime handler, OS/architecture, kernel, and checkpoint format support.
- Controller RBAC to manage Jobs and read Pods (the `SandboxSnapshot` reconciler
  RBAC for the pause path is only needed on clusters still served by the OSEP-0008
  fallback path).

## Upgrade & Migration Strategy

This change is additive for the public API and migrates the in-cluster mechanism.

- **Public API:** `POST /pause` gains an optional body. Existing clients keep
  calling `/pause`, `/resume`, and `GET` with the same payloads because missing
  pause bodies and missing fields default to configured values.
- **CRD rollout:** install the `SandboxSnapshotClass`, `SandboxSnapshotClaim`, and
  `SandboxSnapshotClaimTemplate` CRDs and update the `BatchSandbox` CRD with
  `spec.operatingMode`, `spec.snapshotStrategy`, and `status.snapshot`
  (`make manifests generate`). Because `operatingMode` replaces the legacy
  boolean `spec.pause`, operators need a storage-version/conversion or pre-rollout
  migration that maps `pause=true` to `operatingMode=KeepFS`,
  `pause=false`/unset to `operatingMode=Running`, and then drops the boolean field
  from stored objects. CRD installation is **not** a hard preflight: until the new
  snapshot CRDs are present the server falls back to the OSEP-0008 `SandboxSnapshot`
  path via CRD detection (see [§8](#8-server-mapping-and-public-api-compatibility)),
  and the inline path activates automatically once the CRDs exist.
- **Backend config migration:** when no `SandboxSnapshotClaim` is referenced, the server
  uses `pause.snapshot_claim` or creates/selects a namespace-local default claim.
  When no `SandboxSnapshotClass` is referenced by that claim and none is annotated
  default, the controller synthesizes an implicit default class from the legacy
  startup flags (`--snapshot-registry`,
  `--snapshot-registry-insecure`) so existing clusters keep working with zero
  config changes. Operators are encouraged to define explicit `SandboxSnapshotClass` and
  `SandboxSnapshotClaim`/`SandboxSnapshotClaimTemplate` objects and set
  `pause.snapshot_claim` or `pause.snapshot_claim_template` in the server.
- **`SandboxSnapshot` coexistence (pause/resume path):** the
  `SandboxSnapshotReconciler` remains the active pause/resume path on clusters
  that have not installed the new snapshot CRDs, and the server falls back to it
  via CRD detection (see [§8](#8-server-mapping-and-public-api-compatibility)).
  Once the new CRDs are present, the inline path takes over and the
  `SandboxSnapshot` pause/resume path can be decommissioned. The CRD and
  reconciler may also be retained for the explicit snapshot API; this OSEP does
  not delete it.
- **Rollout sequence:**
  1. Install/extend CRDs.
  2. Deploy the controller/server that reconciles `spec.operatingMode` and
      `spec.snapshotStrategy` inline,
     reads `SandboxSnapshotClaim`/`SandboxSnapshotClass`, and materializes one
     generated claim per sandbox/pod from `SandboxSnapshotClaimTemplate` when
     requested.
  3. Create one or more `SandboxSnapshotClass` objects (mark one default).
  4. Create namespace-local `SandboxSnapshotClaim` or `SandboxSnapshotClaimTemplate` objects
     that reference those classes; backend access uses node/workload identity.
  5. Set `pause.snapshot_claim` or `pause.snapshot_claim_template` in the server
     (optional; default claim otherwise).
  6. Decommission `SandboxSnapshot`-based pause once e2e parity is confirmed.
- **Rollback:** rollback to the OSEP-0008 controller is safe only when no
  sandboxes are currently paused under OSEP-0015. If paused OSEP-0015 sandboxes
  exist, operators must either resume them before rollback or run a downgrade
  bridge that materializes compatible `SandboxSnapshot` objects from
  `BatchSandbox.status.snapshot` before deploying the old controller. Old clients
  remain compatible throughout because they do not send the optional pause body.
