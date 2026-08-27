---
title: Highly Available Server-Managed Snapshots
authors:
  - "@cwj2001"
creation-date: 2026-08-27
last-updated: 2026-08-27
status: draft
---

# OSEP-0023: Highly Available Server-Managed Snapshots

## Summary

Define public `snapshotId` as a control-plane-scoped resource. Any healthy
OpenSandbox Lifecycle Server replica in that control plane must be able to
query, restore, and safely continue an unfinished snapshot operation without
session affinity to the Server that accepted the original request.

The public lifecycle API remains unchanged:

- `POST /sandboxes/{sandboxId}/snapshots`
- `GET /snapshots/{snapshotId}`
- `GET /snapshots`
- `DELETE /snapshots/{snapshotId}`
- `POST /sandboxes` with `snapshotId`

SQLite remains supported for single-node and development deployments. A
multi-replica Server deployment requires a shared transactional snapshot store
and cross-replica operation recovery.

## Motivation

The official Helm chart defaults to two Lifecycle Server replicas behind one
Kubernetes Service. The current snapshot catalog is local SQLite metadata, and
create/recovery work is process-local. Consequently, a restore request routed
to another replica cannot resolve a valid `snapshotId`, even when the
Kubernetes `SandboxSnapshot` CR and OCI image are available.

This violates the expected semantics of a public resource ID: its resolution
scope must be at least as broad as the load-balancer scope that accepts it.

This proposal addresses #1654.

## Goals

1. A Ready snapshot can be queried and restored by any Server replica in the
   configured control plane.
2. A Server crash during `Creating` or `Deleting` does not strand the operation;
   another replica can take over safely.
3. Concurrent replicas do not create duplicate commit work or issue conflicting
   cleanup.
4. Tenant and Kubernetes namespace isolation remain enforced at catalog lookup.
5. Existing API and SDK callers remain compatible.
6. The official Helm chart prevents unsupported snapshot-plus-topology
   combinations.

## Non-goals

- Exposing OCI image references to clients.
- Cross-cluster snapshot migration.
- Memory/process/VM checkpoint portability.
- Replacing Kubernetes `SandboxSnapshot` CRs or the image committer.
- Making SQLite safe for shared network filesystems.
- Moving the snapshot lifecycle into an external Gateway.

## Proposal

### Shared snapshot catalog

Introduce a configuration-selected snapshot repository adapter. SQLite remains
the default only for a single Server. A multi-replica deployment uses a shared,
transactional database adapter, initially PostgreSQL.

The catalog stores the existing snapshot identity, tenant namespace, lifecycle
state, restore configuration, and timestamps. When Ready, the restore image is
recorded as an immutable OCI digest reference where the runtime provides one.

### Operation ownership and recovery

`Creating` and `Deleting` are asynchronous operations, not ownerless records.
The shared catalog adds internal operation fields:

- generation;
- lease owner;
- lease expiry;
- attempt count;
- last-operation timestamp.

Replicas claim due operations atomically. A worker may transition or clean up
an operation only while it owns its lease. If a worker exits, another replica
may claim work after lease expiry.

A takeover first reads the deterministic Kubernetes `SandboxSnapshot` CR. It
must observe an existing CR before attempting creation, and it must validate the
source sandbox after an already-exists response. This prevents duplicate commit
jobs.

### Repository interface

Keep a small repository seam. The service should request high-level operations
such as `claim_due_operations`, `renew_operation_lease`, and
`complete_if_leased`; SQL locking and database-specific queries remain inside
the adapter. Callers continue to use the existing snapshot service interface.

### Deployment contract

The Helm chart must reject or clearly fail configuration where public snapshot
operations are enabled with `server.replicaCount > 1` and a local SQLite store.
The production values and runbook must document shared-store credentials,
migrations, operation lease tuning, registry digest retention, and rollback.

## Alternatives

### Restrict snapshot deployments to one active Server

Safe as an immediate compatibility guard, but does not meet high-availability
requirements. It is an acceptable temporary mode, not the target architecture.

### Shared SQLite on a RWX volume

Rejected. It does not provide supported multi-process/network-filesystem
coordination or operation ownership and cannot safely replace a transactional
HA catalog.

### Sticky sessions or routing each snapshot to its creator Server

Rejected. Server failure still makes restore unavailable and resource scope
remains inconsistent with the public API.

### Make Kubernetes CRs the complete public catalog

Not selected by this proposal. It could be revisited, but it requires a full
public resource contract for tenant isolation, filtering, deletion, artifact
lifecycle, and operation ownership. It is larger than extending the existing
server-managed catalog seam.

## Upgrade and migration

SQLite users remain supported with a single Server. Operators moving to HA:

1. quiesce snapshot mutations;
2. import SQLite records into the shared store;
3. verify IDs, namespace, state, and restore image references;
4. deploy the shared-store configuration;
5. scale Server replicas only after health and migration checks pass.

No dual-write mode is introduced.

## Test plan

Run an end-to-end Kubernetes test through a normal Service with no session
affinity:

1. Server A creates, Server B gets and restores a Ready snapshot.
2. Server A exits during Creating; B acquires the expired lease and reaches a
   deterministic Ready or Failed terminal state.
3. Two replicas scan the same due record; only one executes the operation.
4. Delete and restore races return documented, deterministic state errors.
5. Tenant A cannot query or restore Tenant B's snapshot.
6. Temporary Kubernetes API, Registry, and shared-store failures converge
   without duplicate commits or silent data loss.
7. Helm validation rejects local SQLite with multiple snapshot-capable Server
   replicas.
