---
title: Fork a sandbox
description: Start an independent sandbox from a running sandbox's root filesystem.
---

# Fork a sandbox

Fork captures the source root filesystem and starts one new sandbox from that
snapshot. It supports Linux Docker containers and non-pooled Kubernetes
BatchSandbox workloads using the standard rootfs snapshot backend. The source
must be Running and have no user volume mounts. FastSandbox, Windows, custom
runtime classes, templates, pool allocations and multi-replica workloads are
not supported.

::: warning Filesystem state only
Fork does not resume running commands, process memory, sockets or application
transactions. Capture may briefly freeze the source. Applications should flush
their progress to disk before requesting a fork. The snapshot represents the
capture time, which can be later than request acceptance.
:::

Submit `POST /sandboxes/{sandboxId}/fork` with a required target lifetime in
seconds. A single request produces one copy:

```json
{
  "timeout": 1800,
  "overrides": {
    "metadata": {"experiment": "candidate-a"},
    "resourceLimits": {"cpu": "1", "memory": "1Gi"}
  }
}
```

The response is `202 Accepted`, with a task `id` and a `Location` pointing to
`/v1/forks/{id}`. Poll that URL to track `Pending`, `Snapshotting`, `Provisioning`,
then `Succeeded` or `Failed`. A successful result includes `sandboxId` and
`snapshotId`. Read endpoints using the usual sandbox endpoint API.

Send an `Idempotency-Key` header to safely retry submissions. Reusing a key with
a different source or body returns a conflict. Keys are isolated by tenant and
retained with the task. A client timeout or disconnect does not cancel the task.
The operation deadline is 30 minutes; failure cleanup continues after that
deadline. `cleanupPending: true` means automatic cleanup is still being retried.
If a worker restarts during an ambiguous target submission, the coordinator
observes the reserved target ID instead of issuing a second create. If that
target never appears, the operation fails at its deadline. Retry a failed fork
with a new idempotency key after checking its cleanup state.

For an uncertain runtime request, cleanup keeps checking for late resources.
`cleanupPending` can remain true indefinitely if the runtime never resolves
that request. Inspect server logs and runtime resources before manual cleanup.

## Configuration

User environment variables, effective resources, metadata, platform, security
settings and the current egress policy are inherited. `overrides` accepts
`env`, `resourceLimits`, `resourceRequests`, `networkPolicy`, `metadata` and
`entrypoint`. Each supplied field replaces the inherited field completely;
null values are rejected. Empty environment or metadata objects clear the user
configuration. Docker fork images neutralize captured environment defaults;
omitted variables can remain present with empty values. A standard Linux `PATH`
is retained for startup, and inherited or explicitly supplied `PATH` overrides it.
Kubernetes image defaults follow the existing snapshot backend's startup rules.

The target defaults to `tail -f /dev/null`; specify `entrypoint` to start an
application explicitly. Lifecycle hooks are not inherited. Timeout starts when
the target is created and does not copy the source expiration.

IDs, endpoints and server-managed credentials are new. Credential Vault bindings
are not copied. Application credentials written into the rootfs are copied, as
are references to external databases and services. Those external resources
remain shared unless the application changes them.

## Snapshot ownership

A successful fork retains its persistent snapshot for reuse. Delete it explicitly
with the snapshot API when no longer needed. Docker can reject deletion while
the image is referenced by a container. Kubernetes snapshot deletion does not
delete registry images; configure registry retention separately.

Failed operations clean up only their own target and snapshot. The source is
never deleted by fork. If the source disappears before capture finishes, the
operation can fail; once the snapshot is Ready, target creation can continue
without the source.

## Lifecycle store

Fork records and leases use the configured SQLite or PostgreSQL lifecycle
store. Server startup creates the additive `sandbox_forks` table automatically.
Workers sharing a PostgreSQL store coordinate with renewable leases. Keep the
same store across server restarts so operations can recover. The existing
snapshot migration command does not migrate fork records; finish pending forks
and cleanup before changing stores, and retain the old store for status queries.
