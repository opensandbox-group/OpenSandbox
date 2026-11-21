---
title: Optional PostgreSQL Sandbox Lifecycle History
authors:
  - "@comqx"
creation-date: 2026-11-13
last-updated: 2026-11-18
status: draft
---

# OSEP-0029: Optional PostgreSQL Sandbox Lifecycle History (Server Writer)

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
  - [Configuration](#configuration)
  - [Internal lifecycle history writer](#internal-lifecycle-history-writer)
  - [Write path and API coupling](#write-path-and-api-coupling)
  - [Phase 1 coverage and known gaps](#phase-1-coverage-and-known-gaps)
  - [Table schema](#table-schema)
  - [Provisioning provenance](#provisioning-provenance)
  - [Observed runtime shape](#observed-runtime-shape)
  - [Delete and terminal state semantics](#delete-and-terminal-state-semantics)
  - [Console BFF reader (follow-up)](#console-bff-reader-follow-up)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Add an **opt-in** lifecycle server writer that upserts sandbox create, metadata label changes, and delete observations into a PostgreSQL table `sandbox_lifecycle_history`, including **per-sandbox** `backend_type` and durable **create provenance** (image, template, snapshot) for all supported provisioning backends. The feature is disabled by default, reuses the existing `[store]` PostgreSQL deployment, does **not** change the public Lifecycle OpenAPI, and treats database errors as **best-effort** (logged, never reflected in API responses).

This OSEP covers the **server writer only**. A separate follow-up (console BFF reader and UI) may read the same table but is out of scope for the initial implementation merge.

## Motivation

Today operators and the optional Developer Console work primarily against **live** lifecycle state (`GET /sandboxes`). Deleted sandboxes, SDK-only creates without console involvement, and historical reporting (usage windows, label filters, image mix) are difficult without a durable store.

[OSEP-0006](0006-developer-console.md) envisioned Phase 2 PostgreSQL storage for console history but is **withdrawn**; the in-tree console is a **BFF adjunct**. The [Developer Console guide](../docs/guides/developer-console.md) states that sandbox history/analytics are **planned for a later release** and that the MVP console uses the Lifecycle API only — it does **not** document any server config key today. Forward-looking console design notes ([`console/docs/07-sandbox-history.md`](../console/docs/07-sandbox-history.md), [`05-feature-matrix.md`](../console/docs/05-feature-matrix.md), [`00-design-principles.md`](../console/docs/00-design-principles.md)) describe optional PostgreSQL-backed history as **post-MVP** and explicitly defer enabling such paths until specified. **This OSEP newly proposes** `[store.lifecycle_history]` in the server; no `lifecycle_history` (or legacy `lifecycle_audit`) key exists under `server/` on `main` yet.

### Goals

1. Durably record sandbox **create** and **delete** (and **metadata label map** updates on open rows) when history writing is enabled, for **all provisioning backends** the server can route today (`kubernetes`, `fast-sandbox`, `docker`, and future backends that implement the same lifecycle service surface).
2. Store **per-sandbox** `backend_type` using the same vocabulary as [`DiagnosticContext`](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/opensandbox_server/services/diagnostics.py) (`backendType`), because server-level runtime configuration does not identify which backend owns a given sandbox (for example, a Kubernetes-configured server may manage both regular Kubernetes sandboxes and FastSandbox instances).
3. Store **observed** image URI and resource limits where the runtime actually scheduled resources (not only the client request), plus **durable create provenance** (image vs template vs snapshot and stable IDs/artifact references) so analytics remain useful after templates or snapshots are deleted.
4. Keep all writes **asynchronous** via `BackgroundTasks` (or equivalent scheduling from backend code) so API latency and success codes are unchanged.
5. Default **off**; SQLite-only deployments with history **disabled** behave exactly as today (see R2 for invalid enablement).
6. Expose an **internal** history writer API so lifecycle HTTP hooks, backend lifecycle operations, and future reconciliation (phase 1.5) share one persistence path without a generic resource-history framework in phase 1.
7. Enable a future console or operator SQL analytics path without new REST endpoints in phase 1.

### Non-Goals

1. OIDC/JWT server auth or RBAC (former OSEP-0006 Phase 2 auth).
2. Changes to `specs/sandbox-lifecycle.yml` or SDK contracts.
3. SQLite or in-memory **history** backends (now or in future revisions of this OSEP; a SQLite path would require a separate OSEP).
4. Console BFF routes, UI, or migration tooling in the same merge as the server writer.
5. Replacing OpenTelemetry metrics/logs or the Node Agent log collection roadmap ([OSEP-0019](0019-node-agent-sandbox-collection.md)).
6. Guaranteed exactly-once delivery or transactional coupling with sandbox state transitions.
7. **Phase 1 merge** — Hooks below the Lifecycle HTTP routes (renew-expiration, Docker timer eviction, Kubernetes controller TTL teardown) that bypass `DELETE /sandboxes/{id}`; see [Phase 1 coverage and known gaps](#phase-1-coverage-and-known-gaps).
8. Security **audit** semantics (attribution, integrity, tamper evidence) as in OSEP-0018 / credential-vault audit — this feature is **usage/history** only.

## Requirements

| ID | Requirement | Priority |
| --- | --- | --- |
| R1 | `[store.lifecycle_history].enabled` defaults to `false` | Must |
| R2 | Writer is active only when `[store].type = "postgresql"` and history is enabled | Must |
| R2a | **Config load fails** when `lifecycle_history.enabled = true` and `store.type != "postgresql"` (fail-fast; not silently ignored) | Must |
| R3 | Create/delete/metadata patch hooks must not change HTTP status, response body, or error codes on DB failure | Must |
| R4 | Rows are keyed by `sandbox_id`; upsert semantics tolerate duplicate or out-of-order events; metadata JSONB **merges** on patch and a delayed create upsert must not discard a successful metadata patch | Must |
| R5 | Terminal delete states (e.g. `Failed`) must not be overwritten with generic `Terminated` | Must |
| R6 | Metadata label map stored as JSONB; operators must understand PII/policy implications | Should |
| R7 | Schema created with `CREATE TABLE IF NOT EXISTS` under an advisory lock; safe for multi-process server | Must |
| R8 | Server closes the history writer pool on shutdown | Must |
| R9 | Phase 1 operators must not treat the table as complete usage accounting until phase 1.5: on renew-intent / OSEP-0009 style deployments, `expires_at` in stored rows can be **systematically stale** (create-time deadline while the sandbox lives longer after renew); sandboxes removed by Docker/Kubernetes TTL without API delete may **never** receive `ended_at`/`deleted_at`, leaving rows open indefinitely and `wall_clock_seconds` absent | Must |
| R10 | Phase 1.5 (follow-up): history writes for `renew_expiration`, runtime-driven expiry, and controller teardown paths that do not hit the delete route | Should |
| R11 | On create, persist `backend_type` for the **sandbox actually provisioned** (same values as `DiagnosticContext.backend_type`, e.g. `kubernetes`, `fast-sandbox`, `docker`), not inferred from global server runtime config alone | Must |
| R12 | On create, persist a **stable provenance subset** in `create_request` JSONB (see [Provisioning provenance](#provisioning-provenance)); merge semantics must not drop provenance on later metadata patches or delayed create upserts (see R4) | Must |
| R13 | Phase 1 implementations expose a single **internal lifecycle history writer** module/service; lifecycle routes and (in phase 1.5) backend teardown/reconcile paths call it — not ad-hoc SQL from each hook site | Must |

## Proposal

Enable `[store.lifecycle_history]`, reuse the store PostgreSQL DSN, and route all history persistence through an **internal lifecycle history writer** (see [Internal lifecycle history writer](#internal-lifecycle-history-writer)). Phase 1 schedules writer calls from existing lifecycle routes (create sandbox, delete sandbox, metadata patch) and from the **composite routing path** that selects the provisioning backend per sandbox. Fast-path and pool-based creates publish the **observed** container image and limits via a **request-scoped capture mechanism** (implementation-defined; not limited to `ContextVar`).

Consumers (console BFF, BI tools) treat the table as **read-only** relative to the server writer.

### Notes/Constraints/Caveats

- **Not a compliance audit log**: no tamper evidence, no operator action attribution, no retention enforcement in v1 (distinct from OSEP-0018 eBPF audit, OSEP-0019 in-sandbox audit trail, and credential-vault audit events).
- **Tenant isolation** is by `tenant_name` column content; authorization for reads is the consumer's responsibility (BFF session, SQL grants).
- **Wall-clock duration** on delete uses observation time when create was never recorded.
- Late create writes after a row is already ended or deleted must not clobber terminal metadata.
- **Create vs metadata PATCH race**: create and patch may run in separate `BackgroundTasks`; patch must merge into an existing or future row so a slow create insert cannot overwrite patched labels (see R4).

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Sensitive data in `metadata` labels | Document operator responsibility; default off; optional future redaction config |
| Write amplification on busy clusters | Async writes; small dedicated pool; disable if undesired |
| Schema drift between server and BFF | Table owned by server migration in repo; BFF documents reader columns in console docs |
| Multi-active server processes | Upsert by `sandbox_id`; advisory lock only for DDL |
| Unbounded table growth | Operator purge/TTL policy (see Upgrade & Migration); optional future `retention_days` |

## Design Details

### Configuration

```toml
[store]
type = "postgresql"
# postgresql.dsn via OPENSANDBOX_STORE_POSTGRESQL_DSN

[store.lifecycle_history]
enabled = true
```

Optional pool knobs may reuse `[store.postgresql]` bounds or use a smaller dedicated history writer pool (implementation detail).

Environment: `OPENSANDBOX_STORE_POSTGRESQL_DSN` (existing).

### Internal lifecycle history writer

The server defines an **internal** (non-REST) writer API, for example `LifecycleHistoryWriter` with methods such as `record_create`, `record_metadata_patch`, and `record_delete` (exact names are implementation detail). All persistence funnels through this API and its PostgreSQL repository.

**Callers (phase 1):**

- Lifecycle HTTP route handlers after successful create, metadata patch, and delete — typically via `BackgroundTasks` that invoke the writer.
- Create path integrations that already know the **selected provisioning backend** and scheduling/publish results (Kubernetes pool, FastSandbox template publish, Docker create).

**Callers (phase 1.5, R10 — specified here, not required in the first merge):**

- `SandboxService.renew_expiration` and renew-intent integrations.
- Backend teardown when Docker TTL, Kubernetes controller TTL, or FastSandbox lifecycle removes a sandbox without `DELETE /sandboxes/{id}`.
- Optional periodic reconcile jobs that refresh terminal fields on open rows.

This OSEP **defines the extension point** (writer API + column schema). It does **not** require every phase 1.5 caller to land in the same PR as the initial writer merge.

The writer must remain **best-effort** (R3): callers catch/log repository errors and never change user-visible lifecycle outcomes.

### Write path and API coupling

Hooks (no new public routes):

- **After successful create** — invoke the internal writer with create response fields, request metadata labels, limits from request or observed shape, **`backend_type`**, and [provenance](#provisioning-provenance) in `create_request`.
- **After metadata patch** — merge `metadata` JSONB on the open row (or enqueue merge if the create row is not visible yet); never replace the whole map with stale create-time labels when a newer patch was applied; do not overwrite `backend_type` or `create_request` provenance unless a future OSEP defines correction semantics.
- **After delete** — record `ended_at` / `deleted_at`, final state, `wall_clock_seconds` from observed sandbox before removal.

Implementation uses `BackgroundTasks` (or equivalent async dispatch from backend code) and catches/logs repository errors at the writer boundary.

### Phase 1 coverage and known gaps

**In scope for the initial server writer merge (phase 1):** lifecycle route hooks (create, metadata patch, explicit delete) **plus** create-time backend routing for all supported provisioning backends, including **FastSandbox** template-based creates, with `backend_type` and provenance populated on the create upsert.

**Known gaps (documented, not silent):**

| Path | Why phase 1 may miss it | Phase 1.5 direction (R10) |
| --- | --- | --- |
| `renew_expiration` / renew-intent | Updates store/runtime without necessarily passing through the metadata or delete routes the hooks attach to | Publish history update with new `expires_at` (and related fields) from `SandboxService.renew_expiration` or renew-intent integration |
| Docker TTL timer | Container removed in `docker_service` without a lifecycle delete call | Hook runtime teardown or periodic reconcile |
| Kubernetes controller TTL | Commented bypass of `delete_sandbox` on TTL expiry | Hook controller teardown or shared deletion helper used by both API and controller |

Until R10 lands, usage-window reporting may show stale `expires_at` or missing terminal rows for sandboxes that expired without an API delete. Console BFF **reconcile** (follow-up) can partially mask gaps but does not replace server-side R10.

### Table schema

Table: `sandbox_lifecycle_history` (v1)

**Semantics:** `sandbox_id` is the **PRIMARY KEY**. Rows are **mutable** — one **condensed last-state record per sandbox**, not an append-only event log. Merging `metadata` into JSONB supports current label maps for analytics but **cannot** answer time-series questions such as “when did label X become Y?”. That is acceptable for phase 1 usage reporting; consumers must not treat the table as a full event history.

| Column | Purpose |
| --- | --- |
| `sandbox_id` | Primary key; at most one row per sandbox |
| `tenant_name`, `namespace` | Multi-tenant context |
| `backend_type` | Provisioning backend for **this sandbox** (`kubernetes`, `fast-sandbox`, `docker`, … — same strings as `DiagnosticContext.backend_type`) |
| `state` | Last known lifecycle state |
| `image_uri` | **Observed** scheduled/run image when known; may differ from client request image for pool/template/FastSandbox paths |
| `lifecycle_created_at` | Sandbox create time as observed |
| `expires_at` | Create-time expiration deadline; **stale after renew until phase 1.5 (R10)** |
| `ended_at`, `deleted_at` | Terminal timestamps when delete (or phase 1.5 teardown) is recorded |
| `wall_clock_seconds` | Derived lifetime when terminal timestamps exist |
| `source` | e.g. `server-lifecycle` |
| `cpu_limit`, `memory_limit`, `cpu_cores`, `memory_gi` | Normalized limits |
| `metadata` | JSONB label map from create/patch (merged, not event log) |
| `create_request` | JSONB **provenance snapshot** — stable subset defined in [Provisioning provenance](#provisioning-provenance); written on create, retained for the life of the row |
| `first_recorded_at`, `last_seen_at` | Bookkeeping |

Index: `(tenant_name, lifecycle_created_at DESC)`.

Optional secondary index `(tenant_name, backend_type, lifecycle_created_at DESC)` may help console analytics; not required for phase 1 merge.

### Provisioning provenance

`image_uri` alone is insufficient when sandboxes are created from **templates**, **snapshots**, or FastSandbox publish flows where the client-facing image string does not fully identify template identity and the resolved artifact. History rows therefore store a **denormalized provenance snapshot** at create time in `create_request` (JSONB). Values are **copied into the row** so reporting still works after the referenced template or snapshot object is deleted upstream.

**Phase 1 stable keys** (snake_case in stored JSON; consumers must tolerate unknown future keys):

| Key | When present | Purpose |
| --- | --- | --- |
| `provisioning_kind` | Always on create | One of `image`, `template`, `snapshot` — how the sandbox was requested |
| `requested_image` | Image or template/snapshot flows with an image field | Client request image string when applicable |
| `template_id` | Template creates | Template identity at create time |
| `snapshot_id` | Snapshot restore creates | Snapshot identity at create time |
| `resolved_image_uri` | When runtime publish/schedule observed an image | Same semantics as column `image_uri`; may duplicate for JSON-only consumers |
| `artifact_refs` | FastSandbox / pool paths | Stable references to resolved artifacts (for example template revision, published image digest, pool manifest image) — structure is implementation-defined but must be sufficient for operators to correlate history with logs after template deletion |

**Sources:** Lifecycle `CreateSandboxRequest` fields, composite backend selection (`CompositeSandboxService` routing), FastSandbox publish/schedule results, and Kubernetes pool manifest observation (see [Observed runtime shape](#observed-runtime-shape)).

**Semantics:** Provenance is set on **create upsert** and treated as **immutable** for phase 1 (metadata patches merge only `metadata`, not `create_request`). Last-state rows still do not reconstruct full label or provenance **timelines** — only the create-time snapshot plus current `metadata` map.

DDL runs at repository init with `pg_advisory_xact_lock` for schema creation.

### Observed runtime shape

For Kubernetes pool / template paths and **FastSandbox** template-based creates, the server records the **scheduled** sandbox container image and `resources.limits` from the pool manifest or FastSandbox publish hook, not only the client `CreateSandboxRequest`. Provenance keys (`template_id`, `artifact_refs`, etc.) are filled from the same routing and publish observations. This aligns console usage analytics with what the data plane actually ran and which backend owned the sandbox.

A **request-scoped capture mechanism** (for example, shape data passed explicitly with pool schedule/publish results — not solely a `ContextVar`) bridges runtime scheduling and the create route history task. Pool/fast-sandbox warm paths run outside the client request task; implementations must not rely on context variables that do not propagate across tasks.

### Delete and terminal state semantics

- Delete observes the sandbox **before** removal when history is enabled (skip extra read when history disabled).
- If delete is the first event, still persist `wall_clock_seconds` when timestamps allow.
- Preserve `Failed` (and similar terminal states) on conflict updates instead of normalizing everything to `Terminated`.

### Console BFF reader (follow-up)

Out of scope for OSEP-0029 implementation phase 1. Expected pattern (documented in console deployment guides):

- BFF reads `sandbox_lifecycle_history` with the same DSN or a read-only role.
- Periodic reconcile may refresh labels from live sandboxes for open rows.
- No requirement to merge reader code in the same PR as the server writer.

## Test Plan

- **Unit tests**: config gating (`enabled` + `postgresql`); **R2a fail-fast** when enabled on SQLite; metadata scheduling; shape parsing; delete conflict behavior; memory unit normalization; **metadata create/patch ordering** (patch after create response, slow create upsert must preserve merged labels); **`backend_type`** from composite routing (e.g. FastSandbox vs Kubernetes on the same server config); **provenance** population for image, template, and snapshot creates (including `artifact_refs` when publish hooks run).
- **Repository tests**: upsert/create/delete/metadata against PostgreSQL when `OPENSANDBOX_TEST_POSTGRESQL_DSN` is set; the implementation PR must register these tests in the existing **server-test PostgreSQL job** (`.github/workflows/server-test.yml`) so they run in required CI when the DSN is provisioned — not optional-only locally.
- **Manual**: enable history on a dev cluster; create/delete sandbox; verify row contents via SQL.
- **Phase 1.5 tests (R10)**: renew-expiration history update; runtime expiry teardown (Docker/Kubernetes) once hooks exist.

Reference draft implementation: [PR #2237](https://github.com/opensandbox-group/OpenSandbox/pull/2237) (to remain **draft** until this OSEP reaches **provisional** or **implementable**; will adopt `lifecycle_history` naming on rebase).

## Drawbacks

- Additional PostgreSQL load and storage growth without built-in retention automation in v1.
- Two components (server writer, console reader) must agree on column meaning.
- Does not capture SDK-only traffic that never hits lifecycle routes (same as live list today).
- Phase 1 does not guarantee complete renew/expiry history until R10 (see [Phase 1 coverage and known gaps](#phase-1-coverage-and-known-gaps)).
- Last-state rows are not deleted when a sandbox ends; table size grows with cumulative creates.

## Alternatives

1. **Console-only polling of Lifecycle API** — misses deleted sandboxes and increases API load; rejected.
2. **New Lifecycle REST history endpoints** — larger contract and SDK surface; deferred.
3. **OpenTelemetry / external SIEM only** — weak fit for product analytics and console history UX.
4. **Event bus (Kafka)** — heavier ops burden for most deployments; out of scope for v1.

## Infrastructure Needed

- Existing PostgreSQL used for `[store]` (no new mandatory service).
- Reuse the repository's existing PostgreSQL CI job; wire history repository tests in the implementation PR (see Test Plan).

## Upgrade & Migration Strategy

- **Default off**: upgrades are no-ops until operators set `lifecycle_history.enabled = true`.
- **DDL**: idempotent `CREATE TABLE IF NOT EXISTS` on first pool open.
- **No data migration** from SQLite; operators enabling history on PostgreSQL start with an empty table.
- **Disabling**: set `enabled = false`; historical rows remain until operators truncate or purge.
- **Retention (operator responsibility in v1)**: rows are not auto-deleted. Example manual purge for rows ended more than 90 days ago:

  ```sql
  DELETE FROM sandbox_lifecycle_history
  WHERE ended_at IS NOT NULL
    AND ended_at < now() - interval '90 days';
  ```

- **Future (Should, not required for phase 1 merge)**: optional `retention_days` under `[store.lifecycle_history]` may automate purge in a follow-up.