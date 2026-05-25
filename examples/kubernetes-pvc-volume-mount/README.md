# Kubernetes PVC Volume Mount Example

This example demonstrates how to mount Kubernetes PersistentVolumeClaims (PVC) into OpenSandbox containers for persistent storage. Data written to a PVC persists across sandbox lifecycles -- when a sandbox is killed and a new one is created with the same PVC, previously written data is still available.

## Prerequisites

### 1. CSI Driver

Kubernetes PVC requires a [Container Storage Interface (CSI)](https://kubernetes-csi.github.io/docs/drivers.html) driver to provision and manage storage. Install the CSI driver that matches your storage backend. Refer to your storage vendor's documentation for installation instructions.

For example, [Alibaba Cloud CSI Driver](https://github.com/kubernetes-sigs/alibaba-cloud-csi-driver) supports the following storage types:

- **Cloud Disk (EBS)** -- block storage, suitable for high-performance single-node read-write scenarios
- **NAS** -- shared file storage, supports multi-node read-write (ReadWriteMany)
- **OSS** -- object storage, suitable for large-scale data read and shared access scenarios
- **CPFS** -- high-performance parallel file system
- **LVM** -- local volume management

### 2. Create a PersistentVolumeClaim

Create a PVC in the namespace where OpenSandbox runs:

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
```

Verify the PVC is bound:

```shell
kubectl get pvc my-pvc -n opensandbox
```

### 3. OpenSandbox Server

Ensure the OpenSandbox server is running with Kubernetes runtime and BatchSandbox workload provider.

### 4. Python SDK

```shell
uv pip install opensandbox
```

## Run the Example

```shell
export OPEN_SANDBOX_API_KEY=your-api-key
export OPEN_SANDBOX_BASE_URL=http://localhost:8080
export SANDBOX_PVC_NAME=my-pvc

python examples/kubernetes-pvc-volume-mount/main.py
```

## What the Example Does

1. Creates a sandbox with a PVC mounted at `/mnt/data`
2. Writes a test file to the PVC
3. Reads the file back to verify
4. Kills the sandbox
5. Creates a new sandbox with the same PVC
6. Reads the file again to verify data persistence across sandbox lifecycles

## Usage

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import PVC, Volume

sandbox = await Sandbox.create(
    image="python:3.11",
    volumes=[
        Volume(
            name="data-volume",
            pvc=PVC(claimName="my-pvc"),
            mountPath="/mnt/data",
            readOnly=False,
        ),
    ],
)

# Run commands against the mounted volume
result = await sandbox.commands.run("ls -la /mnt/data")
output = "\n".join(msg.text for msg in result.logs.stdout)
print(output)
```

## Important Notes

- **Pool mode does not support volumes.** Use template mode instead.
- PVC must exist before creating the sandbox.
- PVC is not deleted when the sandbox is killed.
- Multiple sandboxes can mount the same PVC if the access mode allows (e.g. `ReadWriteMany`).

## References

- [OSEP-0003: Volume and VolumeBinding Support](../../oseps/0003-volume-and-volumebinding-support.md)
- [Kubernetes CSI Drivers](https://kubernetes-csi.github.io/docs/drivers.html)
- [Alibaba Cloud CSI Driver](https://github.com/kubernetes-sigs/alibaba-cloud-csi-driver)
