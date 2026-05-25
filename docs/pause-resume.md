# Pause and Resume Guide

This guide explains how to use the pause and resume features for Kubernetes-backed sandboxes in OpenSandbox. Pause commits the sandbox's root filesystem as an OCI image and releases cluster resources. Resume restores the sandbox from that image.

## Table of Contents

- [Overview](#overview)
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

- `Pending`: snapshot request accepted, waiting to resolve source Pod / create commit Job
- `Committing`: commit Job is running and pushing snapshot images
- `Succeed`: snapshot is ready and can be used for the next resume
- `Failed`: snapshot creation failed

### What Is Preserved

| | Preserved? |
|--|-----------|
| Root filesystem contents | ✅ Yes — committed as OCI image |
| Environment variables | ✅ Yes — from BatchSandbox template |
| Running processes / memory | ❌ No — process state is not checkpointed |
| Explicit volume mounts | Depends on volume type |

Pause/resume is currently single-replica only. The internal pause snapshot records one source Pod's container images and does not store per-replica state, so the Kubernetes controller rejects pause requests unless `BatchSandbox.spec.replicas=1`.

---

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
    │ nerdctl: commit container rootfs → OCI image
    │ nerdctl: push to registry
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
| `--snapshot-registry` | string | `""` | **Required.** OCI registry prefix. Images are stored as `<registry>/<sandboxName>-<container>:snap-gen<N>`. |
| `--snapshot-registry-insecure` | bool | `false` | Enables insecure registry mode for snapshot push operations. Use only for HTTP or self-signed local registries. |
| `--snapshot-push-secret` | string | `""` | Kubernetes Secret name for pushing snapshots. Must be `kubernetes.io/dockerconfigjson` type. |
| `--resume-pull-secret` | string | `""` | Kubernetes Secret name injected into resumed sandboxes for pulling snapshot images. Can be the same as push secret. |
| `--image-committer-image` | string | `"image-committer:dev"` | Image used by commit Jobs. |
| `--commit-job-timeout` | duration | `"10m"` | Timeout for commit Jobs. |

### Helm chart support

The `opensandbox-controller` Helm chart now exposes the snapshot-related controller values directly:

- `controller.snapshot.imageCommitterImage`
- `controller.snapshot.commitJobTimeout`
- `controller.snapshot.registry`
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

The pause is asynchronous. The sandbox transitions through:
`running` → `pausing` → `paused`

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

The sandbox transitions through:
`paused` → `resuming` → `running`

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

Server-managed public snapshots use the same repository layout but a stable
snapshot-id-derived tag:
```
<snapshot-registry>/<sandboxName>-<containerName>:snap-<snapshotIdHex>
```

The controller distinguishes the two modes by owner reference. Pause/resume
snapshots are created by the `BatchSandbox` controller and have a controller
ownerReference to the owning `BatchSandbox`; public snapshots are created by the
Lifecycle server and do not use that ownerReference.

### Commit Job

The controller creates a short-lived Kubernetes `Job` for each pause:

- **Job name**: `<snapshotName>-commit`
- **Node affinity**: Runs on the **same node** as the source Pod (containerd socket access required)
- **Timeout**: 10 minutes (`ActiveDeadlineSeconds`)
- **TTL**: 5 minutes after completion (`TTLSecondsAfterFinished`)
- **Image**: `image-committer` (configurable via controller `--image-committer-image` flag)

The commit Job mounts the host containerd socket from the source node and runs as UID 0. This gives the `image-committer` image node-level container runtime access. Use only a trusted image, preferably pinned by digest or controlled by an admission policy.

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
| `conditions` | list | `Ready` / `Failed` conditions with reason and message |
| `sourcePodName` | string | Pod name used for commit |
| `sourceNodeName` | string | Node where commit Job runs |
| `containers` | list | `{containerName, imageUri, imageDigest}` per container |
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

- **Documentation**: [OpenSandbox GitHub](https://github.com/alibaba/OpenSandbox)
- **Issues**: [GitHub Issues](https://github.com/alibaba/OpenSandbox/issues)
- **Design Document**: [OSEP-0008](/oseps/0008-pause-resume-rootfs-snapshot)
- **Kubernetes controller**: [`kubernetes/README.md`](../kubernetes/README.md#pause-and-resume-rootfs-snapshot)
