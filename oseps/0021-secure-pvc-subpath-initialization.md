---
title: Secure Initialization of Missing PVC Volume SubPaths
authors:
  - "@cwj2001"
creation-date: 2026-08-19
last-updated: 2026-08-20
status: draft
---

# OSEP-0021: Secure Initialization of Missing PVC Volume SubPaths

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Support boundary](#support-boundary)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Public interface](#public-interface)
  - [Invariant behavior](#invariant-behavior)
  - [Validation and execution order](#validation-and-execution-order)
  - [Errors and diagnostics](#errors-and-diagnostics)
  - [Security model](#security-model)
  - [Runtime adapters](#runtime-adapters)
  - [Pool incompatibility](#pool-incompatibility)
  - [Retain, delete, and reject decisions](#retain-delete-and-reject-decisions)
  - [Feature-scoped creation and rollout](#feature-scoped-creation-and-rollout)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
  - [Pre-publication note](#pre-publication-note)
<!-- /toc -->

## Summary

This proposal adds an opt-in, mount-scoped way to initialize a missing directory
used as a Kubernetes PVC `subPath`. The only new volume/mount configuration
field is `Volume.createSubPathIfMissing`, defaulting to `false` beside the
existing backend-neutral `subPath`. The compatibility protocol also defines a
distinct feature-scoped create route. For an opt-in PVC mount,
`pvc.createIfNotExists: false` MUST be explicitly present; omission is rejected
before the existing PVC provisioning path. The initial implementation is
limited to Linux Kubernetes non-Pool PVC mounts and uses a server-owned,
trusted, restricted init mechanism; the requested final mount remains
read-only or read-write as requested.

Omission preserves ordinary `subPath` behavior. There is no silent fallback to
another runtime or to an unsafe directory-creation path, and a provider that
cannot honor the opt-in request fails before creating sandbox-side effects.

## Motivation

`subPath` selects a directory within an already provisioned volume, while the
PVC itself has an independent lifecycle. A PVC may exist and be correctly
bound even though the selected directory does not. Kubernetes then rejects a
container mount that names the missing directory. This makes a valid storage
allocation unusable for workflows that assign a new per-sandbox or per-task
directory at creation time.

The current contract also needs to be precise about what is and is not created.
Creating a PVC is storage lifecycle management; creating one directory below a
PVC is mount preparation. Conflating them can grant a request authority over
storage provisioning, hide a provider limitation, or change the behavior of
existing clients. A narrowly scoped opt-in makes the preparation operation
explicit and keeps the default fail-if-missing behavior intact.

### Goals

- Add a stable, mount-scoped opt-in for initializing a missing canonical
  relative PVC subpath.
- Start with only Linux Kubernetes PVC mounts on the non-Pool creation path.
- Preserve the requested final mount access mode, including a read-only final
  mount, while permitting a server-owned init-only read-write preparation step
  where the platform requires it.
- Validate all requests and provider capability before sandbox-side effects.
- Use Linux `openat2` containment and a dedicated trusted initializer rather
  than exposing arbitrary execution controls.
- Keep the existing `code`/`message` error envelope, with a minimal stable
  volume error vocabulary and normative HTTP retry classification through the
  existing transport semantics.
- Make the feature-scoped route safe with old and mixed-version server
  replicas.
- Leave PVC provisioning, deletion, ownership, permissions, and cleanup to
  their existing lifecycle owners unless explicitly covered by this proposal.

### Non-Goals

- Creating, binding, resizing, deleting, or otherwise managing a PVC.
- Initializing arbitrary host, NFS, object, OSS, CSI, or Docker volume paths.
- Supporting Pool sandboxes in the first implementation.
- Exposing UID, GID, mode, image, command, PodSpec, lifecycle hook, or generic
  init-container knobs to callers.
- Providing a generic hook or arbitrary command execution API.
- Repairing or changing permissions, ownership, labels, or existing directory
  contents.
- Rolling back directories created before a later mount or sandbox failure.
- Replacing Kubernetes/OpenSandbox lifecycle APIs with direct Kubernetes API
  calls from clients.
- Guaranteeing coordination between different claims, paths, sandboxes, or
  providers beyond coalescing identical requests during one create operation.
- Making a missing subpath succeed by silently disabling `subPath`, changing
  the requested mount mode, or falling back to another provider.

## Requirements

1. The only new volume/mount configuration field introduced by this OSEP is
   `Volume.createSubPathIfMissing: boolean`, beside the existing backend-neutral
   `subPath`, with default `false`. The OSEP also defines a distinct public
   feature-scoped create route as its compatibility protocol.
2. The opt-in is valid initially only for a Linux Kubernetes PVC mount on the
   non-Pool path, placed on an operator-certified homogeneous Linux node pool
   with an approved StorageClass/filesystem profile. Scheduler placement MUST
   be constrained to that profile; an unavailable profile is rejected before
   side effects.
3. `subPath` is canonical relative path data. Absolute paths, empty segments,
   traversal, and other unsafe forms are rejected according to the validation
   rules below.
4. The initializer must never follow symlinks or escape the PVC root, including
   during concurrent replacement or traversal. Linux `openat2` with
   `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS |
   RESOLVE_NO_XDEV` is required; `O_NOFOLLOW`-only fallback is forbidden.
5. The operation creates only missing path components needed for the requested
   subpath and never chmods/chowns, alters ACLs, xattrs, or labels, renames,
   deletes, replaces, or writes entries/content in pre-existing directories.
   Creating a child necessarily changes the immediate existing parent directory
   entries and may change its mtime/ctime; atime, mtime, ctime, and
   provider-visible metadata are not guaranteed.
6. Identical `(PVC claim, canonical subpath)` requests in one create operation
   may be coalesced internally. Existing mount semantics, including repeated or
   overlapping `subPath` mounts, remain unchanged unless already invalid.
7. A read-only final volume mount remains read-only. The dedicated init
   container and its server-owned read-write PVC-root mount remain in the Pod
   spec, complete before workload containers start, and are unavailable to
   workload containers. The init binary exits 0 only after preparation
   succeeds; Kubernetes then starts the workload, and no post-exit server
   interpretation can retrospectively keep it stopped.
8. The PVC must already exist and remain subject to the existing PVC lifecycle
   and namespace rules. For an opt-in request, the `pvc` object MUST explicitly
   contain `createIfNotExists: false`; omission is rejected even though the
   upstream default is true. Presence-aware validation occurs before the
   existing PVC provisioning path, and `true` is rejected.
9. The trusted init binary exits 0 only after preparation succeeds; reserved
   exit 20 and 21 map to the normative 422 and 503 categories in
   [Errors and diagnostics](#errors-and-diagnostics). Termination data is
   diagnostic-only and cannot override the exit code. The existing
   `code`/`message` envelope is retained, with no host paths, secrets, pod
   details, or raw command output exposed. Current ordinary create misuse and
   missing/true `pvc.createIfNotExists` return
   `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST`/400 before side
   effects. Clients classify retryability from the status/category, and SDKs
   do not automatically replay create requests.
10. New SDKs MUST invoke the feature-scoped route for opt-in requests. A raw
    legacy server returns its normal 404/405 for that route and creates nothing;
    only that result is mapped locally to a typed unsupported result, with no
    automatic retry through the standard route. A current/new server that
    cannot support the effective tuple returns the specified 412 error before
    side effects.

## Proposal

`Volume.subPath` remains the existing backend-neutral mount field. This OSEP
adds `createSubPathIfMissing` only to the PVC mount contract:

```yaml
volumes:
  - name: task-data
    pvc:
      claimName: task-data
      createIfNotExists: false
    mountPath: /workspace/data
    subPath: tenants/acme/task-42
    createSubPathIfMissing: true
    readOnly: true
```

The boolean is a property of this PVC mount, not of the PVC resource and not of
the sandbox in general. For an opt-in, the `pvc` object MUST explicitly contain
`createIfNotExists: false`; an omitted field is rejected before the existing
PVC provisioning path, even though the upstream default is true. A request with
`createSubPathIfMissing` omitted is otherwise identical to an existing request
with the same `subPath` and retains ordinary provider behavior. For the initial
Kubernetes adapter, ordinary behavior for a missing subpath is to fail during
mount preparation; the opt-in never changes that behavior by omission.

### Support boundary

The first supported tuple is:

| Dimension | Initial boundary |
| --- | --- |
| Runtime | Kubernetes on Linux |
| Storage | Existing PVC in the sandbox namespace |
| Node pool | Operator-certified homogeneous Linux node pool |
| Storage profile | Operator-approved StorageClass/filesystem profile |
| Placement | Scheduler constrained to the certified/profile-matched nodes |
| Creation path | Non-Pool sandbox creation |
| Selection | `subPath` present and canonical relative |
| Option | `createSubPathIfMissing: true` |
| Final access | `readOnly` either `true` or `false` |

All other runtime, operating-system, backend, and Pool combinations are
unsupported in this proposal. The provider returns an unsupported-capability
error before it creates a Pod, init container, mount, or directory.
There is no silent fallback to a normal mount, to a different backend, or to a
direct Kubernetes operation by the client.

### Notes/Constraints/Caveats

- The PVC is an existing named storage resource. Its binding, retention, and
  deletion remain governed by the platform and existing OpenSandbox behavior.
- The subpath directory is data inside that PVC. It is not a Kubernetes object,
  is not independently retained by OpenSandbox, and is not deleted when a
  sandbox is deleted.
- The initializer never chmods/chowns, alters ACLs, xattrs, labels, renames,
  deletes, replaces, or writes entries/content in pre-existing directories. It
  creates missing components with server-defined safe defaults; callers cannot
  choose those defaults in this API. Creating a child changes the immediate
  existing parent directory entries and may change its mtime/ctime; atime,
  mtime, ctime, and provider-visible metadata are not guaranteed.
- The dedicated init container's read-write PVC-root mount is not a permission
  grant to workload containers. It remains in the Pod spec for the init
  container, completes before workload startup, and is not mounted into any
  workload container; workload containers receive only the requested final
  access mode.
- `createIfNotExists` is not an alternative spelling or compatibility alias.
  For opt-in requests it MUST be present and false; true or omission is invalid
  and must not be translated into `createSubPathIfMissing`.
- No public field may carry an image, command, PodSpec fragment, hook, UID, GID,
  mode, or equivalent execution or permission control.

### Risks and Mitigations

- **Path escape or symlink attack:** require Linux `openat2` with
  `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS |
  RESOLVE_NO_XDEV`, rooted at the mounted PVC. A missing operator-certified
  node/storage profile returns
  `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` with HTTP 412 before Pod
  creation. A runtime `openat2`/filesystem failure after init starts uses exit
  20 or 21 and the normal post-Pod precedence; never fall back to
  `O_NOFOLLOW` alone.
- **Unexpected write exposure:** keep the init container and its read-write
  mount server-owned, scope that mount only to the init container, and use a
  restricted non-privileged context. Kubernetes must complete the init
  container before starting workload containers.
- **Partial creation:** report the operation as failed if any component cannot
  be prepared. Do not claim rollback; directories already created remain in
  the PVC and are safe to reuse on a retry.
- **Mixed-version rollout:** send opt-in requests only through the distinct
  feature-scoped create route, whose routing and server enforcement are atomic.
  Do not rely on semver or a preflight result.
- **Mount compatibility:** coalesce identical preparation requests internally,
  but preserve existing repeated and overlapping `subPath` mount semantics.
- **Existing-directory effects:** protect pre-existing directory entries and
  content as specified; allow the immediate parent entry and mtime/ctime effect
  required to create a child, without promising provider metadata stability.
- **Diagnostic disclosure:** retain detailed initialization causes in
  access-controlled server logs and standard diagnostics; return only the
  existing `code`/`message` envelope with sanitized text.

## Design Details

### Public interface

The only new volume/mount configuration field is:

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `subPath` | string | absent | Canonical relative directory below the PVC root to mount |
| `createSubPathIfMissing` | boolean | `false` | Permit secure initialization of missing components for this mount |

The field is valid only when `subPath` is present. `true` requests
initialization; `false` and omission request ordinary runtime behavior. The
API does not expose a separate initializer object or any execution parameters.
Existing schema fields keep their current meaning, including `readOnly`.
The `subPath` row is shown only for placement context; this OSEP does not
redefine that backend-neutral field.

In addition to this configuration field, the compatibility protocol defines the
distinct public feature-scoped create route described in
[Feature-scoped creation and rollout](#feature-scoped-creation-and-rollout).
That is a transport-protocol addition, not a volume/mount configuration field.

Example of a read-write final mount:

```yaml
volumes:
  - name: task-data
    pvc:
      claimName: task-data
      createIfNotExists: false
    mountPath: /workspace/data
    subPath: tenants/acme/task-42
    createSubPathIfMissing: true
    readOnly: false
```

Example of a read-only final mount:

```yaml
volumes:
  - name: model-data
    pvc:
      claimName: model-data
      createIfNotExists: false
    mountPath: /models
    subPath: released/2026-08
    createSubPathIfMissing: true
    readOnly: true
```

For the second example, the dedicated init container uses a server-owned
read-write PVC-root mount, while `/models` in the final sandbox is read-only.
The read-write root mount remains scoped to the init container in the Pod spec;
it is not mounted into workload containers and cannot alter the requested mode.

### Invariant behavior

The following are contract invariants:

1. **Opt-in only:** omission and `false` never create a missing subpath as a
   consequence of this OSEP.
2. **Existing data preserved:** existing directories are opened and used, not
   recreated, chmod-ed, chowned, renamed, or emptied.
3. **PVC lifecycle is separate:** the server may verify and mount an existing
   claim, but does not create or delete the claim. Subpath initialization does
   not alter claim retention or binding.
4. **Final access preserved:** the final `readOnly` setting is exactly the
   requested setting. The server-owned read-write root mount is present only
   on the dedicated init container; it is not mounted into workload
   containers.
5. **No unsafe fallback:** an unsupported or failed opt-in never becomes a
   mount without `subPath`, a mount of the PVC root, or a mount with altered
   permissions.
6. **No rollback promise:** paths created by a failed attempt are not removed
   automatically. A retry observes them as existing directories and must still
   apply the same safety checks.
7. **Existing mount semantics:** identical preparation requests may share one
   internal action, while repeated or overlapping `subPath` mounts retain their
   existing behavior and validation.
8. **Exit gate:** the init binary exits 0 only after successful preparation;
   exit 20 and 21 are the only reserved nonzero preparation categories. A
   server cannot keep the workload stopped after exit 0 based on diagnostics.
9. **Trusted boundary:** only the server's dedicated initializer can perform
   this preparation. A user image, user command, generic hook, or arbitrary
   PodSpec cannot opt into it.

### Validation and execution order

The provider must use an order that makes unsupported requests fail before
side effects:

1. Parse the request with presence awareness. For every opt-in PVC volume,
   require an explicitly present `pvc.createIfNotExists: false` before entering
   the existing PVC provisioning/validation path; reject omission, `true`,
    malformed volume entries, invalid backend combinations, or an opt-in
    without `subPath` with
    `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST`/400 when the route or
    field combination is invalid.
2. Determine the runtime, operating system, creation path, and configured
   operator-certified node/StorageClass/filesystem profile. If that profile or
   its required `openat2` policy is unavailable, return
   `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` with HTTP 412 before
   creating or modifying any resource. Do not claim that runtime filesystem
   behavior is fully knowable preflight; failures observed after init starts
   use exit 20 or 21 and normal post-Pod precedence.
3. Validate the existing PVC reference and namespace using the ordinary PVC
   lifecycle rules. Do not create a PVC.
4. Parse and canonicalize every requested `subPath`. Reject absolute paths,
   empty or ambiguous components, `.`/`..` components, NUL bytes, separator
   tricks, non-canonical representations, and values beyond the bounded plan
   limits. The canonical value is relative to the PVC root and contains no
   leading separator. Invalid requests return
   `VOLUME::INVALID_SUB_PATH` with HTTP 400.
5. Construct the complete volume and mount plan using the existing volume
   validation rules. Coalesce identical claim/path preparation requests
   internally, but do not add source-ancestor overlap rejection or change the
   existing behavior for repeated or overlapping `subPath` mounts.
6. Verify that every requested final mount can be represented with its exact
   requested `readOnly` value. Add the server-owned read-write PVC-root mount
   only to the dedicated init container without changing the final plan.
7. Construct the Pod spec with a dedicated trusted init container and a
   server-owned read-write PVC-root mount scoped only to that init container.
   It runs in a restricted, non-privileged context and receives no
   caller-controlled command, image, identity, mode, or PodSpec settings.
8. Start the Pod with the init container and workload containers in the same
   Pod spec. Kubernetes starts the init container first and does not start
   workload containers until it completes.
9. From the PVC root, the init container walks directory file descriptors using
   `openat2` with the required resolution flags, `mkdirat` for missing
   components, and FD-based directory type validation. Never invoke a shell or
   reconstruct a path string.
10. Observe the bounded structured initializer result for diagnostics, but use
    the process exit status as the startup gate. Exit 0 means success; exit 20
    maps to terminal 422 and exit 21 maps to unavailable 503. Absent, malformed,
    oversized, or untrusted diagnostic data cannot override the exit status. An
    unexpected nonzero status, OOM, or timeout remains an existing lifecycle
    infrastructure error. On any failure, do not roll back directories already
    created. On exit 0, Kubernetes starts workload containers with the exact
    requested final access mode; no server interpretation after exit 0 can keep
    them stopped, and the read-write PVC-root mount is unavailable to them.
11. If final mount or sandbox creation fails after initialization, report that
    failure without deleting initialized directories. The operation remains
    safe to retry subject to normal idempotency and provider rules.

If any pre-side-effect step fails, no initializer, sandbox pod, or directory
may be created by this request.

### Errors and diagnostics

The feature-scoped route uses the existing error envelope with only `code`
and `message`. This OSEP defines the following minimal stable volume categories
and normative HTTP statuses:

| Code | HTTP status | Meaning and retry classification |
| --- | --- | --- |
| `VOLUME::INVALID_SUB_PATH` | 400 Bad Request | A supplied `subPath` is malformed, non-canonical, unsafe, or otherwise invalid. Do not retry without changing the request. |
| `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST` | 400 Bad Request | Opt-in was sent through ordinary create, omits `subPath`, or the opt-in PVC omitted `createIfNotExists` or set it to true. Do not retry without changing the request/route. |
| `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` | 412 Precondition Failed | The effective runtime, OS, backend, Pool path, resolver, filesystem, or provider cannot honor the opt-in. Do not retry unless capability/configuration changes. |
| `VOLUME::PVC_SUBPATH_INITIALIZATION_FAILED` | 422 Unprocessable Content | The trusted initializer returned a deterministic terminal result, including a non-directory or symlink component, permission denial, or read-only filesystem. Do not automatically retry. |
| `VOLUME::PVC_SUBPATH_INITIALIZATION_UNAVAILABLE` | 503 Service Unavailable | The trusted initializer exits with reserved code 21 for a transient or indeterminate storage/runtime failure. Retry may be appropriate; include `Retry-After` when the retry delay is known. |

The precedence is normative:

1. Preflight path failures return `VOLUME::INVALID_SUB_PATH`/400; ordinary-route
   opt-in misuse or missing/true `pvc.createIfNotExists` returns
   `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST`/400. Unsupported
   effective tuples return
   `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED`/412, both before side
   effects.
2. After the trusted init container starts, exit 20 decides
   `VOLUME::PVC_SUBPATH_INITIALIZATION_FAILED`/422 and exit 21 decides
   `VOLUME::PVC_SUBPATH_INITIALIZATION_UNAVAILABLE`/503. Diagnostic data cannot
   change that decision.
3. An unexpected nonzero status, image pull, scheduling, PVC attach, OOM, or
   timeout before or without a reserved init result remains an existing
   lifecycle infrastructure error, not a fabricated subpath 422/503. A final
   main container mount failure remains the existing lifecycle error.
4. No directory rollback is attempted for any later failure.

The init result is bounded and structured, with a fixed version, a short token
from a server-owned allowlist only, and reserved exit categories. Exit code `0`
means success, `20` is the reserved terminal category (422), and `21` is the
reserved unavailable category (503). The diagnostic result is emitted before
exiting where possible, but absent, malformed, oversized, or untrusted data
cannot override the exit code; any unexpected nonzero status remains an
existing lifecycle infrastructure error. Raw initializer logs remain
server-side; the API exposes only the existing `code`/`message` envelope with
sanitized token-derived text. No diagnostic IDs, retry fields, path fields, or
other `ErrorResponse` properties are added, and host paths, kubeconfig details,
service-account tokens, secrets, raw PodSpec, and unfiltered output are never
exposed.

Clients classify retryability using the HTTP status and stable code. SDKs and
client helpers must not automatically replay a create request, including for
503; an explicit caller may retry according to the category and any
`Retry-After` value.

### Security model

The initializer is a dedicated trusted server component, not a user-provided
container or hook. Callers cannot control its executable, command, image,
identity, privileges, or PodSpec. They supply only bounded, validated relative
path data in a server-generated plan. The operator selects an immutable,
digest-pinned initializer artifact. A fixed root identity is used only if
necessary for mount-root access; it is server-selected and not caller
configurable.

The initializer has the fixed identity `runAsUser: 0`, `runAsGroup: 0`, and no
supplemental groups. Its profile is `privileged: false`,
`allowPrivilegeEscalation: false`, `automountServiceAccountToken: false`, with
no host mounts, a read-only container root, RuntimeDefault seccomp, and
`capabilities.drop: [ALL]` with no additions. If this fixed profile cannot
access the PVC root, the initializer returns the terminal failure rather than
relaxing the profile. It has explicit CPU, memory, and active-deadline limits.
The initializer neither requires nor intentionally uses network access; this
proposal does not claim per-container network isolation under the shared Pod
network namespace. The server bounds plan path length, component length,
component count, and total path count before creating the Pod. No generic POSIX
ownership/permission repair is provided; pre-existing directory entries and
content are protected as specified, while child creation may affect its
immediate parent directory entries and mtime/ctime.

Path handling has two independent stages: lexical canonicalization before any
mount action, and secure filesystem traversal after the PVC is mounted. The
second stage is authoritative. Linux `openat2` MUST use
`RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS |
RESOLVE_NO_XDEV`. The feature is enabled only for an operator-certified,
homogeneous Linux node pool and approved StorageClass/filesystem profile, with
scheduler placement constrained to that profile. If the configured/certified
profile is missing, return
`VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` with HTTP 412 before Pod
creation. A runtime `openat2`/filesystem failure after init starts uses exit 20
or 21 and normal post-Pod precedence; do not use an `O_NOFOLLOW`-only fallback.

The implementation walks directory file descriptors: use `mkdirat` for a
missing component, then reopen it with `openat2` and validate the file
descriptor is a directory before continuing; use `openat`/`openat2` for existing
components with FD-based type validation. Never invoke a shell,
reconstruct an absolute path, or validate containment using strings alone.
Symlink components, unexpected non-directory components, mount-point escapes,
and races that invalidate containment are rejected.

### Runtime adapters

The core API carries the opt-in field, while the runtime adapter owns the
platform-specific preparation:

- **Kubernetes/Linux non-Pool adapter:** supported first. It verifies the PVC,
  adds the dedicated restricted init container and its read-write PVC-root
  mount to the Pod spec, and emits workload `volumeMount` entries with the
  requested `readOnly` value and `subPath`. The init-only mount remains in the
  Pod spec but is not available to workload containers.
- **Kubernetes Pool adapter:** unsupported in this OSEP. Pool task and pod
  assembly have different lifecycle and mount ownership, so the adapter must
  reject the opt-in rather than accidentally initialize from a pool worker.
- **Docker and other adapters:** unsupported initially. They may later define
  their own implementation only through a new proposal or an explicitly
  reviewed extension that preserves this field's opt-in and security contract.
- **Direct client Kubernetes access:** prohibited. Clients use the normal
  sandbox creation API; the provider performs the server-side preparation.

An adapter that supports ordinary `subPath` but not secure initialization must
return `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` for `true`. It must not
silently treat `true` as `false`.

### Pool incompatibility

Pool sandboxes are deliberately outside the first support boundary. A pool may
pre-create or reuse storage and may run a task through a different container
and mount lifecycle. Initializing a path during ordinary pod creation could
therefore affect a pool-owned resource at the wrong time or create a race
between tasks. Until a separate design proves ownership, isolation, and
lifecycle semantics for pools, a Pool request with `createSubPathIfMissing: true`
is rejected before pool allocation or task creation.

### Retain, delete, and reject decisions

These decisions constrain later implementation and review:

**Retain**

- The existing `Volume`/`subPath` model and its ordinary omission behavior.
- A single boolean, mount-scoped opt-in with a false default.
- Existing PVC provisioning, binding, namespace, retention, and deletion
  ownership.
- Final `readOnly`/read-write semantics and provider-specific unsupported
  errors.
- A dedicated trusted initializer, secure descriptor-relative traversal,
  request-local coalescing, and the normative code/status retry classification;
  SDKs do not automatically replay create requests.

**Delete**

- Any unconditional rule that creates missing subpaths when the option is
  omitted.
- Any implicit PVC creation or cleanup coupled to subpath preparation.
- Any rollback process that promises to remove directories after a later
  failure.
- Any dependency on user-image startup or an in-sandbox setup command.

**Reject**

- Public UID/GID/mode, image, command, PodSpec, hook, or generic init knobs.
- `createIfNotExists: true` or aliases that broaden creation semantics.
- Direct Kubernetes API requirements for business clients.
- Silent fallback on unsupported providers, old servers, Pool requests, unsafe
  paths, or failed initialization.
- Symlink-following, string-only containment checks, or privileged host access.

### Feature-scoped creation and rollout

Opt-in creation is normative only through this additive route proposed by this
OSEP; it is new and is not an assertion about an existing OpenSandbox endpoint:

```http
POST /v1/sandboxes/with-subpath-initialization
operationId: createSandboxWithSubPathInitialization
```

The route uses the same authentication, request body, and success response as
ordinary sandbox create. It is allowed only when one or more volumes set
`createSubPathIfMissing: true`; requests without that opt-in use ordinary
create. The route is feature-scoped.

Route dispatch and effective-tuple validation are server-enforced parts of one
create operation:

- A current server that recognizes `createSubPathIfMissing: true` on ordinary
  `POST /v1/sandboxes` MUST reject it with
  `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST` and HTTP 400 before PVC,
  workload, or directory side effects. Missing or true `pvc.createIfNotExists`
  uses the same rejection. A legacy server may ignore an unknown field, so
  opt-in callers MUST use this feature-scoped route.
- A raw legacy server that does not implement this route returns its normal
  404 or 405 before PVC, initializer, Pod, or workload mutation. It must not
  remap the route to ordinary create.
- New SDKs MUST invoke this route for opt-in requests. They map only that raw
  legacy 404/405 response to a local typed unsupported result and do not retry
  through the standard route automatically.
- A current/new server that implements the route but cannot support the
  effective runtime, OS, backend, Pool, resolver, filesystem, and provider
  tuple returns `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` with HTTP 412
  before PVC, initializer, Pod, or workload mutation.
- If a deployment cannot route this feature-scoped route atomically to current
  implementations while old and new API replicas are mixed, it sends no
  opt-in traffic. Operators keep the feature disabled until routing is safe.

No capability discovery endpoint or generic capability route is part of this
proposal. A false or omitted option continues to use ordinary create behavior.

## Test Plan

No implementation is included in this OSEP branch. An implementation claiming
conformance must add focused tests at each boundary:

### Contract and validation tests

- Omitted and `false` options preserve ordinary missing-subpath behavior.
- `true` without `subPath`, or with `createIfNotExists: true`, is rejected with
  `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST`/400 before any side
  effect. A supplied absolute path, empty component, dot component, traversal,
  NUL byte, or non-canonical path is rejected with `VOLUME::INVALID_SUB_PATH`/
  400 before any side effect.
- An opt-in PVC with omitted `createIfNotExists` is rejected with
  `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST`/400 despite the
  upstream default being true;
  explicit `createIfNotExists: false` is accepted for the existing PVC path.
- Unsupported runtime, OS, backend, Pool, resolver, filesystem, and provider
  combinations return
  `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED` with HTTP 412 before pod,
  initializer, mount, or filesystem changes.
- Invalid input returns `VOLUME::INVALID_SUB_PATH` with HTTP 400.
- Existing directories retain ownership, POSIX permission bits, ACLs, labels,
  and contents; no chmod/chown, ACL/xattr/label change, rename, delete,
  replacement, or entry/content write is permitted. Creating a child may
  change the immediate parent entries and mtime/ctime; other metadata is not
  asserted.
- Identical claim/path preparations may be coalesced; repeated and overlapping
  `subPath` mounts retain existing volume semantics.

### Secure filesystem tests

- Missing one or more nested components are created beneath the opened PVC
  root.
- Symlink components, symlink replacement races, non-directory components,
  and attempted root escapes are rejected using `openat2` with
  `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS |
  RESOLVE_NO_XDEV`; no `O_NOFOLLOW`-only fallback is accepted.
- Missing certification, a non-homogeneous node pool, an unapproved
  StorageClass/filesystem profile, or unconstrained scheduler placement returns
  `VOLUME::PVC_SUBPATH_INITIALIZATION_UNSUPPORTED`/412 before Pod creation.
- A runtime `openat2`/filesystem failure after init begins exits 20 or 21 and
  follows the post-Pod precedence rather than being preflighted as 412.
- Non-directory, symlink, permission, and read-only filesystem failures return
  `VOLUME::PVC_SUBPATH_INITIALIZATION_FAILED` with HTTP 422. Transient or
  indeterminate I/O/runtime failures return
  `VOLUME::PVC_SUBPATH_INITIALIZATION_UNAVAILABLE` with HTTP 503 and `Retry-After`
  when known.
- The init exits 20 for deterministic terminal preparation failure and 21 for
  transient/indeterminate preparation failure. Missing or malformed diagnostic
  data does not override either exit code; exit 0 starts the workload even when
  diagnostic data is absent.
- Unexpected nonzero init status, OOM, timeout, image pull, scheduling, or PVC
  attach failures before or without a reserved init result remain existing
  lifecycle infrastructure errors, not fabricated subpath errors.
- Already-created directories remain after a later component or final mount
  failure; a safe retry can reuse them.

### Kubernetes integration tests

- Non-Pool PVC creation succeeds for a missing subpath with final `readOnly:
  false`.
- Non-Pool PVC creation succeeds for a missing subpath with final `readOnly:
  true`, and the running workload cannot write through the final mount.
- The dedicated init container completes before workload startup, and its
  read-write PVC-root mount is absent from workload container mounts and
  inaccessible to workload containers.
- Existing PVC lifecycle is unchanged: no claim is created or deleted.
- Pool requests, unsupported adapters, and raw legacy 404/405 responses from
  the feature-scoped route fail before sandbox-side effects; current/new
  unsupported tuples return 412.
- Final mount failures preserve the requested mode and do not trigger
  directory rollback.
- Clients classify retryability from the status/code mapping, and SDKs do not
  automatically replay create requests.

### Compatibility tests

- An opt-in request reaches its configured workspace only through the
  feature-scoped route on a current implementation that supports the effective
  tuple.
- A current ordinary `POST /v1/sandboxes` carrying
  `createSubPathIfMissing: true` is rejected with
  `VOLUME::INVALID_PVC_SUBPATH_INITIALIZATION_REQUEST`/400 before PVC,
  workload, or directory side effects; missing or true
  `pvc.createIfNotExists` uses the same result.
- Existing requests without the field continue to work without changes to
  image, command, environment, workspace, or artifact handling.
- A raw legacy 404/405, an unsupported current tuple, and a mixed-replica route
  each produce an explicit unsupported result rather than silently changing the
  mount or retrying through standard create. SDKs surface the legacy result as
  `SubPathInitializationUnsupportedError`.

## Drawbacks

The feature adds a preparation phase and a trusted server component to sandbox
creation. Storage providers need careful filesystem testing, and a failed
request can leave an empty directory because rollback is intentionally not
promised. The narrow initial boundary also means users of Docker, Pool, or
other storage adapters must continue to provision subdirectories themselves.

## Alternatives

- **Require callers to pre-create directories:** simple and broadly portable,
  but requires extra credentials, setup races, and a privileged or out-of-band
  operation for common sandbox launches.
- **Create the directory in the user container:** works only when the image
  starts with sufficient access and cannot safely support a final read-only
  mount; it also turns storage preparation into an image-specific contract.
- **Expose an init image/command/PodSpec hook:** flexible but grants arbitrary
  execution and changes the security boundary. It is intentionally rejected.
- **Create every missing subpath by default:** convenient, but breaks the
  existing distinction between validation and mutation and makes omission
  unsafe for clients relying on fail-if-missing behavior.
- **Create or alter the PVC:** solves a different lifecycle problem and would
  give the mount option storage-provisioning authority it does not need.
- **Use a privileged host-side path operation:** potentially easier to
  implement, but violates tenant isolation and cannot establish safe
  descriptor-relative containment for the mounted filesystem.

## Infrastructure Needed

The first implementation needs a Kubernetes/Linux provider capable of:

- discovering and mounting an existing PVC for the preparation operation;
- running one dedicated trusted initializer under the deployment's restricted,
  non-privileged security policy;
- performing the required Linux `openat2` traversal; and
- reporting effective-tuple support and sanitized error information through the
  existing lifecycle path.

No public service, direct-client Kubernetes credential, registry, or generic
hook infrastructure is required by this proposal. The init container and its
read-write mount are owned by the Pod spec and normal Pod lifecycle, with no
user-configurable image or command; the mount is scoped only to the init
container and is not physically removed before workload startup.

## Upgrade & Migration Strategy

This is an additive opt-in. Existing requests and SDKs that omit
`createSubPathIfMissing` retain their ordinary runtime behavior. OSEP-0003
should describe missing-subpath behavior as runtime-dependent/fail-if-missing
and defer opt-in secure initialization to this OSEP; no existing volume request
is migrated automatically.

An opt-in request is not permitted to inherit the upstream `pvc` default:
`pvc.createIfNotExists: false` must be present. Requests that omit it are
rejected before the existing PVC provisioning path; this requirement does not
change ordinary requests that do not opt in.

The only new volume/mount configuration field is the mount-scoped boolean;
requests that set it to `true` must use the distinct feature-scoped route. The
route is a compatibility protocol addition, not additional volume
configuration. Current standard-route misuse is a 400 before side effects;
legacy feature-route 404/405 is surfaced as
`SubPathInitializationUnsupportedError` and is never retried through standard
create.

Rollout proceeds in this order:

1. Implement and test the server/provider capability and the secure initializer
   behind a disabled-by-default feature gate.
2. Deploy the additive feature-scoped route only to replicas that enforce its
   atomic effective-tuple check. Verify that legacy replicas return not-found
   or method-not-allowed for that route without side effects.
3. Configure routing so the additive route cannot reach an old replica. If that
   cannot be guaranteed while replicas are mixed, send no opt-in traffic.
4. Enable client/orchestrator use only for explicit opt-in requests and retain
   metrics for the four stable volume categories and their normative HTTP
   statuses. SDKs must not automatically replay create requests.

There is no data migration and no PVC migration. Existing directories are not
rewritten. Operators must decide when the provider's restricted initializer and
feature-scoped route are ready; implementations must enforce the normative
code/status mapping and no-automatic-replay rule before clients use the opt-in.

### Pre-publication note

OSEP-0021 intentionally avoids the unmerged PR #1573/OSEP-0020 number
collision. It is an independent proposal and must not claim to supersede or
modify that PR.
