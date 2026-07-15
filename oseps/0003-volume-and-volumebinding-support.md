---
title: Volume Support
authors:
  - "@hittyt"
creation-date: 2026-01-29
last-updated: 2026-02-11
status: implementing
---

# OSEP-0003: Volume Support

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
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Introduce a runtime-neutral volume model in the Lifecycle API to enable persistent storage mounts across Docker and Kubernetes sandboxes. The proposal adds explicit volume definitions, mount semantics, and security constraints so that artifacts can persist beyond sandbox lifecycles without relying on file transfers.

This proposal focuses on file persistence via filesystem mounts. It is not a general-purpose storage abstraction (e.g., block or object storage APIs); those are only supported indirectly when exposed as a filesystem by the runtime or host.

```text
Time --------------------------------------------------------------->

Volume lifecycle:  [provisioned]-------------------------[retained]--->
Sandbox lifecycle:           [create]---[running]---[stop/delete]
                              |                         |
                          bind volume              unbind volume
```

## Motivation

OpenSandbox users running long-lived agents need artifacts (web pages, images, reports) to persist after a sandbox is terminated or restarted. Today, the API only supports transient filesystem operations via upload/download and provides no mount semantics; as a result, users must move large outputs out-of-band. This proposal adds first-class storage semantics while maintaining runtime portability and security boundaries.

### Goals

- Add a volume mount field to the Lifecycle API without breaking existing clients.
- Support Docker bind mounts (local path), Docker named volumes, and OSS mounts as the initial MVP.
- Provide a runtime-neutral `pvc` backend that maps to Docker named volumes and Kubernetes PersistentVolumeClaims, enabling portable cross-container data sharing.
- Provide secure, explicit controls for read/write access and path isolation.
- Keep runtime-specific details out of the core API where possible.

### Non-Goals

- Full-featured storage orchestration (auto-provisioning, snapshots, backups).
- Automatic cross-sandbox sharing or locking semantics are out of scope; only explicit volume mounts are supported.
- Guaranteeing portability for every storage backend in every runtime.
- Managing backend storage lifecycle (provisioning, resizing, and cleanup) is out of scope; users own and manage underlying storage resources independently.

## Requirements

- Backward compatible with existing sandbox creation requests.
- Works with both Docker and Kubernetes runtimes.
- Enforces path safety and explicit read/write permissions.
- Supports per-sandbox isolation (via subPath or equivalent).
- Clear error messages when a runtime does not support a requested backend.

## Proposal

Add a new optional field to the Lifecycle API:
- `volumes[]`: defines storage mounts for the sandbox. Each entry includes a named backend-specific struct (e.g., `host`, `ossfs`, `pvc`, `nfs`) and common mount settings (`name`, `mountPath`, `readOnly`, `subPath`).

The core API describes what storage is required using strongly-typed backend definitions. Each backend type has its own dedicated struct with explicit fields, making the schema self-documenting and enabling compile-time validation in typed SDKs. Runtime providers translate the model into platform-specific mounts.

### Notes/Constraints/Caveats

- Sandbox runtime (Docker/Kubernetes) and storage backend (host/ossfs/pvc) are independent dimensions. The API is designed so the same SDK request can target different runtimes; if a runtime cannot support a backend, it must return a clear validation error.
- OSS/S3/GitFS are popular production backends; this proposal keeps the model extensible so these can be supported early by adding new backend structs.
- The MVP targets Docker with `host`, `pvc`, and `ossfs` backends, and Kubernetes with `host`, `ossfs`, and `pvc` backends. The `pvc` backend is runtime-neutral: it maps to Docker named volumes in Docker and PersistentVolumeClaims in Kubernetes. Other backends (e.g., `nfs`) are described for future extension and may be unsupported initially.
- Kubernetes template merging currently replaces lists; this proposal requires list-merge or append behavior for volumes/volumeMounts to preserve user input.
- Exactly one backend struct must be specified per volume entry; specifying zero or multiple backend structs is a validation error.

### Risks and Mitigations

- Security risk: Docker hostPath mounts can expose host data. Mitigation: enforce allowlist prefixes, forbid path traversal, and use `readOnly: true` for read-only access when appropriate.
- Portability risk: different backends behave differently. Mitigation: keep core API minimal and require explicit backend selection.
- Operational risk: storage misconfiguration causes startup failures. Mitigation: validate mounts early and provide clear error responses.

## Design Details

### API schema changes
Add to `CreateSandboxRequest`:

```yaml
volumes:
  # Host path mount (read-write by default)
  - name: workdir
    host:
      path: "/data/opensandbox/user-a"
    mountPath: /mnt/work
    subPath: "task-001"

  # OSSFS mount
  - name: data
    ossfs:
      bucket: "my-bucket"
      endpoint: "oss-cn-hangzhou.aliyuncs.com"
      path: "/sandbox/user-a"
      accessKeyId: "AKIDEXAMPLE"
      accessKeySecret: "SECRETEXAMPLE"
      version: "2.0"
    mountPath: /mnt/data

  # PVC mount (platform-managed named volume, read-only)
  # Kubernetes: maps to PersistentVolumeClaim
  # Docker: maps to named volume
  - name: models
    pvc:
      claimName: "shared-models-pvc"
    mountPath: /mnt/models
    readOnly: true

  # NFS mount (future, read-only)
  - name: shared
    nfs:
      server: "nfs.example.com"
      path: "/exports/sandbox"
      options: "nfsvers=4.1,hard,timeo=600"
    mountPath: /mnt/shared
    readOnly: true
```

### Core semantics
- `volumes[]` declares storage mounts. Each volume entry contains:
  - `name`: unique identifier for the volume within the sandbox.
  - Exactly one backend struct (`host`, `ossfs`, `pvc`, `nfs`, etc.) with backend-specific typed fields.
  - `mountPath`: absolute path inside the container where the volume is mounted.
  - `readOnly` (optional): if true, the volume is mounted as read-only. Defaults to false (read-write).
  - `subPath` (optional): subdirectory under the backend path to mount.

### Backend struct definitions
Each backend type is defined as a distinct struct with explicit typed fields:

**`host`** - Host path bind mount:
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `path` | string | Yes | Absolute path on the host filesystem |

**`ossfs`** - Alibaba Cloud OSS mount via ossfs:
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `bucket` | string | Yes | OSS bucket name |
| `endpoint` | string | Yes | OSS endpoint URL (e.g., `oss-cn-hangzhou.aliyuncs.com`) |
| `accessKeyId` | string | Yes | Access key ID for inline authentication |
| `accessKeySecret` | string | Yes | Access key secret for inline authentication |
| `version` | string | No | ossfs version: `1.0` or `2.0` (default: `2.0`) |
| `options` | []string | No | Mount options list (e.g., `["allow_other", "umask=0022"]`) |

**`pvc`** - Platform-managed named volume:
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `claimName` | string | Yes | Name of the volume on the target platform (PVC name in Kubernetes, Docker volume name in Docker) |

The `pvc` backend is a runtime-neutral abstraction for referencing a pre-existing, platform-managed named volume. The semantics are identical across runtimes: claim an existing volume by name, mount it into the container, and leave volume lifecycle management to the user. In Kubernetes this maps to a PersistentVolumeClaim; in Docker this maps to a named volume (created via `docker volume create`).

**`nfs`** - NFS mount (future):
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `server` | string | Yes | NFS server hostname or IP |
| `path` | string | Yes | Absolute export path on the NFS server |
| `options` | string | No | Comma-separated mount options (e.g., `nfsvers=4.1,hard,timeo=600`) |

Additional backends (e.g., `s3`) can be added by defining new structs following this pattern.

### Backend constraints
Validation rules for each backend struct to reduce runtime-only failures:

- **`host`**: `path` must be an absolute path (e.g., `/data/opensandbox/user-a`). Reject relative paths and require normalization before validation.
- **`ossfs`**: `bucket` must be a valid bucket name. `endpoint` must be a valid OSS endpoint. `accessKeyId` and `accessKeySecret` are required for current MVP. `version` must be `1.0` or `2.0`; if omitted, defaults to `2.0`. In OSSFS backend, `subPath` represents bucket prefix. The runtime performs the mount during sandbox creation.
- **`pvc`**: `claimName` must be a valid resource name (DNS label: lowercase alphanumeric and hyphens, max 63 characters). The volume identified by `claimName` must already exist on the target platform; the runtime validates existence before container creation. In Kubernetes, the PVC must exist in the same namespace as the sandbox pod. In Docker, a named volume with the given name must exist (created via `docker volume create`); if the volume does not exist, the request fails validation rather than auto-creating it, to maintain explicit volume lifecycle management.
- **`nfs`**: `server` must be a valid hostname or IP. `path` must be an absolute path (e.g., `/exports/sandbox`).

These constraints are enforced in request validation and surfaced as clear API errors; runtimes may apply stricter checks.

### Permissions and ownership
Volume permissions are a frequent source of runtime failures and must be explicit in the contract:
- Default behavior: OpenSandbox does not automatically fix ownership or permissions on mounted storage. Users are responsible for ensuring the backend target is writable by the sandbox process UID/GID.
- Docker `host`: host path permissions are enforced by the host filesystem. Even with `readOnly: false`, writes will fail if the host path is not writable by the container user.
- Docker `pvc` (named volume): Docker named volumes created with the default `local` driver are owned by root. If the container runs as a non-root user, write access depends on the volume's filesystem permissions. Users should ensure correct ownership when creating the volume or use an init process to fix permissions.
- Kubernetes: filesystem permissions vary by storage driver. Future enhancement: add optional `fsGroup` field to backend structs that support it for pod-level volume access control.

### Concurrency and isolation
SubPath provides path-level isolation, not concurrency control. If multiple sandboxes mount the same volume without distinct `subPath` values and use `readOnly: false`, they may overwrite each other. OpenSandbox does not provide file-locking or coordination; users are responsible for handling concurrent access safely.

### Docker mapping
- `host` backend maps to bind mounts. `host.path + subPath` resolves to a concrete host directory.
- The host config uses `mounts`/`binds` with `ReadOnly` set from `readOnly` field.
- If the resolved host path does not exist, the request fails validation (do not auto-create host directories in MVP to avoid permission and security pitfalls).
- Allowed host paths are restricted by a server-side allowlist; users must specify a `host.path` under permitted prefixes. The allowlist is an operator-configured policy and should be documented for users of a given deployment.
- `pvc` backend maps to Docker named volumes. `pvc.claimName` is used as the Docker volume name in the bind string (e.g., `my-volume:/mnt/data:rw`). Docker recognizes non-absolute-path sources as named volume references. The named volume must already exist (created via `docker volume create`); if it does not exist, the request fails validation. When `subPath` is specified, the runtime resolves the volume's host-side `Mountpoint` via `docker volume inspect` and appends the `subPath` to produce a standard bind mount (e.g., `/var/lib/docker/volumes/my-volume/_data/subdir:/mnt/data:rw`). This requires the volume to use the `local` driver; non-local drivers are rejected when `subPath` is present because their `Mountpoint` may not be a real filesystem path. The resolved path must exist on the host; if it does not, the request fails validation.
- `ossfs` backend requires the runtime to mount OSS via ossfs during sandbox creation. Current MVP uses inline credentials (`accessKeyId`/`accessKeySecret`). In OSSFS backend, `subPath` is treated as bucket prefix and is resolved/validated on host before bind-mounting into the container. If the runtime does not support ossfs mounting, the request is rejected.

### Kubernetes mapping
- `pvc` backend maps to Kubernetes `persistentVolumeClaim` volume source: `pvc.claimName` → `volumes[].persistentVolumeClaim.claimName`.
- `nfs` backend maps to Kubernetes `nfs` volume source: `nfs.server` → `volumes[].nfs.server`, `nfs.path` → `volumes[].nfs.path`.
- `mountPath` maps to `volumeMounts.mountPath`.
- `subPath` maps to `volumeMounts.subPath`.
- `ossfs` backend maps to OSS CSI driver or equivalent runtime-specific mount configured with the struct fields.
- `host` backend maps to `hostPath` volume source and is node-local. For persistence guarantees in multi-node clusters, users must pin scheduling (node affinity) or use LocalPersistentVolume; otherwise data can disappear if the pod is rescheduled.

### Example: Host path mount
Create a sandbox that mounts a host directory:

```yaml
volumes:
  - name: workdir
    host:
      path: "/data/opensandbox/user-a"
    mountPath: /mnt/work
    subPath: "task-001"
```

Python SDK example (host):

```python
from opensandbox.api.lifecycle.client import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models.create_sandbox_request import CreateSandboxRequest
from opensandbox.api.lifecycle.models.image_spec import ImageSpec
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits
from opensandbox.api.lifecycle.models.volume import Volume
from opensandbox.api.lifecycle.models.host import Host

client = AuthenticatedClient(base_url="https://api.opensandbox.io", token="YOUR_API_KEY")

resource_limits = ResourceLimits.from_dict({"cpu": "500m", "memory": "512Mi"})
request = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    timeout=3600,
    resource_limits=resource_limits,
    entrypoint=["python", "-c", "print('hello')"],
    volumes=[
        Volume(
            name="workdir",
            host=Host(
                path="/data/opensandbox/user-a",
            ),
            mount_path="/mnt/work",
            sub_path="task-001",
        )
    ],
)

post_sandboxes.sync(client=client, body=request)
```

### Example: OSSFS mount
Create a sandbox that mounts an OSS bucket via ossfs:

```yaml
volumes:
  - name: workdir
    ossfs:
      bucket: "my-bucket"
      endpoint: "oss-cn-hangzhou.aliyuncs.com"
      path: "/sandbox/user-a"
      accessKeyId: "AKIDEXAMPLE"
      accessKeySecret: "SECRETEXAMPLE"
      version: "2.0"
      options:
        - "allow_other"
        - "umask=0022"
    mountPath: /mnt/work
    subPath: "task-001"
```

Runtime mapping (Docker):
- host path: runtime resolves target path under configured mount root (e.g., `/mnt/ossfs/<bucket>/<path>`), performs on-demand mount (or reuses existing mount), then bind-mounts into the container
- container path: `/mnt/work`
- readOnly: false (default, read-write)

### Example: Python SDK (lifecycle client)
Use the Python SDK lifecycle client to create a sandbox with an OSSFS volume mount (future typed model):

```python
from opensandbox.api.lifecycle.client import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models.create_sandbox_request import CreateSandboxRequest
from opensandbox.api.lifecycle.models.image_spec import ImageSpec
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits
from opensandbox.api.lifecycle.models.volume import Volume
from opensandbox.api.lifecycle.models.ossfs import OSSFS

client = AuthenticatedClient(base_url="https://api.opensandbox.io", token="YOUR_API_KEY")

resource_limits = ResourceLimits.from_dict({"cpu": "500m", "memory": "512Mi"})
request = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    timeout=3600,
    resource_limits=resource_limits,
    entrypoint=["python", "-c", "print('hello')"],
    volumes=[
        Volume(
            name="workdir",
            ossfs=OSSFS(
                bucket="my-bucket",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                path="/sandbox/user-a",
                access_key_id="AKIDEXAMPLE",
                access_key_secret="SECRETEXAMPLE",
                version="2.0",
                options=["allow_other", "umask=0022"],
            ),
            mount_path="/mnt/work",
            sub_path="task-001",
        )
    ],
)

post_sandboxes.sync(client=client, body=request)
```

### Example: PVC mount (cross-runtime)
The `pvc` backend provides a portable way to reference platform-managed named volumes. The same API request works on both Docker and Kubernetes:

```yaml
volumes:
  - name: shared-data
    pvc:
      claimName: "my-shared-volume"
    mountPath: /mnt/data
    subPath: "task-001"
```

Runtime mapping (Docker):
The `claimName` is used as the Docker named volume name. The volume must already exist (created via `docker volume create my-shared-volume`). When `subPath` is specified, the runtime resolves the volume's host-side `Mountpoint` via `docker volume inspect` and appends the subPath to produce a standard bind mount:
```text
# Docker bind string generated by the runtime (with subPath):
# Mountpoint = /var/lib/docker/volumes/my-shared-volume/_data
/var/lib/docker/volumes/my-shared-volume/_data/task-001:/mnt/data:rw

# Without subPath, the named volume is used directly:
# my-shared-volume:/mnt/data:rw
```

Runtime mapping (Kubernetes):
The `claimName` maps to a PersistentVolumeClaim in the same namespace.
```yaml
volumes:
  - name: shared-data
    persistentVolumeClaim:
      claimName: my-shared-volume
containers:
  - name: sandbox
    volumeMounts:
      - name: shared-data
        mountPath: /mnt/data
        subPath: task-001
```

Python SDK example (PVC):

```python
from opensandbox.api.lifecycle.client import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models.create_sandbox_request import CreateSandboxRequest
from opensandbox.api.lifecycle.models.image_spec import ImageSpec
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits
from opensandbox.api.lifecycle.models.volume import Volume
from opensandbox.api.lifecycle.models.pvc import PVC

client = AuthenticatedClient(base_url="https://api.opensandbox.io", token="YOUR_API_KEY")

resource_limits = ResourceLimits.from_dict({"cpu": "500m", "memory": "512Mi"})
request = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    timeout=3600,
    resource_limits=resource_limits,
    entrypoint=["python", "-c", "print('hello')"],
    volumes=[
        Volume(
            name="shared-data",
            pvc=PVC(
                claim_name="my-shared-volume",
            ),
            mount_path="/mnt/data",
            sub_path="task-001",
        )
    ],
)

post_sandboxes.sync(client=client, body=request)
```

#### Cross-container data sharing with PVC (Docker)
Multiple sandboxes can share data through the same named volume. This is more convenient and secure than using host paths, as Docker manages the storage location and no host paths need to be exposed:

```python
# Sandbox A: writes data to the shared volume
sandbox_a = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    entrypoint=["python", "-c", "open('/mnt/shared/result.txt','w').write('hello')"],
    volumes=[
        Volume(name="shared", pvc=PVC(claim_name="team-data"), mount_path="/mnt/shared")
    ],
)

# Sandbox B: reads data from the same shared volume
sandbox_b = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    entrypoint=["python", "-c", "print(open('/mnt/shared/result.txt').read())"],
    volumes=[
        Volume(name="shared", pvc=PVC(claim_name="team-data"), mount_path="/mnt/shared")
    ],
)
```

### Example: Kubernetes NFS (future)
Create a sandbox that mounts an NFS export with subPath isolation (non-MVP):

```yaml
volumes:
  - name: workdir
    nfs:
      server: "nfs.example.com"
      path: "/exports/sandbox"
      options: "nfsvers=4.1,hard,timeo=600"
    mountPath: /mnt/work
    subPath: "task-001"
```

Runtime mapping (Kubernetes):
```yaml
volumes:
  - name: workdir
    nfs:
      server: nfs.example.com
      path: /exports/sandbox
containers:
  - name: sandbox
    volumeMounts:
      - name: workdir
        mountPath: /mnt/work
        readOnly: false
        subPath: task-001
```

Python SDK example (NFS, future):

```python
from opensandbox.api.lifecycle.client import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models.create_sandbox_request import CreateSandboxRequest
from opensandbox.api.lifecycle.models.image_spec import ImageSpec
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits
from opensandbox.api.lifecycle.models.volume import Volume
from opensandbox.api.lifecycle.models.nfs import NFS

client = AuthenticatedClient(base_url="https://api.opensandbox.io", token="YOUR_API_KEY")

resource_limits = ResourceLimits.from_dict({"cpu": "500m", "memory": "512Mi"})
request = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    timeout=3600,
    resource_limits=resource_limits,
    entrypoint=["python", "-c", "print('hello')"],
    volumes=[
        Volume(
            name="workdir",
            nfs=NFS(
                server="nfs.example.com",
                path="/exports/sandbox",
                options="nfsvers=4.1,hard,timeo=600",
            ),
            mount_path="/mnt/work",
            sub_path="task-001",
        )
    ],
)

post_sandboxes.sync(client=client, body=request)
```

### Provider validation
- Reject unsupported backend types per runtime (e.g., `nfs` is only valid in Kubernetes).
- Validate that exactly one backend struct is specified per volume entry.
- Normalize and validate `subPath` against traversal; reject `..` and absolute path inputs.
- Enforce allowlist prefixes for `host.path` in Docker.
- For `ossfs` backend, validate required fields (`bucket`, `endpoint`, `accessKeyId`, `accessKeySecret`).
- For `pvc` backend, validate `claimName` is a valid DNS label (lowercase alphanumeric and hyphens, max 63 characters). In Kubernetes, validate the PVC exists in the same namespace. In Docker, validate the named volume exists via the Docker API (`docker volume inspect`).
- For `nfs` backend, validate required fields (`server`, `path`).
- `subPath` is created if missing under the resolved backend path; if creation fails due to permissions or policy, the request is rejected.

### Configuration (example)
Host path allowlists are configured by the control plane (server/execd) and enforced at validation time. Example `config.toml`:

```toml
[storage]
allow_host_paths = ["/data/opensandbox", "/tmp/sandbox"]
ossfs_mount_root = "/mnt/ossfs"
```

## Test Plan

- Unit tests for schema validation and path normalization.
- Unit tests for backend struct validation:
  - Reject volume entries with zero or multiple backend structs.
  - Validate required fields per backend type.
- Provider unit tests:
  - Docker `host`: bind mount generation, read-only enforcement, allowlist rejection.
  - Docker `pvc`: named volume bind generation, volume existence validation, read-only enforcement, `claimName` format validation, rejection when volume does not exist, `subPath` resolution via `Mountpoint` for `local` driver, rejection of `subPath` for non-local drivers, rejection when resolved subPath does not exist.
  - Docker `ossfs`: mount option validation, inline credential validation (`accessKeyId`/`accessKeySecret`), version validation (`1.0`/`2.0`), `subPath`-as-prefix resolution, mount failure handling.
  - Kubernetes `pvc`: PVC reference validation, volume mount generation.
- Integration tests:
  - Docker: sandbox creation with `host` volume, sandbox creation with `pvc` (named volume), `pvc` with `subPath` mount, cross-container data sharing via named volume.
  - Kubernetes: sandbox creation with `pvc`, sandbox creation with `host` volume.
- Negative tests for unsupported backends and invalid paths.

## Drawbacks

- Adds API surface area and increases runtime provider complexity.
- Docker bind mounts introduce security considerations and operational policy requirements.

## Alternatives

- Keep using file upload/download only: simpler but does not satisfy persistence requirements.
- Use runtime-specific `extensions` only: faster to ship but fractures API consistency and increases client complexity.

## Infrastructure Needed

The runtime must have the ability to perform filesystem mounts for the requested backend types. For `ossfs` backend, the runtime must have ossfs 1.0 or 2.0 installed; the MVP assumes the runtime can mount using the struct fields provided in the request.

## Upgrade & Migration Strategy

This change is additive for volume support and supports OSSFS inline credentials (`accessKeyId`/`accessKeySecret`). If a client submits volume fields to a runtime that does not support them, the API will return a clear validation error.

## Kubernetes Feasibility (Design Only)

Kubernetes runtime is not implemented in this phase, but API compatibility is preserved by design:

- Keep request schema runtime-neutral: `volumes[].ossfs` has consistent shape across Docker and Kubernetes.
- Introduce runtime adapters:
  - Docker adapter performs host-side ossfs mount + bind using inline credentials.
  - Kubernetes adapter can map OSSFS fields to native Secret/CSI references in a future phase.
- Keep failure semantics aligned:
  - Missing credential reference -> validation error with shared error code family.
  - Runtime unsupported backend -> explicit `UNSUPPORTED_VOLUME_BACKEND`.
- Keep `subPath` semantics aligned:
  - API meaning remains "`subPath` is mounted under backend path".
  - Docker resolves to host path (`subPath` as OSS prefix); Kubernetes maps to `volumeMounts.subPath`.
