---
title: Sandbox Fleets Runtime (fast-sandbox backend)
authors:
  - "@fengcone"
  - "@Pangjiping"
creation-date: 2026-02-08
last-updated: 2026-09-01
status: provisional
---
# OSEP-0007: Sandbox Fleets Runtime (fast-sandbox backend)

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Architecture](#architecture)
  - [API surface](#api-surface)
  - [The `fleets` runtime type and API reuse model](#the-fleets-runtime-type-and-api-reuse-model)
  - [Template-based sandbox Create (spec)](#template-based-sandbox-create-spec)
  - [Template Management API](#template-management-api)
  - [Policy proxy (spec)](#policy-proxy-spec)
  - [Egress / Network Policy](#egress--network-policy)
  - [Status Mapping](#status-mapping)
  - [Extensions Field Support](#extensions-field-support)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Construction Phases](#construction-phases)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)

<!-- /toc -->

## Summary

Introduce a new OpenSandbox backend type, **`sandbox fleets`**, backed by [fast-sandbox](https://github.com/opensandbox-group/fast-sandbox). A fleet runs many sandboxes as isolated runtimes (container / gVisor / Kata / Firecracker) inside pre-warmed **Fastlet** pods, reached through fast-sandbox's gRPC Fast-Path control plane and its authenticated proxy data plane. The architecture removes the per-sandbox K8s scheduler, watch-propagation, and kubelet path from the create hot path; any cross-backend latency claim still requires a reproducible OpenSandbox end-to-end benchmark.

`fleets` is **additive and parallel** to `docker` / `kubernetes` and coexists at the **instance level**: one server may serve both backends. The integration is deliberately scoped:

- **Create** reuses the existing `POST /sandboxes` route: a new **`templateId`** field switches it to template-based creation. `templateId` is mutually exclusive with `image`/`snapshotId`, and in template mode `entrypoint`/`env`/`resourceLimits`/`resourceRequests`/`volumes`/`platform`/`credentialProxy`/`secureAccess`/`lifecycle` are rejected — the workload shape is fixed by the template's golden image.
- **Template management** is a first-class API surface (`POST/GET/DELETE /templates`). The server persists template metadata in its store (SQLite/PostgreSQL), resolves it to a fast-sandbox `SandboxTemplate` CRD, and returns the build asynchronously (201 + `Pending`). Templates are **tenant-private**.
- **Lifecycle** (get / delete / renew-expiration / list / metadata) and **execd** (exec / file) reuse the existing public contracts and response models unchanged, so upstream SDKs are unaffected. Shared lifecycle routes dispatch to the owning backend by sandbox ID prefix (`flt-` vs plain UUID).
- **Egress** (`network_policy`) is delivered through fast-sandbox's **Sandbox Actions** mechanism (an `egress` action binding enforced by a Pool-declared handler). Runtime policy read/update/delete goes through a shared **policy proxy** (`/sandboxes/{id}/networkpolicy`) that rewrites the binding via `UpdateSandbox` for fleets and proxies the sandbox-side sidecar for other backends.
- `credential_proxy` is **not** supported on `fleets`.

**Implementation baseline.** This revision is aligned to fast-sandbox master: **FastPath v2** (atomic expiry/metadata in Create, `action_bindings` for Sandbox Actions, named-component/raw-port endpoint resolution) and the `sandbox.fast.io/v1alpha2` CRDs, including **`SandboxTemplate`** (declarative Firecracker golden-image builds). The API design depends on the latter two; earlier revisions (e.g. `aac0c2c`) are not a sufficient baseline. fast-sandbox publishes **no release-grade Sandbox Create benchmark**; its dated engineering baseline measured warm runc creates through `RuntimeReady` at mean 76.02 ms / p95 83.15 ms (concurrency 1, cached artifacts, excluding Infra readiness and the OpenSandbox path) — evidence about the implementation, not a fleets target.

> **Terminology correction**: fast-sandbox has no "fast mode". Every create is **CRD-first**: it ranks in-memory candidates, persists one Sandbox CRD containing the complete initial intent, then performs atomic Fastlet admission. The gRPC entry avoids the K8s *scheduler* and *watch propagation*, not the CRD/etcd write. This OSEP uses fast-sandbox terminology (**Fastlet** / **SandboxPool**), not earlier "Agent"/"AgentPool" drafts.

## Motivation

The Kubernetes runtime's per-sandbox path includes an API write, scheduler and watch propagation, kubelet reconciliation, runtime startup, and possibly an image pull. OpenSandbox already mitigates this with a BatchSandbox **pool** (`extensions.poolRef`), which removes pod startup but retains the K8s API write and watch propagation. For AI-agent and serverless workloads that need rapid provisioning, fast-sandbox additionally removes scheduler/watch/kubelet work from the per-sandbox hot path while still committing durable intent through a synchronous CRD write. Its placement is an in-memory Top-K registry (image-cache affinity, then normalized load, then stable-hash tiebreak) with atomic Fastlet admission; runtimes are created through a direct containerd socket inside warm Fastlet pods. K8s is retained for what it is good at — Fastlet Pod resource accounting, cluster management, and pool-granularity scheduling. This OSEP sets no universal absolute-latency threshold; the acceptance benchmark must measure fleets vs the pool backend end to end under the same environment.

### Goals

- Add a `fleets` runtime type as a new `FleetSandboxService` (both `SandboxService` and `ExtensionService`, **not** a Kubernetes `WorkloadProvider`), coexisting with `docker` / `kubernetes` in the same server instance
- Provide a **template management API** (`POST` / `GET` / `DELETE /templates`) backed by server-persisted metadata and fast-sandbox `SandboxTemplate` CRD builds; templates are tenant-private
- Extend **`POST /sandboxes` with a `templateId`** field that selects template-based creation; `templateId` is mutually exclusive with the standard create fields, and template mode accepts only `templateId` + `timeout` + `metadata` + `networkPolicy` + `extensions` (optional `extensions.poolRef` overrides the server-configured default pool)
- Route shared lifecycle requests by sandbox ID prefix (`flt-` for fleets, plain UUID for existing backends) without a registration store
- Reuse the existing lifecycle API and response models (`CreateSandboxResponse` / `Sandbox`) and the execd exec/file pattern with no breaking changes to routes or SDKs
- Deliver `network_policy` via fast-sandbox **Sandbox Actions** and serve runtime policy CRUD through a shared **policy proxy** backed by `UpdateSandbox`
- Demonstrate lower p50/p95 user-visible creation latency than the `kubernetes` pool backend, measured from SDK create start until `Running` and the execd endpoint are usable
- Provide flexible deployment: users can bring their own fast-sandbox or use OpenSandbox-provided charts

### Non-Goals

- Replacing or removing the existing Docker or Kubernetes runtimes
- Supporting `volumes`, `platform` node-selectors, `resource_requests`, `credential_proxy`, `snapshot_id`, or `secure_access` on `fleets`
- Supporting `pause` / `resume` / runtime snapshot on `fleets` (fast-sandbox explicit non-goals; template golden-image builds are not runtime snapshots)
- Accepting `image` / `entrypoint` / `env` / `resourceLimits` / `resourceRequests` in template-mode Create — workload shape is fixed at template build time
- Implementing `fleets` as a Kubernetes `WorkloadProvider` or a full fast-sandbox operator
- Changing the OpenSandbox sandbox lifecycle API or SDKs in a breaking way
- Direct management of fast-sandbox `Sandbox` / `SandboxPool` CRDs or Fastlet pods (owned by the fast-sandbox controller)
- Platform-shared (cross-tenant) templates in this revision; templates are tenant-private

## Requirements

- Register as a new `SandboxService` enabled alongside other runtimes; must not modify the `WorkloadProvider` contract
- Implement `ExtensionService` (startup unconditionally requires it for renew-on-access) and map `fleets` to `NoopSnapshotRuntime`
- Keep the public lifecycle and execd contracts and SDKs unchanged; template-mode Create is an additive `templateId` field on the shared request schema, not a new endpoint or a mutation of existing create semantics
- Persist template metadata (id, tenant namespace, build spec, phase, artifact references) in the configured store (SQLite/PostgreSQL), tenant-isolated; resolve sandbox Create only against `Succeeded` templates owned by the current tenant, otherwise 404 (no existence leak)
- Return stable ingress-gateway endpoint handles for ports 44772 (execd), 18080 (policy proxy), and arbitrary user ports without requiring the sandbox route to be ready
- Extend the ingress provider/proxy contract to carry the requested port, complete upstream URL/path, upstream-only headers, and route expiry
- Enforce `network_policy` through the egress action handler (never silently ignored); accept it at create and make it rewritable through the policy proxy
- Map fast-sandbox states to OpenSandbox states, preserving actual-NotFound semantics (missing CRD → 404; retained `Stopped`/expired CRD → `Terminated`) and list page/total semantics (FastPath continue tokens exhausted, then filtered and re-paged)
- Preserve tenant isolation: every namespaced FastPath call resolves the current tenant to a fast-sandbox namespace; the authenticated namespace survives in stable routes and renew intents; locks/throttles/caches key on `(namespace, sandbox_id)`. If this mapping is incomplete, `fleets` must **reject** tenant configuration
- gRPC reachability from the OpenSandbox Server to the fast-sandbox Fast-Path Server

## Proposal

`fleets` is implemented as a `FleetSandboxService` speaking gRPC FastPath v2 to the fast-sandbox Fast-Path Server. It is **not** a Kubernetes `WorkloadProvider`: that ABC is saturated with K8s semantics (namespace, CR metadata, pod-spec mutation) and cannot host a separate gRPC control plane. Choosing a new `SandboxService` is what lets lifecycle routes, exec/file access, and endpoint patterns be reused unchanged — they all funnel through the `SandboxService` ABC and the `get_endpoint` + in-sandbox HTTP contracts rather than through pod semantics.

### Architecture

```
OpenSandbox Server ── lifecycle routes ──> SandboxService (ABC)
                       │                        │
              FleetSandboxService        get_endpoint()
               │  gRPC FastPath (9090)         │
               ▼                                ▼
        fast-sandbox Fast-Path        ingress gateway (tenant + sandbox_id + port)
        (in-memory Top-K → CRD write  → Fastlet Proxy / Sandbox Proxy)
               ▼
        Fastlet Pod: many sandbox runtimes via direct containerd,
        each with own netns + private IP; execd :44772; egress handler :18080
```

Data flow per create (cached artifacts): `OpenSandbox → gRPC Fast-Path → in-memory Top-K → K8s API (Sandbox CRD write) → atomic Fastlet admission → containerd`. The scheduler and watch propagation are bypassed; the CRD write is retained and precedes runtime creation.

### API surface

| Endpoint | Backend | Notes |
| --- | --- | --- |
| `POST/GET /templates`, `GET/DELETE /templates/{templateId}` | fleets | tenant-scoped; 201 + `Pending` async build |
| `POST /sandboxes` | shared | existing route; `templateId` field selects template-based (fleets) creation |
| `GET/PATCH/DELETE /sandboxes/{id}/networkpolicy` | shared | policy proxy; fleets rewrites the egress binding, other backends proxy the sandbox-side sidecar |
| `GET/DELETE /sandboxes/{id}`, list, renew, metadata, endpoints | shared | dispatched by ID prefix |
| `pause` / `resume` / snapshots / logs | — | unsupported on fleets |

### The `fleets` runtime type and API reuse model

`fleets` coexists with `docker` / `kubernetes` **in the same server instance**. Sandbox IDs are server-generated: existing backends keep plain RFC 4122 UUIDs; fleets IDs are `flt-<uuid>`. Shared lifecycle endpoints split by prefix in O(1) with no registration store:

```
GET /sandboxes/{id}  →  id.startswith("flt-")  → FleetSandboxService
                     →  otherwise              → docker / kubernetes service
```

`flt-<uuid>` is valid on the fast-sandbox side because FastPath v2 accepts a client-supplied `request_id` that becomes the Sandbox CRD `metadata.name` — idempotency key, CRD name, and OpenSandbox sandbox ID are the same string.

The "reused" areas reuse *different* things. There is **no server-side exec API endpoint**: exec/file is an HTTP contract clients speak directly to components inside the sandbox; the server's only role is `get_endpoint`.

| Area | Reused | Built for `fleets` |
| --- | --- | --- |
| Lifecycle (get/delete/renew/list/metadata) | Public routes + `SandboxService` ABC | Map to FastPath v2; adapt pagination/error semantics; dispatch by ID prefix |
| execd exec/file | `specs/execd-api.yaml` (client → execd:44772) is backend-agnostic | Declare Pool Infra Component `execd`; map port 44772 → `component_name = "execd"` |
| egress `network_policy` | `NetworkPolicy` schema; `egress-api.yaml` shapes | Serialize to an `egress` action binding; serve runtime CRUD via policy proxy |
| Endpoint resolution | `get_endpoint` → stable `Endpoint` | Tenant-scoped handle before readiness; lazy FastPath route resolution on traffic |

### Template-based sandbox Create (spec)

**`POST /sandboxes` with `templateId` — template-based Create.** The shared route gains one additive field; no new endpoint is introduced.

```yaml
CreateSandboxRequest (template mode):
  templateId: string            # mutually exclusive with image/snapshotId;
                                # tenant-owned template, must be phase=Succeeded
  timeout: integer              # required in template mode; seconds, converted
                                # once to absolute expires_at_unix_seconds
  metadata: map<string,string>
  networkPolicy: NetworkPolicy  # → egress action binding input
  extensions:
    poolRef: string                        # optional; overrides the server-configured
                                           # default SandboxPool (same key as the
                                           # kubernetes pool mode)
    "access.renew.extend.seconds": string  # optional; renew-on-access
```

Validation (server-side, at the same point as the existing `CreateSandboxRequest` model validator):

- `templateId` is **mutually exclusive** with `image` and `snapshotId`; providing it alongside either is a 400
- In template mode, `entrypoint` / `env` / `resourceLimits` / `resourceRequests` / `volumes` / `platform` / `credentialProxy` / `secureAccess` / `lifecycle` are **rejected** (400) — the workload shape is fixed by the template's golden image; they are not silently ignored
- In template mode, `timeout` is required; the standard mode's `image`/`snapshotId` exactly-one rule does not apply
- Pool selection: server-configured `default_pool_ref` (fleets config), optionally overridden by `extensions.poolRef` (the existing kubernetes pool-mode key; not a new field)
- Without `templateId`, the route behaves exactly as today (docker / kubernetes standard and pool modes)

| Response | Detail |
| --- | --- |
| 202 | shared `CreateSandboxResponse`; `id` is `flt-<uuid>`, `entrypoint` populated from the template's guest command |
| 400 | template-mode field conflicts (image/snapshotId/entrypoint/env/resourceLimits/... present), missing `templateId`/`timeout`, invalid `timeout` |
| 401 / 403 | auth / tenant access |
| 404 | unknown `templateId` for the tenant, or template not `Succeeded` (no existence leak) |
| 409 | idempotency conflict (same sandbox ID, changed intent) |
| 429 | pool capacity unavailable before acquisition timeout; `Retry-After` |
| 503 | template artifact or FastPath temporarily unavailable |

Mapping to FastPath `CreateSandboxRequest` (template mode):

| Client field | FastPath mapping |
| --- | --- |
| `templateId` | store lookup `(templateId, namespace)` → artifact reference → `image` |
| (none) | `pool_ref` = `extensions.poolRef` if set, else the configured `default_pool_ref` |
| `timeout` | one absolute `expires_at_unix_seconds` (reused verbatim on retries) |
| `metadata` | `metadata` (labels; OpenSandbox/K8s label validation) |
| `networkPolicy` | `action_bindings[{handler: "egress", input: <policy JSON>}]` |
| `extensions["access.renew.extend.seconds"]` | reserved metadata key (hidden from public metadata/list) |
| (none) | `request_id` = the `flt-<uuid>` sandbox ID; `namespace` = tenant namespace; `completion` = aggregate `READY` |

The fast-sandbox v2 Create persists image, absolute expiry, metadata, Pool, and action bindings atomically in the initial CRD write; there is no follow-up Update or rollback. The same `request_id`, normalized intent, and absolute expiry are idempotent across retries. If Create times out at the readiness wait, return the accepted sandbox as `Pending`; the SDK can still obtain stable lazy gateway endpoints and continue its health loop. On an ambiguous post-persistence error, Get the same namespaced ID and return accepted `Pending` when durable intent exists; never retry with a new ID or recomputed expiry.

### Template Management API

Sandboxes are created from **templates**, not per-request images. A template is a golden image whose build is declared and executed by fast-sandbox: `SandboxTemplate` (`sandbox.fast.io/v1alpha2`) converts a source OCI image into a bootable rootfs plus a validated full snapshot (`vmstate.snap` + `memory.snap`), optionally OverlayBD-packaged, and publishes the digest-addressed artifacts to an S3-compatible object store. Build inputs not exposed as template client fields — kernel, machine sizing, execd injection, guest init, readiness — are supplied by the server from its own configuration (see the note below). At runtime the published artifact reference is what FastPath persists as `SandboxSpec.Image`; the runtime agent pulls and restores it on demand. The workload shape is therefore fixed at build time, which is why template-mode Create rejects image/entrypoint/env/resource fields. OpenSandbox owns the mapping from public `templateId` to the published S3 artifact.

#### API Contract (spec)

Routes live under `/templates` (mounted at the `/v1` API prefix).

| Method | Path | Status | Description |
| --- | --- | --- | --- |
| POST | `/templates` | 201 | Accept a build; server generates `templateId`, persists the row, creates the CRD, returns `phase: Pending`; `Location` header |
| GET | `/templates` | 200 | List current tenant's templates, shared pagination envelope |
| GET | `/templates/{templateId}` | 200 | One template (status + artifact refs); 404 for unknown/other-tenant ids |
| DELETE | `/templates/{templateId}` | 204 | Delete CRD + DB row; does not affect sandboxes already created (their Sandbox CRDs hold their own image references) |

**`POST /templates` — `CreateFleetsTemplateRequest`** (simplified build intent; `additionalProperties: false`):

```yaml
CreateFleetsTemplateRequest:
  required: [image, publish]
  properties:
    image:      string              # source OCI image (registry reference)
    resourceLimits: ResourceLimits  # optional; reuses the shared CreateSandboxRequest
                                    # schema (map<string,string> of Kubernetes
                                    # quantities, e.g. {"cpu": "2", "memory": "2Gi"})
    entrypoint: array[string]       # optional; guest business command (argv)
    metadata:   map<string,string>  # optional; same semantics as CreateSandboxRequest
                                    # metadata (labels; filtering, management, tagging)
    readiness:  object              # optional; build-side readiness gate
      probe:          string        #   e.g. "tcp://127.0.0.1:44772" or "cmd://..."
      warmupSeconds:  integer       #   fallback warmup; default 60
    publish:    string              # required; S3-compatible target "s3://bucket/path"
    format:     enum[native, overlaybd]  # optional; default overlaybd
  additionalProperties: false
```

> **Server-side build inputs.** Kernel, execd image, and guest environment variables are not template client fields in this revision: the server supplies kernel and execd defaults from its `[fleets]` configuration and injects them into the `SandboxTemplate` CRD as needed.

Errors: `400` (validation), `401`, `409` (CRD name conflict on retry), `500`.

**`FleetsTemplate`** (returned by POST/GET; `additionalProperties: false`):

```yaml
FleetsTemplate:
  templateId:   string        # server-generated "tpl_<uuid>"
  image:        string
  resourceLimits: ResourceLimits
  entrypoint:   array[string]
  metadata:     map<string,string>
  readiness:    object        # { probe, warmupSeconds }
  publish:      string
  format:       enum[native, overlaybd]
  status:
    phase:          enum[Pending, Building, Succeeded, Failed]
    manifestRef:    string    # S3 manifest reference; present when Succeeded
    lastBuildTime:  datetime
    message:        string    # failure reason when Failed
  createdAt:    datetime
  updatedAt:    datetime
```

**`GET /templates`** — `FleetsTemplateListResponse` (shared pagination envelope, same field semantics as `ListSandboxesResponse`): `items: array[FleetsTemplate]`, `page`, `pageSize`, `totalItems`, `totalPages`, `hasNextPage` (all required). Query parameters mirror the sandbox list contract:

| Parameter | Semantics |
| --- | --- |
| `page` / `pageSize` | pagination (defaults match the shared list contract) |
| `name` | optional exact match on the template name |
| `metadata` | optional URL-encoded key-value pairs, AND logic, same wire format as `GET /sandboxes` (e.g. `?metadata=project%3DApollo%26env%3Dprod`) |

Filtering is evaluated in the store against the persisted `metadata_json` column, so list queries do not page through fast-sandbox. Unknown/other-tenant templates are never returned; results are always scoped to the current tenant's `namespace`.

**Build lifecycle.** `POST` returns 201 + `Pending` immediately; clients poll `GET /templates/{templateId}` until `Succeeded` (or `Failed` with `message`). Only `Succeeded` templates can be referenced by template-mode Create; any other phase yields 404. A `Failed` template can be deleted and rebuilt under a new `templateId`.

#### Persistence (DB schema)

Template metadata is authoritative in the server's configured store (SQLite or PostgreSQL), mirroring the existing snapshot repository pattern (`repositories/snapshots/`). The store is the source of truth for the public catalog; the CRD is the execution projection.

**SQLite schema** (initialization mirrors `repositories/snapshots/sqlite.py`):

```sql
CREATE TABLE IF NOT EXISTS fleets_templates (
    template_id      TEXT PRIMARY KEY,
    namespace        TEXT NOT NULL,
    crd_name         TEXT NOT NULL,
    name             TEXT,
    spec_json        TEXT NOT NULL,          -- normalized CreateFleetsTemplateRequest (JSON)
    metadata_json    TEXT NOT NULL DEFAULT '{}',  -- user metadata (JSON object)
    source_image     TEXT NOT NULL,
    publish          TEXT NOT NULL,
    format           TEXT NOT NULL DEFAULT 'overlaybd',
    phase            TEXT NOT NULL,          -- Pending | Building | Succeeded | Failed
    manifest_ref     TEXT,
    message          TEXT,
    created_at       TEXT NOT NULL,          -- ISO-8601 UTC
    updated_at       TEXT NOT NULL,
    UNIQUE (namespace, crd_name)
);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_namespace  ON fleets_templates(namespace);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_phase      ON fleets_templates(phase);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_created_at ON fleets_templates(created_at DESC);
```

**PostgreSQL schema** (initialization mirrors `repositories/snapshots/postgresql.py`):

```sql
CREATE TABLE IF NOT EXISTS fleets_templates (
    template_id      TEXT PRIMARY KEY,
    namespace        TEXT NOT NULL,
    crd_name         TEXT NOT NULL,
    name             TEXT,
    spec_json        JSONB NOT NULL,
    metadata_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_image     TEXT NOT NULL,
    publish          TEXT NOT NULL,
    format           TEXT NOT NULL DEFAULT 'overlaybd',
    phase            TEXT NOT NULL,
    manifest_ref     TEXT,
    message          TEXT,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    UNIQUE (namespace, crd_name)
);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_namespace  ON fleets_templates(namespace);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_phase      ON fleets_templates(phase);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_created_at ON fleets_templates(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_fleets_templates_spec_image ON fleets_templates((spec_json ->> 'image'));
CREATE INDEX IF NOT EXISTS idx_fleets_templates_metadata   ON fleets_templates USING GIN (metadata_json jsonb_path_ops);
```

**Column semantics.** `template_id` = public ID `tpl_<uuid>`, never reused after deletion. `namespace` = tenant's fast-sandbox namespace, the isolation key for every query. `crd_name` = `SandboxTemplate` CRD name in that namespace; `UNIQUE (namespace, crd_name)` protects against double-builds. `spec_json` = normalized build intent stored verbatim at POST (the CRD spec derives from it, so a rebuild is reproducible). `metadata_json` = the user metadata from the template request, also projected onto the CRD labels (with reserved-key protection, like sandbox metadata). `phase` written as `Pending` at create and updated by status sync. `manifest_ref` copied from CRD status on success and consumed by fleets Create as the artifact reference.

**Metadata filtering.** `GET /templates?metadata=...` is evaluated in the store against `metadata_json` with AND semantics. PostgreSQL uses the JSONB containment operator (`metadata_json @> '{"k":"v"}'`) backed by the GIN index; SQLite uses per-key `json_extract(metadata_json, '$.k') = v` checks (correctness over index speed at the expected template catalog scale). The tenant `namespace` predicate is always applied first. Reserved metadata keys follow the sandbox convention (e.g. `opensandbox.io/` prefix rejected at POST).

**Status sync.** The build executes in fast-sandbox; the DB is not authoritative for progress. Reads (`GET /templates...`) first read the CRD status and update the row (`phase`, `manifest_ref`, `message`, `updated_at`) when changed, then respond. An optional background task may refresh `Pending`/`Building` rows; reads already converge, so it is not required. Transition: `Pending → Building → Succeeded | Failed` (reflecting the CRD phase).

**Repository.** `FleetTemplateRepository` implements `create` / `get(template_id, namespace)` / `list(namespace, query)` / `delete(template_id, namespace)` / `update_status(...)`, with SQLite/PostgreSQL backends selected by the existing `[store]` config (`repositories/fleets/factory.py` + `sqlite.py` + `postgresql.py`). An SQLite → PostgreSQL one-shot migration follows the `repositories/snapshots/migrate.py` pattern; since the catalog is only metadata, a fresh table can alternatively re-sync from CRDs on first reads.

#### Multi-tenancy

Templates are **tenant-private**: every row is scoped by `namespace` (the tenant's fast-sandbox namespace, resolved through the same `TenantProvider` as sandboxes), and the `SandboxTemplate` CRD is created in that namespace. List returns only the current tenant's rows; GET/DELETE and sandbox Create resolve `(templateId, namespace)` and return 404 for other tenants (no existence leak). A future platform-shared scope can be added as an additive `scope` column; out of scope for this revision.

### Policy proxy (spec)

Runtime policy operations are served by the server under the shared route **`/sandboxes/{sandboxId}/networkpolicy`**, dispatched by sandbox ID prefix:

- **fleets** (`flt-`): the server itself is the implementation — `GetSandbox` (read current `egress` binding) + `UpdateSandbox(ReplaceActionBindings)` (rewrite). Policy never reaches the sandbox side directly.
- **docker / kubernetes**: the same route acts as a server-side reverse proxy to the sandbox-side sidecar (port 18080), preserving the existing sandbox-side contract for SDK callers.

Request and response shapes are **reused from `specs/egress-api.yaml`** (`PolicyStatusResponse`, `NetworkRule`), so SDK callers see the same JSON regardless of backend.

| Method | Request body | Success | Semantics |
| --- | --- | --- | --- |
| GET | — | 200 `PolicyStatusResponse` | Read current policy; for fleets, read the `egress` binding — empty/absent binding reported as allow-all (`mode: allow_all`) |
| PATCH | `array[NetworkRule]` (minItems 1) | 200 `PolicyStatusResponse` | Merge per egress-api semantics: read current, merge incoming (first target wins; incoming overrides existing), replace whole binding |
| DELETE | `array[string]` targets (minItems 1) | 200 `PolicyStatusResponse` | Remove rules by target (idempotent; missing targets ignored), replace whole binding |

Errors: `400` (malformed rule/target), `401`, `403` (other-tenant sandbox), `404` (unknown sandbox), `409` (binding update conflict), `503` + `Retry-After` (FastPath unavailable). For fleets, the proxy waits for the update to be accepted (`committed_generation`) but not for handler convergence; the `egress` binding is the only binding this revision manages — other bindings are preserved and rewritten in declaration order. PATCH/DELETE removing the last rule clears the binding (`input: null`), which the Actions protocol delivers as removal.

### Egress / Network Policy

**Delivered via fast-sandbox's Sandbox Actions mechanism; not a reuse of Kubernetes NetworkPolicy.**

fast-sandbox delivers per-sandbox opaque configuration to **Action Handlers** — Pool-declared Pod-local extensions (not plugins inside the user's sandbox). A Pool declares handlers with a loopback target port and subscribed lifecycle hooks (`sandbox.runtime-ready`, `sandbox.data-plane-ready`). FastPath v2 carries bindings natively (`action_bindings` on Create, `ReplaceActionBindings` on Update); the Fastlet synchronizes binding input to the handler at each hook with Ready barriers and terminal cleanup on deletion. The **egress handler** is one such handler (the canonical Pool example exposes it on loopback 18080 and consumes the `egress` binding).

Mapping: `networkPolicy` serializes directly into `actionBindings[{handler: "egress", input: <policy JSON>}]` at Create (completion defaults to aggregate `READY`, so the response returns only after the binding is effective); runtime CRUD goes through the policy proxy. Update/delete follows the Actions protocol: retained bindings keep declaration order, removed live bindings are cleared with `SetBinding(input=null)` as a Ready barrier, and sandbox deletion triggers reverse-order terminal `RemoveBinding` cleanup.

Notes:

- Enforcement runs in the egress handler process; co-located sandboxes with different policies are isolated by handler state keyed on sandbox identity in the binding revision
- Enforcement capability (FQDN vs CIDR, DNS mediation) is a property of the deployed handler image, not of the OpenSandbox contract; the proxy reports the effective `enforcementMode` in `PolicyStatusResponse`, and a pool/template that cannot enforce a requested policy must fail explicitly at Create
- `credential_proxy` is not supported on `fleets` in any phase

### Status Mapping

| fast-sandbox `Sandbox` state | OpenSandbox State |
| --- | --- |
| RuntimeState Ready + DataPlaneState Ready (+ bindings Ready) | Running |
| Pending / Creating | Pending |
| Draining (delete in progress) | Stopping |
| Stopped with `reason=Expired` (CRD retained) | Terminated |
| Stopped (CRD retained) | Terminated |
| Failed / Unavailable | Failed |
| Actual missing CRD / gRPC `codes.NotFound` | HTTP 404 (no synthetic Sandbox object) |

fast-sandbox splits `RuntimeReady` from `DataPlaneReady`; OpenSandbox reports **Running only when both (and any bindings) are Ready**, matching the "endpoint usable" expectation. A retained expired CRD maps to `Terminated`; a genuinely missing CRD maps to 404. Delete and expiry are eventual: preflight Get preserves the public DELETE 404 contract, then submit and poll.

### Extensions Field Support

The fleets Create `extensions` field supports exactly two keys:

| Extension Key | Description |
| --- | --- |
| `poolRef` | Optional; overrides the server-configured `default_pool_ref` (same key and semantics as the kubernetes pool mode) |
| `access.renew.extend.seconds` | Decimal string; persisted under a fleets-reserved DNS-safe FastPath metadata key, hidden from public metadata/list filters, served through `ExtensionService` |

All other extension keys are rejected. fast-sandbox `failure_policy` is not exposed; the backend uses FastPath v2's default `MANUAL` policy and 60-second recovery timeout.

### Notes/Constraints/Caveats

- The fast-sandbox control plane (Fast-Path Servers, Reconcilers, Sandbox Proxy), Fastlet pools, egress handler, and NodeJanitor must be deployed separately (by the user or via OpenSandbox-provided Helm charts)
- fast-sandbox uses its own CRD types (`Sandbox`, `SandboxPool`, `SandboxTemplate`, group `sandbox.fast.io/v1alpha2`); OpenSandbox does not manipulate them directly (except creating/deleting `SandboxTemplate` on behalf of the template API)
- execd is injected via fast-sandbox's **Infra Component** mechanism, pinned by OCI digest on the Pool; execd runs without `EXECD_ACCESS_TOKEN` — FastPath route credentials protect the upstream hop, OpenSandbox protects its public gateway, application `Authorization` passes through
- Per-sandbox egress is the egress handler's job, not Kubernetes NetworkPolicy; runtime mutation goes through the policy proxy
- **Tenant isolation** relies on fast-sandbox namespaces (`ListSandboxes` is namespace-only); `FleetSandboxService` must map each tenant to a distinct namespace on every call and carry an authenticated namespace claim into stable routes and background renew work, or reject `[tenants]` configuration
- **Fleets configuration** (`[fleets]`): `fastpath_endpoint` (gRPC address), `default_pool_ref` (default SandboxPool for template mode; overridable per request via `extensions.poolRef`), `execd_component_name` (default `execd`), and `endpoint_access_mode` / `require_ingress_gateway` for the ingress adapter
- `get_sandbox_logs` is unsupported on fleets (execd has no sandbox-entrypoint log endpoint); `inspect`/events are backed by `GetSandboxDiagnostics` (lifecycle events only)

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Fast-Path becomes a single point of failure | Multi-active Fast-Path; retry with the same ID and absolute expiry; read durable intent to disambiguate post-persistence failures |
| gRPC API drift in fast-sandbox | Version pinning; compatibility matrix |
| State drift (sandboxes managed outside OpenSandbox) | Periodic reconciliation via gRPC `GetSandbox` |
| SDK fetches endpoints before readiness | `get_endpoint` returns a stable tenant-scoped gateway URL without FastPath; gateway resolves lazily, returns retryable 503 while Pending |
| Background renew lacks tenant context | Authenticated namespace in stable routes/renew intents; composite `(namespace, sandbox_id)` keys for locks/throttles/caches |
| FastPath pagination ≠ OpenSandbox totals | Follow continue tokens to exhaustion, filter, then compute page/totals |
| NotFound not normalized upstream | Fix `GetSandbox` to return `codes.NotFound`; adapter maps only that code to 404 |
| Egress handler behavior differs from pod sidecar | Same `NetworkPolicy` semantics + same `PolicyStatusResponse` shape; enforcement differences surfaced via `enforcementMode`, never silent |
| Template build fails / artifacts missing | Status persisted and pollable; Create refuses non-`Succeeded` with 404; `message` explains failure |
| Cross-tenant template reference | Every row/CRD scoped to tenant namespace; `(templateId, namespace)` resolution, no namespace scanning |

## Construction Phases

Single implementation track (the earlier phase 1a/1b split is obsolete: network policy ships through the already-available Sandbox Actions mechanism).

- **Template management**: `FleetTemplateRepository` (SQLite/PostgreSQL) + `POST/GET/DELETE /templates`; simplified request → `SandboxTemplate` CRD mapping; lazy status sync; tenant scoping
- **Service seam and coexistence**: `FleetSandboxService(SandboxService, ExtensionService)`, enable alongside other runtimes in `factory.py`, `FleetsRuntimeConfig`, `NoopSnapshotRuntime`, `flt-` ID prefix routing on shared lifecycle routes
- **FastPath v2 client**: Create / Get / Delete / Update / List / Diagnostics / ResolveEndpoint / GetPool / ListPools
- **Template-mode Create**: add `templateId` to `CreateSandboxRequest` with the mutual-exclusion validator; `templateId` (tenant-scoped row, `Succeeded`) → artifact reference; `pool_ref` = `extensions.poolRef` or configured `default_pool_ref`; one absolute expiry, metadata, `networkPolicy` → egress binding, renew extension → reserved metadata key; idempotent across retries; ambiguous-error recovery returns accepted `Pending`
- **Ingress contract and fleets provider**: lazy tenant-scoped `get_endpoint`; gateway resolves namespace + port + complete upstream route on traffic; 44772 → component `execd`; raw ports otherwise; 503 while Pending
- **Policy proxy**: `GET/PATCH/DELETE /sandboxes/{id}/networkpolicy` — fleets backed by `GetSandbox` + `UpdateSandbox(ReplaceActionBindings)`, other backends proxied to the sandbox-side sidecar
- **Lifecycle semantics**: get/delete/renew/list/metadata; reserved metadata kept private; FastPath pages exhausted before filtering and page/total calculation; preflight Delete 404; status/NotFound mapping as specified
- **Exit criteria**: one server serves `fleets` and another runtime side by side; full SDK flow (template build → create → exec → file → policy update → delete) passes on a Kind cluster without SDK changes, including a Create that initially returns `Pending`; tenant isolation, background renewal, list pagination/totals, `network_policy` create + policy-proxy CRUD, and actual-NotFound behavior verified

## Test Plan

- **Unit Tests**: FastPath v2 wrapper; template repository tenant scoping; template request → CRD mapping; template metadata validation (reserved keys) and metadata filter evaluation (SQLite json_extract / PostgreSQL JSONB containment); atomic Create mapping and idempotent retry; ambiguous post-persistence recovery; Pending-safe endpoint discovery; lazy ResolveEndpoint 503/refresh; policy proxy GET/PATCH/DELETE merge semantics; namespace propagation and composite lock keys; `ExtensionService` persistence; status/error mapping; list filtering and page/total calculation
- **Startup Tests**: fleets satisfies unconditional `ExtensionService`; `NoopSnapshotRuntime`; unsupported errors for logs/pause/resume/snapshot
- **Integration Tests**: Kind cluster with fast-sandbox + MinIO + KVM build node; template build lifecycle (Pending → Succeeded/Failed); create/get/delete/renew/list/metadata; template metadata create + `?metadata=` list filtering; actual NotFound vs retained expiry; multi-page list; Pending endpoint discovery → execd readiness; `network_policy` create + policy-proxy CRUD; tenant-scoped background renewal
- **E2E Tests**: full SDK flow (template build → create → exec → file → policy update → delete), lifecycle + exec/file identical to the pod backend
- **Egress Tests**: deny-by-default block; FQDN allowlist permit/expiry; co-located policy isolation; policy-proxy semantics against `egress-api.yaml`; binding restart recovery/cleanup; explicit failure when a pool/template cannot enforce the requested policy
- **Performance Tests**: create latency/density vs the `kubernetes` pool backend under the same environment, cache state, concurrency, and readiness boundary (SDK start → `Running` + execd usable). fast-sandbox's internal `RuntimeReady` is a separate diagnostic milestone, not a substitute. No absolute-millisecond threshold; the acceptance report records commit/env/cache-state/concurrency/percentiles per fast-sandbox methodology

## Drawbacks

- **Added dependency**: deploying and managing the fast-sandbox control plane, Fastlet pools, egress handler, and NodeJanitor
- **Feature gap vs pod backend**: no volumes, pause/resume/snapshot, per-request image/entrypoint/env (workload shape fixed by template), no per-sandbox K8s NetworkPolicy or node scheduling
- **Template build latency**: tens of seconds per build before the first sandbox can be created; templates amortize image conversion and snapshot packaging
- **Egress handler dependency**: enforcement capability depends on the deployed handler image; differences must be surfaced explicitly
- **Operational complexity**: teams need to understand both OpenSandbox and fast-sandbox concepts
- **gRPC on the backend surface** (vs pure HTTP/REST); fast-sandbox is a newer, smaller-community project

## Alternatives

1. **Full replacement of the pod backend** — rejected: volumes/snapshots/NetworkPolicy have no shared-Fastlet equivalent
2. **`fleets` as a K8s `WorkloadProvider`** — rejected: that ABC cannot host a separate gRPC control plane
3. **Declarative CRD path only (no gRPC)** — rejected: loses the in-memory-placement latency benefit
4. **Kubernetes NetworkPolicy for egress** — rejected: all sandboxes SNAT to one Fastlet pod IP; per-sandbox policy is the egress handler's job
5. **Direct `FastletPodIP:port` endpoints** — deferred: port allocation + auth handling in OpenSandbox; the ingress gateway reuses fast-sandbox's authenticated proxy chain
6. **Per-request image Create** — rejected: Firecracker needs a golden image that cannot be produced at sandbox latency; the template API makes the build explicit and amortizable
7. **Global-exclusive `fleets` runtime** — rejected: instance-level coexistence with ID-prefix routing serves both backends from one deployment

## Infrastructure Needed

- **CI/CD**: Kind cluster with fast-sandbox (Fast-Path, Reconcilers, Sandbox Proxy, Fastlet pool, egress handler, NodeJanitor), MinIO, and a KVM build node for template builds
- **Documentation**: fleets deployment guide; template build guide; execd-as-Infra-Component setup; egress handler setup; compatibility matrix
- **Helm Charts** (optional): unified charts deploying OpenSandbox Server + fast-sandbox components
- **Cross-repo coordination**: fast-sandbox maintainers for `GetSandbox` NotFound normalization and the egress action handler contract

## Upgrade & Migration Strategy

- **Backwards compatible**: default runtime unchanged; `fleets` is opt-in by adding it to the enabled runtime list; existing UUID sandbox IDs never collide with `flt-<uuid>`
- **No migration**: existing Docker/Kubernetes users unaffected
- **Enable by config**: add `"fleets"` to `config.runtime.type`, add the `[fleets]` block, deploy fast-sandbox + ingress gateway, then build templates before creating sandboxes
- **Rollback**: remove `"fleets"` from the enabled list; fleets sandboxes are ephemeral and template rows are metadata only
- **Choosing a backend**: use `kubernetes` for volumes, pause/resume/snapshot, per-request image/entrypoint/env, per-sandbox K8s NetworkPolicy, or per-sandbox node scheduling; use `fleets` for latency-sensitive, stateless, high-density workloads whose image set is small enough to amortize into templates, needing lifecycle + exec/file + per-sandbox egress policy
