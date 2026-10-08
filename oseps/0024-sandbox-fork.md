---
title: Single-copy sandbox fork
authors:
  - "@GodBlf"
creation-date: 2026-10-04
last-updated: 2026-10-04
status: draft
---

# OSEP-0024: Single-copy sandbox fork

## Summary

[Issue #1284](https://github.com/opensandbox-group/OpenSandbox/issues/1284)
needs an ergonomic operation for branching an expensive sandbox setup. Compose
the existing persistent snapshot and restore primitives behind a durable task.
The operation creates one independent runtime identity from the captured rootfs.
It does not clone memory or user volumes.

## Interface

`POST /sandboxes/{sandboxId}/fork` accepts a target timeout and limited create
overrides. It returns a task with HTTP 202. `GET /forks/{forkId}` exposes progress,
failure and the resulting snapshot and sandbox IDs. Optional tenant-scoped
idempotency keys make submission retries safe.

## Orchestration

Persist the effective target configuration, source identity, snapshot ID and
target ID before side effects. Coordinate snapshot capture and sandbox creation
with renewable database leases in SQLite or PostgreSQL. Recover existing
resources by their stable IDs after restart rather than allocating replacements.
Reuse normal snapshot consistency, workload provisioning and tenant checks.

Success keeps the public snapshot, avoiding image-reference races and allowing
reuse. Failure deletes only the operation's resources; pending cleanup is
persisted and retried. OCI registry retention remains an operator responsibility.

## Constraints and alternatives

Support Linux Docker and standard single-replica Kubernetes BatchSandbox.
Reject user mounts, pools, FastSandbox and unsupported workload constraints
before accepting the task. Default to the snapshot restore entrypoint rather
than rerunning source startup hooks. Batch count, volume branching, cancellation,
memory checkpointing and stronger snapshot consistency are follow-up work.

A synchronous endpoint has fewer public resources but inherits proxy timeout and
disconnect ambiguity. SDK-only composition leaves cleanup and operation recovery
to each user. A durable server task handles those concerns once.

## Validation and compatibility

Additive endpoints and store tables preserve existing snapshot/create behavior.
Test inheritance, identity separation, volume rejection, tenant isolation,
idempotency, competing leases, restart at every phase and failure cleanup.
Verify rootfs divergence in Docker and Kubernetes integration environments.

See the [user guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/guides/sandbox-fork.md)
for the configuration and artifact ownership contract.
