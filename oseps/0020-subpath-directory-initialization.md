---
title: Explicit SubPath Directory Initialization for PVC Volumes
authors:
  - "@gegemeimingzi"
creation-date: 2026-08-19
last-updated: 2026-08-19
status: provisional
---

# OSEP-0020: Explicit SubPath Directory Initialization for PVC Volumes

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
  - [API Surface](#api-surface)
  - [Validation Rules](#validation-rules)
  - [Kubernetes Provider Behavior](#kubernetes-provider-behavior)
  - [Error Semantics](#error-semantics)
  - [Idempotency and Metadata](#idempotency-and-metadata)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

OpenSandbox's `pvc` backend maps `Volume.subPath` to a Kubernetes
`volumeMounts.subPath`. When the referenced directory does not already exist
inside the PVC, Kubernetes does not create it for `subPath` mounts (unlike
`subPathExpr` with some CSI drivers, and unlike `hostPath` with
`DirectoryOrCreate`), so workloads fail to start or fail to write.

This OSEP proposes an explicit, Kubernetes-only opt-in on the `pvc` backend —
`pvc.createSubPathIfMissing` — that guarantees the directory referenced by a
non-empty `Volume.subPath` exists before the main sandbox container starts,
implemented as an init container that runs before the sandbox container. The
opt-in defaults to `false`, preserves exact existing behavior when omitted, and
fails the create request (never silently downgrades) when the guarantee cannot
be established.

## Motivation

Callers today must pre-create a required subPath directory out of band, for
example by launching a temporary provisioner sandbox that runs
`mkdir -p /data/runs/42` and then exits. This is:

1. **Fragile**: a second resource (the provisioner sandbox) must be created,
   waited on, and cleaned up by the caller on every create path.
2. **Non-atomic**: the directory creation is not coupled to the sandbox create
   request, so a retried or concurrent create can observe a half-prepared
   state.
3. **Silently divergent across providers**: OSEP-0003 states that a missing
   `subPath` is created (L495), but its Docker named-volume mapping requires
   the resolved path to exist (L203) and its test plan expects rejection when
   the resolved `subPath` does not exist (L514). The Kubernetes provider maps
   `subPath` to `volumeMounts.subPath` without any existence guarantee, while
   the Docker provider defers the outcome to container creation. No provider
   implements the documented creation guarantee today.

An explicit opt-in gives callers a single-request, fail-closed way to obtain
the documented guarantee, scoped to the Kubernetes provider where the
semantics are well-defined.

### Goals

- Provide a per-volume opt-in (`pvc.createSubPathIfMissing`) that, when
  `true`, guarantees a non-empty `Volume.subPath` directory exists on a
  Kubernetes PVC before the main sandbox container starts.
- The guarantee is all-or-nothing: the create request either succeeds with the
  directory present, or fails with a stable, documented error.
- The opt-in defaults to `false`; omission preserves exact existing behavior.
- Initialization is idempotent: re-running against an existing directory is a
  no-op that leaves metadata unchanged.
- Document the Kubernetes-only scope explicitly; Docker and other providers
  reject the opt-in with a clear error rather than silently ignoring it.

### Non-Goals

- **Not** implementing directory initialization on the Docker provider or any
  provider other than Kubernetes.
- **Not** changing `pvc.createIfNotExists` semantics: it remains backing-storage
  creation only, never subPath directory creation.
- **Not** combining backing-storage creation and directory initialization in a
  single request (see [Notes/Constraints/Caveats](#notesconstraintscaveats)).
- **Not** proposing repair for Pending, unbound, inaccessible, or otherwise
  unusable PVCs.
- **Not** supporting Windows workloads, block-mode volumes, or read-only final
  mounts.
- **Not** providing automatic cleanup of initialized directories when a sandbox
  is deleted.

## Requirements

- The opt-in must be a new field on the `pvc` backend object in the `Volume`
  schema (`specs/sandbox-lifecycle.yml`), additive and backward-compatible.
- `createSubPathIfMissing: true` with an empty `subPath` must be rejected by
  validation.
- The opt-in applies the existing relative-path and no-`..` rules already
  enforced by `ensure_valid_sub_path` in
  `server/opensandbox_server/services/validators.py`.
- The Kubernetes provider must reject the opt-in when it cannot establish the
  guarantee (e.g. unsupported runtime, unusable claim) **before** persistent
  mutation or main-container start.
- Initialization must complete before the main sandbox container starts; if it
  fails, create returns an error.
- `createIfNotExists: true` combined with `createSubPathIfMissing: true` in
  the same request is rejected in the initial scope.
- The final mount must be read-write for the initial scope (a read-only final
  mount combined with the opt-in is rejected).
- Newly created directories must have well-defined creator identity and initial
  UID, GID, mode, umask, and ACL behavior, documented in the release notes and
  resolved from the init container's `securityContext` and the PVC mount
  options.
- Initialization retries and stable errors: transient init-container failures
  are retried by the Kubernetes pod lifecycle (restartPolicy), and terminal
  failures surface as a stable, documented sandbox error carrying the init
  container's termination message.
- `volumes` remain incompatible with `extensions.poolRef` under the existing
  Lifecycle schema; the opt-in does not change that constraint.

## Proposal

Add a boolean field `createSubPathIfMissing` (default `false`) to the `pvc`
backend object in the `Volume` schema. When `true` and the backend is
Kubernetes, the server provisions an init container in the sandbox pod that
creates the directory referenced by `Volume.subPath` (recursively, with
`mkdir -p`) before the main sandbox container starts.

The init container runs with the same PVC mounted at a scratch mount point and
executes `mkdir -p <subPath>` inside that mount. Because the init container
shares the pod's PVC, the directory lands on the same storage that the final
mount targets, and the final mount's `subPath` resolves to the now-existing
directory.

Illustrative request:

```yaml
volumes:
  - name: workspace
    pvc:
      claimName: workspace-data
      createIfNotExists: false
      createSubPathIfMissing: true
    mountPath: /workspace
    subPath: runs/42
    readOnly: false
```

### Notes/Constraints/Caveats

- **Kubernetes-only**: The opt-in is defined and implemented only for the
  Kubernetes provider. The Docker provider and any other backend must reject a
  request with `createSubPathIfMissing: true` with a stable, documented error
  (`UNSUPPORTED_SUBPATH_INIT`) rather than silently downgrading to ordinary
  `subPath` mounting.
- **No silent downgrade**: An unsupported provider or server version must
  reject the request before initialization or main-container start.
- **Backing-storage independence**: Backing-storage creation
  (`createIfNotExists`) and directory initialization are explicitly
  independent. The initial scope rejects combining them in one request to
  avoid cross-resource sequencing and rollback across PVC creation, binding,
  mountability, and directory initialization. A later design may allow both
  only if it defines their ordering and failure semantics.
- **Multiple volumes**: When several `Volume` entries reference the same PVC
  claim with different `subPath` values, each entry with the opt-in gets its
  own initialization; the init container mounts the claim once and creates
  each requested subPath.
- **Unknown-field handling**: Because the current request-model handling may
  ignore an unknown field on older servers, a client that sets
  `createSubPathIfMissing: true` against a server that does not support it
  could silently lose the guarantee. The implementation must therefore reject
  the combination via schema validation (the field is part of the published
  spec, and `additionalProperties: false` on the `pvc` object means an unknown
  field is rejected on servers that implement the schema). Server and SDK
  release notes must call out the minimum server version that supports the
  field.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Path traversal via `subPath` (`..`, absolute paths) | Reuse the existing `ensure_valid_sub_path` validator; no new traversal surface. |
| Symlink escapes inside the PVC (a parent component of `subPath` is a symlink pointing outside the claim) | Documented as caller responsibility in the initial scope; the init container runs `mkdir -p` which follows symlinks. A stricter design (e.g. `O_NOFOLLOW` walking) is deferred; the guarantee is "directory exists at the resolved path", matching `volumeMounts.subPath` resolution semantics. |
| Concurrent creates racing to create the same directory | `mkdir -p` is idempotent and safe under concurrency; no locks required. |
| Init container image availability / trust boundary | Reuse the existing execd or a minimal busybox-style image already available in the sandbox image set; the image is server-configured and operator-controlled. |
| Init container needs write permission on the PVC | The init container runs as root or with the PVC's owning UID/GID; exact `securityContext` is a server-side configuration detail resolved at implementation time, consistent with how the sandbox container is configured. |
| Partial creation leaves persistent directories on failure | Documented: initialization is idempotent and `mkdir -p` leaves a valid directory on partial success; no automatic cleanup is provided (explicitly out of scope). |

## Design Details

### API Surface

In `specs/sandbox-lifecycle.yml`, under the `PVC` object properties, add:

```yaml
createSubPathIfMissing:
  type: boolean
  default: false
  description: |
    When true (Kubernetes provider only), ensures that the directory
    referenced by the volume's `subPath` exists on the backing claim before
    the main sandbox container starts. The directory is created recursively
    by an init container if missing. Requires a non-empty `subPath` and
    `createIfNotExists: false`. Rejected on providers other than Kubernetes.
```

Corresponding additions to the server API model in
`server/opensandbox_server/api/schema.py` (`PVC` class) and the Kubernetes
volume-helper path in `server/opensandbox_server/services/k8s/volume_helper.py`
are implementation details defined at implementation time, with tests per the
Test Plan below.

### Validation Rules

- `createSubPathIfMissing: true` with empty/absent `subPath` → `400`
  `INVALID_SUB_PATH_INIT`: "createSubPathIfMissing requires a non-empty
  subPath".
- `createSubPathIfMissing: true` with `createIfNotExists: true` → `400`
  `INVALID_SUB_PATH_INIT`: "createSubPathIfMissing cannot be combined with
  createIfNotExists in the initial scope".
- `createSubPathIfMissing: true` with `readOnly: true` final mount → `400`
  `INVALID_SUB_PATH_INIT`: "createSubPathIfMissing requires a read-write final
  mount".
- `createSubPathIfMissing: true` on a non-Kubernetes runtime (e.g. Docker) →
  `400` `UNSUPPORTED_SUBPATH_INIT`: "createSubPathIfMissing is only supported
  on the Kubernetes provider".

### Kubernetes Provider Behavior

When all validation passes, the server adds an init container to the sandbox
pod before the sandbox container:

1. Mount the PVC at a scratch mount path (e.g. `/mnt/subpath-init`).
2. Run `mkdir -p <subPath>` with the resolved relative path.
3. Exit 0 on success; exit non-zero on failure, which fails the pod and
   surfaces as a sandbox create/run failure with the init container's log
   attached to the error.

The final `volumeMounts.subPath` is unchanged; the init container only
guarantees the target directory exists.

### Error Semantics

- Validation errors are returned synchronously by the create request
  (`4xx`), before any cluster mutation.
- Init-container failures surface as pod-level failures: the sandbox does not
  reach Ready, and the server reports the init container's termination
  message with a stable error code.
- Transient init-container failures (e.g. pod eviction, node pressure) are
  retried by the Kubernetes pod lifecycle under the sandbox pod's
  `restartPolicy`; the create request stays in a retrying state until success
  or a terminal failure.
- Terminal failures (non-zero exit after retries, unusable claim, mount
  failure) produce a stable, documented error code (e.g.
  `SUBPATH_INIT_FAILED`) with the init container's logs attached, so callers
  can distinguish "directory not initialized" from other sandbox failures.

### Idempotency and Metadata

- If the directory already exists, `mkdir -p` succeeds and leaves all metadata
  (ownership, mode, umask-derived attributes, ACLs) unchanged.
- For newly created directories, ownership/UID/GID/mode follow the init
  container's `securityContext` and the PVC's mount options, applied under the
  init container's umask; exact defaults (including any default ACL inherited
  from the PVC mount or filesystem) are resolved at implementation time and
  documented in the release notes.
- Successful existence does not guarantee that the workload can write the
  directory (e.g. permissions may still block writes); this is consistent with
  ordinary `subPath` mounts.

## Test Plan

Unit tests (`server/tests/`):

- Validation: empty `subPath` + opt-in → rejected; `createIfNotExists: true`
  + opt-in → rejected; read-only final mount + opt-in → rejected; valid
  request → accepted.
- Validator keeps rejecting absolute paths and `..` components even with the
  opt-in set.
- Kubernetes volume-helper: request with opt-in produces an init container
  with `mkdir -p` and the correct scratch mount; request without opt-in
  produces no init container (byte-for-byte parity with today's output).
- Docker provider: opt-in → `UNSUPPORTED_SUBPATH_INIT` rejection.

Integration tests:

- Kind-based e2e (kubernetes `test/e2e/`): create a PVC-backed sandbox with
  `createSubPathIfMissing: true` and a non-existent nested `subPath`; assert
  the sandbox reaches Ready and the directory exists; assert a second create
  against the same path is idempotent.
- Negative e2e: `createSubPathIfMissing: true` on a read-only mount → create
  rejected before pod creation.

Manual verification:

- `helm install` the server, run the e2e scenarios above against a Kind
  cluster, and capture pod spec showing the init container.

## Drawbacks

- Adds a new API field and Kubernetes-only behavior, increasing the surface
  area of the `pvc` backend.
- Init containers add a small startup latency to sandbox creation when the
  opt-in is used (one extra container start).
- The guarantee is weaker than it appears: "directory exists" does not imply
  "workload can write" (permissions), and symlink resolution inside the PVC is
  caller responsibility.

## Alternatives

1. **Document that callers must pre-create directories** (status quo). Rejected:
   leaves the OSEP-0003 contradiction unresolved and imposes fragile
   out-of-band provisioning on every affected caller.
2. **Use `subPathExpr`** with a fixed expression. Rejected: `subPathExpr` does
   not create directories either; it only expands variables.
3. **Init container per provider, general mechanism**: implement the same
   opt-in for Docker via named-volume path initialization. Deferred: Docker
   named volumes have different mount semantics and the OSEP-0003 contract for
   them expects rejection on missing paths; a separate OSEP is needed.
4. **Server-side directory creation via a Kubernetes Job**: heavier, requires
   Job lifecycle management and race handling; init container is simpler and
   naturally tied to the pod lifecycle.

## Infrastructure Needed

- No new repositories, services, or third-party dependencies.
- The init container image is drawn from the existing sandbox image set
  (server-configured), so no new image build pipeline is required.
- Kind-based e2e already exists in `kubernetes/test/e2e/`; no new cluster
  infrastructure.

## Upgrade & Migration Strategy

- **Backward compatibility**: existing valid requests that omit
  `createSubPathIfMissing` retain exact current semantics. The new field
  defaults to `false`.
- **Server version gating**: servers that implement the new schema field
  accept the opt-in; older servers reject it via `additionalProperties: false`
  schema validation (the field will not exist in their published schema), so
  the guarantee can never be silently lost.
- **Docs and release notes**: update the volume documentation in `docs/`,
  SDK reference, and release notes to state the Kubernetes-only scope, the
  error codes, and the minimum server version required.
- **SDKs**: add the field to SDK request builders and regenerate clients from
  the updated spec, so typed SDKs expose `createSubPathIfMissing`.
