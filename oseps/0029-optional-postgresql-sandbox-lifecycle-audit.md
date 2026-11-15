---
title: Optional PostgreSQL Sandbox Lifecycle Audit
authors:
  - "@comqx"
creation-date: 2026-11-13
last-updated: 2026-11-15
status: draft
---

# OSEP-0029: Optional PostgreSQL Sandbox Lifecycle Audit (Server Writer)

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
  - [Write path and API coupling](#write-path-and-api-coupling)
  - [Phase 1 coverage and known gaps](#phase-1-coverage-and-known-gaps)
  - [Table schema](#table-schema)
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

Add an **opt-in** lifecycle server writer that upserts sandbox create, metadata label changes, and delete observations into a PostgreSQL table `sandbox_lifecycle_history`. The feature is disabled by default, reuses the existing `[store]` PostgreSQL deployment, does **not** change the public Lifecycle OpenAPI, and treats database errors as **best-effort** (logged, never reflected in API responses).

This OSEP covers the **server writer only**. A separate follow-up (console BFF reader and UI) may read the same table but is out of scope for the initial implementation merge.

## Motivation

Today operators and the optional Developer Console work primarily against **live** lifecycle state (`GET /sandboxes`). Deleted sandboxes, SDK-only creates without console involvement, and historical reporting (usage windows, label filters, image mix) are difficult without a durable store.

[OSEP-0006](0006-developer-console.md) envisioned Phase 2 PostgreSQL audit but is **withdrawn**; the in-tree console is a **BFF adjunct** ([Developer Console guide](../docs/guides/developer-console.md)). That guide already describes optional `[store.lifecycle_audit]` as a deployment pattern. This OSEP formalizes the **server-side writer** contract so implementations can land upstream without expanding the Lifecycle API.

### Goals

1. Durably record sandbox **create** and **delete** (and **metadata label map** updates on open rows) when audit is enabled.
2. Store **observed** image URI and resource limits where the runtime actually scheduled resources (not only the client request).
3. Keep all writes **asynchronous** via `BackgroundTasks` so API latency and success codes are unchanged.
4. Default **off**; SQLite-only deployments behave exactly as today.
5. Enable a future console or operator SQL analytics path without new REST endpoints in phase 1.

### Non-Goals

1. OIDC/JWT server auth or RBAC (former OSEP-0006 Phase 2 auth).
2. Changes to `specs/sandbox-lifecycle.yml` or SDK contracts.
3. SQLite or in-memory audit backends.
4. Console BFF routes, UI, or migration tooling in the same merge as the server writer.
5. Replacing OpenTelemetry metrics/logs or the Node Agent log collection roadmap ([OSEP-0019](0019-node-agent-sandbox-collection.md)).
6. Guaranteed exactly-once delivery or transactional coupling with sandbox state transitions.
7. **Phase 1 merge** — Hooks below the Lifecycle HTTP routes (renew-expiration, Docker timer eviction, Kubernetes controller TTL teardown) that bypass `DELETE /sandboxes/{id}`; see [Phase 1 coverage and known gaps](#phase-1-coverage-and-known-gaps).

## Requirements

| ID | Requirement | Priority |
| --- | --- | --- |
| R1 | `lifecycle_audit.enabled` defaults to `false` | Must |
| R2 | Writer is active only when `[store].type = "postgresql"` and audit is enabled | Must |
| R3 | Create/delete/metadata patch hooks must not change HTTP status, response body, or error codes on DB failure | Must |
| R4 | Rows are keyed by `sandbox_id`; upsert semantics tolerate duplicate or out-of-order events; metadata JSONB **merges** on patch and a delayed create upsert must not discard a successful metadata patch | Must |
| R5 | Terminal delete states (e.g. `Failed`) must not be overwritten with generic `Terminated` | Must |
| R6 | Metadata label map stored as JSONB; operators must understand PII/policy implications | Should |
| R7 | Schema created with `CREATE TABLE IF NOT EXISTS` under an advisory lock; safe for multi-process server | Must |
| R8 | Server closes the audit pool on shutdown | Must |
| R9 | Phase 1 documents known gaps for renew / runtime-only expiry; operators must not treat history as complete usage accounting until phase 1.5 | Must |
| R10 | Phase 1.5 (follow-up): audit writes for `renew_expiration`, runtime-driven expiry, and controller teardown paths that do not hit the delete route | Should |

## Proposal

Enable `[store.lifecycle_audit]`, reuse the store PostgreSQL DSN, and schedule audit upserts from existing lifecycle routes (create sandbox, delete sandbox, metadata patch). Fast-path and pool-based creates publish the **observed** container image and limits via a request-scoped shape captured at schedule time.

Consumers (console BFF, BI tools) treat the table as **read-only** relative to the server writer.

### Notes/Constraints/Caveats

- **Not a compliance audit log**: no tamper evidence, no operator action attribution, no retention enforcement in v1.
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

## Design Details

### Configuration

```toml
[store]
type = "postgresql"
# postgresql.dsn via OPENSANDBOX_STORE_POSTGRESQL_DSN

[store.lifecycle_audit]
enabled = true
```

Optional pool knobs may reuse `[store.postgresql]` bounds or use a smaller dedicated audit pool (implementation detail).

Environment: `OPENSANDBOX_STORE_POSTGRESQL_DSN` (existing).

### Write path and API coupling

Hooks (no new public routes):

- **After successful create** — upsert row with create response fields, request metadata labels, limits from request or observed shape.
- **After metadata patch** — merge `metadata` JSONB on the open row (or enqueue merge if the create row is not visible yet); never replace the whole map with stale create-time labels when a newer patch was applied.
- **After delete** — record `ended_at` / `deleted_at`, final state, `wall_clock_seconds` from observed sandbox before removal.

Implementation uses `BackgroundTasks` and catches/logs repository errors.

### Phase 1 coverage and known gaps

**In scope for the initial server writer merge (phase 1):** lifecycle route hooks only — create, metadata patch, and explicit delete after the server observes the sandbox.

**Known gaps (documented, not silent):**

| Path | Why phase 1 may miss it | Phase 1.5 direction (R10) |
| --- | --- | --- |
| `renew_expiration` / renew-intent | Updates store/runtime without necessarily passing through the metadata or delete routes the hooks attach to | Publish audit update with new `expires_at` (and related fields) from `SandboxService.renew_expiration` or renew-intent integration |
| Docker TTL timer | Container removed in `docker_service` without a lifecycle delete call | Hook runtime teardown or periodic reconcile |
| Kubernetes controller TTL | Commented bypass of `delete_sandbox` on TTL expiry | Hook controller teardown or shared deletion helper used by both API and controller |

Until R10 lands, usage-window reporting may show stale `expires_at` or missing terminal rows for sandboxes that expired without an API delete. Console BFF **reconcile** (follow-up) can partially mask gaps but does not replace server-side R10.

### Table schema

Table: `sandbox_lifecycle_history` (v1)

| Column | Purpose |
| --- | --- |
| `sandbox_id` | Primary key |
| `tenant_name`, `namespace` | Multi-tenant context |
| `state` | Last known lifecycle state |
| `image_uri` | Observed or requested image |
| `lifecycle_created_at`, `expires_at`, `ended_at`, `deleted_at` | Timestamps |
| `wall_clock_seconds` | Derived lifetime |
| `source` | e.g. `server-lifecycle` |
| `cpu_limit`, `memory_limit`, `cpu_cores`, `memory_gi` | Normalized limits |
| `metadata` | JSONB label map from create/patch |
| `create_request` | Optional JSONB snapshot (implementation-defined subset) |
| `first_recorded_at`, `last_seen_at` | Bookkeeping |

Index: `(tenant_name, lifecycle_created_at DESC)`.

DDL runs at repository init with `pg_advisory_xact_lock` for schema creation.

### Observed runtime shape

For Kubernetes pool / template paths, the server records the **scheduled** sandbox container image and `resources.limits` from the pool manifest (or fast-sandbox publish hook), not only the client `CreateSandboxRequest`. This aligns console usage analytics with what the data plane actually ran.

Context variable `LifecycleAuditShape` bridges async runtime code and the create route audit task.

### Delete and terminal state semantics

- Delete observes the sandbox **before** removal when audit is enabled (skip extra read when audit disabled).
- If delete is the first event, still persist `wall_clock_seconds` when timestamps allow.
- Preserve `Failed` (and similar terminal states) on conflict updates instead of normalizing everything to `Terminated`.

### Console BFF reader (follow-up)

Out of scope for OSEP-0029 implementation phase 1. Expected pattern (documented in console deployment guides):

- BFF reads `sandbox_lifecycle_history` with the same DSN or a read-only role.
- Periodic reconcile may refresh labels from live sandboxes for open rows.
- No requirement to merge reader code in the same PR as the server writer.

## Test Plan

- **Unit tests**: config gating (`enabled` + `postgresql`); metadata scheduling; shape parsing; delete conflict behavior; memory unit normalization; **metadata create/patch ordering** (patch after create response, slow create upsert must preserve merged labels).
- **Repository tests**: upsert/create/delete/metadata against PostgreSQL when `OPENSANDBOX_TEST_POSTGRESQL_DSN` is set; the implementation PR must register these tests in the existing **server-test PostgreSQL job** (`.github/workflows/server-test.yml`) so they run in required CI when the DSN is provisioned — not optional-only locally.
- **Manual**: enable audit on a dev cluster; create/delete sandbox; verify row contents via SQL.
- **Phase 1.5 tests (R10)**: renew-expiration audit update; runtime expiry teardown (Docker/Kubernetes) once hooks exist.

Reference draft implementation: [PR #2237](https://github.com/opensandbox-group/OpenSandbox/pull/2237) (to remain **draft** until this OSEP reaches **provisional** or **implementable**).

## Drawbacks

- Additional PostgreSQL load and storage growth without built-in retention.
- Two components (server writer, console reader) must agree on column meaning.
- Does not capture SDK-only traffic that never hits lifecycle routes (same as live list today).
- Phase 1 does not guarantee complete renew/expiry history until R10 (see [Phase 1 coverage and known gaps](#phase-1-coverage-and-known-gaps)).

## Alternatives

1. **Console-only polling of Lifecycle API** — misses deleted sandboxes and increases API load; rejected.
2. **New Lifecycle REST audit endpoints** — larger contract and SDK surface; deferred.
3. **OpenTelemetry / external SIEM only** — weak fit for product analytics and console history UX.
4. **Event bus (Kafka)** — heavier ops burden for most deployments; out of scope for v1.

## Infrastructure Needed

- Existing PostgreSQL used for `[store]` (no new mandatory service).
- Reuse the repository's existing PostgreSQL CI job; wire audit repository tests in the implementation PR (see Test Plan).

## Upgrade & Migration Strategy

- **Default off**: upgrades are no-ops until operators set `lifecycle_audit.enabled = true`.
- **DDL**: idempotent `CREATE TABLE IF NOT EXISTS` on first pool open.
- **No data migration** from SQLite; operators enabling audit start with an empty history table.
- **Disabling**: set `enabled = false`; historical rows remain until operators truncate or archive.
