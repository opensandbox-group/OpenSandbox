---
title: Secure Initialization of Missing PVC Volume SubPaths
authors:
  - "@cwj2001"
creation-date: 2026-08-19
last-updated: 2026-08-19
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
  - [Capability-aware creation and rollout](#capability-aware-creation-and-rollout)
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
field is `Volume.createSubPathIfMissing`, defaulting to `false` beside `subPath`.
The compatibility protocol also defines a distinct public capability-aware
create route and required header. The initial implementation is limited to
Linux Kubernetes non-Pool PVC mounts and uses a server-owned, trusted,
restricted init mechanism; the requested final mount remains read-only or
read-write as requested.

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
- Use secure file-descriptor-relative, no-follow traversal and a dedicated
  trusted initializer rather than exposing arbitrary execution controls.
- Keep the existing `code`/`message` error envelope, with a minimal stable
  volume error vocabulary and normative HTTP retry classification through the
  existing transport semantics.
- Make capability-aware rollout safe with old and mixed-version server
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
   `Volume.createSubPathIfMissing: boolean`, beside `subPath`, with default
   `false`. The OSEP also defines a distinct public capability-aware create
   route and required capability header as its compatibility protocol.
2. The opt-in is valid initially only for a Linux Kubernetes PVC mount on the
   non-Pool path. Unsupported combinations are rejected before side effects.
3. `subPath` is canonical relative path data. Absolute paths, empty segments,
   traversal, and other unsafe forms are rejected according to the validation
   rules below.
4. The initializer must never follow symlinks or escape the PVC root, including
   during concurrent replacement or traversal.
5. Existing directories are left untouched. The operation creates only missing
   path components needed for the requested subpath.
6. Identical `(PVC claim, canonical subpath)` requests in one create operation
   may be coalesced internally. Existing mount semantics, including repeated or
   overlapping `subPath` mounts, remain unchanged unless already invalid.
7. A read-only final volume mount remains read-only. The dedicated init
   container and its server-owned read-write PVC-root mount remain in the Pod
   spec, complete before workload containers start, and are unavailable to
   workload containers.
8. The PVC must already exist and remain subject to the existing PVC lifecycle
   and namespace rules. This feature is not `createIfNotExists`; that behavior
   must remain `false`, and a request attempting to enable it is rejected.
9. Initialization outcomes use the normative stable code/status mapping in
   [Errors and diagnostics](#errors-and-diagnostics), retain the existing
   `code`/`message` envelope, and do not expose host paths, secrets, pod
   details, or raw command output. Clients classify retryability from the
   status/category, and SDKs do not automatically replay create requests.
10. Capability negotiation must work when clients encounter old or mixed
    server replicas, without requiring every replica to understand the new
    field.

## Proposal

Add the following field to each existing `Volume` object:

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

The field is a property of this mount, not of the PVC and not of the sandbox
in general. A request with the field omitted is identical to an existing
request with the same `subPath` and retains the provider's ordinary behavior.
For the initial Kubernetes adapter, ordinary behavior for a missing subpath is
to fail during mount preparation; an adapter may report its documented
runtime-dependent behavior, but it must not infer opt-in initialization from
omission.

### Support boundary

The first supported tuple is:

| Dimension | Initial boundary |
| --- | --- |
| Runtime | Kubernetes on Linux |
| Storage | Existing PVC in the sandbox namespace |
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
- The initializer does not change the permissions or ownership of an existing
  directory. It creates missing components with server-defined safe defaults;
  callers cannot choose those defaults in this API.
- The dedicated init container's read-write PVC-root mount is not a permission
  grant to workload containers. It remains in the Pod spec for the init
  container, completes before workload startup, and is not mounted into any
  workload container; workload containers receive only the requested final
  access mode.
- `createIfNotExists` is not an alternative spelling or compatibility alias.
  It must be false for this feature; true is invalid and must not be translated
  into `createSubPathIfMissing`.
- No public field may carry an image, command, PodSpec fragment, hook, UID, GID,
  mode, or equivalent execution or permission control.

### Risks and Mitigations

- **Path escape or symlink attack:** use canonical relative validation followed
  by descriptor-relative, no-follow traversal rooted at the mounted PVC.
- **Unexpected write exposure:** keep the init container and its read-write
  mount server-owned, scope that mount only to the init container, and use a
  restricted non-privileged context. Kubernetes must complete the init
  container before starting workload containers.
- **Partial creation:** report the operation as failed if any component cannot
  be prepared. Do not claim rollback; directories already created remain in
  the PVC and are safe to reuse on a retry.
- **Mixed-version rollout:** send opt-in requests only through the distinct
  capability-aware create route, whose routing and server enforcement are
  atomic. Do not rely on semver or a preflight result.
- **Mount compatibility:** coalesce identical preparation requests internally,
  but preserve existing repeated and overlapping `subPath` mount semantics.
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

In addition to this configuration field, the compatibility protocol defines the
distinct public capability-aware create route and required
`OpenSandbox-Required-Capability` header described in
[Capability-aware creation and rollout](#capability-aware-creation-and-rollout).
Those are transport-protocol additions, not volume/mount configuration fields.

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
8. **Trusted boundary:** only the server's dedicated initializer can perform
   this preparation. A user image, user command, generic hook, or arbitrary
   PodSpec cannot opt into it.

### Validation and execution order

The provider must use an order that makes unsupported requests fail before
side effects:

1. Parse the request and reject malformed volume entries, invalid backend
   combinations, `createIfNotExists: true`, or an opt-in without `subPath`.
2. Determine the runtime, operating system, creation path, and provider
   capability. If the complete requested tuple is unsupported, return an
   unsupported-capability error before creating or modifying any resource.
3. Validate the existing PVC reference and namespace using the ordinary PVC
   lifecycle rules. Do not create a PVC.
4. Parse and canonicalize every requested `subPath`. Reject absolute paths,
   empty or ambiguous components, `.`/`..` components, NUL bytes, separator
   tricks, and non-canonical representations. The canonical value is relative
   to the PVC root and contains no leading separator.
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
9. From the PVC root, the init container opens or creates each missing
   component using descriptor-relative operations with no-follow semantics.
   Re-check component types and containment while traversing; never resolve a
   path by string concatenation or follow a symlink.
10. Wait for the initializer result. On failure, return the stable category and
    retryability, retain only sanitized diagnostics, and do not roll back
    directories already created. On success, Kubernetes starts workload
    containers with the exact requested final access mode; the read-write
    PVC-root mount is unavailable to those workload containers.
11. If final mount or sandbox creation fails after initialization, report that
    failure without deleting initialized directories. The operation remains
    safe to retry subject to normal idempotency and provider rules.

If any pre-side-effect step fails, no initializer, sandbox pod, or directory
may be created by this request.

### Errors and diagnostics

The capability-aware route uses the existing error envelope with only `code`
and `message`. This OSEP defines the following minimal stable volume categories
and normative HTTP statuses:

| Code | HTTP status | Meaning and retry classification |
| --- | --- | --- |
| `VOLUME_SUBPATH_INVALID` | 400 Bad Request | The opt-in or path is malformed, non-canonical, unsafe, or otherwise invalid. Do not retry without changing the request. |
| `VOLUME_SUBPATH_UNSUPPORTED` | 412 Precondition Failed | The requested runtime, OS, backend, Pool path, capability, or provider cannot honor the opt-in. Do not retry unless capability/configuration changes. |
| `VOLUME_SUBPATH_INITIALIZATION_FAILED` | 422 Unprocessable Content | Deterministic or terminal initialization failure, including a non-directory or symlink component, permission denial, or read-only filesystem. Do not automatically retry. |
| `VOLUME_SUBPATH_INITIALIZATION_UNAVAILABLE` | 503 Service Unavailable | Transient or indeterminate storage/runtime initializer failure. Retry may be appropriate; include `Retry-After` when the retry delay is known. |

Existing PVC validation, final mount, and sandbox-creation errors remain in
their existing categories. The response must not add diagnostic IDs, retry
fields, path fields, or other `ErrorResponse` properties, and must not expose
host paths, kubeconfig details, service-account tokens, secrets, raw PodSpec,
or unfiltered initializer output. Detailed causes remain in server logs and
standard diagnostics with sanitized `message` text.

Clients classify retryability using the HTTP status and stable code. SDKs and
client helpers must not automatically replay a create request, including for
503; an explicit caller may retry according to the category and any
`Retry-After` value.

### Security model

The initializer is a dedicated trusted server component, not a user-provided
container or hook. It is non-privileged and runs under a restricted context
with only the minimum PVC access and filesystem capabilities needed to create
directories. It has no network requirement, no access to unrelated host paths,
and no caller-controlled executable or arguments. The implementation must
follow the deployment's pod security policy and service-account boundaries.

Path handling has two independent stages: lexical canonicalization before any
mount action, and secure filesystem traversal after the PVC is mounted. The
second stage is authoritative. Every component is opened relative to an
already-opened directory descriptor, with no-follow behavior and type checks;
the implementation must not convert the untrusted relative path into an
unanchored absolute path. Symlink components, unexpected non-directory
components, mount-point escapes, and races that invalidate containment are
rejected.

The initializer creates missing directories only. It does not recursively
modify existing content and does not expose a mode, UID, or GID selector. The
server owns safe creation policy. If a storage provider cannot enforce these
properties, it is unsupported rather than being given a weaker fallback.

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
return `VOLUME_SUBPATH_UNSUPPORTED` for `true`. It must not silently treat
`true` as `false`.

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

### Capability-aware creation and rollout

Opt-in creation is normative only through the following additive route proposed
by this OSEP; this path is new and is not an assertion about an existing
OpenSandbox endpoint:

```http
POST /v1/sandboxes/capability-aware-create
OpenSandbox-Required-Capability: volume.subpath-initialization.v1
```

The request body and successful response otherwise match the standard lifecycle
create request and response. The `OpenSandbox-Required-Capability` header is
required on this route and has the shown value for a request containing
`createSubPathIfMissing: true`. The standard create route remains the route for
ordinary requests; clients must not send this opt-in through that route.

Route dispatch and capability validation are server-enforced parts of one
create operation:

- A legacy server that does not implement the additive route returns not-found
  or method-not-allowed before PVC, initializer, Pod, or workload mutation. It
  must not remap this route to ordinary create.
- A capable server requires the header, validates the body, and resolves the
  effective provider capability for the complete supported tuple atomically
  before mutating a PVC mount, creating an initializer, or creating a
  workload. An unavailable capability/provider is rejected with
  `VOLUME_SUBPATH_UNSUPPORTED` before those side effects.
- A capable server receiving the opt-in on the standard route rejects it with
  `VOLUME_SUBPATH_UNSUPPORTED` before side effects. This does not make it safe
  for clients to use the standard route against old servers; clients must use
  the additive route.
- A capability discovery endpoint, if provided, is advisory only. It cannot
  authorize an opt-in create, and semver checks or a separate preflight cannot
  replace route dispatch plus server-side enforcement.
- If a deployment cannot route the additive route atomically to capable API
  replicas while old and new replicas are mixed, it must send no opt-in
  traffic. Operators may keep the feature disabled until routing is safe.

The route, required header, and error behavior are the minimal negotiation
contract; this OSEP does not define the rest of the lifecycle endpoint
implementation. A false or omitted option continues to use ordinary create
behavior.

## Test Plan

No implementation is included in this OSEP branch. An implementation claiming
conformance must add focused tests at each boundary:

### Contract and validation tests

- Omitted and `false` options preserve ordinary missing-subpath behavior.
- `true` without `subPath`, `createIfNotExists: true`, absolute paths, empty
  components, dot components, traversal, NUL bytes, and non-canonical paths
  are rejected before any side effect.
- Unsupported runtime, OS, backend, Pool, and provider capability combinations
  return `VOLUME_SUBPATH_UNSUPPORTED` before pod, initializer, mount, or
  filesystem changes.
- Invalid input returns `VOLUME_SUBPATH_INVALID` with HTTP 400; unsupported
  capability returns `VOLUME_SUBPATH_UNSUPPORTED` with HTTP 412.
- Existing directories remain byte-for-byte and metadata-for-metadata
  unchanged by the preparation operation.
- Identical claim/path preparations may be coalesced; repeated and overlapping
  `subPath` mounts retain existing volume semantics.

### Secure filesystem tests

- Missing one or more nested components are created beneath the opened PVC
  root.
- Symlink components, symlink replacement races, non-directory components,
  and attempted root escapes are rejected using descriptor-relative no-follow
  operations.
- Non-directory, symlink, permission, and read-only filesystem failures return
  `VOLUME_SUBPATH_INITIALIZATION_FAILED` with HTTP 422. Transient or
  indeterminate I/O/runtime failures return
  `VOLUME_SUBPATH_INITIALIZATION_UNAVAILABLE` with HTTP 503 and `Retry-After`
  when known.
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
- Pool requests, unsupported adapters, and old/mixed server capability results
  fail before sandbox-side effects.
- Final mount failures preserve the requested mode and do not trigger
  directory rollback.
- Clients classify retryability from the status/code mapping, and SDKs do not
  automatically replay create requests.

### Compatibility tests

- An opt-in request reaches its configured workspace only when capability
  negotiation succeeds.
- Existing requests without the field continue to work without changes to
  image, command, environment, workspace, or artifact handling.
- An old server, an unknown capability, and a mixed replica route each produce
  an explicit unsupported result rather than silently changing the mount.

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
- performing descriptor-relative, no-follow filesystem traversal; and
- reporting versioned capability and sanitized error information through the
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

The only new volume/mount configuration field is the mount-scoped boolean;
requests that set it to `true` must use the distinct capability-aware route and
required header. The route and header are compatibility protocol additions, not
additional volume configuration.

Rollout proceeds in this order:

1. Implement and test the server/provider capability and the secure initializer
   behind a disabled-by-default feature gate.
2. Deploy the additive capability-aware route only to replicas that enforce its
   required header and atomic provider check. Verify that legacy replicas return
   not-found or method-not-allowed for that route without side effects.
3. Configure routing so the additive route cannot reach an old replica. If that
   cannot be guaranteed while replicas are mixed, send no opt-in traffic.
4. Enable client/orchestrator use only for explicit opt-in requests and retain
   metrics for the four stable volume categories and their normative HTTP
   statuses. SDKs must not automatically replay create requests.

There is no data migration and no PVC migration. Existing directories are not
rewritten. Operators must decide when the provider's restricted initializer and
capability-aware route are ready; implementations must enforce the normative
code/status mapping and no-automatic-replay rule before clients use the opt-in.

### Pre-publication note

OSEP-0021 intentionally avoids the unmerged PR #1573/OSEP-0020 number
collision. It is an independent proposal and must not claim to supersede or
modify that PR.
