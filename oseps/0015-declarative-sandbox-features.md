---
title: Declarative Sandbox Features
authors:
  - "@GodBlf"
creation-date: 2026-07-14
last-updated: 2026-07-17
status: draft
---

# OSEP-0015: Declarative Sandbox Features

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Public API](#public-api)
  - [Feature Capability Negotiation](#feature-capability-negotiation)
  - [Administrator-Controlled Feature Registry](#administrator-controlled-feature-registry)
  - [Feature OCI Artifact Contract](#feature-oci-artifact-contract)
  - [Docker Provisioning](#docker-provisioning)
  - [Kubernetes Provisioning](#kubernetes-provisioning)
  - [Compatibility Matrix](#compatibility-matrix)
  - [Security Boundary](#security-boundary)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Name, Version, and Platform Resolution](#name-version-and-platform-resolution)
  - [Activation and Environment Conflict Rules](#activation-and-environment-conflict-rules)
  - [Persistence and Read-After-Create Consistency](#persistence-and-read-after-create-consistency)
  - [Failure Semantics](#failure-semantics)
  - [Implementation Phases](#implementation-phases)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Add a first-class, declarative `features` field to sandbox creation so callers
can request administrator-approved, versioned tools such as Git or uv without
building a custom sandbox image or running a package manager during creation.
The server resolves each request through an administrator-controlled registry
to a digest-pinned, platform-specific OCI feature image, provisions its
self-contained payload before execd and the user entrypoint start, and returns
the immutable resolution as `resolvedFeatures`.

This proposal addresses the additive half of
[issue #1289](https://github.com/opensandbox-group/OpenSandbox/issues/1289).
It deliberately does not define `deny_tools` or a command blacklist. Hiding or
rejecting command names is not an enforceable security boundary; a separate
security/execution-profile OSEP is required for tool restriction.

## Motivation

Today a user who needs Git, uv, Node.js, Playwright, or another tool in a
sandbox must choose one of three imperative paths:

1. Build and distribute a custom sandbox base image.
2. Change `entrypoint` or environment variables to bootstrap tools;
3. Call execd after creation and run a base-image package manager or download
   script.

These paths make otherwise identical sandbox requests depend on mutable image
tags, Linux distribution package managers, internet availability, and repeated
downloads. They are also difficult for operators to audit because the server
does not resolve or record the installed toolchain.

The lifecycle API already states that `extensions` is for provider-specific,
experimental, or temporary behavior and that standard parameters should become
core API fields. Declarative features are a cross-provider public contract and
therefore must not be encoded in `extensions`.

### Goals

- Add an additive, structured `features` field to `CreateSandboxRequest`.
- Let SDKs accept convenient `"name"` and `"name@version"` inputs while keeping
  the online JSON representation structured and unambiguous.
- Resolve every requested feature to an exact version, OCI manifest digest,
  operating system, and architecture before a successful create returns.
- Restrict feature supply to an administrator-managed allowlist of
  digest-pinned OCI images.
- Install self-contained feature payloads before execd and the user entrypoint
  start, without depending on software in the sandbox base image.
- Preserve caller order for installation and activation.
- Reuse the existing Docker pre-start execd injection pattern and the existing
  Kubernetes execd init-container plus shared `emptyDir` pattern.
- Make create and get responses reproducible by persisting the resolved feature
  set in provider metadata.
- Fail clearly for unknown, duplicate, incompatible, conflicting, or unsupported
  requests. No requested feature may be silently ignored.
- Keep requests without `features` behaviorally compatible with current
  sandbox creation.

### Non-Goals

- Client-supplied OCI image or artifact references.
- An online marketplace, public feature catalog service, or independent feature
  registry API.
- Running `apt`, `apk`, `yum`, or another base-image package manager while a
  sandbox is created.
- Downloading a feature's transitive dependencies from the internet during
  sandbox creation.
- Transitive feature dependency declaration or dependency solving.
- Adding, removing, or upgrading a feature after sandbox creation.
- Pool-backed creation through `extensions.poolRef`.
- Snapshot restore or including feature state in a snapshot contract.
- Windows feature images or activation.
- Tool denial, command-name filtering, `PATH` hiding, or a `deny_tools` API.
- Treating a feature as a sandbox isolation or privilege boundary.

## Requirements

- `features` MUST be a top-level core field, not an `extensions` convention.
- A wire-level feature request MUST contain a canonical lowercase `name` and MAY
  contain an exact `version`.
- Omitting `version` MUST select the administrator registry's configured default
  for that feature. It MUST NOT mean `latest` or a version range.
- A successful create response for a non-empty feature request, and subsequent
  reads of that Sandbox resource, MUST include the ordered `resolvedFeatures`
  with name, exact resolved version, platform-specific OCI digest, OS, and
  architecture.
- Callers MUST NOT submit an OCI repository, tag, digest, URL, installer script,
  or registry credentials in the public feature request.
- SDKs and the CLI MUST confirm lifecycle feature capability before sending a
  non-empty `features` request. An older or incompatible server MUST be detected
  before sandbox creation is attempted.
- The lifecycle feature capability MUST be a fleet-wide readiness assertion for
  every backend behind the advertised base URL. It MUST remain disabled during
  a mixed-version rollout or rollback.
- Every registry entry MUST be digest-pinned and MUST declare at least one
  supported platform.
- Registry configuration MUST be validated when the server starts. A configured
  but invalid registry MUST prevent startup.
- A feature payload MUST be self-contained and relocatable under
  `/opt/opensandbox/features/<name>/<version>`.
- Feature provisioning MUST NOT require package managers, interpreters, shells,
  or libraries from the sandbox base image.
- Feature provisioning MUST NOT download feature dependencies from the internet.
  Pulling the administrator-approved, digest-pinned OCI image is the only
  required supply-chain fetch.
- Feature activation MUST complete before execd and the user entrypoint start.
- Features MUST install and activate in request order.
- A feature-supplied Kubernetes installer MUST write only to its own staging
  volume. It MUST NOT receive a writable mount containing execd, bootstrap,
  another feature's staging output, or final activation inputs.
- Unknown names, unknown versions, duplicate names, incompatible platforms, and
  activation environment conflicts MUST fail creation explicitly.
- A provisioning failure MUST clean up a partially created Docker container or
  Kubernetes workload using the provider's existing rollback behavior.
- Version 1 MUST support Linux image-based standard creation through Docker and
  all current Kubernetes workload providers.
- Version 1 MUST reject pool mode, snapshot restore, and Windows when `features`
  is non-empty.

## Proposal

The lifecycle server gains a feature resolver between request validation and
provider provisioning. The resolver accepts only names and exact optional
versions, looks them up in a registry loaded at server startup, computes the
platform candidates, and produces an immutable ordered resolution plan.

```text
CreateSandboxRequest.features
             |
             v
  validate names/order/duplicates
             |
             v
 administrator feature registry
  (name, version, platform -> digest)
             |
             v
 ordered resolution/provisioning plan
        /                    \
       v                      v
 Docker pre-start copy   Kubernetes init containers
       \                      /
        v                    v
  activation before execd and entrypoint
             |
             v
 persisted resolvedFeatures -> create/get/diagnostics
```

### Public API

The eventual lifecycle OpenAPI change is additive. `CreateSandboxRequest` gains
the following field:

```yaml
features:
  type: array
  maxItems: 32
  items:
    $ref: '#/components/schemas/FeatureRequest'
  description: >-
    Ordered administrator-approved features to provision before sandbox start.
```

The request schema is intentionally small:

```yaml
FeatureRequest:
  type: object
  additionalProperties: false
  required: [name]
  properties:
    name:
      type: string
      description: Canonical lowercase feature name.
    version:
      type: string
      description: Optional exact version. Omission selects the registry default.
```

For example:

```yaml
image:
  uri: python:3.12
entrypoint: ["python", "/app/main.py"]
resourceLimits:
  cpu: "500m"
  memory: "512Mi"
features:
  - name: git
    version: "2.47.1"
  - name: uv
```

The online JSON is always an array of objects. String shorthand is not accepted
by the server. Sandbox SDKs MAY expose `"git"` and `"git@2.47.1"` conveniences,
but adapters MUST convert those values to `FeatureRequest` objects before JSON
serialization. A literal `@` in a feature name is not supported, so SDK parsing
uses the final `@` as the version separator and rejects an empty component.

The CLI implementation in a later phase provides a repeatable option:

```text
osb sandbox create --feature git --feature git-lfs@3.7.0 ...
```

`CreateSandboxResponse` and `Sandbox` gain `resolvedFeatures`:

```yaml
ResolvedFeature:
  type: object
  additionalProperties: false
  required: [name, version, digest, platform]
  properties:
    name:
      type: string
    version:
      type: string
    digest:
      type: string
      description: Platform-specific OCI manifest digest, including sha256:.
    platform:
      $ref: '#/components/schemas/PlatformSpec'
```

```json
{
  "resolvedFeatures": [
    {
      "name": "git",
      "version": "2.47.1",
      "digest": "sha256:0123456789abcdef...",
      "platform": {"os": "linux", "arch": "amd64"}
    },
    {
      "name": "uv",
      "version": "0.8.3",
      "digest": "sha256:fedcba9876543210...",
      "platform": {"os": "linux", "arch": "amd64"}
    }
  ]
}
```

The response array preserves request order. It is required on a successful
create with non-empty `features`. It may be omitted for requests without
features and for resources created by older servers. A pending Kubernetes
resource may omit it until node platform resolution completes, but the server
MUST persist it before returning a successful create response.

Feature names match `^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$` and are at most 63
characters. The server rejects non-canonical case rather than silently folding
it. Versions are exact, case-sensitive registry keys, at most 128 characters,
and match `^[0-9A-Za-z](?:[0-9A-Za-z._+-]*[0-9A-Za-z])?$`. Ranges, wildcards,
`latest`, path separators, and blank versions are invalid. A request may contain
at most 32 features. Repeating a normalized feature name is invalid even when
the two entries request different versions.

### Feature Capability Negotiation

Current pre-feature servers ignore unknown `CreateSandboxRequest` fields because
their Pydantic model does not forbid extras. A new SDK therefore cannot safely
assume that an older server will reject `features`: doing so could start the user
entrypoint without the requested tools.

Phase 1 adds an additive lifecycle `GET /v1/capabilities` endpoint. A server that
implements this OSEP returns at least:

```json
{
  "declarativeSandboxFeatures": {
    "apiVersion": 1
  }
}
```

Additional capability fields may be added later. Clients ignore unknown fields,
but MUST understand the advertised declarative-feature API version before using
it. SDKs MUST call this endpoint immediately before every create with a
non-empty `features` list. They MUST NOT reuse a cached positive result for a
later create.

A `404`, missing `declarativeSandboxFeatures`, unsupported `apiVersion`, invalid
response, or transport failure means feature creation is unavailable. The SDK
MUST raise an unsupported-server/capability error without sending the create
request. It MUST NOT probe by sending a feature request, create and then delete
an unverified sandbox, or downgrade the request into `extensions`. The CLI uses
the same SDK guard. Callers using raw HTTP are responsible for performing the
same handshake.

The response is a deployment-level readiness assertion, not evidence about only
the backend that served the GET. An operator MUST NOT advertise
`declarativeSandboxFeatures` at a load-balanced base URL until every backend
that can receive the subsequent create request implements API version 1. During
Phase 1 rollout, feature capability remains disabled while old instances are
drained; it is enabled only after the fleet reaches that compatibility floor.
This fleet gate closes the GET/create race in which the two requests reach
different versions.

Rollback reverses that sequence: revoke the fleet capability first, wait for
in-flight capability checks and feature creates to drain, and only then admit an
older backend. The server deployment and ingress/gateway configuration MUST make
this gate atomic from the client's base-URL perspective. Per-instance capability
advertising in a mixed fleet is non-conformant, even if the SDK performs a fresh
GET before each create.

Capability support and registry readiness are distinct. A feature-aware server
advertises API version 1 even when no registry file is configured; a subsequent
feature create then returns `FEATURE_REGISTRY_NOT_CONFIGURED`. This distinction
lets clients tell an old server from an operator configuration error.

### Administrator-Controlled Feature Registry

An operator enables features by configuring a server-side registry file path.
The first implementation uses a static TOML file loaded once at process startup;
changing it requires a server restart. This is configuration, not a network
service and not an end-user API.

An illustrative registry is:

```toml
schema_version = 1

[[features]]
name = "git"
default_version = "2.47.1"

[[features.versions]]
version = "2.47.1"
image = "registry.example.com/opensandbox/features/git@sha256:<index-digest>"

[[features.versions.platforms]]
os = "linux"
arch = "amd64"
digest = "sha256:<amd64-manifest-digest>"

[[features.versions.platforms]]
os = "linux"
arch = "arm64"
digest = "sha256:<arm64-manifest-digest>"

[[features]]
name = "uv"
default_version = "0.8.3"

[[features.versions]]
version = "0.8.3"
image = "registry.example.com/opensandbox/features/uv@sha256:<index-digest>"

[[features.versions.platforms]]
os = "linux"
arch = "amd64"
digest = "sha256:<amd64-manifest-digest>"
```

`image` is always a complete OCI reference with an `@sha256:` digest. It may
identify a multi-platform OCI index or a single-platform manifest. Every
declared platform also pins the child manifest digest returned in
`resolvedFeatures`. For a single-platform image, the image and child digest may
be the same.

Server startup validation MUST reject:

- an unknown registry schema version;
- an invalid or non-canonical feature name;
- duplicate feature names;
- a feature with no versions;
- an invalid or duplicate version within one feature;
- a missing default version or a default that does not name exactly one entry;
- an image reference that uses a tag, omits a digest, or uses a digest algorithm
  not supported by the implementation;
- an empty platform list;
- duplicate or unnormalized OS/architecture pairs;
- a platform declaration without a pinned child digest;
- a non-Linux platform in the version 1 registry;
- an OCI index whose descriptors do not contain the declared child digest for
  the declared platform; and
- artifact metadata whose feature name, version, or platform disagrees with the
  registry entry.

OCI registry credentials, mirrors, trust roots, and pull policy are operator
configuration. They never appear in `FeatureRequest`. A configured registry
that cannot be validated is a startup error so the server does not accept
requests against a partially trusted catalog. If no feature registry path is
configured, ordinary requests continue to work and non-empty `features`
requests fail with a clear `FEATURE_REGISTRY_NOT_CONFIGURED` error.

The registry is the allowlist. Digest pinning makes resolution immutable and
cacheable, but does not by itself prove publisher identity or code quality.
Operators remain responsible for building, scanning, reviewing, and promoting
feature artifacts. Signature enforcement, SBOM policy, and transparency logs
may be added later without allowing caller-supplied references.

### Feature OCI Artifact Contract

A feature is distributed as a normal OCI image so existing Docker and
Kubernetes image distribution, authentication, mirrors, and node layer caches
can be reused. The image has the following required layout:

```text
/opt/opensandbox-feature/
  metadata.json
  activation.json
  payload/
  install
```

- `metadata.json` declares artifact schema version, feature name, version, OS,
  and architecture. These values must match the registry and selected manifest.
- `payload/` is the complete relocatable filesystem tree. It is copied to
  `/opt/opensandbox/features/<name>/<version>` without running a sandbox base
  image package manager.
- `activation.json` is declarative. It describes scalar environment assignments
  and ordered path-list prepend/append operations using `${FEATURE_ROOT}` as the
  only substitution. It cannot contain shell commands, dependency downloads,
  transitive feature requirements, or arbitrary host paths.
- `install` is a self-contained executable used by Kubernetes feature init
  containers. It must not rely on a shell, libc, package manager, interpreter,
  or binary from the sandbox image. It copies the payload, metadata, and
  activation document into the feature's dedicated staging volume. It never
  writes the shared runtime-assets volume or a final feature path. The Docker
  server-side extractor does not execute `install`; trusted server code performs
  validation and final placement directly.

An example activation document is:

```json
{
  "schemaVersion": 1,
  "set": [
    {
      "name": "GIT_EXEC_PATH",
      "value": "${FEATURE_ROOT}/libexec/git-core"
    }
  ],
  "prependPath": [
    {
      "name": "PATH",
      "values": ["${FEATURE_ROOT}/bin"]
    }
  ]
}
```

The feature-supplied installer is not trusted to validate restrictions on its
own output. A trusted OpenSandbox extractor or merger MUST reject archive path
traversal, absolute payload paths, writes outside the assigned feature root, and
special device nodes before moving staged content into runtime assets. Payload
modes and symlinks are preserved only when their resolved targets remain inside
the feature root. Final installation uses a temporary sibling directory followed
by an atomic rename so a failed feature never appears complete.

Version 1 has no feature-to-feature dependency metadata and no install hook
beyond deterministic payload placement. If feature B requires feature A, the
administrator must publish B with that dependency in B's self-contained
payload, or callers must request both and the artifacts must be independently
valid. The resolver never inserts, reorders, or upgrades features implicitly.

### Docker Provisioning

The Docker provider already creates a temporary container from the configured
execd image, extracts execd/bootstrap artifacts, caches them by platform, creates
the sandbox container without starting it, copies the artifacts through the
Docker archive API, and starts the container only after injection succeeds.
Feature provisioning extends that sequence:

1. Resolve the effective Linux OS/architecture from the explicit request
   platform or the Docker daemon and selected sandbox image.
2. Resolve every feature to the registry's platform-specific manifest digest.
3. Pull the digest-pinned feature image using operator credentials.
4. Extract and validate its metadata, payload, and activation document using a
   temporary container, following the existing execd extraction pattern.
5. Cache the validated payload by `(manifest digest, OS, architecture)`. Cache
   population is concurrency-safe and uses a temporary entry followed by an
   atomic ready marker.
6. Create the stopped sandbox container with a compact ordered
   `resolvedFeatures` Docker label.
7. Copy each payload into its final feature root in request order, compile the
   activation plan, and inject execd/bootstrap artifacts.
8. Start the sandbox container. The bootstrap applies the compiled activation
   environment before it launches execd and the user entrypoint.

The cache key never uses a mutable tag, requested name, or default-version
alias. A new digest is a new cache entry. Cached content is validated before it
is marked ready, and failed population removes the temporary entry.

Any pull, validation, copy, activation compilation, or start failure follows
the current `_create_and_start_container` rollback contract: remove the partial
sandbox container, clean provider-created sidecars/volumes as applicable, and
return an explicit create error. A feature failure must not leave a stopped
container that looks like a sandbox.

### Kubernetes Provisioning

Both existing Kubernetes workload providers already use an `execd-installer`
init container to copy execd and `bootstrap.sh` into a shared `emptyDir`, which
the main sandbox container mounts at `/opt/opensandbox`. Feature provisioning
extends the generated Pod spec without changing the public workload CRDs:

1. Keep the existing `execd-installer` init container. Only trusted OpenSandbox
   init containers and the main container mount the runtime-assets `emptyDir`.
2. Allocate one dedicated staging `emptyDir` per requested feature and add one
   feature init container in request order. Each uses the registry's
   digest-pinned OCI image, mounts only its own staging volume as writable, and
   invokes its self-contained installer. It cannot mount runtime assets or
   another feature's staging volume.
3. Add a trusted `feature-merger` init container from the pinned execd image.
   It mounts every feature staging volume read-only and runtime assets writable.
   In request order it validates metadata, paths, types, modes, symlinks, and the
   activation document, then atomically installs payloads under
   `features/<name>/<version>` and activation inputs under
   `features/.resolved/activation-inputs/<request-index>-<name>.json`.
4. Add a final activation-compiler init container from the pinned execd image.
   It reads the persisted activation inputs, validates them against the request
   environment, verifies that their indexes exactly match the resolution plan,
   and writes one ordered, shell-escaped activation file into the shared volume.
5. Start the main container only after every init container succeeds. Its
   bootstrap sources the compiled activation file before execd and the user
   entrypoint.

Kubernetes init containers are sequential, so request order is preserved. The
Pod retains `automountServiceAccountToken: false`; feature init containers do
not receive API credentials and do not need network access after kubelet pulls
their OCI image. Implementations SHOULD apply the existing restricted
init-container security defaults and grant no additional Linux capabilities.
The final merged Pod spec MUST prove that feature-supplied containers have no
mount path or volume reference that aliases runtime assets or another staging
volume.

When `platform` is explicit, all selected features must contain that platform.
When it is omitted, the resolver computes the intersection of platforms across
the selected feature versions and constrains Pod scheduling to that
intersection. The init-container `image` uses the pinned OCI index digest, so
kubelet selects the matching child manifest after node assignment. The server
then reads the assigned node OS/architecture, verifies the expected child
digest, and patches the ordered resolved metadata onto the workload before a
successful create returns. If the intersection is empty or the assigned
platform cannot be verified, creation fails.

This approach relies on the node's normal OCI layer cache; version 1 does not
introduce a cluster-wide shared payload cache, daemonset, or persistent volume.
Every sandbox receives a fresh `emptyDir`, while common feature layers are
downloaded once per node according to normal kubelet/runtime cache policy.

The provider must validate the final merged Pod template. Operator templates
may add unrelated init containers, but they may not remove or reorder generated
feature init containers, replace their digest-pinned images, or shadow the
isolated staging or runtime-assets mounts. They also may not replace the trusted
merger/compiler images or make a staging mount writable in those containers.
Init failure, scheduling failure, metadata-patch failure, or readiness timeout
uses the existing workload deletion and PVC cleanup path.

### Compatibility Matrix

| Creation mode | Docker | Kubernetes | Version 1 behavior |
|---|---:|---:|---|
| Linux image-based standard creation | Yes | Yes | Supported |
| `extensions.poolRef` / pool mode | No | No | Reject non-empty `features` |
| Snapshot restore | No | No | Reject non-empty `features` |
| Windows | No | No | Reject non-empty `features` |
| Add/remove/upgrade after create | No | No | No API; immutable for sandbox lifetime |
| Caller-supplied OCI artifact | No | No | Rejected by schema and resolver |

"Kubernetes" includes the current BatchSandbox template-mode provider and the
current kubernetes-sigs agent-sandbox provider. It does not include the
BatchSandbox pool path. Unsupported combinations fail before provider side
effects whenever possible. The server never drops `features` and proceeds with
an unmodified sandbox.

Requests that omit `features` retain current behavior on Linux, Windows, image,
snapshot, and pool flows. An empty array is equivalent to omission.

### Security Boundary

Feature OCI artifacts are approved supply code, but provisioning does not assume
their installer is correct. Their executables eventually run inside the sandbox
and their activation can affect the environment inherited by execd and the user
process. Version 1 controls this risk with an administrator allowlist,
digest-pinned indexes and manifests, platform/metadata verification, isolated
installer staging, trusted final merging, constrained installation roots, and
explicit operator registry credentials.

This mechanism does not make a sandbox more isolated and must not be presented
as a security profile. A malicious or compromised allowlisted feature can still
affect all processes in its sandbox. Operators should apply their normal image
build provenance, vulnerability scanning, SBOM, review, and promotion controls
before adding a digest to the registry.

This OSEP intentionally does not add `deny_tools`. An execd command-string or
tool-name blacklist and removing directories from `PATH` are usability controls,
not security controls. They are bypassable by:

- invoking the same binary through an absolute path;
- renaming or copying a binary before invoking it;
- invoking behavior through an interpreter, dynamic linker, library API, or a
  caller-supplied static binary; and
- bypassing execd entirely by starting a process through the user entrypoint, a
  child process, another in-container service, or another runtime execution
  path.

Removing a known binary has the same problem when the filesystem is writable or
the caller can upload code. A command policy enforced only at one execd endpoint
cannot constrain all process creation in the container.

Tool restriction belongs in a later security/execution-profile OSEP based on
mechanisms the runtime can actually enforce, such as `networkPolicy` and egress
policy, container capabilities, seccomp, AppArmor, a read-only root filesystem,
and the isolated execution model in
[OSEP-0013](0013-isolated-execution-api.md). Phase 4 below is reserved for that
separate proposal and implementation.

### Notes/Constraints/Caveats

- Defaults are operator policy. Omitting `version` is reproducible only after
  the server records the exact resolved version and digest. Repeating the same
  request after an administrator changes a default may produce a newer feature;
  pin a version when cross-time reproducibility is required.
- Feature order is observable through path precedence. Reordering a request may
  change which executable is selected when two features provide the same binary
  name, even when their scalar environment assignments do not conflict.
- Feature payloads consume sandbox writable storage and increase cold-start
  latency. Version 1 does not add separate feature quotas.
- The feature root is immutable by lifecycle API contract, but it is not
  necessarily read-only inside every sandbox. Runtime hardening is separate.
- Registry reload, garbage collection, signature policy, and catalog discovery
  are deliberately deferred. Static startup loading keeps the first trust and
  consistency model reviewable.
- Diagnostics SHOULD report the persisted resolution and the feature/install
  stage that failed, but diagnostics must never expose registry credentials.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Compromised feature artifact affects execd or the user process | Administrator allowlist, index and child digest pinning, metadata verification, and trusted artifact promotion |
| Mutable defaults reduce repeatability | Return and persist exact version, digest, OS, and architecture; allow callers to pin exact versions |
| Multi-platform index differs from declared catalog | Validate OCI descriptors and selected child digest against the registry |
| Archive traversal or symlink escape writes outside feature root | Strict path validation, reject special devices, install into a temporary root, then atomically rename |
| Concurrent Docker pulls corrupt cache | Digest/platform cache key, per-key locking, validation, and atomic ready marker |
| Feature environments conflict | Declarative activation format and deterministic pre-start conflict validation |
| Mixed server versions race capability GET against create | Fleet-wide capability gate; disable during rollout/rollback; no positive SDK caching |
| Feature installer overwrites execd, bootstrap, or another feature | Per-feature staging volumes only; trusted merger owns the runtime-assets mount and final paths |
| Kubernetes template overrides generated security/provisioning fields | Validate the final merged Pod spec before API submission |
| Feature init fails after workload creation | Surface init status, delete the workload, and use existing managed-PVC cleanup safeguards |
| Large feature sets increase startup latency and metadata size | Maximum 32 requests, compact persisted JSON, layer caching, and documented operational metrics |
| Users mistake tool denial for a security boundary | Exclude `deny_tools`, document bypasses, and require a separate enforceable-policy OSEP |

## Design Details

### Name, Version, and Platform Resolution

Resolution is deterministic and ordered:

1. Treat omitted or empty `features` as no feature request.
2. Validate the list length and each structured request.
3. Reject a repeated canonical name, retaining the original request indexes in
   the error message.
4. Look up each name in the startup-validated registry.
5. Use the requested exact version, or the feature's single configured default.
6. Compute the effective platform or candidate-platform intersection.
7. Select and verify the platform-specific manifest digest for every feature.
8. Produce the immutable ordered resolution plan used by provisioning and
   provider metadata.

There is no semantic-version comparison, range selection, dependency solver,
or implicit fallback to another version. An unknown explicit version fails even
if the feature has a default. An explicit platform fails if any feature lacks
that exact normalized OS/architecture. In version 1, every effective platform
must have `os=linux`.

For Docker, effective platform resolution occurs before sandbox container
creation and uses existing platform normalization and image inspection. For
Kubernetes without an explicit platform, the registry intersection is applied
as scheduling constraints and exact selection completes after node assignment.
The create operation remains pending until this selection, init completion,
workload readiness, and resolved-metadata persistence all succeed.

### Activation and Environment Conflict Rules

Provisioning copies activation documents without executing them. A trusted
activation compiler validates and merges them in request order into a generated
file under `/opt/opensandbox/features/.resolved/`. The uncompiled documents live
under `.resolved/activation-inputs/` and use a zero-padded request index in each
filename, so a missing, duplicated, or reordered input is detectable. Bootstrap
sources only the compiled output before the existing execd launch and before
dispatching the requested entrypoint.

The following rules make activation deterministic:

- `${FEATURE_ROOT}` expands only to the selected feature's final absolute root.
  No other shell or environment interpolation is performed.
- Variable names must match `^[A-Za-z_][A-Za-z0-9_]*$`.
- `set` assigns a scalar. Two features assigning the same scalar variable is a
  conflict, even when values happen to match.
- A feature scalar assignment that collides with a caller-provided `env` key is
  a conflict. There is no hidden precedence rule.
- OpenSandbox-reserved bootstrap variables, including `EXECD`, `EXECD_ENVS`,
  and feature-plan paths, cannot be assigned by a feature.
- `prependPath` and `appendPath` are the only composable operations. Entries are
  applied in request order, empty elements are rejected, and duplicate expanded
  path elements are rejected as an activation conflict.
- A variable cannot be used by both scalar `set` and a path-list operation.
- Activation cannot unset a variable, execute a command, read a secret, or
  reference a path outside its own feature root in version 1.

Docker performs this compilation before container start. Kubernetes performs it
in the final activation-compiler init container. A compiler failure is a create
failure, not a warning. The same conformance fixtures must be used by both
providers so conflict and ordering behavior cannot drift.

### Persistence and Read-After-Create Consistency

The resolution is stored as compact ordered JSON in provider-owned metadata:

- Docker uses the immutable `opensandbox.io/resolved-features` container label.
- Kubernetes uses the
  `sandbox.opensandbox.io/resolved-features` workload annotation.

The persisted object contains only the public resolved fields: name, version,
platform child digest, OS, and architecture. It does not contain registry
credentials, mutable tags, local cache paths, or activation environment values.

Docker resolves before container creation, so the label is present from the
first observable container state. Kubernetes may initially create a workload
with an ordered unresolved plan so scheduling can select a node; after node
assignment it patches the exact resolved annotation. The server does not return
create success until that patch is readable. If persistence fails, creation is
rolled back.

Create response, get response, list items, and diagnostics read the same
provider metadata rather than re-resolving against the current registry. This
ensures an administrator default change does not rewrite the reported state of
an existing sandbox. Malformed persisted metadata is a diagnostic/provider
error and must not be silently replaced with current registry data.

### Failure Semantics

Feature errors use the lifecycle API's existing structured error envelope. The
implementation introduces stable machine-readable reasons such as:

| Condition | HTTP class | Reason |
|---|---:|---|
| Registry not configured | 400 | `FEATURE_REGISTRY_NOT_CONFIGURED` |
| Invalid name/version or duplicate request | 400 | `INVALID_FEATURE_REQUEST` |
| Unknown name | 400 | `FEATURE_NOT_FOUND` |
| Unknown exact version | 400 | `FEATURE_VERSION_NOT_FOUND` |
| Unsupported pool/snapshot/Windows combination | 400 | `FEATURES_UNSUPPORTED_FOR_CREATION_MODE` |
| No compatible platform | 400 | `FEATURE_PLATFORM_UNSUPPORTED` |
| Activation collision | 400 | `FEATURE_ACTIVATION_CONFLICT` |
| Approved OCI registry unavailable | 503 | `FEATURE_IMAGE_UNAVAILABLE` |
| Digest/artifact validation or install failure | 500 | `FEATURE_PROVISIONING_FAILED` |

Validation errors identify the request index and feature name where safe.
Supply-chain errors identify name, version, and expected digest but do not
include credentials. Kubernetes errors also report the failed init-container
name and termination reason. Provider rollback failure is logged and surfaced
through current cleanup diagnostics rather than hiding the original feature
failure.

Unknown fields in `FeatureRequest`, raw strings in online JSON, caller OCI
references, and non-empty `features` on an unsupported creation mode are hard
errors. There is no best-effort mode.

### Implementation Phases

Implementation proceeds only after this OSEP is approved:

**Phase 1: contract, resolver, and providers**

- Add the lifecycle OpenAPI request/response schemas and examples.
- Add the lifecycle capability endpoint and advertise
  `declarativeSandboxFeatures.apiVersion = 1`.
- Add the fleet-wide feature capability gate and document rollout/drain
  integration for the supported deployment paths.
- Add matching server Pydantic schemas and provider metadata codecs.
- Add feature-registry configuration, startup validation, resolution, and OCI
  artifact validation.
- Implement Docker extraction/cache/pre-start injection and cleanup.
- Implement ordered Kubernetes feature init containers with isolated staging
  volumes, a trusted merger into runtime assets, activation compilation,
  scheduling constraints, persistence, and cleanup for every existing non-pool
  workload provider.

**Phase 2: generated clients and Sandbox SDK alignment**

- Regenerate lifecycle clients from the OpenAPI source of truth.
- Add aligned public feature request/convenience input and resolved response
  models to the Python, JavaScript, Kotlin, C#, and Go Sandbox SDKs.
- Require all SDKs to negotiate lifecycle feature capability before sending a
  non-empty feature request, with no create attempt on older servers.
- Add cross-language serialization tests proving string convenience inputs
  produce structured wire JSON and resolved metadata is preserved.

**Phase 3: CLI, documentation, catalog examples, and end-to-end coverage**

- Add repeatable CLI `--feature NAME[@VERSION]` support and help/output tests.
- Publish user and operator documentation, including registry and artifact
  authoring guidance.
- Add an example feature catalog for tests and documentation. This OSEP itself
  does not create that catalog or publish real feature images.
- Add Docker and Kubernetes cross-runtime end-to-end tests.

**Phase 4: separate enforceable security/execution profile proposal**

- Write and review a separate OSEP for security/execution profiles.
- Base restrictions on enforceable runtime primitives such as egress policy,
  capabilities, seccomp, AppArmor, read-only filesystems, and OSEP-0013 isolated
  execution, not command strings or `PATH` filtering.
- Implement any approved profile only after its own security review.

Merging this OSEP does not implement issue #1289. The issue should remain open
until the OSEP is approved and the end-to-end implementation, SDKs, CLI,
documentation, and cross-runtime tests are complete. Phase 4 may be tracked by
a separate issue because this OSEP rejects the proposed blacklist as a security
mechanism.

## Test Plan

Phase 1 must include focused tests at each ownership boundary.

**Contract and server schema tests**

- OpenAPI validation accepts ordered structured requests with and without exact
  versions and rejects raw strings, extra fields, invalid names/versions, more
  than 32 entries, and duplicates.
- Existing create payloads without `features` remain valid.
- The capability endpoint advertises API version 1 independently of registry
  configuration and remains forward-compatible with unknown capability fields.
- Mixed-version rollout tests prove capability remains absent until every
  backend behind the base URL is compatible; rollback tests revoke it and drain
  in-flight work before an older backend becomes eligible.
- Create, get, and list schemas serialize the same `resolvedFeatures` shape and
  preserve order.
- Pool, snapshot, and Windows combinations return the documented explicit
  error rather than dropping the field.

**Registry and resolver unit tests**

- Startup rejects every invalid-registry case listed above, including tag-only
  references, missing child digests, duplicate keys, invalid defaults, missing
  platforms, and OCI descriptor mismatches.
- Exact versions and defaults resolve correctly; missing names/versions never
  fall back.
- Platform normalization, explicit selection, Kubernetes intersection, empty
  intersection, and Docker effective-platform behavior are covered for amd64
  and arm64.
- Request order is stable through resolution and persisted JSON encoding.

**Artifact and activation conformance tests**

- A minimal feature installs into the exact target root in a base image without
  `apt`, `apk`, `yum`, shell, Python, or internet access.
- Payload permissions and safe relative symlinks are preserved.
- Absolute paths, `..` traversal, escaping symlinks, device nodes, metadata
  mismatch, and writes outside the assigned root fail atomically.
- Scalar conflicts, caller-env conflicts, reserved variables, mixed scalar/path
  operations, duplicate path segments, and malformed activation documents fail.
- Ordered path composition is identical in Docker and Kubernetes fixtures.
- Bootstrap observes feature activation before both execd and the user
  entrypoint start.
- Missing, duplicated, or reordered persisted activation inputs fail before the
  main container starts.

**Docker provider tests**

- Images are pulled only by pinned digest and use operator credentials.
- Cache keys include manifest digest and platform; concurrent cache population
  produces one validated ready entry; failed population leaves no usable entry.
- Feature payloads are injected after container creation and before start in
  request order, alongside existing execd/bootstrap injection.
- Failures at pull, extraction, validation, copy, compile, and start remove the
  partial sandbox and preserve existing sidecar/managed-volume cleanup.
- Create followed by get returns byte-equivalent resolved feature metadata from
  the Docker label even after registry defaults change.

**Kubernetes provider tests**

- Both BatchSandbox template mode and agent-sandbox manifests contain ordered
  feature init containers with per-feature staging volumes, a trusted merger,
  a final activation compiler, and a main container that cannot start first.
- Feature-supplied containers mount only their own staging volume; malicious or
  buggy writes cannot alter execd, bootstrap, another feature's output, runtime
  assets, or final activation inputs.
- The trusted merger mounts staging read-only, rejects escaping or malformed
  output, and persists each validated activation document in the indexed shared
  input path before the final compiler runs.
- Generated feature images retain digest references after template merge.
- Platform intersection becomes scheduling constraints; explicit mismatches and
  empty intersections fail before workload creation.
- Node-selected OS/architecture maps to the declared child digest and is
  persisted before create success.
- Init failure, scheduling failure, annotation patch failure, and readiness
  timeout delete the workload and follow existing managed-PVC cleanup rules.
- Create followed by get returns the persisted annotation rather than resolving
  the current registry.

**Phase 2 and Phase 3 tests**

- Python, JavaScript, Kotlin, C#, and Go SDK tests cover typed requests,
  `name@version` convenience parsing where offered, structured JSON output, and
  resolved response conversion.
- Against a pre-feature server that ignores unknown request fields, every SDK
  detects the missing capability and proves that no create request was sent.
- Load-balanced mixed-version tests route capability and create to different
  candidate instances and prove the fleet gate prevents feature capability from
  being advertised until both are compatible.
- CLI tests cover repeated `--feature`, parsing errors, exact SDK calls, and
  JSON/YAML rendering.
- Docker and Kubernetes end-to-end tests create an offline minimal sandbox with
  two ordered features, run each installed binary through execd, inspect
  activation, compare create/get metadata, and verify cleanup.
- Multi-architecture end-to-end coverage verifies that the returned child
  digest and OS/architecture match the runtime-selected platform.

## Drawbacks

- This adds an operator-maintained artifact supply chain and server startup
  dependency on validating its registry.
- Installing payloads at create time is slower than baking the same tools into a
  sandbox base image, especially on a cold Docker host or Kubernetes node.
- Static registry reload and no dependency solver make version 1 less flexible
  than devcontainer feature ecosystems.
- A self-contained payload may duplicate libraries across features and consume
  more disk than distribution package management.
- Kubernetes platform resolution without an explicit platform adds scheduling
  and metadata-patch complexity.
- Digest pinning and an allowlist reduce supply-chain ambiguity but do not make
  feature code safe.

## Alternatives

### Let callers submit arbitrary OCI references

Rejected. It would turn the lifecycle API into an arbitrary pre-start code
execution and registry-credential surface, bypass administrator review, make
egress/trust policy harder to enforce, and make supportability depend on
unbounded artifact formats. The public request therefore contains only name and
exact optional version; administrators own all OCI references and credentials.

### Run `apt`, `apk`, or `yum` during creation

Rejected. It depends on the sandbox distribution, root privileges, package
manager presence, mutable repository metadata, DNS and internet availability,
and installation scripts outside the OpenSandbox supply chain. It is also slow
and difficult to reproduce. Version 1 feature payloads are self-contained and
need no base-image package manager or dependency download.

### Enforce `deny_tools` with execd command strings or tool names

Rejected as a security mechanism. String matching does not identify the program
that the kernel will execute and controls only the execd paths where the filter
is installed. Absolute paths, renamed/copied binaries, interpreters, dynamic
linkers, uploaded binaries, and non-execd process entrypoints all bypass it.
Removing a path entry is equally bypassable. Enforceable restriction requires a
separate OSEP and runtime controls such as egress policy, capabilities, seccomp,
AppArmor, read-only filesystems, and isolated execution.

### Continue requiring custom sandbox images

This remains a valid optimization for stable, frequently used toolchains and
may have better cold-start performance. It is not sufficient as the only API:
it forces every user to operate an image build pipeline and does not give the
lifecycle server a structured, persisted feature resolution. Declarative
features complement rather than replace custom images.

### Encode features in `extensions`

Rejected. `extensions` is opaque and intended for provider-specific,
experimental, or temporary behavior. It cannot express a generated public SDK
model or a stable cross-provider response contract, and using it would conflict
with the lifecycle specification's source-of-truth guidance.

### Use mutable tags instead of digests

Rejected. Tags cannot support reproducible create/get metadata, safe cache keys,
or an auditable administrator allowlist. Human-readable versions remain in the
registry, but every index and platform manifest is pinned by digest.

## Infrastructure Needed

The proposal requires no new online service. Implementations reuse configured
OCI registries, Docker image storage, and Kubernetes node layer caches. Operators
need a static feature registry file and OCI credentials/trust configuration for
the allowlisted repositories.

Phase 3 needs CI-owned example feature images for amd64 and arm64, published by
digest to a repository accessible from Docker and Kind end-to-end jobs. Those
images are test/documentation fixtures, not a public marketplace. Their build
pipeline should produce provenance and SBOMs using the project's normal release
controls.

## Upgrade & Migration Strategy

The API addition is backward-compatible for callers that do not send
`features`. A server may be upgraded with no registry configured; all existing
creation flows remain unchanged, while feature requests receive an explicit
configuration error.

Rollout order follows the implementation phases: server/OpenAPI/providers first,
then regenerated clients and handwritten SDK alignment, then CLI/docs/examples
and end-to-end coverage. Phase 1 must deploy compatible feature handling to every
backend behind a base URL while the fleet capability remains disabled. After old
instances are drained, the operator enables the deployment-level capability;
only then may a feature-aware SDK or CLI be released against that endpoint. New
SDKs perform a fresh capability check for every non-empty feature create and do
not cache a positive result. Because current pre-feature servers silently ignore
unknown request fields, a missing or incompatible capability response is a
client-side hard failure and no create request is sent. Clients must not
downgrade the request into `extensions`.

Registry defaults may change only for future creates. Existing sandboxes retain
their payloads and persisted `resolvedFeatures` for their lifetime. Removing a
registry version does not rewrite or invalidate a running sandbox, though it
prevents new resolution of that version. Operators should retain old digests in
their OCI registry until no supported workflow needs to recreate them.

Rollback first revokes feature capability for the whole base URL, then waits for
in-flight capability checks and feature creates to drain before any older server
is admitted. Removing the registry path alone is not a compatibility gate; a
feature-aware fleet without a registry remains capable but returns
`FEATURE_REGISTRY_NOT_CONFIGURED`. Existing sandboxes continue to run because
their payload is already local and activation occurs inside their lifecycle.
There is no data migration and no dynamic uninstall operation.

OSEP approval authorizes implementation planning; it does not mean the feature
is implemented. GitHub issue #1289 remains open until this proposal is approved
and Phases 1 through 3 are complete end to end. The security-profile work in
Phase 4 proceeds under its own proposal and review.
