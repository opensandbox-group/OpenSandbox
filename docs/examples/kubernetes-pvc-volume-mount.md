---
title: Kubernetes PVC
description: Mount Kubernetes PersistentVolumeClaims into OpenSandbox containers for persistent storage.
---

# Kubernetes PVC Volume Mount

This example shows how to back a sandbox with a Kubernetes [PersistentVolumeClaim](https://kubernetes.io/docs/concepts/storage/persistent-volumes/) (PVC). Data written to a PVC outlives the sandbox process, so files are still there when a follow-up sandbox mounts the same claim.

OpenSandbox supports two modes for sourcing the PVC. Pick based on who owns the claim's lifecycle:

| Mode | `createIfNotExists` | `deleteOnSandboxTermination` | Who owns the PVC | When to use |
|------|---------------------|------------------------------|------------------|-------------|
| **Bring your own** | `false` | _ignored_ | You (provisioned out-of-band) | Long-lived shared storage; model caches; baseline datasets that multiple sandboxes reuse |
| **Server-managed, persistent** | `true` | `false` (default) | You (after first create) | Provision on first use, keep across sandbox lifecycles |
| **Server-managed, ephemeral** | `true` | `true` | Server | Scratch storage scoped to a single sandbox; auto-cleaned when the sandbox terminates |

Both modes mount the resulting PVC the same way; the difference is only in provisioning and cleanup. See [PVC lifecycle](#pvc-lifecycle) for the cleanup mechanics in detail.

## Prerequisites

### CSI Driver

Kubernetes PVCs need a [Container Storage Interface (CSI)](https://kubernetes-csi.github.io/docs/drivers.html) driver to provision and attach storage. Install one that matches your storage backend. For example, the [Alibaba Cloud CSI Driver](https://github.com/kubernetes-sigs/alibaba-cloud-csi-driver) covers:

- **Cloud Disk (EBS)** -- block storage; high-performance single-node read-write
- **NAS** -- shared file storage; multi-node read-write (`ReadWriteMany`)
- **OSS** -- object storage; large-scale shared read
- **CPFS** -- high-performance parallel file system
- **LVM** -- local volume management

### OpenSandbox Server

The server must run on the Kubernetes runtime with the BatchSandbox workload provider. The stock Helm chart grants the RBAC needed by both BYO mounts and server-managed provisioning (`get`/`create`/`list`/`delete`/`patch` on `persistentvolumeclaims`).

### Python SDK

```shell
uv pip install opensandbox
```

## Mode 1: Bring your own PVC

Use this when the PVC is part of your platform setup -- e.g. a shared NAS that several sandboxes reuse, or a pre-warmed disk holding model weights.

### 1. Create the PVC

```yaml
# pvc.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-pvc
  namespace: opensandbox
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: <your-storage-class>
  resources:
    requests:
      storage: 10Gi
```

```shell
kubectl apply -f pvc.yaml
kubectl get pvc my-pvc -n opensandbox        # should be Bound
```

### 2. Mount it from a sandbox

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import PVC, Volume

sandbox = await Sandbox.create(
    image="python:3.11",
    volumes=[
        Volume(
            name="data-volume",
            pvc=PVC(
                claimName="my-pvc",
                createIfNotExists=False,      # never auto-provision a BYO claim
            ),
            mountPath="/mnt/data",
            readOnly=False,
        ),
    ],
)

result = await sandbox.commands.run("ls -la /mnt/data")
print("\n".join(msg.text for msg in result.logs.stdout))
```

The PVC is never deleted by OpenSandbox: when the sandbox terminates, the claim stays bound and the data is available for the next sandbox that mounts it.

### Run the end-to-end example

```shell
export OPEN_SANDBOX_API_KEY=your-api-key
export OPEN_SANDBOX_BASE_URL=http://localhost:8080
export SANDBOX_PVC_NAME=my-pvc

python examples/kubernetes-pvc-volume-mount/main.py
```

![Kubernetes PVC Volume Mount demo](../public/images/kubernetes-pvc-volume-mount-demo.png)

The script creates a sandbox, writes a marker file under `/mnt/data`, kills it, then creates a second sandbox bound to the same PVC and confirms the marker is still there.

## Mode 2: Server-managed PVC

Use this when the sandbox should own its storage. The server creates the PVC on demand the first time the claim name is referenced. You control whether the claim survives sandbox termination through `deleteOnSandboxTermination`.

### Provision and persist (default)

The classic "first sandbox provisions, later sandboxes reuse" pattern -- e.g. a warm cache that survives crashes and restarts but never needs manual `kubectl apply`.

```python
sandbox = await Sandbox.create(
    image="python:3.11",
    volumes=[
        Volume(
            name="cache",
            pvc=PVC(
                claimName="agent-cache",
                createIfNotExists=True,                   # auto-provision on first use
                deleteOnSandboxTermination=False,         # default: keep the PVC
                storageClass="alibaba-cloud-disk-ssd",    # optional; defaults to cluster default
                storage="20Gi",                           # optional; defaults to server config
                accessModes=["ReadWriteOnce"],            # optional; defaults to ReadWriteOnce
            ),
            mountPath="/mnt/cache",
        ),
    ],
)
```

The first call provisions `agent-cache`; subsequent calls with the same `claimName` mount the existing PVC and skip provisioning. The PVC remains until you delete it with `kubectl`.

### Provision and clean up

Ephemeral scratch storage scoped to one sandbox -- the server reclaims the PVC when the sandbox terminates (including TTL expiry).

```python
sandbox = await Sandbox.create(
    image="python:3.11",
    timeout=600,                                   # 10-minute sandbox
    volumes=[
        Volume(
            name="scratch",
            pvc=PVC(
                claimName=f"scratch-{run_id}",   # unique per run; opted-in PVCs are owned exclusively
                createIfNotExists=True,
                deleteOnSandboxTermination=True,
                storage="5Gi",
            ),
            mountPath="/mnt/scratch",
        ),
    ],
)
```

::: tip Cleanup scope
The server only deletes PVCs it provisioned with this opt-in. Pre-existing PVCs and PVCs provisioned with `deleteOnSandboxTermination=false` are never touched. An opted-in PVC is exclusively owned by the sandbox that created it — a second sandbox attempting to mount the same `claimName` is rejected with `409 CONFLICT`. Use unique `claimName`s per sandbox, or use a non-opted-in / pre-existing PVC if you need to share storage.
:::

## PVC lifecycle

The cleanup behavior follows the mode used at create time:

| Source | Cleanup on sandbox termination |
|--------|-------------------------------|
| Pre-existing PVC (BYO) | Never touched by the server. |
| Auto-created, `deleteOnSandboxTermination=false` (default) | PVC persists; caller owns cleanup. |
| Auto-created, `deleteOnSandboxTermination=true` | Server deletes the PVC. |

For opted-in PVCs, the server labels them with `opensandbox.io/volume-managed-by=server` and `opensandbox.io/id=<sandbox-id>`, then runs cleanup through two layered paths:

1. **`ownerReferences`** -- the PVC is patched to point at the sandbox's workload custom resource right after creation. Kubernetes garbage collection cascade-deletes the PVC whenever the CR is removed, including controller-driven TTL expiry that never reaches the `DELETE /sandboxes/{id}` API.
2. **Label-selector sweep** -- on `DELETE /sandboxes/{id}`, the server lists PVCs by those labels and deletes them best-effort. This catches cases where the `ownerReferences` patch failed (e.g. RBAC) and ensures the PVC is gone as soon as the API returns.

Both paths only match server-labeled PVCs, so BYO and opted-out claims are never reclaimed. The underlying PV follows its `StorageClass.reclaimPolicy` once the PVC is deleted.

## Important notes

::: warning
- **Pool mode does not support volumes.** Use template mode instead.
- Multiple sandboxes can mount the same PVC if the access mode allows (e.g. `ReadWriteMany`) — but only when the PVC is **not** opted into auto-cleanup. PVCs created with `deleteOnSandboxTermination=true` are owned exclusively by the creating sandbox; the server rejects attempts by other sandboxes to mount them with `409 CONFLICT`.
- All mounts of the same `claimName` in a single request must agree on `createIfNotExists` and `deleteOnSandboxTermination`; mismatches are rejected with `400 INVALID_PARAMETER`.
:::

## References

- [OSEP-0003: Volume and VolumeBinding Support](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0003-volume-and-volumebinding-support.md)
- [Kubernetes CSI Drivers](https://kubernetes-csi.github.io/docs/drivers.html)
- [Alibaba Cloud CSI Driver](https://github.com/kubernetes-sigs/alibaba-cloud-csi-driver)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/kubernetes-pvc-volume-mount)
