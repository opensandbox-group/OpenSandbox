---
title: Highly Available Server-Managed Snapshots
authors:
  - "@cwj2001"
creation-date: 2026-08-27
last-updated: 2026-08-27
status: draft
---

# OSEP-0023: Highly Available Server-Managed Snapshots

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
  - [Snapshot Catalog](#snapshot-catalog)
  - [Operation Ownership and Recovery](#operation-ownership-and-recovery)
  - [Restore](#restore)
  - [Deployment Contract](#deployment-contract)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Public `snapshotId` is a control-plane-scoped resource. Any healthy Lifecycle
Server replica in that control plane can query and restore it, and an unfinished
snapshot operation survives loss of the replica that accepted the original
request.

The implementation adds a PostgreSQL-backed snapshot catalog and lease-based
operation recovery for multi-replica Server deployments. The existing lifecycle
API and SDK contracts remain unchanged. SQLite remains the single-Server and
development implementation.

## Motivation

The official `opensandbox-server` Helm chart deploys two Lifecycle Server
replicas behind one Kubernetes Service by default. Public snapshot metadata is
currently stored in each receiving Server's local SQLite database, and snapshot
creation/recovery work is process-local. A snapshot created through Server A
therefore cannot reliably be restored when the Service routes the request to
Server B, even when the Kubernetes `SandboxSnapshot` CR and OCI artifact are
available.

A public resource ID must resolve across the same control-plane scope as the
load balancer that accepts it. This proposal makes the existing public
`snapshotId` contract consistent with the official multi-replica deployment.
It addresses #1654.

### Goals

- Any Server replica in one configured control plane can get, list, delete, and
  restore a Ready `snapshotId`.
- Server failure during `Creating` or `Deleting` does not strand the operation;
  another replica takes over without duplicate commit work.
- Tenant and Kubernetes namespace isolation remain enforced at catalog lookup
  and at restore.
- Public lifecycle routes, request shapes, response shapes, and SDK contracts
  remain backward-compatible.
- Helm rejects a snapshot-capable multi-replica deployment backed by local
  SQLite.
- SQLite remains usable for local development and explicitly single-Server
  deployments.

### Non-Goals

- Exposing snapshot OCI image references to public API clients.
- Cross-cluster snapshot export or migration.
- Memory, process, or VM checkpoint portability.
- Replacing Kubernetes `SandboxSnapshot` CRs or image-committer.
- Making SQLite safe on RWX/network filesystems.
- Moving snapshot lifecycle ownership to an external Gateway.
- General-purpose distributed job scheduling beyond snapshot operations.

## Requirements

- A multi-replica Server deployment uses PostgreSQL as the shared snapshot
  catalog. SQLite is not a supported multi-replica store.
- A catalog record contains the existing identity, tenant namespace, lifecycle
  state, restore configuration, and timestamps, plus internal operation lease
  state.
- At most one worker owns an in-flight snapshot operation at a time.
- A worker may update an in-flight operation only while its lease is valid.
- Recovery first observes the deterministic Kubernetes `SandboxSnapshot` CR;
  it does not blindly create another commit operation.
- Ready records retain an immutable OCI artifact identity: image URI plus the
  pushed image digest when the runtime reports it.
- The lifecycle API preserves its existing `Creating`, `Ready`, `Failed`, and
  `Deleting` behavior and error semantics.
- Normal Kubernetes Service routing must work; session affinity is not a
  requirement or a mitigation.

## Proposal

Add a PostgreSQL-backed adapter behind the existing server-managed snapshot
repository seam, and make snapshot operation ownership explicit with database
leases. Every Server replica runs the same bounded reconciler. It claims due
`Creating` and `Deleting` records from the shared catalog, observes the
Kubernetes runtime state, and transitions records atomically.

SQLite continues to serve local and single-Server installations. Multi-replica
snapshot support is enabled only with the PostgreSQL adapter. Existing clients
continue to create and restore snapshots using the same lifecycle API.

### Notes/Constraints/Caveats

- PostgreSQL is selected because the implementation requires shared durable
  records, transactional conditional writes, and safe multi-replica claiming.
  This OSEP does not define a generic database abstraction for unrelated Server
  resources.
- The Kubernetes `SandboxSnapshot` CR and OCI registry remain runtime facts,
  but they do not replace the server catalog. The catalog remains responsible
  for public snapshot identity, tenant scope, lifecycle state, and operation
  ownership.
- A Ready artifact must be retained by registry policy for as long as its
  catalog record is restorable. Registry GC remains an operational dependency.
- The scope is one configured OpenSandbox control plane. Cross-cluster restore
  is intentionally excluded.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| A worker crashes after creating a CR | Lease expiry permits takeover; the new worker reads the deterministic CR before creating work. |
| Two replicas race to claim an operation | PostgreSQL conditional claim and lease ownership allow one winner only. |
| A stale worker writes after losing ownership | Every completion/update is conditional on current lease owner and generation. |
| Registry artifact is deleted after Ready | Persist artifact digest, document retention requirements, and surface restore failure through normal lifecycle state. |
| PostgreSQL adds operational cost | Keep SQLite as the zero-dependency single-Server/development path. |
| Existing multi-replica SQLite deployments rely on accidental behavior | Helm validation fails closed instead of allowing random `SNAPSHOT::NOT_FOUND`. |

## Design Details

### Snapshot Catalog

Extend the existing `SnapshotRepository` interface with a PostgreSQL adapter.
The PostgreSQL schema persists current `SnapshotRecord` fields and adds internal
columns:

```text
operation_generation
lease_owner
lease_expires_at
operation_attempt
last_operation_at
artifact_digest
```

The adapter exposes narrow snapshot-specific operations rather than leaking SQL
or lock details to lifecycle handlers:

```text
claim_due_operations(instance_id, lease_until, limit)
renew_operation_lease(snapshot_id, instance_id, generation, lease_until)
complete_if_leased(snapshot_id, instance_id, generation, expected_state, update)
```

A conditional update or row lock inside the adapter enforces ownership. The
service layer remains the only caller that understands snapshot lifecycle
transitions.

### Operation Ownership and Recovery

The create route atomically persists a `Creating` record and returns the
existing accepted response. It does not bind the operation to the HTTP-serving
replica.

Every replica periodically claims due operations with a short lease. For a
claimed `Creating` record, the worker:

1. calculates the deterministic `SandboxSnapshot` CR name;
2. reads the CR;
3. creates it only when absent;
4. on an already-exists result, reads and validates the CR source sandbox;
5. observes terminal runtime status; and
6. conditionally commits `Ready` or `Failed` while still holding the lease.

For `Deleting`, the worker similarly claims, observes/deletes the runtime
artifact as defined by the runtime adapter, and conditionally removes the
catalog record. Server startup does not own recovery exclusively; every healthy
replica participates through the same lease protocol.

### Restore

`POST /sandboxes` with `snapshotId` resolves the record from the shared catalog.
It verifies tenant namespace, verifies `Ready`, reads `restore_config.image`,
and creates the sandbox through the existing lifecycle path. The receiving
Server may be different from the one that accepted snapshot creation.

No new public `imageUri` response field is required for this proposal.

### Deployment Contract

Add explicit store configuration:

```toml
[store]
type = "postgres"
dsn = "postgresql://..."
```

`type = "sqlite"` remains supported for one Server.

The Helm chart validates that snapshot-capable `server.replicaCount > 1` cannot
use `store.type = "sqlite"`. It provides PostgreSQL connection configuration
through a Secret, a unique Server instance ID, migration execution, and
readiness checks that include catalog connectivity and schema compatibility.

## Test Plan

- Repository contract tests run against SQLite and PostgreSQL adapters.
- PostgreSQL integration tests verify atomic claim, renewal, lease expiry,
  stale-worker rejection, and conditional completion.
- Lifecycle integration tests start two Server instances with one PostgreSQL
  catalog: A creates and B gets/restores a Ready snapshot.
- Failure injection terminates A while Creating; B takes over and converges to
  one deterministic Ready or Failed result.
- Concurrent scans from two replicas generate one runtime operation only.
- Delete/restore races preserve documented state and error semantics.
- Multi-tenant tests prove a tenant cannot get or restore another tenant's
  snapshot.
- Kubernetes HA e2e runs through a normal Service without session affinity,
  verifies restored markers, confirms no duplicate commit Job, and cleans up.
- Helm tests reject `replicaCount > 1` with SQLite and accept PostgreSQL-backed
  multi-replica values.

## Drawbacks

PostgreSQL becomes required infrastructure for high-availability snapshots.
Lease recovery adds implementation and operational complexity, and a short
period can elapse before a failed worker is taken over. The feature therefore
has a more substantial operational footprint than the current local snapshot
implementation.

## Alternatives

### Single active Server

Rejected as the target architecture. It is a safe temporary restriction but
cannot satisfy high availability while the official chart defaults to multiple
Server replicas.

### Shared SQLite on RWX storage

Rejected. SQLite on a shared network filesystem does not provide the required
multi-replica operation ownership or a supported HA transaction model.

### Sticky sessions or creator-Server routing

Rejected. A Server failure still makes restore unavailable and `snapshotId`
remains narrower in scope than the public Service.

### Kubernetes CR as the complete public catalog

Rejected for this proposal. It would require re-designing public identity,
tenancy, filtering, delete semantics, artifact lifecycle, and operation
ownership. Extending the existing server-managed snapshot catalog is the
smaller compatible change.

## Infrastructure Needed

A PostgreSQL service reachable by Lifecycle Server replicas is required for HA
snapshot deployments. Operators must provision a Secret for the DSN or
connection parameters, database backup/restore, and monitoring for catalog
connectivity, migration status, lease takeovers, and failed operations.

No new Kubernetes CRD, registry product, or external API is required.

## Upgrade & Migration Strategy

SQLite remains the default for local and single-Server deployments. An operator
migrating existing snapshots to HA:

1. stops snapshot create/delete mutations;
2. backs up the SQLite database;
3. runs a one-time import into PostgreSQL;
4. verifies IDs, tenant namespaces, lifecycle states, restore images, and
   artifact digests where present;
5. deploys Server replicas with PostgreSQL configuration and schema checks; and
6. enables multi-replica snapshot traffic only after the HA e2e verification.

No dual-write mode is introduced. Rollback before enabling new mutations returns
to the SQLite deployment from the verified backup; rollback after new HA
mutations requires preserving the PostgreSQL catalog because it is then the
source of truth.
