---
title: Pause & Resume
description: Pause and resume Kubernetes sandboxes using root filesystem images or a workspace checkpoint volume.
---

# Pause and Resume Guide

This guide explains how to use the pause and resume features for Kubernetes-backed sandboxes in OpenSandbox. By default, pause commits the sandbox's root filesystem as an OCI image and releases cluster resources. Resume restores the sandbox from that image.

## Table of Contents

- [Overview](#overview)
- [Workspace-only pause on Kubernetes](#workspace-only-pause-on-kubernetes)
- [Architecture](#architecture)
- [Prerequisites](#prerequisites)
- [Controller Configuration](#controller-configuration)
- [Registry and Secret Setup](#registry-and-secret-setup)
- [Usage Guide](#usage-guide)
- [Administrator Guide](#administrator-guide)
- [SandboxSnapshot Reference](#sandboxsnapshot-reference)
- [Troubleshooting](#troubleshooting)

---

## Overview

### What Pause and Resume Does

| | Behavior |
|--|---------|
| **Pause** | Creates an internal `SandboxSnapshot`, commits the running container root filesystem as an OCI image, then quiesces the sandbox runtime and releases Pods / pooled allocations |
| **Resume** | Reuses the same `BatchSandbox`, rewrites its template to the latest snapshot image, and recreates the runtime from that image |
| **sandboxId** | Stable across pause/resume cycles — callers use the same ID throughout the sandbox lifetime |
| **Replica support** | Currently limited to `BatchSandbox.spec.replicas=1`. Server-created Kubernetes sandboxes use `replicas: 1`; direct CRs with another replica count are rejected by the controller pause entry. |

### Key Design Principle

**Controller-level configuration**: Registry URL and push/pull secrets are configured on the Kubernetes controller manager, not in `~/.sandbox.toml`. SDK users and API callers require **no code changes** to use pause/resume — they just call `pause` and `resume` on the existing sandbox ID.

### Lifecycle

```text
Time ---------------------------------------------------------------->

Sandbox lifecycle:   [Running]--[Pausing]--[Paused]--[Resuming]--[Running]
                         |                     |
                  commit rootfs          rewrite template images
                  push to registry       recreate runtime from snapshot
                  release pods/alloc
```

### State Machine Details

The sandbox transitions through both stable and intermediate states:

| State | Type | Description |
|-------|------|-------------|
| `Running` | Stable | Sandbox is active and processing requests |
| `Pausing` | Intermediate | Pause operation in progress. Snapshot commit is coordinated through an internal `SandboxSnapshot` resource. |
| `Paused` | Stable | Sandbox is paused, the latest rootfs snapshot is ready, and runtime Pods / pooled allocations have been released |
| `Resuming` | Intermediate | Resume operation in progress. The controller is rewriting the sandbox template to the latest snapshot image and recreating the runtime |
| `Failed` | Stable | Operation failed (check `reason` and `message` for details) |

The Lifecycle API exposes only the coarse-grained sandbox states above. For detailed snapshot progress, inspect the internal `SandboxSnapshot` resource:

Pod termination can continue after the controller reports `Paused`. A resume request can be submitted during this interval; the controller waits for the old Pod to be removed before recreating its replacement. Pods with a deletion timestamp do not contribute new runtime failure conditions, including terminal exit statuses reported by Kubernetes during deletion. Failures already recorded on the sandbox remain terminal, and failures of replacement Pods are still reported normally.

- `Pending`: snapshot request accepted, waiting to resolve source Pod / create commit Job
- `Committing`: commit Job is running and pushing snapshot images
- `Succeed`: snapshot is ready and can be used for the next resume
- `Failed`: snapshot creation failed

For `rootfs-v1` snapshots, `Succeed` is published only after the commit Job reports an image digest for every container. If that result is unavailable — for example, when the commit Pod was removed before the controller could read its termination message — the snapshot is marked `Failed` with `CommitResultUnavailable` or `CommitResultIncomplete`. The runtime Pods are kept, the sandbox reports `PauseFailed`, and the pause can be retried.

### What Is Preserved

| | Preserved? |
|--|-----------|
| Root filesystem contents | ✅ Yes — committed as OCI image |
| Environment variables | ✅ Yes — from BatchSandbox template |
| Running processes / memory | Rootfs mode: no. Opt-in QEMU-in-runc mode: the QEMU process and Guest memory are restored; other outer processes restart. See [QEMU VMState Snapshots](/guides/qemu-vmstate-snapshots). |
| Explicit volume mounts | Depends on volume type |
| Credential Vault entries | No - stored only in egress sidecar memory; re-inject from a trusted control plane after resume |

Pause/resume is currently single-replica only. The internal pause snapshot records one source Pod's container images and does not store per-replica state, so the Kubernetes controller rejects pause requests unless `BatchSandbox.spec.replicas=1`.

See [Credential Vault](/guides/credential-vault#persistence-across-pause-and-resume) for the required post-resume credential re-injection procedure.

### Durability and abnormal termination

The root filesystem is durable only after the snapshot reaches `Succeed` and the OCI image has been pushed to the configured registry. A pause request being accepted, or a snapshot being in `Pending` or `Committing`, does not provide a durability guarantee.

Changes made after the last successful snapshot remain in the container's writable layer. They are not automatically snapshotted when the process exits, the container is OOM-killed, the Pod enters `Failed`, the Pod is evicted, or the node becomes unavailable. The lifecycle API does not currently expose a post-failure snapshot or recovery operation, and a new container created by a Kubernetes restart does not inherit the old writable layer.

The image-committer can sometimes read a stopped container directly from containerd while its metadata and writable snapshot still exist on the source node. This is a best-effort operator recovery path, not a supported durability contract: the normal snapshot API requires a `Running` sandbox, and containerd or kubelet garbage collection, Pod replacement, node loss, or a reused container identity can make the old layer unavailable or ambiguous.

For data that must survive an unexpected termination, store it in a persistent volume or an external service and take explicit root filesystem snapshots for reproducible environment state. Files in mounted volumes follow the persistence and backup behavior of that volume; they are not included in the OCI rootfs snapshot.

---

## Workspace-only pause on Kubernetes

A single-replica, non-pooled BatchSandbox can save the ordinary `/workspace` directory to an object-storage-backed PVC on pause. Resume uses the original application image and an init container to restore the archive into a fresh volume mounted at `/workspace`. Each pause creates a new archive. Files elsewhere in the container and running processes are discarded when the Pod is released.

The application image must provide Python 3.12 or newer. Use a dedicated, writable PVC backed by a bucket or prefix accessible only to this sandbox. The CSI driver must support sequential writes, fsync and durable close.

**Shared server template.** Put the strategy and logical volume name in `batchsandbox_template_file`. The server uses `sandbox` as the application container name.

```yaml
spec:
  workspacePause:
    containerName: sandbox
    volumeName: checkpoints
```

**Per-sandbox create request.** Select the dedicated PVC for this sandbox and pass the mount in the SDK's `volumes` list. Set `claim_name` to the PVC provisioned for this sandbox.

```python
volumes = [
    Volume(
        name="checkpoints",
        pvc=PVC(claimName=claim_name, createIfNotExists=False, deleteOnSandboxTermination=False),
        mountPath="/mnt/opensandbox-checkpoints",
    )
]
```

For a directly managed BatchSandbox, add the strategy above and the following mount to the individual sandbox manifest. Replace `my-sandbox-bucket` with its dedicated claim.

```yaml
spec:
  template:
    spec:
      containers:
        - name: sandbox
          volumeMounts:
            - name: checkpoints
              mountPath: /mnt/opensandbox-checkpoints
      volumes:
        - name: checkpoints
          persistentVolumeClaim:
            claimName: my-sandbox-bucket
```

The archive supports regular files, directories and symbolic links. Absolute links into the workspace are stored as relative links. Links to external paths, such as a virtual environment’s interpreter, keep their link text. Their targets must exist in the resumed image. Restore creates symbolic links after extracting file contents. Sockets, FIFOs and devices fail capture with the entry name. Restore preserves ordinary Unix access bits and clears setuid and setgid bits. A root init container restores numeric owners and groups. With a non-root init container, new files use the ownership assigned by the filesystem.

Stop application writers before requesting pause. Use the existing pause and resume APIs. A failed export leaves the source Pod available for retry. On resume the init container verifies the archive digest before publishing the workspace. If this step fails, the init container can retry on the same volume. Once published, the workspace keeps subsequent edits if the init container runs again. A replacement Pod with a fresh volume restores from the active archive, so keep that archive for the lifetime of the sandbox. Inspect the init container logs if the volume or archive is unavailable. New objects use the prefix `<BatchSandbox UID>/` on the dedicated claim. While paused, inspect the internal snapshot’s `status.workspace`. After resume, the Pod template annotations `sandbox.opensandbox.io/workspace-archive` and `sandbox.opensandbox.io/workspace-claim` identify the checkpoint required by replacement Pods. Preserve both references during transitions. To reclaim all completed and abandoned uploads safely, expire that UID prefix after the sandbox and its Pods have been deleted. Legacy flat archive keys remain readable. Do not apply an age-only expiration rule to a live sandbox.

Temporary API or exec connection errors and a source Pod waiting to run are retried with backoff within the snapshot’s configured commit timeout. Configuration errors and a nonzero exit from the archive helper fail the export and preserve the source Pod.

If `status.workspace.sha256` is missing or is not a 64-character hexadecimal digest, resume leaves the sandbox paused and reports `ResumeFailed` with reason `InvalidWorkspaceRestore`. Uppercase and lowercase hexadecimal digits are accepted.

New checkpoints record the uncompressed tar size in `status.workspace.sizeBytes`. The restore init container requests node-local ephemeral storage for the archive size plus the larger of 25% or 64 MiB of headroom. Its workspace `emptyDir` uses this budget unless the application has a larger storage request or an explicit storage limit. The controller respects an explicit limit and rejects a checkpoint larger than that limit before releasing the source Pod. Allow additional capacity for many small files and for writes after resume. Older checkpoints without size metadata keep their previous storage settings.

During restore, inspect the BatchSandbox’s `Progressing` condition for reason `WorkspaceRestoreRetrying` if the init container fails and Kubernetes retries it. The condition identifies the Pod and init container. The application image supplies the interpreter and the workload can write its PVC. The digest verifies stored bytes under that trust assumption.

Keep the pause strategy fixed once a workspace has been restored. To migrate back to rootfs, copy the live workspace to durable storage and start a new sandbox. If the requested strategy changes during an in-flight pause, the controller reports `WorkspaceConfigChanged` and retains the Pod and snapshot. Restore the original setting to finish the pending pause.

## Architecture

```
API caller
    │ POST /v1/sandboxes/{id}/pause
    ▼
OpenSandbox Server
    │ PATCH BatchSandbox.spec.pause=true
    ▼
BatchSandbox Controller (Kubernetes)
    │ validates lifecycle state
    │ creates internal SandboxSnapshot CR
    ▼
SandboxSnapshot Controller
    │ resolves running Pod
    │ creates commit Job on the same node
    ▼
commit Job Pod (image-committer)
    │ containerd API: commit container rootfs → OCI image
    │ OCI registry resolver: push image
    ▼
SandboxSnapshot.status.phase = Succeed
    │ BatchSandbox.status.phase = Paused
    │ deletes Pods or releases pooled allocation
    ▼
Cluster resources released

--- Later: resume ---

API caller
    │ POST /v1/sandboxes/{id}/resume
    ▼
OpenSandbox Server
    │ PATCH BatchSandbox.spec.pause=false
    ▼
BatchSandbox Controller
    │ reads internal SandboxSnapshot
    │ rewrites pod template images from snapshot
    │ clears poolRef for pooled sandboxes
    │ recreates runtime Pods
    ▼
Sandbox running again with restored filesystem
```

---

## Prerequisites

1. **Kubernetes cluster** with the OpenSandbox controller deployed
2. **OCI-compatible container registry** accessible from cluster nodes (push) and the Kubernetes API (pull)
3. **Kubernetes Secrets** of type `kubernetes.io/dockerconfigjson` for registry authentication
4. **Controller manager** configured with snapshot registry and secret flags

---

## Controller Configuration

Configure the controller manager deployment with snapshot flags:

```yaml
- --snapshot-registry=registry.example.com/sandboxes
- --snapshot-registry-insecure=false
- --snapshot-push-secret=registry-snapshot-push-secret
- --resume-pull-secret=registry-pull-secret
```

### Configuration Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `--snapshot-registry` | string | `""` | **Required.** OCI registry prefix. By default, images are stored as `<registry>/<sandboxName>-<container>:snap-gen<N>`. |
| `--snapshot-image-uri-template` | string | `""` | Go named-field template for snapshot image URIs; empty uses the default naming rule below. |
| `--snapshot-registry-insecure` | bool | `false` | Enables insecure registry mode for snapshot push operations. Use only for HTTP or self-signed local registries. |
| `--snapshot-push-secret` | string | `""` | Kubernetes Secret name for pushing snapshots. Must be `kubernetes.io/dockerconfigjson` type. |
| `--image-committer-pod-template-file` | string | `""` | Path to a PodTemplateSpec overlay for commit Job Pods. |
| `--resume-pull-secret` | string | `""` | Kubernetes Secret name injected into resumed sandboxes for pulling snapshot images. Can be the same as push secret. |
| `--image-committer-image` | string | `"image-committer:dev"` | Image used by commit Jobs. |
| `--commit-job-timeout` | duration | `"10m"` | Timeout for commit Jobs. |

### Helm chart support

The `opensandbox-controller` Helm chart now exposes the snapshot-related controller values directly:

- `controller.snapshot.imageCommitterImage`
- `controller.snapshot.imageCommitterPodTemplate`
- `controller.snapshot.commitJobTimeout`
- `controller.snapshot.registry`
- `controller.snapshot.imageURITemplate`
- `controller.snapshot.registryInsecure`
- `controller.snapshot.snapshotPushSecret`
- `controller.snapshot.resumePullSecret`

For the all-in-one `opensandbox` chart, use the same values under the `opensandbox-controller.*` prefix.

### Startup behavior

The server no longer carries dedicated pause/resume config. Missing registry or secret settings are surfaced by the Kubernetes controllers when a `SandboxSnapshot` is processed, for example as `SandboxSnapshot.status.conditions[type=Failed]` with reasons like `RegistryNotConfigured`.

---

## Registry and Secret Setup

### Step 1: Prepare your registry

Any OCI-compatible registry works (Docker Hub, GitHub Container Registry, Harbor, a private `registry:2` instance, etc.). The registry must be:

- **Reachable from cluster nodes** (for the commit Job to push)
- **Reachable from the Kubernetes API server / kubelet** (for image pull on resume)

### Step 2: Create the push secret

```bash
kubectl create secret docker-registry registry-snapshot-push-secret \
  --docker-server=registry.example.com \
  --docker-username=<username> \
  --docker-password=<password-or-token> \
  --namespace=<sandbox-namespace>
```

### Step 3: Create the pull secret

The pull secret is used by the resumed `BatchSandbox` Pod to pull the snapshot image. It can be the same secret as the push secret if your credentials have both read and write access:

```bash
kubectl create secret docker-registry registry-pull-secret \
  --docker-server=registry.example.com \
  --docker-username=<username> \
  --docker-password=<password-or-token> \
  --namespace=<sandbox-namespace>
```

### Using a private `registry:2` (development)

For development with a cluster-internal `registry:2` deployment:

```bash
# Create a registry deployment
kubectl create deployment docker-registry \
  --image=registry:2 --port=5000

kubectl expose deployment docker-registry --port=5000

# No authentication needed for internal registry
# Leave snapshot push/pull secret flags empty on the controller manager
```

---

## Usage Guide

Once the controller manager is configured and the server is running, pause/resume works through the standard Lifecycle API. No SDK changes are needed.

### Pause a sandbox

```bash
curl -X POST http://localhost:8080/v1/sandboxes/{sandbox_id}/pause \
  -H "Content-Type: application/json"
```

**Response:** `202 Accepted` with an empty body.

The pause is asynchronous. The sandbox transitions through: `running` → `pausing` → `paused`

### Check pause status

```bash
curl http://localhost:8080/v1/sandboxes/{sandbox_id}
```

When `status` is `paused`, the filesystem has been committed and cluster resources have been released.

### Resume a sandbox

```bash
curl -X POST http://localhost:8080/v1/sandboxes/{sandbox_id}/resume \
  -H "Content-Type: application/json"
```

**Response:** `202 Accepted` with an empty body.

The sandbox transitions through: `paused` → `resuming` → `running`

### Multiple pause/resume cycles

Pause and resume can be repeated. Each pause cycle produces a new snapshot image tag (`snap-gen1`, `snap-gen2`, ...). The latest snapshot is always used for the next resume.

---

## Administrator Guide

### Controller RBAC

The OpenSandbox controller requires the following RBAC permissions for pause/resume (included in the Helm chart and `make manifests` output):

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `sandboxsnapshots` | get, list, watch, create, update, patch, delete | Manage SandboxSnapshot CRs |
| `jobs` / `jobs/status` | full | Create/monitor commit Jobs |
| `secrets` | get | Validate push secret exists before creating commit Job |
| `pods` | get, list, watch | Find running Pod for commit |

### Snapshot image naming

Internal pause/resume snapshot images are named:
```
<snapshot-registry>/<sandboxName>-<containerName>:snap-gen<N>
```

For example, with `--snapshot-registry=registry.example.com/sandboxes`, sandbox `my-sandbox`, container `sandbox`, first pause:
```
registry.example.com/sandboxes/my-sandbox-sandbox:snap-gen1
```

Server-managed public snapshots use the same repository layout but a stable snapshot-id-derived tag:
```
<snapshot-registry>/<sandboxName>-<containerName>:snap-<snapshotIdHex>
```

The controller distinguishes the two modes by owner reference. Pause/resume snapshots are created by the `BatchSandbox` controller and have a controller ownerReference to the owning `BatchSandbox`; public snapshots are created by the Lifecycle server and do not use that ownerReference.

#### Custom image names

Set `--snapshot-image-uri-template` on the controller manager, or `controller.snapshot.imageURITemplate` in the controller Helm chart, to replace the naming rule. An empty value uses this Go `text/template`:

```text
{{.Registry}}/{{.SandboxName}}-{{.ContainerName}}:{{.SnapshotTag}}
```

The template receives these named fields:

| Field | Value |
|-------|-------|
| `Registry` | The required `--snapshot-registry` prefix, including any repository path |
| `Namespace` | The snapshot's Kubernetes namespace |
| `SandboxName` | The source BatchSandbox name |
| `SnapshotName` | The SandboxSnapshot name |
| `SnapshotUID` | The SandboxSnapshot UID |
| `SnapshotCreationTime` | The SandboxSnapshot `metadata.creationTimestamp` as a Go `time.Time` in the controller process's local timezone |
| `ContainerName` | The source container name, or `vmstate` for a QEMU VM-state artifact |
| `SnapshotTag` | The existing `snap-gen<N>` or public snapshot tag described above |
| `ArtifactKind` | `rootfs` for container images, or `vmstate` for a QEMU VM-state artifact |

For example, use one repository with a distinct tag for each snapshot artifact:

```yaml
controller:
  snapshot:
    registry: registry.example.com/sandboxes
    imageURITemplate: '{{.Registry}}/snapshots:{{.SnapshotUID}}-{{.ArtifactKind}}-{{.ContainerName}}'
```

To group images by the snapshot creation date in the controller's local timezone while retaining the sandbox, container, and snapshot tag:

```yaml
controller:
  snapshot:
    registry: registry.example.com/team
    imageURITemplate: '{{.Registry}}/library/snapshots-{{date "2006-01-02" .SnapshotCreationTime}}:{{.SandboxName}}-{{.ContainerName}}-{{.SnapshotTag}}'
```

The date functions follow Helm's names and argument order and accept a Go `time.Time`, such as `SnapshotCreationTime`:

```text
{{date "2006-01-02" .SnapshotCreationTime}}
{{dateInZone "2006-01-02" .SnapshotCreationTime "UTC"}}
{{dateInZone "2006-01-02-15-04-05" .SnapshotCreationTime "+08:00"}}
{{dateInZone "2006-01-02" .SnapshotCreationTime "America/New_York"}}
```

`date` uses Go's `time.Local`. `dateInZone` accepts `Local` or an empty timezone for the same default, `UTC` or `Z`, a signed `HH:MM` offset (hours 00–23, minutes 00–59), or an IANA `Area/Location` name. Fixed offsets such as `+05:30` do not change with daylight saving time; named zones follow the rules for the snapshot's creation instant. Unknown zones and malformed offsets fail template execution. These helpers reject a zero timestamp instead of substituting the current time.

On Unix, the local timezone comes from `TZ`, or `/etc/localtime` when `TZ` is unset. In Kubernetes this is the controller container's timezone; set its `TZ` environment variable or provide its timezone files to select the intended local zone. The controller embeds timezone data for named zones when the OS has none.

Formatting uses Go's standard layouts: `2006-01-02` for a date, `20060102-150405` for a compact timestamp, and `2006-01-02-15-04-05.000` for milliseconds. Java-style `yyyy-MM-dd` patterns are not supported. Layouts must produce a valid image reference in their template position: spaces, colons, or uppercase month names are unsuitable in a repository name and fail validation.

Native methods remain available: `.SnapshotCreationTime.Format "2006-01-02"` uses the local timezone, while `.SnapshotCreationTime.UTC.Format "2006-01-02"` selects UTC explicitly. Templates written for the earlier UTC-only field should select UTC explicitly to preserve their date when the controller's local zone is not UTC.

The source is always the snapshot creation timestamp, so retries after midnight keep the same date with the same timezone configuration. Existing commit Jobs retain their resolved targets when the template or timezone setting changes.

For the umbrella chart, nest these values under `opensandbox-controller`. Helm passes the template literally; it does not evaluate the Go template fields. For Kustomize or a direct Deployment, add an argument to the manager container:

```yaml
- '--snapshot-image-uri-template={{.Registry}}/snapshots:{{.SnapshotUID}}-{{.ArtifactKind}}-{{.ContainerName}}'
```

The controller renders the URI before creating the commit Job. The image committer pushes directly to that URI, and restore uses the recorded reference; there is no intermediate push or rename. QEMU rootfs and VM-state artifacts use the same template and original snapshot tag.

Templates must produce valid tagged OCI image references without a digest. Custom template results are normalized to fully qualified references before they are recorded and passed to the image committer: `snapshots:tag` becomes `docker.io/library/snapshots:tag`, and `team/snapshots:tag` becomes `docker.io/team/snapshots:tag`. Include an explicit registry host (a hostname containing a dot, a host with a port, or `localhost`) to target another registry. An empty template keeps the original naming rule and registry prefix unchanged. Names are not automatically sanitized or truncated. Include fields that keep artifacts and snapshots distinct; the controller rejects duplicate targets within a snapshot, but cannot detect collisions with other snapshots. Template syntax errors prevent controller startup; rendering errors, invalid references, and duplicate targets fail the snapshot before a commit Job is created. Changes apply when creating new commit Jobs; existing Jobs keep their resolved targets, and completed snapshots retain their recorded restore references.

### Runtime compatibility

The built-in `rootfs-v1` image committer requires a container runtime that exposes compatible containerd task pause/resume and writable-snapshot APIs. gVisor RuntimeClasses whose handler is `runsc` do not currently satisfy that contract. The Lifecycle server rejects public snapshot creation for those workloads before it persists a snapshot record or creates a `SandboxSnapshot` resource, leaving the running sandbox untouched. Rootfs pause uses the same committer, so the Lifecycle server also answers HTTP 409 to a rootfs pause of a BatchSandbox whose RuntimeClass handler is `runsc` or starts with `runsc-`, or whose runtime it cannot verify, and leaves the sandbox running.

With `spec.workspacePause` configured, the controller archives files through Pod exec. This strategy does not require the rootfs committer or its RuntimeClass check.

Native gVisor checkpoint/restore requires a dedicated snapshot backend and is not provided by the `rootfs-v1` committer.

### Commit Job

The controller creates a short-lived Kubernetes `Job` for each pause:

- **Job name**: `<snapshotName>-commit`
- **Node affinity**: Runs on the **same node** as the source Pod (containerd socket access required)
- **Timeout**: 10 minutes (`ActiveDeadlineSeconds`)
- **TTL**: 5 minutes after completion (`TTLSecondsAfterFinished`)
- **Image**: `image-committer` (configurable via controller `--image-committer-image` flag)

The commit Job mounts the host containerd socket from the source node and runs as UID 0. This gives the `image-committer` image node-level container runtime access. Use only a trusted image, preferably pinned by digest or controlled by an admission policy.

The built-in image committer uses containerd APIs directly for container lookup, task pause/unpause, writable-snapshot image creation, and registry push. Before committing, it verifies that the node's content store contains every base-image blob for the node platform and fetches missing blobs from the original image digest; this avoids snapshot failures after CRI discards compressed content during image unpack. Source registries use HTTPS by default. For a trusted source registry that requires skipped TLS verification or plain HTTP, inject `SOURCE_IMAGE_REGISTRY_INSECURE=true` into the `commit` container through the image-committer Pod template. The reusable Go contracts and default implementations are exposed in [`pkg/imagecommitter`](https://github.com/opensandbox-group/OpenSandbox/tree/main/kubernetes/pkg/imagecommitter); provider-specific binaries can supply a credential provider and reuse the standard CLI contract. The Job also retains the host `/run/containerd/fifo` mount so compatible implementations can use containerd task exec. Any preparation command used by the built-in implementation is best effort and does not change the commit interface.

The operator-controlled Pod template can add labels, annotations, a ServiceAccount, scheduling settings, init containers, sidecars, and settings on the required `commit` container such as resources, `env`, and `envFrom`. The template is the merge base, and controller-generated invariants override conflicting template values. The controller reserves the source `nodeName`, restart policy, committer image, image pull policy, command, arguments, security context, termination-message settings, and required environment variables, mounts, and volumes. Environment variables, mounts, and volumes are merged by name, with required controller values winning conflicts. Additional regular sidecars must terminate for the Job to complete.

Any ServiceAccount or admission configuration referenced by the template must exist in every sandbox namespace. Unpause Jobs do not receive the commit Pod template because they do not access the registry.

If the commit Job fails, the controller creates a best-effort `<snapshotName>-unpause` Job on the same node to unpause any source containers that may have been left paused by an abrupt committer exit.

Deleting a `SandboxSnapshot` cleans up Kubernetes commit/unpause Jobs, but does not delete pushed OCI images from the registry. Repeated pause cycles create tags such as `snap-gen<N>`; configure registry retention or garbage collection externally.

### Monitoring

Check SandboxSnapshot status:

```bash
kubectl get sandboxsnapshot -n <namespace>
# NAME          PHASE       SANDBOX_ID     AGE
# my-snapshot   Succeed     my-sandbox     5m

kubectl describe sandboxsnapshot my-snapshot -n <namespace>
```

Key fields to watch:

- `status.phase`: `Pending` → `Committing` → `Succeed` / `Failed`
- `status.conditions`: readiness or failure reasons with human-readable messages
- `status.containers`: image URIs for each committed container
- `status.sourcePodName` / `status.sourceNodeName`: resolved execution source for the snapshot

#### Monitoring Sandbox State Transitions

When checking sandbox state via the Lifecycle API, you'll see intermediate states:

**Pause flow:**
```bash
curl http://localhost:8080/v1/sandboxes/{sandbox_id}
# Response during pause:
{
  "id": "my-sandbox",
  "status": {
    "state": "Pausing",
    "reason": "PAUSING",
    "message": "Pausing sandbox"
  }
}
```

**Resume flow:**
```bash
curl http://localhost:8080/v1/sandboxes/{sandbox_id}
# Response during resume:
{
  "id": "my-sandbox",
  "status": {
    "state": "Resuming",
    "reason": "RESUMING",
    "message": "Resuming sandbox"
  }
}
```

---

## SandboxSnapshot Reference

### Spec fields

| Field | Type | Description |
|-------|------|-------------|
| `sandboxName` | string | Target `BatchSandbox` name in the same namespace |

### Status fields (set by Controller)

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `Pending` / `Committing` / `Succeed` / `Failed` |
| `format` | string | `rootfs-v1` (default), `qemu-v1`, or `workspace-v1` |
| `workspace` | object | For `workspace-v1`, identifies the source Pod UID, container, volume, PVC, archive key and SHA-256. New checkpoints also include the uncompressed tar size in `sizeBytes`. |
| `conditions` | list | `Ready` / `Failed` conditions with reason and message |
| `sourcePodName` | string | Pod name used for commit |
| `sourceNodeName` | string | Node where commit Job runs |
| `containers` | list | `{containerName, imageUri, imageDigest}` per container. `imageDigest` is the image config digest, preserving the image ID semantics of earlier image-committer versions. |
| `observedGeneration` | int | Last processed spec generation |

---

## Troubleshooting

### 1. Snapshot stuck in `Failed` — push secret not found

**Cause**: The controller manager was configured with a `--snapshot-push-secret` that does not exist in the sandbox namespace.

**Solution**:
```bash
kubectl get secret registry-snapshot-push-secret -n <namespace>
# If missing:
kubectl create secret docker-registry registry-snapshot-push-secret \
  --docker-server=<registry> \
  --docker-username=<user> \
  --docker-password=<token> \
  -n <namespace>
```

The controller validates secret existence **before** creating the commit Job (fail-fast). Once the secret is created, trigger a new pause cycle.

---

### 2. Snapshot stuck in `Committing` for a long time

**Check the commit Job and its Pod:**

```bash
kubectl get job -n <namespace> -l sandbox.opensandbox.io/snapshot=<snapshotName>
kubectl describe pod <commit-pod-name> -n <namespace>
```

**Common causes:**

| Symptom | Cause | Solution |
|---------|-------|---------|
| `ContainerCreating` for >30s | Secret missing or wrong type | Re-create secret as `kubernetes.io/dockerconfigjson` |
| `FailedMount` event | Secret not found | See issue #1 above |
| Pod running but job never completes | Registry unreachable from node | Check network connectivity from node to registry |
| `unauthorized` in Pod logs | Wrong credentials in secret | Verify secret content with `kubectl get secret ... -o yaml` |

---

### 3. Wrong secret type

Docker registry secrets **must** be type `kubernetes.io/dockerconfigjson`. Generic secrets (`Opaque`) will cause a `FailedMount` error.

```bash
# Check secret type
kubectl get secret registry-snapshot-push-secret -o jsonpath='{.type}'
# Expected: kubernetes.io/dockerconfigjson

# If wrong type, delete and recreate:
kubectl delete secret registry-snapshot-push-secret
kubectl create secret docker-registry registry-snapshot-push-secret \
  --docker-server=<registry> \
  --docker-username=<user> \
  --docker-password=<token>
```

---

### 4. Registry unreachable (`Committing` → `Failed` after timeout)

**Symptoms**: Commit Job Pod starts, runs for a while, then fails with a push error.

**Check:**

```bash
# Inspect commit Pod logs
kubectl logs <commit-pod-name> -n <namespace>

# Test registry connectivity from a node
kubectl run registry-test --rm -it --image=alpine -- \
  wget -O- https://<registry>/v2/ --timeout=5
```

**Common causes:**
- Registry behind a firewall not accessible from cluster nodes
- Self-signed TLS certificate not trusted by containerd
- Wrong registry URL (http vs https)

---

### 5. Resume accepted but the runtime Pod fails to start

**Cause**: The snapshot image cannot be pulled.

```bash
kubectl describe pod <resumed-pod-name> -n <namespace>
# Look for: ErrImagePull or ImagePullBackOff
```

**Check:**
- `--resume-pull-secret` is correctly configured and the Secret exists in the namespace
- The registry is accessible from the node pulling the image
- The snapshot image was successfully pushed during pause (check `status.containers`)

---

### 6. SandboxSnapshot not being processed (no status)

**Cause**: The OpenSandbox controller is not running.

```bash
kubectl get pods -n opensandbox-system
kubectl logs -n opensandbox-system deployment/opensandbox-controller-manager
```

---

## Getting Help

- **Documentation**: [OpenSandbox GitHub](https://github.com/opensandbox-group/OpenSandbox)
- **Issues**: [GitHub Issues](https://github.com/opensandbox-group/OpenSandbox/issues)
- **Kubernetes controller**: [Kubernetes Overview](/architecture/control-plane/operator)

## Measure resume latency

See [Kubernetes Resume Benchmark](/guides/kubernetes-resume-benchmark) for an opt-in rootfs benchmark with raw samples, P50/P95, failure reporting, and explicit cache conditions.

## Create a differently sized RootfsOnly Sandbox

Upgrade FSB first, then OpenSandbox. On a ResourceAware Firecracker pool, send `POST /sandboxes` with a ready RootfsOnly snapshot, for example `{"snapshotId":"snap_example","timeout":600,"resourceLimits":{"memory":"4Gi"}}`. Only memory changes. Omitting both limits inherits the captured CPU and memory. Supply any dimensions missing from older snapshots. Full restores retain their recorded shape.

The Python, Go, JavaScript, Kotlin and C# SDKs ordinarily send their default CPU and memory. Use the SDK's snapshot resource inheritance option (or Kotlin's `rootfsSnapshot`) to omit defaults. An older FastPath that lacks the RootfsOnly create RPC returns HTTP 503 through OpenSandbox. Upgrade fast-sandbox before retrying.
